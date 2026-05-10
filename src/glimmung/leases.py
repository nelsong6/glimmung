"""Lease lifecycle: acquire, heartbeat, release, sweep, promote.

Acquisition is the only interesting bit — it uses optimistic concurrency
on the hosts container's `_etag` to grab a free host without colliding
with concurrent acquirers. The retry loop is bounded by the number of
free hosts (always small at this scale).

Native Kubernetes phases use the same lease documents for queue visibility
and cancellation, but their capacity is virtual: capped by project/global
active native leases instead of registered host rows.

`promote_pending` re-tries pending leases against current free capacity.
Called periodically and after every release.
"""

import secrets
from datetime import UTC, datetime, timedelta
from typing import Any

from azure.core import MatchConditions
from azure.cosmos.exceptions import (
    CosmosAccessConditionFailedError,
    CosmosResourceExistsError,
    CosmosResourceNotFoundError,
)
from ulid import ULID

from glimmung.db import Cosmos, query_all
from glimmung.models import Host, Lease, LeaseState
from glimmung.settings import Settings


NATIVE_K8S_HOST = "native-k8s"
NATIVE_K8S_METADATA_KEY = "native_k8s"
NATIVE_SLOT_INDEX_METADATA_KEY = "native_slot_index"
NATIVE_SLOT_NAME_METADATA_KEY = "native_slot_name"
_MAX_CONFLICT_RETRIES = 3
_COUNTER_PREFIX = "__counter:lease-number:"


def _utcnow_iso() -> str:
    return datetime.now(UTC).isoformat()


def _callback_token() -> str:
    return secrets.token_urlsafe(24)


def _with_callback_token(metadata: dict[str, Any]) -> dict[str, Any]:
    out = {**metadata}
    out.setdefault("lease_callback_token", _callback_token())
    return out


def _strip_meta(doc: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in doc.items() if not k.startswith("_")}


async def next_lease_number(cosmos: Cosmos, *, project: str) -> int:
    """Allocate the next human-facing lease number scoped to one project."""
    counter_id = _counter_id(project)
    for attempt in range(_MAX_CONFLICT_RETRIES):
        try:
            doc = await cosmos.leases.read_item(item=counter_id, partition_key=project)
        except CosmosResourceNotFoundError:
            try:
                seed = await _seed_lease_number_counter(cosmos, project=project)
                return int(seed["last_allocated"])
            except CosmosResourceExistsError:
                continue

        current_next = int(doc.get("next_lease_number") or 1)
        updated = {
            **doc,
            "next_lease_number": current_next + 1,
            "updated_at": _utcnow_iso(),
        }
        try:
            await cosmos.leases.replace_item(
                item=counter_id,
                body=_strip_meta(updated),
                etag=doc["_etag"],
                match_condition=MatchConditions.IfNotModified,
            )
            return current_next
        except CosmosAccessConditionFailedError:
            if attempt == _MAX_CONFLICT_RETRIES - 1:
                raise
            continue
    raise RuntimeError("unreachable")


def _counter_id(project: str) -> str:
    return f"{_COUNTER_PREFIX}{project}"


async def _seed_lease_number_counter(cosmos: Cosmos, *, project: str) -> dict[str, Any]:
    highest = await _highest_lease_number(cosmos, project=project)
    first = highest + 1
    now = _utcnow_iso()
    doc = {
        "id": _counter_id(project),
        "project": project,
        "kind": "lease_number_counter",
        "last_allocated": first,
        "next_lease_number": first + 1,
        "created_at": now,
        "updated_at": now,
    }
    await cosmos.leases.create_item(doc)
    return doc


async def _highest_lease_number(cosmos: Cosmos, *, project: str) -> int:
    docs = await query_all(
        cosmos.leases,
        "SELECT * FROM c WHERE c.project = @p AND IS_DEFINED(c.leaseNumber)",
        parameters=[{"name": "@p", "value": project}],
    )
    highest = 0
    for doc in docs:
        try:
            highest = max(highest, int(doc.get("leaseNumber") or 0))
        except (TypeError, ValueError):
            continue
    return highest


def _matches(host_caps: dict[str, Any], required: dict[str, Any]) -> bool:
    """`required` is a subset of `host_caps`. Lists check ARRAY_CONTAINS_ALL semantics."""
    for key, want in required.items():
        have = host_caps.get(key)
        if isinstance(want, list):
            if not isinstance(have, list) or not set(want).issubset(have):
                return False
        else:
            if have != want:
                return False
    return True


async def acquire(
    cosmos: Cosmos,
    settings: Settings,
    *,
    project: str,
    workflow: str | None = None,
    requirements: dict[str, Any],
    metadata: dict[str, Any],
    ttl_seconds: int | None = None,
) -> tuple[Lease, Host | None]:
    """Try to acquire a host. Returns (lease, host) — host is None if no free
    capacity matches; the lease is created in `pending` state and the caller
    can poll or be notified later. If a host is found, the lease is `active`.
    """
    now = _utcnow_iso()
    lease_id = str(ULID())
    lease_number = await next_lease_number(cosmos, project=project)
    ttl = ttl_seconds or settings.lease_default_ttl_seconds
    metadata = _with_callback_token(metadata)

    # Cross-partition query — hosts container is small, this is cheap.
    candidates_raw = await query_all(
        cosmos.hosts,
        "SELECT * FROM c WHERE (NOT IS_DEFINED(c.currentLeaseId) OR c.currentLeaseId = null) AND c.drained = false",
    )
    candidates = [c for c in candidates_raw if _matches(c.get("capabilities", {}), requirements)]
    # Bin-pack: prefer the host that's been idle longest (NULLs first).
    candidates.sort(key=lambda h: h.get("lastUsedAt") or "")

    for candidate in candidates:
        try:
            updated = {
                **candidate,
                "currentLeaseId": lease_id,
                "lastUsedAt": now,
                "lastHeartbeat": now,
            }
            await cosmos.hosts.replace_item(
                item=candidate["id"],
                body=updated,
                etag=candidate["_etag"],
                match_condition=MatchConditions.IfNotModified,
            )
            lease = Lease(
                id=lease_id,
                lease_number=lease_number,
                project=project,
                workflow=workflow,
                host=candidate["name"],
                state=LeaseState.CLAIMED,
                requirements=requirements,
                metadata=metadata,
                requested_at=datetime.fromisoformat(now),
                assigned_at=datetime.fromisoformat(now),
                ttl_seconds=ttl,
            )
            await cosmos.leases.create_item(_lease_to_doc(lease))
            return lease, Host.model_validate(_camel_to_snake(updated))
        except CosmosAccessConditionFailedError:
            # Someone else grabbed this host between our query and replace.
            # Try the next candidate.
            continue

    # No free host matched. Persist a pending lease so the dashboard sees it.
    pending = Lease(
        id=lease_id,
        lease_number=lease_number,
        project=project,
        workflow=workflow,
        host=None,
        state=LeaseState.PENDING,
        requirements=requirements,
        metadata=metadata,
        requested_at=datetime.fromisoformat(now),
        ttl_seconds=ttl,
    )
    await cosmos.leases.create_item(_lease_to_doc(pending))
    return pending, None


def _native_host(now: str | None = None) -> Host:
    ts = datetime.fromisoformat(now or _utcnow_iso())
    return Host(
        id=NATIVE_K8S_HOST,
        name=NATIVE_K8S_HOST,
        capabilities={"native_k8s": True},
        current_lease_id=None,
        last_heartbeat=ts,
        last_used_at=ts,
        drained=False,
        created_at=ts,
    )


async def acquire_native(
    cosmos: Cosmos,
    settings: Settings,
    *,
    project: str,
    workflow: str | None = None,
    requirements: dict[str, Any],
    metadata: dict[str, Any],
    ttl_seconds: int | None = None,
) -> tuple[Lease, Host | None]:
    """Acquire virtual native-runner capacity.

    The lease is ACTIVE immediately when both the per-project and global
    native concurrency caps have room; otherwise it is PENDING and the
    promote loop activates it later. `requirements` are retained on the lease
    for observability/future scheduling but are not matched against host rows.
    """
    now = _utcnow_iso()
    ttl = ttl_seconds or settings.lease_default_ttl_seconds
    lease_number = await next_lease_number(cosmos, project=project)
    native_metadata = _with_callback_token({**metadata, NATIVE_K8S_METADATA_KEY: True})
    slot_index = await _available_native_slot(cosmos, settings, project=project, metadata=metadata)
    if slot_index is not None:
        _set_native_slot_metadata(native_metadata, project=project, slot_index=slot_index)
    state = LeaseState.CLAIMED if slot_index is not None else LeaseState.PENDING
    lease = Lease(
        id=str(ULID()),
        lease_number=lease_number,
        project=project,
        workflow=workflow,
        host=NATIVE_K8S_HOST if state == LeaseState.CLAIMED else None,
        state=state,
        requirements=requirements,
        metadata=native_metadata,
        requested_at=datetime.fromisoformat(now),
        assigned_at=datetime.fromisoformat(now) if state == LeaseState.CLAIMED else None,
        ttl_seconds=ttl,
    )
    await cosmos.leases.create_item(_lease_to_doc(lease))
    return lease, _native_host(now) if state == LeaseState.CLAIMED else None


async def native_capacity_available(
    cosmos: Cosmos,
    settings: Settings,
    *,
    project: str,
) -> bool:
    return await _available_native_slot(cosmos, settings, project=project, metadata={}) is not None


async def _available_native_slot(
    cosmos: Cosmos,
    settings: Settings,
    *,
    project: str,
    metadata: dict[str, Any],
) -> int | None:
    active = await _active_native_leases(cosmos)
    project_cap = _native_project_cap(settings)
    global_cap = max(1, int(getattr(settings, "native_runner_global_concurrency", 5)))
    project_active = [doc for doc in active if doc.get("project") == project]
    if len(project_active) >= project_cap or len(active) >= global_cap:
        return None

    used = {
        slot
        for doc in project_active
        if (slot := _native_slot_index(doc.get("metadata") or {})) is not None
    }
    preferred = _preferred_native_slot(metadata)
    if preferred is not None:
        if 1 <= preferred <= project_cap and preferred not in used:
            return preferred
        return None
    for slot in range(1, project_cap + 1):
        if slot not in used:
            return slot
    return None


async def _active_native_leases(cosmos: Cosmos) -> list[dict[str, Any]]:
    return await query_all(
        cosmos.leases,
        "SELECT * FROM c WHERE c.state = @s AND c.metadata.native_k8s = true",
        parameters=[{"name": "@s", "value": LeaseState.CLAIMED.value}],
    )


def _native_project_cap(settings: Settings) -> int:
    return max(1, int(getattr(settings, "native_runner_project_concurrency", 5)))


def _preferred_native_slot(metadata: dict[str, Any]) -> int | None:
    for value in (
        metadata.get(NATIVE_SLOT_INDEX_METADATA_KEY),
        (metadata.get("phase_inputs") or {}).get("validation_slot_index")
        if isinstance(metadata.get("phase_inputs"), dict)
        else None,
    ):
        try:
            slot = int(str(value))
        except (TypeError, ValueError):
            continue
        if slot > 0:
            return slot
    return None


def _native_slot_index(metadata: dict[str, Any]) -> int | None:
    try:
        slot = int(str(metadata.get(NATIVE_SLOT_INDEX_METADATA_KEY) or ""))
    except ValueError:
        return None
    return slot if slot > 0 else None


def _set_native_slot_metadata(
    metadata: dict[str, Any],
    *,
    project: str,
    slot_index: int,
) -> None:
    metadata[NATIVE_SLOT_INDEX_METADATA_KEY] = str(slot_index)
    existing_slot_name = str(metadata.get(NATIVE_SLOT_NAME_METADATA_KEY) or "").strip()
    if existing_slot_name:
        metadata[NATIVE_SLOT_NAME_METADATA_KEY] = existing_slot_name
        return
    slot_prefix = str(metadata.get("native_slot_prefix") or project).strip().strip(".")
    metadata[NATIVE_SLOT_NAME_METADATA_KEY] = f"{slot_prefix or project}-{slot_index}"


async def heartbeat(cosmos: Cosmos, lease_id: str, project: str) -> Lease:
    lease_doc = await cosmos.leases.read_item(item=lease_id, partition_key=project)
    if lease_doc["state"] != LeaseState.CLAIMED.value:
        raise ValueError(f"lease {lease_id} is in state {lease_doc['state']}, cannot heartbeat")

    host_name = lease_doc["host"]
    host_doc = await cosmos.hosts.read_item(item=host_name, partition_key=host_name)
    host_doc["lastHeartbeat"] = _utcnow_iso()
    await cosmos.hosts.replace_item(item=host_name, body=host_doc)
    return Lease.model_validate(_camel_to_snake(lease_doc))


async def release(cosmos: Cosmos, lease_id: str, project: str) -> Lease:
    lease_doc = await cosmos.leases.read_item(item=lease_id, partition_key=project)
    if lease_doc["state"] in (LeaseState.RELEASED.value, LeaseState.EXPIRED.value):
        return Lease.model_validate(_camel_to_snake(lease_doc))

    host_name = lease_doc.get("host")
    if host_name:
        try:
            host_doc = await cosmos.hosts.read_item(item=host_name, partition_key=host_name)
            if host_doc.get("currentLeaseId") == lease_id:
                host_doc["currentLeaseId"] = None
                await cosmos.hosts.replace_item(item=host_name, body=host_doc)
        except CosmosResourceNotFoundError:
            pass  # host deleted out from under us; lease still gets marked released

    lease_doc["state"] = LeaseState.RELEASED.value
    lease_doc["releasedAt"] = _utcnow_iso()
    await cosmos.leases.replace_item(item=lease_id, body=lease_doc)
    return Lease.model_validate(_camel_to_snake(lease_doc))


async def try_assign_pending(cosmos: Cosmos, lease_doc: dict[str, Any]) -> Host | None:
    """Try to assign a free host to an already-persisted pending lease.
    Returns the host if successful, None if no capacity matches."""
    if (lease_doc.get("metadata") or {}).get(NATIVE_K8S_METADATA_KEY):
        return None
    requirements = lease_doc.get("requirements", {})
    candidates_raw = await query_all(
        cosmos.hosts,
        "SELECT * FROM c WHERE (NOT IS_DEFINED(c.currentLeaseId) OR c.currentLeaseId = null) AND c.drained = false",
    )
    candidates = [c for c in candidates_raw if _matches(c.get("capabilities", {}), requirements)]
    candidates.sort(key=lambda h: h.get("lastUsedAt") or "")

    now = _utcnow_iso()
    for candidate in candidates:
        try:
            updated = {
                **candidate,
                "currentLeaseId": lease_doc["id"],
                "lastUsedAt": now,
                "lastHeartbeat": now,
            }
            await cosmos.hosts.replace_item(
                item=candidate["id"],
                body=updated,
                etag=candidate["_etag"],
                match_condition=MatchConditions.IfNotModified,
            )
            lease_doc["host"] = candidate["name"]
            lease_doc["state"] = LeaseState.CLAIMED.value
            lease_doc["assignedAt"] = now
            await cosmos.leases.replace_item(item=lease_doc["id"], body=lease_doc)
            return Host.model_validate(_camel_to_snake(updated))
        except CosmosAccessConditionFailedError:
            continue
    return None


async def try_activate_native_pending(
    cosmos: Cosmos,
    settings: Settings,
    lease_doc: dict[str, Any],
) -> Host | None:
    """Try to activate a pending native lease against virtual capacity."""
    if not (lease_doc.get("metadata") or {}).get(NATIVE_K8S_METADATA_KEY):
        return None
    if lease_doc.get("state") != LeaseState.PENDING.value:
        return None
    metadata = dict(lease_doc.get("metadata") or {})
    slot_index = await _available_native_slot(
        cosmos,
        settings,
        project=lease_doc["project"],
        metadata=metadata,
    )
    if slot_index is None:
        return None

    now = _utcnow_iso()
    _set_native_slot_metadata(metadata, project=lease_doc["project"], slot_index=slot_index)
    lease_doc["metadata"] = metadata
    lease_doc["host"] = NATIVE_K8S_HOST
    lease_doc["state"] = LeaseState.CLAIMED.value
    lease_doc["assignedAt"] = now
    await cosmos.leases.replace_item(item=lease_doc["id"], body=lease_doc)
    return _native_host(now)


async def promote_pending_native(cosmos: Cosmos, settings: Settings) -> list[tuple[dict[str, Any], Host]]:
    """Activate pending native leases while virtual capacity is available."""
    pending = await query_all(
        cosmos.leases,
        (
            "SELECT * FROM c WHERE c.state = @s AND c.metadata.native_k8s = true "
            "ORDER BY c.requestedAt ASC"
        ),
        parameters=[{"name": "@s", "value": LeaseState.PENDING.value}],
    )
    assigned: list[tuple[dict[str, Any], Host]] = []
    for lease_doc in pending:
        host = await try_activate_native_pending(cosmos, settings, lease_doc)
        if host is not None:
            assigned.append((lease_doc, host))
    return assigned


async def promote_pending(cosmos: Cosmos) -> list[tuple[dict[str, Any], Host]]:
    """Walk pending leases (oldest first) and try to assign each. Returns
    list of (lease_doc, host) for newly-assigned leases so the caller can
    fire workflow_dispatch for them."""
    pending = await query_all(
        cosmos.leases,
        "SELECT * FROM c WHERE c.state = @s ORDER BY c.requestedAt ASC",
        parameters=[{"name": "@s", "value": LeaseState.PENDING.value}],
    )
    assigned: list[tuple[dict[str, Any], Host]] = []
    for lease_doc in pending:
        host = await try_assign_pending(cosmos, lease_doc)
        if host is not None:
            assigned.append((lease_doc, host))
    return assigned


async def sweep_expired(cosmos: Cosmos, settings: Settings) -> int:
    """Reclaim hosts whose holders haven't heartbeated within the grace window,
    and reap stranded native-k8s leases whose env-destroy callback never fired.

    Runs on a timer. Returns count of leases expired.
    """
    cutoff = (datetime.now(UTC) - timedelta(seconds=settings.lease_default_ttl_seconds)).isoformat()
    stale_hosts = await query_all(
        cosmos.hosts,
        "SELECT * FROM c WHERE IS_DEFINED(c.currentLeaseId) AND c.currentLeaseId != null AND c.lastHeartbeat < @cutoff",
        parameters=[{"name": "@cutoff", "value": cutoff}],
    )

    expired_count = 0
    for host_doc in stale_hosts:
        lease_id = host_doc["currentLeaseId"]
        # Find the lease — cross-partition because we don't know the project here.
        lease_results = await query_all(
            cosmos.leases,
            "SELECT * FROM c WHERE c.id = @id",
            parameters=[{"name": "@id", "value": lease_id}],
        )
        for lease_doc in lease_results:
            lease_doc["state"] = LeaseState.EXPIRED.value
            lease_doc["releasedAt"] = _utcnow_iso()
            await cosmos.leases.replace_item(item=lease_doc["id"], body=lease_doc)
            expired_count += 1

        host_doc["currentLeaseId"] = None
        try:
            await cosmos.hosts.replace_item(item=host_doc["id"], body=host_doc)
        except CosmosAccessConditionFailedError:
            continue

    # Native-k8s leases have no host record (the host is virtual / in-memory),
    # so the host-driven path above can't reach them. Sweep them directly off
    # the leases container using `assignedAt` as the staleness reference: if
    # an active native lease has been assigned for longer than the TTL, the
    # env-destroy job's `native_completed` callback never fired and the lease
    # would otherwise stay ACTIVE forever, holding a virtual capacity slot.
    stale_native = await query_all(
        cosmos.leases,
        (
            "SELECT * FROM c WHERE c.state = @s "
            "AND c.metadata.native_k8s = true "
            "AND c.assignedAt < @cutoff"
        ),
        parameters=[
            {"name": "@s", "value": LeaseState.CLAIMED.value},
            {"name": "@cutoff", "value": cutoff},
        ],
    )
    for lease_doc in stale_native:
        lease_doc["state"] = LeaseState.EXPIRED.value
        lease_doc["releasedAt"] = _utcnow_iso()
        await cosmos.leases.replace_item(item=lease_doc["id"], body=lease_doc)
        expired_count += 1

    return expired_count


def _lease_to_doc(lease: Lease) -> dict[str, Any]:
    return {
        "id": lease.id,
        "leaseNumber": lease.lease_number,
        "project": lease.project,
        "workflow": lease.workflow,
        "host": lease.host,
        "state": lease.state.value,
        "requirements": lease.requirements,
        "metadata": lease.metadata,
        "requestedAt": lease.requested_at.isoformat(),
        "assignedAt": lease.assigned_at.isoformat() if lease.assigned_at else None,
        "releasedAt": lease.released_at.isoformat() if lease.released_at else None,
        "ttlSeconds": lease.ttl_seconds,
    }


def _camel_to_snake(d: dict[str, Any]) -> dict[str, Any]:
    """Convert Cosmos's camelCase fields to the snake_case our pydantic models expect."""
    mapping = {
        "tokenHash": "token_hash",
        "currentLeaseId": "current_lease_id",
        "lastHeartbeat": "last_heartbeat",
        "lastUsedAt": "last_used_at",
        "createdAt": "created_at",
        "requestedAt": "requested_at",
        "assignedAt": "assigned_at",
        "releasedAt": "released_at",
        "ttlSeconds": "ttl_seconds",
        "leaseNumber": "lease_number",
        "githubRepo": "github_repo",
        "workflowFilename": "workflow_filename",
        "workflowRef": "workflow_ref",
        "triggerLabel": "trigger_label",
        "defaultRequirements": "default_requirements",
        "retryWorkflowFilename": "retry_workflow_filename",
        "triageWorkflowFilename": "triage_workflow_filename",
        "defaultBudget": "default_budget",
    }
    out: dict[str, Any] = {}
    for k, v in d.items():
        if k.startswith("_"):
            continue
        out[mapping.get(k, k)] = v
    return out

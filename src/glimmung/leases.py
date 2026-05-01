"""Lease lifecycle: acquire, heartbeat, release, sweep, promote.

Acquisition is the only interesting bit — it uses optimistic concurrency
on the hosts container's `_etag` to grab a free host without colliding
with concurrent acquirers. The retry loop is bounded by the number of
free hosts (always small at this scale).

`promote_pending` re-tries pending leases against current free capacity.
Called periodically and after every release.
"""

from datetime import UTC, datetime, timedelta
from typing import Any

from azure.core import MatchConditions
from azure.cosmos.exceptions import CosmosAccessConditionFailedError, CosmosResourceNotFoundError
from ulid import ULID

from glimmung.db import Cosmos, query_all
from glimmung.models import Host, Lease, LeaseState
from glimmung.settings import Settings


def _utcnow_iso() -> str:
    return datetime.now(UTC).isoformat()


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
    ttl = ttl_seconds or settings.lease_default_ttl_seconds

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
                project=project,
                workflow=workflow,
                host=candidate["name"],
                state=LeaseState.ACTIVE,
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


async def heartbeat(cosmos: Cosmos, lease_id: str, project: str) -> Lease:
    lease_doc = await cosmos.leases.read_item(item=lease_id, partition_key=project)
    if lease_doc["state"] != LeaseState.ACTIVE.value:
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
            lease_doc["state"] = LeaseState.ACTIVE.value
            lease_doc["assignedAt"] = now
            await cosmos.leases.replace_item(item=lease_doc["id"], body=lease_doc)
            return Host.model_validate(_camel_to_snake(updated))
        except CosmosAccessConditionFailedError:
            continue
    return None


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
    """Reclaim hosts whose holders haven't heartbeated within the grace window.

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
    return expired_count


def _lease_to_doc(lease: Lease) -> dict[str, Any]:
    return {
        "id": lease.id,
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

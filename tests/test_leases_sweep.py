"""Sweep loop tests for the lease primitives.

Two reap paths exist: the host-driven path (traditional self-hosted GH
runner leases stamped against a real `hosts` row) and the lease-driven
native-k8s path (virtual host, no row to consult). Both rely on the
same TTL — `lease_default_ttl_seconds`.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from types import SimpleNamespace
from typing import Any

import pytest

from glimmung import leases as lease_ops
from glimmung.models import LeaseState

from tests.cosmos_fake import FakeContainer


def _cosmos() -> SimpleNamespace:
    return SimpleNamespace(
        hosts=FakeContainer("hosts", "/name"),
        leases=FakeContainer("leases", "/project"),
    )


def _settings(ttl_seconds: int = 14400) -> SimpleNamespace:
    return SimpleNamespace(lease_default_ttl_seconds=ttl_seconds)


async def _put_native_lease(
    cosmos,
    *,
    lease_id: str,
    project: str,
    state: LeaseState,
    assigned_at: datetime | None,
    requested_at: datetime | None = None,
    metadata: dict[str, Any] | None = None,
) -> None:
    requested_at = requested_at or assigned_at or datetime.now(UTC)
    base_metadata = {"native_k8s": True}
    if metadata:
        base_metadata.update(metadata)
    await cosmos.leases.create_item({
        "id": lease_id,
        "project": project,
        "workflow": "native-agent",
        "host": lease_ops.NATIVE_K8S_HOST if state == LeaseState.CLAIMED else None,
        "state": state.value,
        "requirements": {},
        "metadata": base_metadata,
        "requestedAt": requested_at.isoformat(),
        "assignedAt": assigned_at.isoformat() if assigned_at else None,
        "releasedAt": None,
        "ttlSeconds": 14400,
    })


async def _put_traditional_lease(
    cosmos,
    *,
    lease_id: str,
    project: str,
    state: LeaseState,
    host: str | None,
    assigned_at: datetime | None = None,
) -> None:
    now = assigned_at or datetime.now(UTC)
    await cosmos.leases.create_item({
        "id": lease_id,
        "project": project,
        "workflow": "issue-agent",
        "host": host,
        "state": state.value,
        "requirements": {},
        "metadata": {},
        "requestedAt": now.isoformat(),
        "assignedAt": now.isoformat() if state == LeaseState.CLAIMED else None,
        "releasedAt": None,
        "ttlSeconds": 14400,
    })


async def _put_host(cosmos, *, name: str, current_lease_id: str | None,
                    last_heartbeat: datetime | None) -> None:
    now = datetime.now(UTC).isoformat()
    await cosmos.hosts.create_item({
        "id": name,
        "name": name,
        "capabilities": {},
        "currentLeaseId": current_lease_id,
        "lastHeartbeat": last_heartbeat.isoformat() if last_heartbeat else None,
        "lastUsedAt": now,
        "drained": False,
        "createdAt": now,
    })


# ─── native-k8s sweep ─────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_sweep_expires_stale_native_lease():
    """A native-k8s lease whose env-destroy callback never fired stays
    ACTIVE indefinitely with no host record to drive the host-based sweep.
    The lease-driven sweep path should reap it once `assignedAt` is older
    than the TTL window."""
    cosmos = _cosmos()
    settings = _settings(ttl_seconds=14400)
    stale_assigned_at = datetime.now(UTC) - timedelta(seconds=14400 + 60)

    await _put_native_lease(
        cosmos,
        lease_id="01STALENATIVE",
        project="ambience",
        state=LeaseState.CLAIMED,
        assigned_at=stale_assigned_at,
    )

    expired = await lease_ops.sweep_expired(cosmos, settings)

    assert expired == 1
    lease_doc = await cosmos.leases.read_item(item="01STALENATIVE", partition_key="ambience")
    assert lease_doc["state"] == LeaseState.EXPIRED.value
    assert lease_doc["releasedAt"] is not None


@pytest.mark.asyncio
async def test_sweep_leaves_fresh_native_lease_active():
    """A native-k8s lease that's still inside its TTL window must not be
    reaped — the env-destroy callback is the normal release path."""
    cosmos = _cosmos()
    settings = _settings(ttl_seconds=14400)
    fresh_assigned_at = datetime.now(UTC) - timedelta(seconds=60)

    await _put_native_lease(
        cosmos,
        lease_id="01FRESHNATIVE",
        project="ambience",
        state=LeaseState.CLAIMED,
        assigned_at=fresh_assigned_at,
    )

    expired = await lease_ops.sweep_expired(cosmos, settings)

    assert expired == 0
    lease_doc = await cosmos.leases.read_item(item="01FRESHNATIVE", partition_key="ambience")
    assert lease_doc["state"] == LeaseState.CLAIMED.value


@pytest.mark.asyncio
async def test_sweep_does_not_touch_pending_native_leases():
    """Pending leases haven't been assigned yet — `assignedAt` is null,
    so they should never be considered stale by the sweep."""
    cosmos = _cosmos()
    settings = _settings(ttl_seconds=14400)
    long_ago = datetime.now(UTC) - timedelta(seconds=14400 + 600)

    await _put_native_lease(
        cosmos,
        lease_id="01PENDINGNATIVE",
        project="ambience",
        state=LeaseState.PENDING,
        assigned_at=None,
        requested_at=long_ago,
    )

    expired = await lease_ops.sweep_expired(cosmos, settings)

    assert expired == 0
    lease_doc = await cosmos.leases.read_item(item="01PENDINGNATIVE", partition_key="ambience")
    assert lease_doc["state"] == LeaseState.PENDING.value


@pytest.mark.asyncio
async def test_sweep_ignores_traditional_lease_via_native_path():
    """A traditional lease (no `metadata.native_k8s` flag) must not be
    matched by the native sweep query, even if its assignedAt is old —
    the host-driven path is responsible for those."""
    cosmos = _cosmos()
    settings = _settings(ttl_seconds=14400)
    stale_assigned_at = datetime.now(UTC) - timedelta(seconds=14400 + 60)

    # Traditional lease whose host still heartbeats (so the host-path
    # leaves it alone) — this proves the native query doesn't sweep it.
    await _put_traditional_lease(
        cosmos,
        lease_id="01TRADLEASE",
        project="spirelens",
        state=LeaseState.CLAIMED,
        host="runner-1",
        assigned_at=stale_assigned_at,
    )
    await _put_host(
        cosmos,
        name="runner-1",
        current_lease_id="01TRADLEASE",
        last_heartbeat=datetime.now(UTC),
    )

    expired = await lease_ops.sweep_expired(cosmos, settings)

    assert expired == 0
    lease_doc = await cosmos.leases.read_item(item="01TRADLEASE", partition_key="spirelens")
    assert lease_doc["state"] == LeaseState.CLAIMED.value


@pytest.mark.asyncio
async def test_sweep_reaps_native_and_traditional_in_one_pass():
    """Both paths run in the same sweep tick. Counts add together."""
    cosmos = _cosmos()
    settings = _settings(ttl_seconds=14400)
    long_ago = datetime.now(UTC) - timedelta(seconds=14400 + 60)

    await _put_native_lease(
        cosmos,
        lease_id="01STALENATIVE",
        project="ambience",
        state=LeaseState.CLAIMED,
        assigned_at=long_ago,
    )
    await _put_traditional_lease(
        cosmos,
        lease_id="01STALETRAD",
        project="spirelens",
        state=LeaseState.CLAIMED,
        host="runner-2",
        assigned_at=long_ago,
    )
    await _put_host(
        cosmos,
        name="runner-2",
        current_lease_id="01STALETRAD",
        last_heartbeat=long_ago,
    )

    expired = await lease_ops.sweep_expired(cosmos, settings)

    assert expired == 2
    native_doc = await cosmos.leases.read_item(item="01STALENATIVE", partition_key="ambience")
    trad_doc = await cosmos.leases.read_item(item="01STALETRAD", partition_key="spirelens")
    assert native_doc["state"] == LeaseState.EXPIRED.value
    assert trad_doc["state"] == LeaseState.EXPIRED.value


@pytest.mark.asyncio
async def test_sweep_ignores_already_expired_native_leases():
    """Once a native lease is EXPIRED or RELEASED, repeated sweep ticks
    must not re-touch it (state filter drops them from the result set)."""
    cosmos = _cosmos()
    settings = _settings(ttl_seconds=14400)
    long_ago = datetime.now(UTC) - timedelta(seconds=14400 + 600)

    await _put_native_lease(
        cosmos,
        lease_id="01ALREADYEXPIRED",
        project="ambience",
        state=LeaseState.EXPIRED,
        assigned_at=long_ago,
    )
    await _put_native_lease(
        cosmos,
        lease_id="01ALREADYRELEASED",
        project="ambience",
        state=LeaseState.RELEASED,
        assigned_at=long_ago,
    )

    expired = await lease_ops.sweep_expired(cosmos, settings)

    assert expired == 0

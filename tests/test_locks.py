"""Lock primitive: API correctness, contention, expiry, sweep.

Backed by the in-memory Cosmos fake (`tests/cosmos_fake.py`) — every
test runs in deterministic time via a `FrozenClock` and exercises the
real `_etag` + `IfNotModified` paths.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from types import SimpleNamespace
from typing import cast

import pytest

from glimmung.locks import (
    LockBusy,
    _doc_id,
    claim_lock,
    extend_lock,
    read_lock,
    release_lock,
    sweep_expired_locks,
)
from glimmung.models import LockState

from tests.cosmos_fake import FakeContainer, FrozenClock


# ─── fixtures ────────────────────────────────────────────────────────────────


@pytest.fixture
def cosmos():
    """A minimal SimpleNamespace standing in for `glimmung.db.Cosmos`. Only
    `cosmos.locks` is needed for the lock primitive."""
    container = FakeContainer("locks", "/scope")
    return cast("object", SimpleNamespace(locks=container))


@pytest.fixture
def clock():
    return FrozenClock()


# ─── doc id derivation ───────────────────────────────────────────────────────


def test_doc_id_encodes_url_unsafe_chars():
    """Cosmos forbids `/`, `\\`, `?`, `#` in ids. URL-encoding handles
    all four uniformly."""
    assert _doc_id("pr", "nelsong6/glimmung#19") == "pr::nelsong6%2Fglimmung%2319"
    assert _doc_id("issue", "nelsong6/spirelens#42") == "issue::nelsong6%2Fspirelens%2342"


def test_doc_id_distinct_across_scopes():
    """Same key in different scopes -> different doc ids -> independent
    locks. (PR #19 and Issue #19 don't conflict.)"""
    assert _doc_id("pr", "x#19") != _doc_id("issue", "x#19")


# ─── claim_lock: happy paths ─────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_claim_creates_lock_when_none_exists(cosmos, clock):
    lock = await claim_lock(
        cosmos, scope="pr", key="repo#1", holder_id="holder-A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    assert lock.scope == "pr"
    assert lock.key == "repo#1"
    assert lock.held_by == "holder-A"
    assert lock.state == LockState.HELD
    assert lock.claimed_at == clock.now()
    assert lock.expires_at == clock.now() + timedelta(seconds=300)
    assert lock.last_heartbeat_at == clock.now()


@pytest.mark.asyncio
async def test_claim_after_release_succeeds(cosmos, clock):
    """Release frees the lock; a fresh claim should succeed."""
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    released = await release_lock(cosmos, scope="pr", key="r#1", holder_id="A")
    assert released is True

    lock = await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="B",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    assert lock.held_by == "B"
    assert lock.state == LockState.HELD


@pytest.mark.asyncio
async def test_claim_takes_over_time_expired_lock(cosmos, clock):
    """Lock past `expires_at` but still in HELD state — claimer can take
    over without waiting for the sweep."""
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    clock.advance(seconds=11)

    lock = await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="B",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    assert lock.held_by == "B"
    assert lock.state == LockState.HELD


@pytest.mark.asyncio
async def test_claim_takes_over_explicitly_expired_lock(cosmos, clock):
    """Sweep marked the lock EXPIRED — claim should still take over."""
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    clock.advance(seconds=11)
    swept = await sweep_expired_locks(cosmos, now_factory=clock.as_factory())
    assert swept == 1

    lock = await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="B",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    assert lock.held_by == "B"
    assert lock.state == LockState.HELD


# ─── claim_lock: contention ──────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_claim_raises_busy_when_held_and_not_expired(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )

    with pytest.raises(LockBusy) as excinfo:
        await claim_lock(
            cosmos, scope="pr", key="r#1", holder_id="B",
            ttl_seconds=300, now_factory=clock.as_factory(),
        )

    assert excinfo.value.lock.held_by == "A"
    assert excinfo.value.lock.state == LockState.HELD


@pytest.mark.asyncio
async def test_claim_same_holder_while_held_raises_busy(cosmos, clock):
    """Strict claim semantics: re-claim by the same holder is also busy.
    Callers that want refresh-or-claim should use extend_lock."""
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    with pytest.raises(LockBusy) as excinfo:
        await claim_lock(
            cosmos, scope="pr", key="r#1", holder_id="A",
            ttl_seconds=300, now_factory=clock.as_factory(),
        )
    assert excinfo.value.lock.held_by == "A"


@pytest.mark.asyncio
async def test_claim_busy_message_carries_diagnostic_info(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    with pytest.raises(LockBusy) as excinfo:
        await claim_lock(
            cosmos, scope="pr", key="r#1", holder_id="B",
            ttl_seconds=300, now_factory=clock.as_factory(),
        )
    msg = str(excinfo.value)
    assert "pr:r#1" in msg
    assert "holder=A" not in msg  # exact format isn't part of the contract
    assert "A" in msg


# ─── multi-scope independence ────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_same_key_in_different_scopes_is_independent(cosmos, clock):
    pr_lock = await claim_lock(
        cosmos, scope="pr", key="r#19", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    issue_lock = await claim_lock(
        cosmos, scope="issue", key="r#19", holder_id="B",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    assert pr_lock.held_by == "A"
    assert issue_lock.held_by == "B"
    assert pr_lock.id != issue_lock.id


# ─── release_lock ────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_release_by_holder_returns_true_and_marks_released(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    assert await release_lock(cosmos, scope="pr", key="r#1", holder_id="A") is True

    lock = await read_lock(cosmos, scope="pr", key="r#1")
    assert lock is not None
    assert lock.state == LockState.RELEASED


@pytest.mark.asyncio
async def test_release_by_non_holder_returns_false(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    assert await release_lock(cosmos, scope="pr", key="r#1", holder_id="B") is False

    lock = await read_lock(cosmos, scope="pr", key="r#1")
    assert lock is not None
    assert lock.state == LockState.HELD
    assert lock.held_by == "A"


@pytest.mark.asyncio
async def test_release_of_missing_lock_returns_false(cosmos):
    assert await release_lock(cosmos, scope="pr", key="r#1", holder_id="A") is False


@pytest.mark.asyncio
async def test_release_is_idempotent(cosmos, clock):
    """Calling release twice should both succeed structurally — first
    transitions, second is a no-op (returns False)."""
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    assert await release_lock(cosmos, scope="pr", key="r#1", holder_id="A") is True
    assert await release_lock(cosmos, scope="pr", key="r#1", holder_id="A") is False


# ─── extend_lock ─────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_extend_by_holder_updates_expiry_and_heartbeat(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    clock.advance(seconds=5)

    extended = await extend_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=60, now_factory=clock.as_factory(),
    )
    assert extended.expires_at == clock.now() + timedelta(seconds=60)
    assert extended.last_heartbeat_at == clock.now()


@pytest.mark.asyncio
async def test_extend_by_non_holder_raises_busy(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    with pytest.raises(LockBusy):
        await extend_lock(
            cosmos, scope="pr", key="r#1", holder_id="B",
            ttl_seconds=300, now_factory=clock.as_factory(),
        )


@pytest.mark.asyncio
async def test_extend_after_release_raises_busy(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    await release_lock(cosmos, scope="pr", key="r#1", holder_id="A")
    with pytest.raises(LockBusy):
        await extend_lock(
            cosmos, scope="pr", key="r#1", holder_id="A",
            ttl_seconds=300, now_factory=clock.as_factory(),
        )


@pytest.mark.asyncio
async def test_extend_of_missing_lock_raises_busy(cosmos, clock):
    with pytest.raises(LockBusy):
        await extend_lock(
            cosmos, scope="pr", key="ghost", holder_id="A",
            ttl_seconds=300, now_factory=clock.as_factory(),
        )


@pytest.mark.asyncio
async def test_extend_after_takeover_raises_busy_for_old_holder(cosmos, clock):
    """A's lock expired; B took over; A's heartbeat must now fail loudly
    so A knows it lost the critical section."""
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    clock.advance(seconds=11)
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="B",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )

    with pytest.raises(LockBusy):
        await extend_lock(
            cosmos, scope="pr", key="r#1", holder_id="A",
            ttl_seconds=10, now_factory=clock.as_factory(),
        )


# ─── read_lock ───────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_read_returns_lock_when_present(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    lock = await read_lock(cosmos, scope="pr", key="r#1")
    assert lock is not None
    assert lock.held_by == "A"


@pytest.mark.asyncio
async def test_read_returns_none_when_absent(cosmos):
    assert await read_lock(cosmos, scope="pr", key="ghost") is None


# ─── sweep_expired_locks ─────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_sweep_marks_expired_held_locks_expired(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    clock.advance(seconds=11)

    swept = await sweep_expired_locks(cosmos, now_factory=clock.as_factory())
    assert swept == 1

    lock = await read_lock(cosmos, scope="pr", key="r#1")
    assert lock is not None
    assert lock.state == LockState.EXPIRED


@pytest.mark.asyncio
async def test_sweep_ignores_non_expired_locks(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    swept = await sweep_expired_locks(cosmos, now_factory=clock.as_factory())
    assert swept == 0

    lock = await read_lock(cosmos, scope="pr", key="r#1")
    assert lock is not None and lock.state == LockState.HELD


@pytest.mark.asyncio
async def test_sweep_ignores_already_released_locks(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    await release_lock(cosmos, scope="pr", key="r#1", holder_id="A")
    clock.advance(seconds=11)

    swept = await sweep_expired_locks(cosmos, now_factory=clock.as_factory())
    assert swept == 0


@pytest.mark.asyncio
async def test_sweep_is_idempotent(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    clock.advance(seconds=11)

    first = await sweep_expired_locks(cosmos, now_factory=clock.as_factory())
    second = await sweep_expired_locks(cosmos, now_factory=clock.as_factory())
    assert first == 1
    assert second == 0


@pytest.mark.asyncio
async def test_sweep_handles_multiple_scopes(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    await claim_lock(
        cosmos, scope="issue", key="r#1", holder_id="B",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    clock.advance(seconds=11)

    swept = await sweep_expired_locks(cosmos, now_factory=clock.as_factory())
    assert swept == 2


# ─── full lifecycle ──────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_full_lifecycle_claim_extend_release_reclaim(cosmos, clock):
    """End-to-end: A claims, A extends, A releases, B claims fresh."""
    a1 = await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    assert a1.held_by == "A"

    clock.advance(seconds=5)
    a2 = await extend_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=20, now_factory=clock.as_factory(),
    )
    assert a2.expires_at > a1.expires_at

    released = await release_lock(cosmos, scope="pr", key="r#1", holder_id="A")
    assert released is True

    b = await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="B",
        ttl_seconds=10, now_factory=clock.as_factory(),
    )
    assert b.held_by == "B"
    assert b.state == LockState.HELD


@pytest.mark.asyncio
async def test_metadata_is_preserved_through_lifecycle(cosmos, clock):
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=300, now_factory=clock.as_factory(),
        metadata={"signal_id": "sig-123", "source": "gh_review"},
    )
    lock = await read_lock(cosmos, scope="pr", key="r#1")
    assert lock is not None
    assert lock.metadata == {"signal_id": "sig-123", "source": "gh_review"}


# ─── round-trip serialization through Cosmos ─────────────────────────────────


@pytest.mark.asyncio
async def test_claimed_at_and_expires_at_round_trip_as_iso8601(cosmos):
    """Cosmos stores datetimes as ISO strings; the model parses them back.
    Real bug surface: if the model's mode='json' dump and the round-trip
    parse don't agree, expires_at comparisons silently break."""
    fixed = datetime(2026, 5, 1, 12, 0, 0, tzinfo=UTC)
    clock = FrozenClock(fixed)
    await claim_lock(
        cosmos, scope="pr", key="r#1", holder_id="A",
        ttl_seconds=600, now_factory=clock.as_factory(),
    )
    lock = await read_lock(cosmos, scope="pr", key="r#1")
    assert lock is not None
    assert lock.claimed_at == fixed
    assert lock.expires_at == fixed + timedelta(seconds=600)

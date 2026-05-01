"""Signal bus + drain-loop tests.

Backed by the in-memory Cosmos fake. Covers signal lifecycle (enqueue
→ list → mark_processing → mark_processed / mark_failed) plus the
drain-loop's interaction with the lock primitive: per-target
serialization, decision-engine integration, lock-hold semantics.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from glimmung import locks as lock_ops
from glimmung.models import (
    Signal,
    SignalSource,
    SignalState,
    SignalTargetType,
)
from glimmung.signals import (
    drain_signals,
    enqueue_signal,
    list_pending_signals,
    lock_key_for,
    lock_scope_for,
    mark_failed,
    mark_processed,
    mark_processing,
)

from tests.cosmos_fake import FakeContainer


# ─── fixtures ────────────────────────────────────────────────────────────────


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        signals=FakeContainer("signals", "/target_repo"),
        locks=FakeContainer("locks", "/scope"),
    )


@pytest.fixture
def settings():
    return SimpleNamespace(
        lease_default_ttl_seconds=14400,
        sweep_interval_seconds=60,
    )


# ─── enqueue ─────────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_enqueue_creates_signal_in_pending_state(cosmos):
    sig = await enqueue_signal(
        cosmos,
        target_type=SignalTargetType.PR,
        target_repo="nelsong6/ambience",
        target_id="100",
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "fix the date format"},
    )
    assert sig.state == SignalState.PENDING
    assert sig.target_type == SignalTargetType.PR
    assert sig.target_id == "100"
    assert sig.payload["feedback"] == "fix the date format"


@pytest.mark.asyncio
async def test_list_pending_returns_signals_oldest_first(cosmos):
    a = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )
    b = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="2", source=SignalSource.GLIMMUNG_UI,
    )
    c = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/b",
        target_id="3", source=SignalSource.GLIMMUNG_UI,
    )
    pending = await list_pending_signals(cosmos)
    ids = [s.id for s, _ in pending]
    assert ids == [a.id, b.id, c.id]


@pytest.mark.asyncio
async def test_list_pending_excludes_non_pending(cosmos):
    sig = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )
    pending = await list_pending_signals(cosmos)
    s, etag = pending[0]
    await mark_processing(cosmos, signal=s, etag=etag)
    assert sig.id not in [p.id for p, _ in await list_pending_signals(cosmos)]


# ─── state transitions ──────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_mark_processing_transitions_pending_to_processing(cosmos):
    sig = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )
    pending = await list_pending_signals(cosmos)
    s, etag = pending[0]
    result = await mark_processing(cosmos, signal=s, etag=etag)
    assert result is not None
    updated, _ = result
    assert updated.state == SignalState.PROCESSING
    assert updated.id == sig.id


@pytest.mark.asyncio
async def test_mark_processing_returns_none_on_etag_race(cosmos):
    """Two drains race on the same signal: one wins via etag, the
    other gets None."""
    await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )
    pending = await list_pending_signals(cosmos)
    s, etag = pending[0]

    # First mark wins.
    first = await mark_processing(cosmos, signal=s, etag=etag)
    assert first is not None

    # Second mark with the stale etag loses.
    second = await mark_processing(cosmos, signal=s, etag=etag)
    assert second is None


@pytest.mark.asyncio
async def test_mark_processed_records_decision(cosmos):
    await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )
    pending = await list_pending_signals(cosmos)
    s, etag = pending[0]
    s, etag = (await mark_processing(cosmos, signal=s, etag=etag))  # type: ignore[misc]
    result, _ = await mark_processed(cosmos, signal=s, etag=etag, decision="dispatch_triage")
    assert result.state == SignalState.PROCESSED
    assert result.processed_decision == "dispatch_triage"
    assert result.processed_at is not None


@pytest.mark.asyncio
async def test_mark_failed_records_reason(cosmos):
    await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )
    pending = await list_pending_signals(cosmos)
    s, etag = pending[0]
    s, etag = (await mark_processing(cosmos, signal=s, etag=etag))  # type: ignore[misc]
    failed = await mark_failed(cosmos, signal=s, etag=etag, reason="decide raised")
    assert failed.state == SignalState.FAILED
    assert "raised" in (failed.failure_reason or "")


# ─── drain loop ──────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_drain_processes_pending_signals_in_order(cosmos, settings):
    a = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )
    b = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="2", source=SignalSource.GLIMMUNG_UI,
    )

    seen: list[str] = []
    async def decide(signal: Signal):
        seen.append(signal.id)
        return ("ignore", False)

    async def apply(signal: Signal, decision: str, holder_id: str):
        pass

    n = await drain_signals(
        cosmos, settings=settings, decide_fn=decide, apply_fn=apply,
    )
    assert n == 2
    assert seen == [a.id, b.id]


@pytest.mark.asyncio
async def test_drain_skips_signals_with_busy_target_lock(cosmos, settings):
    """If a target's lock is already held, the drain skips signals on
    that target — they stay PENDING and get re-evaluated next tick."""
    sig = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )

    # Pre-claim the target lock as a different holder.
    await lock_ops.claim_lock(
        cosmos, scope=lock_scope_for(SignalTargetType.PR),
        key=lock_key_for(sig), holder_id="someone-else",
        ttl_seconds=600,
    )

    async def decide(signal: Signal):
        raise AssertionError("decide should never be called when lock is busy")

    async def apply(*args, **kwargs):
        raise AssertionError("apply should never be called when lock is busy")

    n = await drain_signals(
        cosmos, settings=settings, decide_fn=decide, apply_fn=apply,
    )
    assert n == 0

    # Signal stays PENDING.
    pending = await list_pending_signals(cosmos)
    assert len(pending) == 1
    assert pending[0][0].id == sig.id


@pytest.mark.asyncio
async def test_drain_holds_lock_when_decision_says_hold(cosmos, settings):
    """For a DISPATCH_TRIAGE-style decision, the drain holds the lock
    past return so the downstream workflow's terminal handler can
    release it. Verify the lock is still HELD after drain returns."""
    sig = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )

    async def decide(signal: Signal):
        return ("dispatch_triage", True)  # hold lock

    async def apply(*args, **kwargs):
        pass

    await drain_signals(cosmos, settings=settings, decide_fn=decide, apply_fn=apply)

    lock = await lock_ops.read_lock(
        cosmos, scope=lock_scope_for(SignalTargetType.PR),
        key=lock_key_for(sig),
    )
    assert lock is not None
    assert lock.state.value == "held"
    assert lock.held_by == sig.id


@pytest.mark.asyncio
async def test_drain_releases_lock_when_decision_says_release(cosmos, settings):
    sig = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )

    async def decide(signal: Signal):
        return ("ignore", False)  # release lock

    async def apply(*args, **kwargs):
        pass

    await drain_signals(cosmos, settings=settings, decide_fn=decide, apply_fn=apply)

    lock = await lock_ops.read_lock(
        cosmos, scope=lock_scope_for(SignalTargetType.PR),
        key=lock_key_for(sig),
    )
    assert lock is not None
    assert lock.state.value == "released"


@pytest.mark.asyncio
async def test_drain_marks_failed_when_decide_raises(cosmos, settings):
    await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )

    async def decide(signal: Signal):
        raise RuntimeError("boom")

    async def apply(*args, **kwargs):
        raise AssertionError("apply should not be called after decide raised")

    await drain_signals(cosmos, settings=settings, decide_fn=decide, apply_fn=apply)

    docs = [d async for d in cosmos.signals.query_items("SELECT * FROM c", parameters=[])]
    assert docs[0]["state"] == "failed"
    assert "boom" in docs[0]["failure_reason"]


@pytest.mark.asyncio
async def test_drain_marks_failed_when_apply_raises(cosmos, settings):
    await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )

    async def decide(signal: Signal):
        return ("dispatch_triage", True)

    async def apply(*args, **kwargs):
        raise RuntimeError("dispatch failed")

    await drain_signals(cosmos, settings=settings, decide_fn=decide, apply_fn=apply)

    docs = [d async for d in cosmos.signals.query_items("SELECT * FROM c", parameters=[])]
    assert docs[0]["state"] == "failed"


@pytest.mark.asyncio
async def test_drain_serializes_two_signals_on_same_target(cosmos, settings):
    """Two reject signals on the same PR queue cleanly: the first
    drain processes signal-1 with hold_lock=True (simulating a
    triage in flight); signal-2 stays PENDING because the lock is
    held. Verifies #19 DoD #6 ('per-PR serialization queues the
    second one rather than racing')."""
    a = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="100", source=SignalSource.GLIMMUNG_UI,
    )
    b = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="100", source=SignalSource.GLIMMUNG_UI,
    )

    seen: list[str] = []
    async def decide(signal: Signal):
        seen.append(signal.id)
        return ("dispatch_triage", True)  # hold lock

    async def apply(*args, **kwargs):
        pass

    n = await drain_signals(cosmos, settings=settings, decide_fn=decide, apply_fn=apply)
    assert n == 1  # only a; b skipped because lock still held by a's holder
    assert seen == [a.id]

    # b still pending.
    pending = await list_pending_signals(cosmos)
    assert [s.id for s, _ in pending] == [b.id]


@pytest.mark.asyncio
async def test_drain_processes_b_after_a_releases_lock(cosmos, settings):
    """Sequel to the above: after a's downstream releases the lock,
    a subsequent drain tick picks up b."""
    a = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="100", source=SignalSource.GLIMMUNG_UI,
    )
    b = await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="100", source=SignalSource.GLIMMUNG_UI,
    )

    async def decide(signal: Signal):
        return ("dispatch_triage", True)

    async def apply(*args, **kwargs):
        pass

    # First tick: process a, hold lock.
    await drain_signals(cosmos, settings=settings, decide_fn=decide, apply_fn=apply)

    # Simulate downstream: release the lock that signal-a's drain claimed.
    await lock_ops.release_lock(
        cosmos, scope=lock_scope_for(SignalTargetType.PR),
        key=lock_key_for(a), holder_id=a.id,
    )

    # Second tick: process b.
    seen: list[str] = []
    async def decide2(signal: Signal):
        seen.append(signal.id)
        return ("dispatch_triage", True)

    n = await drain_signals(cosmos, settings=settings, decide_fn=decide2, apply_fn=apply)
    assert n == 1
    assert seen == [b.id]


@pytest.mark.asyncio
async def test_drain_handles_multiple_targets_in_one_tick(cosmos, settings):
    """Different targets don't serialize against each other — both
    process in the same tick."""
    await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="1", source=SignalSource.GLIMMUNG_UI,
    )
    await enqueue_signal(
        cosmos, target_type=SignalTargetType.PR, target_repo="r/a",
        target_id="2", source=SignalSource.GLIMMUNG_UI,
    )

    async def decide(signal: Signal):
        return ("ignore", False)

    async def apply(*args, **kwargs):
        pass

    n = await drain_signals(cosmos, settings=settings, decide_fn=decide, apply_fn=apply)
    assert n == 2

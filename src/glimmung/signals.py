"""Signal bus.

A Signal is a unit of work for the orchestrator. Webhooks (GH PR
review, issue/PR comment) and the glimmung UI (reject button) enqueue
Signals; a background drain loop processes them.

Every signal targets a `(target_type, target_repo, target_id)` tuple
— PR signals target the PR number, issue signals target the issue
number, run signals target the Run ULID. The drain loop claims the
matching `(scope, key)` lock from the lock primitive (#22) before
invoking the decision engine, so concurrent signals on the same
target serialize cleanly. The lock is **held past drain return** for
DISPATCH_TRIAGE decisions — the triage workflow's `workflow_run.completed`
handler releases it on terminal Run transition. For IGNORE / ABORT_*
decisions the drain releases immediately.

API:
- `enqueue_signal(...)` — append to the queue.
- `list_pending_signals(...)` — drain reads ordered by enqueued_at.
- `mark_processing` / `mark_processed` / `mark_failed` — state machine
  transitions, etag-validated.
- `drain_signals(app, decide_fn, apply_fn, lock_scope_for)` — generic
  drain over a deciding function. Decision engines plug in here.
"""

from __future__ import annotations

import logging
from datetime import UTC, datetime
from typing import Any, Callable

from azure.core import MatchConditions
from azure.cosmos.exceptions import CosmosAccessConditionFailedError
from ulid import ULID

from glimmung.db import Cosmos, query_all
from glimmung.locks import LockBusy, claim_lock, release_lock
from glimmung.models import Signal, SignalSource, SignalState, SignalTargetType

log = logging.getLogger(__name__)


def _utcnow() -> datetime:
    return datetime.now(UTC)


def _strip_meta(doc: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in doc.items() if not k.startswith("_")}


async def enqueue_signal(
    cosmos: Cosmos,
    *,
    target_type: SignalTargetType,
    target_repo: str,
    target_id: str,
    source: SignalSource,
    payload: dict[str, Any] | None = None,
) -> Signal:
    """Append a Signal to the queue. Returns the persisted Signal.

    Idempotency note: this does not deduplicate. Callers that want
    "exactly once" semantics on a given event (webhook redelivery,
    user double-click) should encode the dedupe key in the payload
    and let the decision engine's idempotent-no-op path handle it
    (e.g., a triage decision where the prior decision is still in
    flight — the per-PR lock serializes that)."""
    now = _utcnow()
    signal = Signal(
        id=str(ULID()),
        target_type=target_type,
        target_repo=target_repo,
        target_id=target_id,
        source=source,
        payload=payload or {},
        state=SignalState.PENDING,
        enqueued_at=now,
    )
    await cosmos.signals.create_item(signal.model_dump(mode="json"))
    log.info(
        "enqueued signal %s: %s/%s/%s source=%s",
        signal.id, target_type.value, target_repo, target_id, source.value,
    )
    return signal


async def list_pending_signals(
    cosmos: Cosmos,
    *,
    limit: int | None = None,
) -> list[tuple[Signal, str]]:
    """Cross-partition scan, oldest-first. Returns `(signal, etag)` so
    the caller can mark_processing without a re-read."""
    docs = await query_all(
        cosmos.signals,
        "SELECT * FROM c WHERE c.state = @s ORDER BY c.enqueued_at ASC",
        parameters=[{"name": "@s", "value": SignalState.PENDING.value}],
    )
    if limit is not None:
        docs = docs[:limit]
    return [(Signal.model_validate(_strip_meta(d)), d["_etag"]) for d in docs]


async def mark_processing(
    cosmos: Cosmos,
    *,
    signal: Signal,
    etag: str,
) -> tuple[Signal, str] | None:
    """PENDING → PROCESSING. Returns (signal, new_etag) on success or
    None if another drain raced and grabbed the same signal."""
    if signal.state != SignalState.PENDING:
        return None
    doc = signal.model_dump(mode="json")
    doc["state"] = SignalState.PROCESSING.value
    try:
        response = await cosmos.signals.replace_item(
            item=signal.id,
            body=doc,
            etag=etag,
            match_condition=MatchConditions.IfNotModified,
        )
        updated = Signal.model_validate(_strip_meta(response))
        return updated, response.get("_etag", etag)
    except CosmosAccessConditionFailedError:
        return None


async def mark_processed(
    cosmos: Cosmos,
    *,
    signal: Signal,
    etag: str,
    decision: str,
) -> tuple[Signal, str]:
    """PROCESSING → PROCESSED. Stores the decision string for audit.
    Returns `(signal, new_etag)` so callers can chain into a subsequent
    state transition (e.g., mark_failed in the drain's apply-error
    branch) without re-reading."""
    doc = signal.model_dump(mode="json")
    doc["state"] = SignalState.PROCESSED.value
    doc["processed_at"] = _utcnow().isoformat()
    doc["processed_decision"] = decision
    response = await cosmos.signals.replace_item(
        item=signal.id, body=doc,
        etag=etag, match_condition=MatchConditions.IfNotModified,
    )
    return Signal.model_validate(_strip_meta(response)), response.get("_etag", etag)


async def mark_failed(
    cosmos: Cosmos,
    *,
    signal: Signal,
    etag: str,
    reason: str,
) -> Signal:
    """Any state → FAILED. Used when the drain raises mid-decision."""
    doc = signal.model_dump(mode="json")
    doc["state"] = SignalState.FAILED.value
    doc["processed_at"] = _utcnow().isoformat()
    doc["failure_reason"] = reason[:500]
    try:
        response = await cosmos.signals.replace_item(
            item=signal.id, body=doc,
            etag=etag, match_condition=MatchConditions.IfNotModified,
        )
        return Signal.model_validate(_strip_meta(response))
    except CosmosAccessConditionFailedError:
        # Best-effort; if someone else already marked the signal, leave it.
        log.warning("mark_failed lost race for signal %s; leaving as-is", signal.id)
        return signal


# ─── drain loop ──────────────────────────────────────────────────────────────


def lock_scope_for(target_type: SignalTargetType) -> str:
    """Map a signal's target_type to the lock primitive scope.

    PR signals serialize through "pr" locks, issue signals through
    "issue" locks, run signals through "run" locks. Keeps the lock
    primitive's scope vocabulary aligned with the signal target
    vocabulary."""
    return target_type.value


def lock_key_for(signal: Signal) -> str:
    """Lock key shape mirrors `dispatch_run`'s issue-lock key:
    `<repo>#<id>`. Same shape for all target types."""
    return f"{signal.target_repo}#{signal.target_id}"


async def drain_signals(
    cosmos: Cosmos,
    *,
    settings: Any,
    decide_fn: Callable,
    apply_fn: Callable,
    lock_ttl_seconds: int | None = None,
) -> int:
    """Walk pending signals oldest-first, claiming the per-target lock
    before invoking the decision engine.

    `decide_fn` is `async (signal: Signal) -> (decision: str, hold_lock: bool)`:
    decision is the string written to signal.processed_decision; the
    lock-hold flag tells the drain whether to release immediately
    (IGNORE / ABORT) or pass ownership to a downstream call (the
    triage workflow's terminal handler, etc.).

    `apply_fn` is `async (signal, decision, lock_holder_id) -> None`:
    the side effect — workflow dispatch, comment, etc. Runs after the
    signal is marked PROCESSED. Lock release is the caller's
    responsibility if `hold_lock` was True.

    Returns the number of signals processed in this drain tick."""
    ttl = lock_ttl_seconds or settings.lease_default_ttl_seconds
    pending = await list_pending_signals(cosmos)
    processed = 0

    for signal, etag in pending:
        scope = lock_scope_for(signal.target_type)
        key = lock_key_for(signal)
        holder_id = signal.id  # signal_id is the natural holder; survives across glimmung restarts

        try:
            await claim_lock(
                cosmos, scope=scope, key=key, holder_id=holder_id,
                ttl_seconds=ttl,
                metadata={"signal_id": signal.id, "source": signal.source.value},
            )
        except LockBusy:
            # Another drain or a prior triage holds the lock; leave the
            # signal PENDING and try again next tick.
            log.debug("drain: signal %s skipped (target lock busy)", signal.id)
            continue

        # We hold the lock — claim the signal.
        claimed = await mark_processing(cosmos, signal=signal, etag=etag)
        if claimed is None:
            # Another drain raced through Cosmos but the lock claim
            # already proved we own this. Treat as transient and bail
            # without releasing — the lock will expire naturally.
            log.warning("drain: signal %s lost mark_processing race; releasing lock", signal.id)
            await release_lock(cosmos, scope=scope, key=key, holder_id=holder_id)
            continue

        signal, etag = claimed

        try:
            decision, hold_lock = await decide_fn(signal)
        except Exception as e:
            log.exception("drain: decide_fn raised on signal %s", signal.id)
            await mark_failed(cosmos, signal=signal, etag=etag, reason=str(e))
            await release_lock(cosmos, scope=scope, key=key, holder_id=holder_id)
            continue

        try:
            signal, etag = await mark_processed(
                cosmos, signal=signal, etag=etag, decision=decision,
            )
        except Exception:
            log.exception("drain: mark_processed failed on signal %s", signal.id)
            await release_lock(cosmos, scope=scope, key=key, holder_id=holder_id)
            continue

        try:
            await apply_fn(signal, decision, holder_id)
        except Exception as e:
            log.exception("drain: apply_fn raised on signal %s", signal.id)
            # Mark FAILED post-hoc (signal is currently PROCESSED). Better
            # than silent: visible in the diagnostic surface. Uses the
            # post-mark_processed etag so the replace_item succeeds.
            await mark_failed(cosmos, signal=signal, etag=etag, reason=str(e))
            await release_lock(cosmos, scope=scope, key=key, holder_id=holder_id)
            continue

        if not hold_lock:
            await release_lock(cosmos, scope=scope, key=key, holder_id=holder_id)
        # else: a downstream handler (workflow_run.completed for triage)
        # will release the lock on its terminal transition. The lock's
        # TTL is the safety net.

        processed += 1

    return processed

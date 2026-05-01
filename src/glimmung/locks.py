"""Generic mutual-exclusion primitive.

A `Lock` is a `(scope, key)` claim with a TTL and a holder id. Used to
serialize work on logical entities — PRs, issues, runs, signal drains —
without conflating them with the host-capacity model in `leases.py`.

API:
- `claim_lock(scope, key, holder_id, ttl_seconds)` — atomic create or
  take-over. Raises `LockBusy(existing)` if the lock is currently held
  by someone else (and not expired).
- `release_lock(scope, key, holder_id)` — idempotent. No-op if the lock
  isn't held by `holder_id`.
- `extend_lock(scope, key, holder_id, ttl_seconds)` — heartbeat. Raises
  `LockBusy` if the holder lost the lock (expired + reclaimed by
  someone else).
- `read_lock(scope, key)` — diagnostic read. Returns `None` if no doc.
- `sweep_expired_locks()` — background sweep: marks `HELD` locks whose
  `expires_at < now` as `EXPIRED`. Does not block take-over (claimers
  can take over an expired-but-still-HELD lock directly via the etag
  CAS path).

Concurrency: every write is `IfNotModified` against `_etag`, with a
read-decide-write retry loop. Take-over of an expired lock and
fresh creation are both guarded by the same retry/etag dance, so two
racing claimers can't both succeed.

Doc id derivation: `f"{scope}::{urllib.parse.quote(key, safe='')}"`.
Deterministic + URL-safe + reversible (the `key` is also stored as a
field for query convenience).
"""

from __future__ import annotations

import logging
from datetime import UTC, datetime, timedelta
from typing import Any, Callable
from urllib.parse import quote

from azure.core import MatchConditions
from azure.cosmos.exceptions import (
    CosmosAccessConditionFailedError,
    CosmosResourceExistsError,
    CosmosResourceNotFoundError,
)

from glimmung.db import Cosmos, query_all
from glimmung.models import Lock, LockState

log = logging.getLogger(__name__)


_DEFAULT_MAX_CLAIM_RETRIES = 5


class LockBusy(Exception):
    """Raised when a claim or extend can't proceed because the lock is
    held by someone else (and not expired). Carries the current Lock
    state for diagnostic / queue-decision purposes at the call site."""

    def __init__(self, lock: Lock):
        self.lock = lock
        super().__init__(
            f"lock {lock.scope}:{lock.key} held by {lock.held_by} "
            f"until {lock.expires_at.isoformat()}"
        )


def _doc_id(scope: str, key: str) -> str:
    """Deterministic Cosmos doc id. Cosmos forbids `/`, `\\`, `?`, `#`
    in ids, so we URL-encode the caller-supplied key. The `::` separator
    is unambiguous because `:` is encoded inside `key`."""
    return f"{scope}::{quote(key, safe='')}"


def _strip_meta(doc: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in doc.items() if not k.startswith("_")}


def _to_doc(lock: Lock) -> dict[str, Any]:
    return lock.model_dump(mode="json")


async def claim_lock(
    cosmos: Cosmos,
    *,
    scope: str,
    key: str,
    holder_id: str,
    ttl_seconds: int,
    metadata: dict[str, Any] | None = None,
    now_factory: Callable[[], datetime] | None = None,
    max_retries: int = _DEFAULT_MAX_CLAIM_RETRIES,
) -> Lock:
    """Claim a lock on `(scope, key)`. Atomic.

    - If no prior lock exists: creates one in `HELD` state.
    - If a prior lock exists in `RELEASED` or `EXPIRED` state, or in
      `HELD` state but past `expires_at`: takes over via etag CAS.
    - If a prior lock is `HELD` and not expired: raises `LockBusy`.

    Caller must pass a stable `holder_id` (e.g. the signal_id or run_id
    that owns this critical section) so `release_lock` and `extend_lock`
    can validate ownership later.

    `now_factory` is for tests. Production callers omit it.
    """
    now_fn = now_factory or _utcnow
    doc_id = _doc_id(scope, key)

    for attempt in range(max_retries):
        now = now_fn()
        new_lock = Lock(
            id=doc_id,
            scope=scope,
            key=key,
            state=LockState.HELD,
            held_by=holder_id,
            claimed_at=now,
            expires_at=now + timedelta(seconds=ttl_seconds),
            last_heartbeat_at=now,
            metadata=metadata or {},
        )

        try:
            existing_doc = await cosmos.locks.read_item(
                item=doc_id, partition_key=scope,
            )
        except CosmosResourceNotFoundError:
            existing_doc = None

        if existing_doc is None:
            try:
                created = await cosmos.locks.create_item(_to_doc(new_lock))
                log.info(
                    "claim_lock: created %s::%s for holder=%s ttl=%ds",
                    scope, key, holder_id, ttl_seconds,
                )
                return Lock.model_validate(_strip_meta(created))
            except CosmosResourceExistsError:
                # Lost the race — another claimer just created. Loop.
                log.debug("claim_lock: create raced for %s::%s, retrying", scope, key)
                continue

        existing = Lock.model_validate(_strip_meta(existing_doc))
        if existing.state == LockState.HELD and existing.expires_at > now:
            raise LockBusy(existing)

        # Take-over: existing lock is RELEASED, EXPIRED, or HELD-but-expired.
        try:
            replaced = await cosmos.locks.replace_item(
                item=doc_id,
                body=_to_doc(new_lock),
                etag=existing_doc["_etag"],
                match_condition=MatchConditions.IfNotModified,
            )
            log.info(
                "claim_lock: took over %s::%s (prior holder=%s state=%s) for new holder=%s",
                scope, key, existing.held_by, existing.state.value, holder_id,
            )
            return Lock.model_validate(_strip_meta(replaced))
        except CosmosAccessConditionFailedError:
            # Someone else mutated between our read and replace. Retry.
            log.debug("claim_lock: take-over raced for %s::%s, retrying", scope, key)
            continue

    raise RuntimeError(
        f"claim_lock: exhausted {max_retries} retries for {scope}::{key} "
        "(persistent contention)"
    )


async def release_lock(
    cosmos: Cosmos,
    *,
    scope: str,
    key: str,
    holder_id: str,
) -> bool:
    """Release a held lock. Idempotent: returns `False` if the lock isn't
    ours (or doesn't exist), `True` if we transitioned it to RELEASED.

    Doesn't retry on contention — if someone else is mutating the lock,
    they're not us, so the right behavior is to leave them to it. The
    only honest race is "we held → expired → someone reclaimed" which
    means our critical section is done anyway.
    """
    doc_id = _doc_id(scope, key)
    try:
        existing_doc = await cosmos.locks.read_item(
            item=doc_id, partition_key=scope,
        )
    except CosmosResourceNotFoundError:
        return False

    if existing_doc.get("held_by") != holder_id:
        return False
    if existing_doc.get("state") != LockState.HELD.value:
        return False

    existing_doc["state"] = LockState.RELEASED.value
    try:
        await cosmos.locks.replace_item(
            item=doc_id,
            body=existing_doc,
            etag=existing_doc["_etag"],
            match_condition=MatchConditions.IfNotModified,
        )
        log.info("release_lock: released %s::%s held_by=%s", scope, key, holder_id)
        return True
    except CosmosAccessConditionFailedError:
        return False


async def extend_lock(
    cosmos: Cosmos,
    *,
    scope: str,
    key: str,
    holder_id: str,
    ttl_seconds: int,
    now_factory: Callable[[], datetime] | None = None,
    max_retries: int = _DEFAULT_MAX_CLAIM_RETRIES,
) -> Lock:
    """Extend a held lock's `expires_at`. Validates the holder.

    Raises `LockBusy` if the lock is no longer ours (expired + reclaimed,
    released by us already, or never existed).
    """
    now_fn = now_factory or _utcnow
    doc_id = _doc_id(scope, key)

    for _ in range(max_retries):
        try:
            existing_doc = await cosmos.locks.read_item(
                item=doc_id, partition_key=scope,
            )
        except CosmosResourceNotFoundError as e:
            raise LockBusy(_synthetic_busy(scope, key, holder_id)) from e

        existing = Lock.model_validate(_strip_meta(existing_doc))
        if existing.held_by != holder_id or existing.state != LockState.HELD:
            raise LockBusy(existing)

        now = now_fn()
        existing_doc["last_heartbeat_at"] = now.isoformat()
        existing_doc["expires_at"] = (now + timedelta(seconds=ttl_seconds)).isoformat()
        try:
            replaced = await cosmos.locks.replace_item(
                item=doc_id,
                body=existing_doc,
                etag=existing_doc["_etag"],
                match_condition=MatchConditions.IfNotModified,
            )
            return Lock.model_validate(_strip_meta(replaced))
        except CosmosAccessConditionFailedError:
            continue

    raise RuntimeError(
        f"extend_lock: exhausted {max_retries} retries for {scope}::{key}"
    )


async def read_lock(cosmos: Cosmos, *, scope: str, key: str) -> Lock | None:
    """Point-read a lock. `None` if no doc. Doesn't normalize state for
    expiry — callers that care about logical state should compare
    `expires_at` to `now` themselves."""
    doc_id = _doc_id(scope, key)
    try:
        doc = await cosmos.locks.read_item(item=doc_id, partition_key=scope)
        return Lock.model_validate(_strip_meta(doc))
    except CosmosResourceNotFoundError:
        return None


async def sweep_expired_locks(
    cosmos: Cosmos,
    *,
    now_factory: Callable[[], datetime] | None = None,
) -> int:
    """Mark `HELD` locks whose `expires_at` is past as `EXPIRED`.

    Background loop. Idempotent. The take-over path in `claim_lock`
    doesn't wait for sweep — it can take over a HELD-but-expired lock
    directly — so this sweep is mostly cosmetic (keeps the dashboard
    honest about which locks are truly held vs. abandoned).
    """
    now_fn = now_factory or _utcnow
    cutoff = now_fn().isoformat()
    expired_docs = await query_all(
        cosmos.locks,
        "SELECT * FROM c WHERE c.state = @s AND c.expires_at < @cutoff",
        parameters=[
            {"name": "@s", "value": LockState.HELD.value},
            {"name": "@cutoff", "value": cutoff},
        ],
    )
    swept = 0
    for doc in expired_docs:
        doc["state"] = LockState.EXPIRED.value
        try:
            await cosmos.locks.replace_item(
                item=doc["id"],
                body=doc,
                etag=doc["_etag"],
                match_condition=MatchConditions.IfNotModified,
            )
            swept += 1
        except CosmosAccessConditionFailedError:
            # Someone else (claimer or sweep peer) is mutating; skip.
            continue
    if swept:
        log.info("sweep_expired_locks: marked %d locks expired", swept)
    return swept


def _utcnow() -> datetime:
    return datetime.now(UTC)


def _synthetic_busy(scope: str, key: str, holder_id: str) -> Lock:
    """Build a placeholder Lock for the LockBusy raised when extend can't
    find any doc. Lets the exception keep its uniform shape (`.lock`
    attribute) without forcing callers to handle a different sentinel."""
    now = _utcnow()
    return Lock(
        id=_doc_id(scope, key),
        scope=scope,
        key=key,
        state=LockState.RELEASED,
        held_by="<missing>",
        claimed_at=now,
        expires_at=now,
    )

"""Native k8s_job event/log persistence.

Native runner Jobs own their internal step orchestration. Glimmung records
the observed boundaries and ordered log chunks so the dashboard and resume
surface can reason about first-class steps without pretending to schedule
inside the container.
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from azure.cosmos.exceptions import CosmosResourceExistsError

from glimmung.db import Cosmos, query_all
from glimmung.models import NativeJobAttempt, NativeRunEventType, NativeStepState, Run
from glimmung.runs import _retry_on_conflict


class NativeEventError(ValueError):
    pass


TERMINAL_STEP_STATES = {
    NativeStepState.SUCCEEDED,
    NativeStepState.FAILED,
    NativeStepState.SKIPPED,
}


def _now() -> datetime:
    return datetime.now(UTC)


def _event_id(*, run_id: str, attempt_index: int, job_id: str, seq: int) -> str:
    return f"{run_id}::{attempt_index:04d}::{job_id}::{seq:012d}"


async def record_native_event(
    cosmos: Cosmos,
    *,
    run: Run,
    etag: str,
    job_id: str,
    seq: int,
    event: NativeRunEventType,
    step_slug: str | None = None,
    message: str | None = None,
    exit_code: int | None = None,
    metadata: dict[str, Any] | None = None,
) -> tuple[Run, str]:
    """Persist one idempotent native event and update the latest attempt.

    Idempotency key is `(run_id, job_id, seq)` encoded in the Cosmos id.
    Duplicate deliveries with identical payload are accepted and re-apply
    state; duplicate seq with different payload is rejected.
    """
    if seq < 1:
        raise NativeEventError("seq must be >= 1")
    if not run.attempts:
        raise NativeEventError(f"run {run.id} has no attempts")
    _validate_target(run, job_id=job_id, event=event, step_slug=step_slug)

    attempt_index = run.attempts[-1].attempt_index
    doc = {
        "id": _event_id(
            run_id=run.id,
            attempt_index=attempt_index,
            job_id=job_id,
            seq=seq,
        ),
        "project": run.project,
        "run_id": run.id,
        "attempt_index": attempt_index,
        "phase": run.attempts[-1].phase,
        "job_id": job_id,
        "seq": seq,
        "event": event.value,
        "step_slug": step_slug or "",
        "message": message or "",
        "exit_code": exit_code,
        "metadata": metadata or {},
        "created_at": _now().isoformat(),
    }
    try:
        await cosmos.run_events.create_item(doc)
    except CosmosResourceExistsError:
        existing = await cosmos.run_events.read_item(
            item=doc["id"], partition_key=run.project,
        )
        if not _same_event(existing, doc):
            raise NativeEventError(
                f"duplicate native event seq {seq} for run {run.id} job {job_id} "
                "has a different payload"
            )

    def apply(r: Run) -> Run:
        attempt = r.attempts[-1]
        job = next((j for j in attempt.jobs if j.job_id == job_id), None)
        if job is None:
            # Validated before the write; this only triggers if the run was
            # concurrently replaced with a different attempt shape.
            raise NativeEventError(f"run {r.id} latest attempt has no job {job_id!r}")
        job.last_seq = max(job.last_seq, seq)

        if event == NativeRunEventType.LOG:
            return r.model_copy(update={"updated_at": _now()})

        step = next((s for s in job.steps if s.slug == step_slug), None)
        if step is None:
            raise NativeEventError(
                f"run {r.id} job {job_id!r} has no step {step_slug!r}"
            )

        if event == NativeRunEventType.STEP_STARTED:
            if job.started_at is None:
                job.started_at = _now()
            if step.started_at is None:
                step.started_at = _now()
            job.state = NativeStepState.ACTIVE
            step.state = NativeStepState.ACTIVE
        elif event == NativeRunEventType.STEP_COMPLETED:
            if step.started_at is None:
                step.started_at = _now()
            step.completed_at = _now()
            step.exit_code = exit_code
            step.message = message
            step.state = NativeStepState.SUCCEEDED
            _refresh_job_state(job)
        elif event == NativeRunEventType.STEP_SKIPPED:
            if job.started_at is None:
                job.started_at = _now()
            if step.started_at is None:
                step.started_at = _now()
            step.completed_at = _now()
            step.exit_code = exit_code
            step.message = message
            step.state = NativeStepState.SKIPPED
            _refresh_job_state(job)
        elif event == NativeRunEventType.STEP_FAILED:
            if step.started_at is None:
                step.started_at = _now()
            step.completed_at = _now()
            step.exit_code = exit_code
            step.message = message
            step.state = NativeStepState.FAILED
            job.state = NativeStepState.FAILED
            job.completed_at = _now()

        return r.model_copy(update={"updated_at": _now()})

    return await _retry_on_conflict(cosmos, run, etag, apply)


def _refresh_job_state(job: NativeJobAttempt) -> None:
    states = [step.state for step in job.steps]
    if any(state == NativeStepState.FAILED for state in states):
        job.state = NativeStepState.FAILED
        job.completed_at = _now()
        return
    if all(state == NativeStepState.SKIPPED for state in states):
        job.state = NativeStepState.SKIPPED
        job.completed_at = _now()
        return
    if all(state in TERMINAL_STEP_STATES for state in states):
        job.state = NativeStepState.SUCCEEDED
        job.completed_at = _now()
        return
    if any(state in TERMINAL_STEP_STATES for state in states):
        job.state = NativeStepState.ACTIVE


async def assert_native_completion_ready(cosmos: Cosmos, *, run: Run) -> None:
    """Validate the latest native attempt can complete.

    Completion requires every declared step to be terminal and every job's
    persisted event stream to have no sequence holes from 1..N.
    """
    if not run.attempts:
        raise NativeEventError(f"run {run.id} has no attempts")
    attempt = run.attempts[-1]
    if attempt.phase_kind != "k8s_job":
        raise NativeEventError(
            f"run {run.id} latest attempt is {attempt.phase_kind!r}, not 'k8s_job'"
        )
    for job in attempt.jobs:
        for step in job.steps:
            if step.state not in TERMINAL_STEP_STATES:
                raise NativeEventError(
                    f"run {run.id} job {job.job_id!r} step {step.slug!r} "
                    f"is {step.state.value}, not terminal"
                )
        docs = await query_all(
            cosmos.run_events,
            (
                "SELECT * FROM c WHERE c.project = @p AND c.run_id = @r "
                "AND c.attempt_index = @a AND c.job_id = @j ORDER BY c.seq ASC"
            ),
            parameters=[
                {"name": "@p", "value": run.project},
                {"name": "@r", "value": run.id},
                {"name": "@a", "value": attempt.attempt_index},
                {"name": "@j", "value": job.job_id},
            ],
        )
        seqs = [int(d["seq"]) for d in docs]
        if not seqs:
            raise NativeEventError(
                f"run {run.id} job {job.job_id!r} has no native events"
            )
        expected = list(range(1, seqs[-1] + 1))
        if seqs != expected:
            raise NativeEventError(
                f"run {run.id} job {job.job_id!r} event sequence has gaps: "
                f"got {seqs}, expected {expected}"
            )


async def list_native_events(
    cosmos: Cosmos,
    *,
    project: str,
    run_id: str,
    attempt_index: int | None = None,
    job_id: str | None = None,
    limit: int | None = None,
) -> list[dict[str, Any]]:
    """Return hot native event/log rows in execution order.

    Cosmos keeps these rows only for the hot-retention window. Archived
    attempts expose their blob URL on the PhaseAttempt; rehydrating old
    archives is a separate read path.
    """
    where = ["c.project = @p", "c.run_id = @r"]
    parameters: list[dict[str, Any]] = [
        {"name": "@p", "value": project},
        {"name": "@r", "value": run_id},
    ]
    if attempt_index is not None:
        where.append("c.attempt_index = @a")
        parameters.append({"name": "@a", "value": attempt_index})
    if job_id is not None:
        where.append("c.job_id = @j")
        parameters.append({"name": "@j", "value": job_id})

    docs = await query_all(
        cosmos.run_events,
        f"SELECT * FROM c WHERE {' AND '.join(where)}",
        parameters=parameters,
    )
    docs.sort(key=lambda d: (
        int(d.get("attempt_index") or 0),
        str(d.get("job_id") or ""),
        int(d.get("seq") or 0),
    ))
    if limit is not None:
        docs = docs[:limit]
    return [
        {k: v for k, v in doc.items() if not k.startswith("_")}
        for doc in docs
    ]


def _validate_target(
    run: Run,
    *,
    job_id: str,
    event: NativeRunEventType,
    step_slug: str | None,
) -> None:
    attempt = run.attempts[-1]
    if attempt.phase_kind != "k8s_job":
        raise NativeEventError(
            f"run {run.id} latest attempt is {attempt.phase_kind!r}, not 'k8s_job'"
        )
    job = next((j for j in attempt.jobs if j.job_id == job_id), None)
    if job is None:
        raise NativeEventError(f"run {run.id} latest attempt has no job {job_id!r}")
    if event != NativeRunEventType.LOG:
        if not step_slug:
            raise NativeEventError(f"{event.value} requires step_slug")
        if not any(s.slug == step_slug for s in job.steps):
            raise NativeEventError(
                f"run {run.id} job {job_id!r} has no step {step_slug!r}"
            )


def _same_event(existing: dict[str, Any], incoming: dict[str, Any]) -> bool:
    keys = ("event", "step_slug", "message", "exit_code", "metadata")
    return all(existing.get(k) == incoming.get(k) for k in keys)

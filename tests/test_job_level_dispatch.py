"""Job-level concurrent dispatch — per-job aggregation.

Phases with N `phase.jobs[*]` now dispatch as N k8s Jobs. Each posts
its own completion via `/v1/runs/{...}/native/completed` with `job_id`.
The phase-level decision engine fires only once every sibling has
reported terminal state.

These tests cover the helpers in `glimmung.app` that drive the
aggregation: `_resolve_native_job_id`, `_attempt_all_jobs_terminal`,
and `_aggregate_attempt_from_jobs`. Pure-function, no I/O.
"""

from __future__ import annotations

from datetime import UTC, datetime

import pytest
from fastapi import HTTPException

from glimmung.app import (
    _aggregate_attempt_from_jobs,
    _attempt_all_jobs_terminal,
    _resolve_native_job_id,
)
from glimmung.models import (
    NativeJobAttempt,
    NativeStepState,
    PhaseAttempt,
    VerificationResult,
)


def _attempt(jobs):
    return PhaseAttempt(
        attempt_index=0,
        phase="work",
        phase_kind="k8s_job",
        workflow_filename="k8s_job:work",
        dispatched_at=datetime.now(UTC),
        jobs=jobs,
    )


def _job(job_id, **overrides):
    return NativeJobAttempt(
        job_id=job_id,
        state=overrides.pop("state", NativeStepState.PENDING),
        **overrides,
    )


# ─── _resolve_native_job_id ────────────────────────────────────────────


def test_resolve_job_id_defaults_to_only_job_for_single_job_phase():
    """Single-job phase callbacks still work without `job_id` — the
    only job is implicit. This is the legacy / current ambience shape
    so existing runner scripts keep working."""
    attempt = _attempt([_job("env-prep")])
    assert _resolve_native_job_id(attempt, None) == "env-prep"


def test_resolve_job_id_requires_explicit_id_for_multi_job_phase():
    """Multi-job phase callbacks must include `job_id` so the right
    sibling is targeted."""
    attempt = _attempt([_job("plan"), _job("impl")])
    with pytest.raises(HTTPException) as excinfo:
        _resolve_native_job_id(attempt, None)
    assert excinfo.value.status_code == 400
    assert "job_id is required" in str(excinfo.value.detail)


def test_resolve_job_id_rejects_unknown_job_id():
    attempt = _attempt([_job("plan"), _job("impl")])
    with pytest.raises(HTTPException) as excinfo:
        _resolve_native_job_id(attempt, "verify")
    assert excinfo.value.status_code == 400


def test_resolve_job_id_returns_explicit_match():
    attempt = _attempt([_job("plan"), _job("impl")])
    assert _resolve_native_job_id(attempt, "impl") == "impl"


# ─── _attempt_all_jobs_terminal ───────────────────────────────────────


def test_all_jobs_terminal_false_until_every_sibling_reports():
    attempt = _attempt([
        _job("plan", completed_at=datetime.now(UTC)),
        _job("impl"),  # still pending
    ])
    assert _attempt_all_jobs_terminal(attempt) is False


def test_all_jobs_terminal_true_when_every_sibling_reports():
    now = datetime.now(UTC)
    attempt = _attempt([
        _job("plan", completed_at=now),
        _job("impl", completed_at=now),
    ])
    assert _attempt_all_jobs_terminal(attempt) is True


def test_all_jobs_terminal_true_for_empty_jobs_list():
    """Legacy fixture path (jobs[] empty) must not deadlock the
    aggregation. Returns True so the caller falls through to the old
    phase-level path."""
    attempt = _attempt([])
    assert _attempt_all_jobs_terminal(attempt) is True


# ─── _aggregate_attempt_from_jobs ─────────────────────────────────────


def test_aggregate_success_when_every_job_succeeded():
    now = datetime.now(UTC)
    attempt = _attempt([
        _job("plan", completed_at=now, conclusion="success",
             outputs={"plan": "p.md"}),
        _job("impl", completed_at=now, conclusion="success",
             outputs={"impl": "i.md"}),
    ])
    conclusion, outputs, verification = _aggregate_attempt_from_jobs(attempt)
    assert conclusion == "success"
    assert outputs == {"plan": "p.md", "impl": "i.md"}
    assert verification is None


def test_aggregate_failure_when_any_job_failed():
    """Any-failure → phase failure. The decision engine reads
    `phase_conclusion = failure` and routes to retry/abort the same way
    a single-job phase failure would."""
    now = datetime.now(UTC)
    attempt = _attempt([
        _job("plan", completed_at=now, conclusion="success",
             outputs={"plan": "p.md"}),
        _job("impl", completed_at=now, conclusion="failure"),
    ])
    conclusion, _outputs, _verification = _aggregate_attempt_from_jobs(attempt)
    assert conclusion == "failure"


def test_aggregate_picks_first_verification_emitted():
    """Verify-phase jobs are conventionally singletons, so at most one
    sibling carries verification. The aggregator surfaces it."""
    now = datetime.now(UTC)
    verdict = VerificationResult(
        schema_version=1,
        status="pass",
        reasons=[],
        evidence_refs=[],
        cost_usd=0.5,
    )
    attempt = _attempt([
        _job("plan", completed_at=now, conclusion="success"),
        _job("verify", completed_at=now, conclusion="success",
             verification=verdict),
    ])
    _conclusion, _outputs, verification = _aggregate_attempt_from_jobs(attempt)
    assert verification is verdict


def test_aggregate_outputs_merge_last_writer_wins_on_collision():
    """Phase-level output validation downstream catches conflicts
    against declared `phase.outputs`; the aggregator itself is
    permissive."""
    now = datetime.now(UTC)
    attempt = _attempt([
        _job("a", completed_at=now, conclusion="success",
             outputs={"shared": "from-a", "only-a": "x"}),
        _job("b", completed_at=now, conclusion="success",
             outputs={"shared": "from-b", "only-b": "y"}),
    ])
    _conclusion, outputs, _verification = _aggregate_attempt_from_jobs(attempt)
    assert outputs == {
        "shared": "from-b",
        "only-a": "x",
        "only-b": "y",
    }

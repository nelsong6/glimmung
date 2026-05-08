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


# ─── _require_native_attempt_token (per-job tokens) ───────────────────


from datetime import UTC, datetime as _datetime  # noqa: E402

import pytest  # noqa: E402

from glimmung.app import (  # noqa: E402
    _attempt_token_sha256,
    _require_native_attempt_token,
)
from glimmung.models import (  # noqa: E402
    BudgetConfig,
    PhaseAttempt as _PhaseAttempt,
    Run,
    RunState,
)


class _FakeRequest:
    def __init__(self, token: str | None):
        self.headers = {"x-glimmung-attempt-token": token} if token else {}


def _run_with_attempts(*attempts):
    now = _datetime.now(UTC)
    return Run(
        id="01TESTRUN", project="p", workflow="w", run_number=1,
        issue_number=1,
        attempts=list(attempts),
        cumulative_cost_usd=0.0,
        budget=BudgetConfig(total=25.0),
        state=RunState.IN_PROGRESS,
        created_at=now, updated_at=now,
    )


def _attempt_with_jobs(idx, jobs, *, attempt_token=None):
    now = _datetime.now(UTC)
    return _PhaseAttempt(
        attempt_index=idx,
        phase="work",
        phase_kind="k8s_job",
        workflow_filename="k8s_job:work",
        dispatched_at=now,
        capability_token_sha256=attempt_token,
        jobs=jobs,
    )


def test_token_validator_returns_attempt_index_and_job_id_for_per_job_token():
    """Post-fan-out shape: the presented token matches one job's hash;
    the validator returns BOTH the attempt index and that job's id."""
    plan_token = "plan-secret-token"
    impl_token = "impl-secret-token"
    attempt = _attempt_with_jobs(0, [
        _job("plan", capability_token_sha256=_attempt_token_sha256(plan_token)),
        _job("impl", capability_token_sha256=_attempt_token_sha256(impl_token)),
    ])
    run = _run_with_attempts(attempt)
    assert _require_native_attempt_token(_FakeRequest(plan_token), run) == (0, "plan")
    assert _require_native_attempt_token(_FakeRequest(impl_token), run) == (0, "impl")


def test_token_validator_falls_back_to_attempt_token_for_legacy_attempts():
    """Pre-fan-out attempts only have the per-attempt token. The
    validator returns (attempt_index, None) — the completion handler
    defaults to the only sibling on single-job phases."""
    legacy_token = "legacy-attempt-token"
    attempt = _attempt_with_jobs(
        0,
        [_job("agent")],
        attempt_token=_attempt_token_sha256(legacy_token),
    )
    run = _run_with_attempts(attempt)
    assert _require_native_attempt_token(_FakeRequest(legacy_token), run) == (0, None)


def test_token_validator_rejects_invalid_token_when_per_job_tokens_set():
    attempt = _attempt_with_jobs(0, [
        _job("plan", capability_token_sha256=_attempt_token_sha256("real-token")),
    ])
    run = _run_with_attempts(attempt)
    with pytest.raises(Exception) as excinfo:
        _require_native_attempt_token(_FakeRequest("wrong-token"), run)
    assert "invalid x-glimmung-attempt-token" in str(excinfo.value.detail)


def test_token_validator_no_token_mode_returns_latest_attempt_no_job_id():
    """No-token fixture path (used by older tests). When no attempt and
    no job carries a hash, fall back to (latest, None)."""
    attempt = _attempt_with_jobs(0, [_job("agent")])
    run = _run_with_attempts(attempt)
    assert _require_native_attempt_token(_FakeRequest(None), run) == (0, None)

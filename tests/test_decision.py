"""Decision engine unit tests. DoD #6 from glimmung#18:

> Decision engine has unit tests covering: pass-on-first, pass-on-retry,
> abort-on-budget, abort-on-attempts.

Plus the malformed-artifact and order-of-precedence cases the issue
implies but doesn't enumerate.
"""

from datetime import UTC, datetime

from glimmung.decision import abort_explanation, decide
from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    Run,
    RunDecision,
    RunPhase,
    VerificationResult,
    VerificationStatus,
)


def _now() -> datetime:
    return datetime.now(UTC)


def _run(*, attempts: list[PhaseAttempt], cumulative_cost: float = 0.0,
         max_attempts: int = 3, max_cost: float = 25.0) -> Run:
    return Run(
        id="01J000000000000000000000RUN",
        project="spirelens",
        workflow="issue-agent",
        issue_repo="nelsong6/spirelens",
        issue_number=42,
        budget=BudgetConfig(max_attempts=max_attempts, max_cost_usd=max_cost),
        attempts=attempts,
        cumulative_cost_usd=cumulative_cost,
        created_at=_now(),
        updated_at=_now(),
    )


def _attempt(idx: int, phase: RunPhase, *, status: VerificationStatus | None,
             cost_usd: float = 0.0, reasons: list[str] | None = None) -> PhaseAttempt:
    verification = None
    if status is not None:
        verification = VerificationResult(
            status=status,
            cost_usd=cost_usd,
            reasons=reasons or [],
        )
    return PhaseAttempt(
        attempt_index=idx,
        phase=phase,
        workflow_filename=f"phase-{idx}.yml",
        dispatched_at=_now(),
        completed_at=_now(),
        conclusion="success" if status == VerificationStatus.PASS else "failure",
        verification=verification,
    )


# ─── DoD coverage ─────────────────────────────────────────────────────────────


def test_pass_on_first_attempt_advances():
    run = _run(
        attempts=[_attempt(0, RunPhase.INITIAL, status=VerificationStatus.PASS, cost_usd=2.5)],
        cumulative_cost=2.5,
    )
    assert decide(run) == RunDecision.ADVANCE


def test_fail_on_first_within_budget_retries():
    run = _run(
        attempts=[_attempt(0, RunPhase.INITIAL, status=VerificationStatus.FAIL, cost_usd=3.0)],
        cumulative_cost=3.0,
    )
    assert decide(run) == RunDecision.RETRY


def test_pass_on_retry_advances():
    run = _run(
        attempts=[
            _attempt(0, RunPhase.INITIAL, status=VerificationStatus.FAIL, cost_usd=3.0),
            _attempt(1, RunPhase.RETRY,   status=VerificationStatus.PASS, cost_usd=4.0),
        ],
        cumulative_cost=7.0,
    )
    assert decide(run) == RunDecision.ADVANCE


def test_abort_on_attempts_budget():
    """3 attempts (default max), all failing — out of retries."""
    run = _run(
        attempts=[
            _attempt(0, RunPhase.INITIAL, status=VerificationStatus.FAIL, cost_usd=2.0),
            _attempt(1, RunPhase.RETRY,   status=VerificationStatus.FAIL, cost_usd=2.0),
            _attempt(2, RunPhase.RETRY,   status=VerificationStatus.FAIL, cost_usd=2.0),
        ],
        cumulative_cost=6.0,
        max_attempts=3,
    )
    assert decide(run) == RunDecision.ABORT_BUDGET_ATTEMPTS


def test_abort_on_cost_budget():
    """Single attempt that ate the entire $25 budget."""
    run = _run(
        attempts=[_attempt(0, RunPhase.INITIAL, status=VerificationStatus.FAIL, cost_usd=30.0)],
        cumulative_cost=30.0,
        max_attempts=5,
        max_cost=25.0,
    )
    assert decide(run) == RunDecision.ABORT_BUDGET_COST


# ─── Beyond-DoD edge cases ────────────────────────────────────────────────────


def test_missing_verification_artifact_is_terminal():
    """No verification.json => contract violation. Retry would just
    reproduce the same broken artifact."""
    attempt = PhaseAttempt(
        attempt_index=0, phase=RunPhase.INITIAL,
        workflow_filename="x.yml", dispatched_at=_now(), completed_at=_now(),
        conclusion="success", verification=None,
    )
    run = _run(attempts=[attempt])
    assert decide(run) == RunDecision.ABORT_MALFORMED


def test_error_status_treated_as_failure_for_retry():
    """ERROR (producer crashed mid-verify) is a substantive verdict —
    retry up to budget, distinct from missing artifact."""
    run = _run(
        attempts=[_attempt(0, RunPhase.INITIAL, status=VerificationStatus.ERROR, cost_usd=1.0)],
        cumulative_cost=1.0,
    )
    assert decide(run) == RunDecision.RETRY


def test_cost_gate_wins_over_attempts_gate():
    """Both gates trip at once — cost wins (it's the harder cap)."""
    run = _run(
        attempts=[
            _attempt(0, RunPhase.INITIAL, status=VerificationStatus.FAIL, cost_usd=12.0),
            _attempt(1, RunPhase.RETRY,   status=VerificationStatus.FAIL, cost_usd=15.0),
            _attempt(2, RunPhase.RETRY,   status=VerificationStatus.FAIL, cost_usd=10.0),
        ],
        cumulative_cost=37.0,
        max_attempts=3,
        max_cost=25.0,
    )
    assert decide(run) == RunDecision.ABORT_BUDGET_COST


def test_pass_wins_over_budget_breach():
    """Even if cost or attempts are over budget, a PASS verdict still
    advances — the run achieved the goal, the budget gate is moot."""
    run = _run(
        attempts=[
            _attempt(0, RunPhase.INITIAL, status=VerificationStatus.FAIL, cost_usd=10.0),
            _attempt(1, RunPhase.RETRY,   status=VerificationStatus.PASS, cost_usd=20.0),
        ],
        cumulative_cost=30.0,
        max_attempts=2,
        max_cost=25.0,
    )
    assert decide(run) == RunDecision.ADVANCE


def test_decide_raises_on_empty_attempts():
    run = _run(attempts=[])
    try:
        decide(run)
    except ValueError:
        return
    raise AssertionError("expected ValueError on empty attempts")


def test_label_override_widens_attempts():
    """A 5x50 label override is honored — fail at attempt 3 stays in
    the loop because max is now 5."""
    run = _run(
        attempts=[
            _attempt(0, RunPhase.INITIAL, status=VerificationStatus.FAIL, cost_usd=3.0),
            _attempt(1, RunPhase.RETRY,   status=VerificationStatus.FAIL, cost_usd=3.0),
            _attempt(2, RunPhase.RETRY,   status=VerificationStatus.FAIL, cost_usd=3.0),
        ],
        cumulative_cost=9.0,
        max_attempts=5,
        max_cost=50.0,
    )
    assert decide(run) == RunDecision.RETRY


# ─── abort_explanation surface ────────────────────────────────────────────────


def test_abort_explanation_attempts_includes_count():
    run = _run(
        attempts=[
            _attempt(0, RunPhase.INITIAL, status=VerificationStatus.FAIL, cost_usd=1.0,
                     reasons=["selector .foo not found"]),
            _attempt(1, RunPhase.RETRY,   status=VerificationStatus.FAIL, cost_usd=1.0),
            _attempt(2, RunPhase.RETRY,   status=VerificationStatus.FAIL, cost_usd=1.0,
                     reasons=["expected status 200, got 500"]),
        ],
        cumulative_cost=3.0,
    )
    text = abort_explanation(run, RunDecision.ABORT_BUDGET_ATTEMPTS)
    assert "max_attempts=3" in text
    assert "expected status 200" in text


def test_abort_explanation_cost_includes_amounts():
    run = _run(
        attempts=[_attempt(0, RunPhase.INITIAL, status=VerificationStatus.FAIL, cost_usd=30.0)],
        cumulative_cost=30.0,
    )
    text = abort_explanation(run, RunDecision.ABORT_BUDGET_COST)
    assert "$30.00" in text
    assert "$25.00" in text


def test_abort_explanation_malformed_self_explanatory():
    attempt = PhaseAttempt(
        attempt_index=0, phase=RunPhase.INITIAL,
        workflow_filename="x.yml", dispatched_at=_now(), completed_at=_now(),
        verification=None,
    )
    run = _run(attempts=[attempt])
    text = abort_explanation(run, RunDecision.ABORT_MALFORMED)
    assert "verification.json" in text

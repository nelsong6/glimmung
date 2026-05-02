"""Decision engine unit tests, restructured for the #69 phase model.

Phases carry their own recycle_policy now; budget is just the cumulative
cost cap. The decision function takes (run, workflow) and routes by the
attempt's phase. ERROR is still retryable (substantive verdict);
malformed artifact is still terminal unless `verify_malformed` is in
the recycle policy's `on:` list.
"""

from datetime import UTC, datetime

from glimmung.decision import abort_explanation, decide
from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    PhaseSpec,
    PrPrimitiveSpec,
    RecyclePolicy,
    Run,
    RunDecision,
    VerificationResult,
    VerificationStatus,
    Workflow,
)


def _now() -> datetime:
    return datetime.now(UTC)


def _workflow(
    *,
    phase_name: str = "agent",
    verify: bool = True,
    max_attempts: int = 3,
    on: tuple[str, ...] = ("verify_fail",),
    total: float = 25.0,
) -> Workflow:
    return Workflow(
        id="issue-agent",
        project="spirelens",
        name="issue-agent",
        phases=[PhaseSpec(
            name=phase_name,
            kind="gha_dispatch",
            workflow_filename="agent.yml",
            verify=verify,
            recycle_policy=RecyclePolicy(
                max_attempts=max_attempts,
                on=list(on),
                lands_at="self",
            ) if verify else None,
        )],
        pr=PrPrimitiveSpec(),
        budget=BudgetConfig(total=total),
        created_at=_now(),
    )


def _run(*, attempts: list[PhaseAttempt], cumulative_cost: float = 0.0,
         total: float = 25.0) -> Run:
    return Run(
        id="01J000000000000000000000RUN",
        project="spirelens",
        workflow="issue-agent",
        issue_repo="nelsong6/spirelens",
        issue_number=42,
        budget=BudgetConfig(total=total),
        attempts=attempts,
        cumulative_cost_usd=cumulative_cost,
        created_at=_now(),
        updated_at=_now(),
    )


def _attempt(idx: int, phase_name: str, *, status: VerificationStatus | None,
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
        phase=phase_name,
        workflow_filename=f"phase-{idx}.yml",
        dispatched_at=_now(),
        completed_at=_now(),
        conclusion="success" if status == VerificationStatus.PASS else "failure",
        verification=verification,
    )


# ─── DoD coverage ─────────────────────────────────────────────────────────────


def test_pass_on_first_attempt_advances():
    wf = _workflow()
    run = _run(
        attempts=[_attempt(0, "agent", status=VerificationStatus.PASS, cost_usd=2.5)],
        cumulative_cost=2.5,
    )
    assert decide(run, wf) == RunDecision.ADVANCE


def test_fail_on_first_within_budget_retries():
    wf = _workflow()
    run = _run(
        attempts=[_attempt(0, "agent", status=VerificationStatus.FAIL, cost_usd=3.0)],
        cumulative_cost=3.0,
    )
    assert decide(run, wf) == RunDecision.RETRY


def test_pass_on_retry_advances():
    wf = _workflow()
    run = _run(
        attempts=[
            _attempt(0, "agent", status=VerificationStatus.FAIL, cost_usd=3.0),
            _attempt(1, "agent", status=VerificationStatus.PASS, cost_usd=4.0),
        ],
        cumulative_cost=7.0,
    )
    assert decide(run, wf) == RunDecision.ADVANCE


def test_abort_on_attempts_budget():
    """3 attempts on the same phase, all failing — out of recycle budget."""
    wf = _workflow(max_attempts=3)
    run = _run(
        attempts=[
            _attempt(0, "agent", status=VerificationStatus.FAIL, cost_usd=2.0),
            _attempt(1, "agent", status=VerificationStatus.FAIL, cost_usd=2.0),
            _attempt(2, "agent", status=VerificationStatus.FAIL, cost_usd=2.0),
        ],
        cumulative_cost=6.0,
    )
    assert decide(run, wf) == RunDecision.ABORT_BUDGET_ATTEMPTS


def test_abort_on_cost_budget():
    """Single attempt that ate the entire $25 budget."""
    wf = _workflow(max_attempts=5, total=25.0)
    run = _run(
        attempts=[_attempt(0, "agent", status=VerificationStatus.FAIL, cost_usd=30.0)],
        cumulative_cost=30.0,
        total=25.0,
    )
    assert decide(run, wf) == RunDecision.ABORT_BUDGET_COST


# ─── Edge cases ────────────────────────────────────────────────────────────────


def test_missing_verification_artifact_is_terminal_when_not_in_recycle_on():
    """No verification.json + verify_malformed not in recycle.on => abort."""
    wf = _workflow(on=("verify_fail",))
    attempt = PhaseAttempt(
        attempt_index=0, phase="agent",
        workflow_filename="x.yml", dispatched_at=_now(), completed_at=_now(),
        conclusion="success", verification=None,
    )
    run = _run(attempts=[attempt])
    assert decide(run, wf) == RunDecision.ABORT_MALFORMED


def test_malformed_with_recycle_on_retries():
    """When verify_malformed is in recycle.on, malformed becomes retryable."""
    wf = _workflow(on=("verify_fail", "verify_malformed"))
    attempt = PhaseAttempt(
        attempt_index=0, phase="agent",
        workflow_filename="x.yml", dispatched_at=_now(), completed_at=_now(),
        conclusion="success", verification=None,
    )
    run = _run(attempts=[attempt])
    assert decide(run, wf) == RunDecision.RETRY


def test_error_status_treated_as_verify_fail():
    """ERROR is a substantive failure — retryable per `verify_fail` trigger."""
    wf = _workflow()
    run = _run(
        attempts=[_attempt(0, "agent", status=VerificationStatus.ERROR, cost_usd=1.0)],
        cumulative_cost=1.0,
    )
    assert decide(run, wf) == RunDecision.RETRY


def test_cost_gate_wins_over_attempts_gate():
    """Both gates trip at once — cost wins (it's the harder cap)."""
    wf = _workflow(max_attempts=3, total=25.0)
    run = _run(
        attempts=[
            _attempt(0, "agent", status=VerificationStatus.FAIL, cost_usd=12.0),
            _attempt(1, "agent", status=VerificationStatus.FAIL, cost_usd=15.0),
            _attempt(2, "agent", status=VerificationStatus.FAIL, cost_usd=10.0),
        ],
        cumulative_cost=37.0,
        total=25.0,
    )
    assert decide(run, wf) == RunDecision.ABORT_BUDGET_COST


def test_pass_wins_over_budget_breach():
    """A PASS verdict on the latest attempt advances regardless of budget."""
    wf = _workflow(max_attempts=2, total=25.0)
    run = _run(
        attempts=[
            _attempt(0, "agent", status=VerificationStatus.FAIL, cost_usd=10.0),
            _attempt(1, "agent", status=VerificationStatus.PASS, cost_usd=20.0),
        ],
        cumulative_cost=30.0,
        total=25.0,
    )
    assert decide(run, wf) == RunDecision.ADVANCE


def test_decide_raises_on_empty_attempts():
    wf = _workflow()
    run = _run(attempts=[])
    try:
        decide(run, wf)
    except ValueError:
        return
    raise AssertionError("expected ValueError on empty attempts")


def test_non_verify_phase_failure_ends_run():
    """Hard rule: non-verify phase, non-success conclusion → abort."""
    wf = _workflow(verify=False)
    attempt = PhaseAttempt(
        attempt_index=0, phase="agent",
        workflow_filename="x.yml", dispatched_at=_now(), completed_at=_now(),
        conclusion="failure",
    )
    run = _run(attempts=[attempt])
    assert decide(run, wf) == RunDecision.ABORT_MALFORMED


def test_non_verify_phase_success_advances():
    wf = _workflow(verify=False)
    attempt = PhaseAttempt(
        attempt_index=0, phase="agent",
        workflow_filename="x.yml", dispatched_at=_now(), completed_at=_now(),
        conclusion="success",
    )
    run = _run(attempts=[attempt])
    assert decide(run, wf) == RunDecision.ADVANCE


# ─── abort_explanation surface ────────────────────────────────────────────────


def test_abort_explanation_attempts_includes_count():
    wf = _workflow(max_attempts=3)
    run = _run(
        attempts=[
            _attempt(0, "agent", status=VerificationStatus.FAIL, cost_usd=1.0,
                     reasons=["selector .foo not found"]),
            _attempt(1, "agent", status=VerificationStatus.FAIL, cost_usd=1.0),
            _attempt(2, "agent", status=VerificationStatus.FAIL, cost_usd=1.0,
                     reasons=["expected status 200, got 500"]),
        ],
        cumulative_cost=3.0,
    )
    text = abort_explanation(run, wf, RunDecision.ABORT_BUDGET_ATTEMPTS)
    assert "max_attempts=3" in text
    assert "expected status 200" in text


def test_abort_explanation_cost_includes_amounts():
    wf = _workflow()
    run = _run(
        attempts=[_attempt(0, "agent", status=VerificationStatus.FAIL, cost_usd=30.0)],
        cumulative_cost=30.0,
    )
    text = abort_explanation(run, wf, RunDecision.ABORT_BUDGET_COST)
    assert "$30.00" in text
    assert "$25.00" in text


def test_abort_explanation_malformed_self_explanatory():
    wf = _workflow()
    attempt = PhaseAttempt(
        attempt_index=0, phase="agent",
        workflow_filename="x.yml", dispatched_at=_now(), completed_at=_now(),
        verification=None,
    )
    run = _run(attempts=[attempt])
    text = abort_explanation(run, wf, RunDecision.ABORT_MALFORMED)
    assert "verification.json" in text

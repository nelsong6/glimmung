"""Triage decision engine — pure-function unit tests, restructured for
the #69 phase model. `decide_triage` now takes (signal, run, workflow)
and reads `workflow.pr.recycle_policy` for both the actionability gate
(no policy → IGNORE) and the attempts cap (counted against the lands_at
phase). Cost cap is run-level via `budget.total`.
"""

from datetime import UTC, datetime

from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    PhaseSpec,
    PrPrimitiveSpec,
    RecyclePolicy,
    Run,
    Signal,
    SignalSource,
    SignalState,
    SignalTargetType,
    TriageDecision,
    VerificationResult,
    VerificationStatus,
    Workflow,
)
from glimmung.triage import abort_explanation, decide_triage, feedback_text


def _now() -> datetime:
    return datetime.now(UTC)


def _workflow(
    *,
    phase_name: str = "agent",
    max_attempts: int = 3,
    on: tuple[str, ...] = ("pr_review_changes_requested",),
    pr_recycle: bool = True,
    total: float = 25.0,
) -> Workflow:
    return Workflow(
        id="issue-agent",
        project="ambience",
        name="issue-agent",
        phases=[PhaseSpec(
            name=phase_name,
            kind="gha_dispatch",
            workflow_filename="agent.yml",
            verify=True,
            recycle_policy=RecyclePolicy(
                max_attempts=3, on=["verify_fail"], lands_at="self",
            ),
        )],
        pr=PrPrimitiveSpec(
            recycle_policy=RecyclePolicy(
                max_attempts=max_attempts,
                on=list(on),
                lands_at=phase_name,
            ) if pr_recycle else None,
        ),
        budget=BudgetConfig(total=total),
        created_at=_now(),
    )


def _run(*, attempts: int = 1, cumulative_cost: float = 2.0,
         total: float = 25.0,
         pr_number: int | None = 100,
         phase_name: str = "agent") -> Run:
    phase_attempts = [
        PhaseAttempt(
            attempt_index=i,
            phase=phase_name,
            workflow_filename=f"phase-{i}.yml",
            dispatched_at=_now(),
            completed_at=_now(),
            verification=VerificationResult(status=VerificationStatus.PASS),
        )
        for i in range(attempts)
    ]
    return Run(
        id="01J0RUNRUNRUNRUNRUNRUNRUN",
        project="ambience",
        workflow="issue-agent",
        issue_repo="nelsong6/ambience",
        issue_number=42,
        budget=BudgetConfig(total=total),
        attempts=phase_attempts,
        cumulative_cost_usd=cumulative_cost,
        pr_number=pr_number,
        created_at=_now(),
        updated_at=_now(),
    )


def _signal(*, source: SignalSource, payload: dict | None = None,
            target_id: str = "100",
            target_type: SignalTargetType = SignalTargetType.PR) -> Signal:
    return Signal(
        id="01JSIGNALSIGNALSIGNALSI",
        target_type=target_type,
        target_repo="nelsong6/ambience",
        target_id=target_id,
        source=source,
        payload=payload or {},
        state=SignalState.PROCESSING,
        enqueued_at=_now(),
    )


# ─── ABORT_NO_RUN ────────────────────────────────────────────────────────────


def test_abort_no_run_when_pr_not_tracked():
    s = _signal(source=SignalSource.GLIMMUNG_UI, payload={"kind": "reject", "feedback": "fix x"})
    assert decide_triage(signal=s, run=None, workflow=_workflow()) == TriageDecision.ABORT_NO_RUN


# ─── DISPATCH_TRIAGE ────────────────────────────────────────────────────────


def test_dispatch_for_glimmung_ui_reject_with_feedback():
    s = _signal(
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "the date format is wrong"},
    )
    assert decide_triage(signal=s, run=_run(), workflow=_workflow()) == TriageDecision.DISPATCH_TRIAGE


def test_dispatch_for_gh_review_changes_requested_with_body():
    s = _signal(
        source=SignalSource.GH_REVIEW,
        payload={"state": "changes_requested", "body": "missing test for the regex case"},
    )
    assert decide_triage(signal=s, run=_run(), workflow=_workflow()) == TriageDecision.DISPATCH_TRIAGE


# ─── IGNORE branches ────────────────────────────────────────────────────────


def test_ignore_when_workflow_has_no_pr_recycle_policy():
    """Workflow didn't opt into PR-feedback recycle — actionable signal IGNOREs."""
    s = _signal(
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "fix"},
    )
    wf = _workflow(pr_recycle=False)
    assert decide_triage(signal=s, run=_run(), workflow=wf) == TriageDecision.IGNORE


def test_ignore_for_glimmung_ui_non_reject_kind():
    s = _signal(source=SignalSource.GLIMMUNG_UI, payload={"kind": "something_else"})
    assert decide_triage(signal=s, run=_run(), workflow=_workflow()) == TriageDecision.IGNORE


def test_ignore_for_gh_review_approved():
    s = _signal(source=SignalSource.GH_REVIEW, payload={"state": "approved"})
    assert decide_triage(signal=s, run=_run(), workflow=_workflow()) == TriageDecision.IGNORE


def test_ignore_for_gh_review_commented_state():
    s = _signal(
        source=SignalSource.GH_REVIEW,
        payload={"state": "commented", "body": "looks ok"},
    )
    assert decide_triage(signal=s, run=_run(), workflow=_workflow()) == TriageDecision.IGNORE


def test_ignore_for_gh_review_changes_requested_with_empty_body():
    s = _signal(
        source=SignalSource.GH_REVIEW,
        payload={"state": "changes_requested", "body": ""},
    )
    assert decide_triage(signal=s, run=_run(), workflow=_workflow()) == TriageDecision.IGNORE


def test_ignore_for_gh_review_comment_source():
    s = _signal(
        source=SignalSource.GH_REVIEW_COMMENT,
        payload={"body": "this looks weird"},
    )
    assert decide_triage(signal=s, run=_run(), workflow=_workflow()) == TriageDecision.IGNORE


def test_ignore_for_gh_comment_source():
    s = _signal(source=SignalSource.GH_COMMENT, payload={"body": "thoughts?"})
    assert decide_triage(signal=s, run=_run(), workflow=_workflow()) == TriageDecision.IGNORE


# ─── budget gates ───────────────────────────────────────────────────────────


def test_abort_budget_cost_when_at_cap():
    s = _signal(
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "fix"},
    )
    run = _run(cumulative_cost=25.0, total=25.0, attempts=1)
    wf = _workflow(total=25.0)
    assert decide_triage(signal=s, run=run, workflow=wf) == TriageDecision.ABORT_BUDGET_COST


def test_abort_budget_attempts_when_at_cap():
    s = _signal(
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "fix"},
    )
    # 3 attempts on the agent phase, cap is 3 — at cap.
    run = _run(cumulative_cost=2.0, attempts=3)
    wf = _workflow(max_attempts=3)
    assert decide_triage(signal=s, run=run, workflow=wf) == TriageDecision.ABORT_BUDGET_ATTEMPTS


def test_cost_gate_wins_over_attempts_gate():
    s = _signal(
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "fix"},
    )
    run = _run(cumulative_cost=30.0, total=25.0, attempts=3)
    wf = _workflow(max_attempts=3, total=25.0)
    assert decide_triage(signal=s, run=run, workflow=wf) == TriageDecision.ABORT_BUDGET_COST


def test_actionability_check_runs_before_budget():
    """IGNORE-shape signal with budget-exhausted Run still IGNOREs."""
    s = _signal(source=SignalSource.GH_REVIEW, payload={"state": "approved"})
    run = _run(cumulative_cost=30.0, total=25.0)
    assert decide_triage(signal=s, run=run, workflow=_workflow(total=25.0)) == TriageDecision.IGNORE


# ─── feedback_text ───────────────────────────────────────────────────────────


def test_feedback_text_from_glimmung_ui():
    s = _signal(
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "the date format is wrong, should be ISO"},
    )
    assert feedback_text(s) == "the date format is wrong, should be ISO"


def test_feedback_text_from_gh_review():
    s = _signal(
        source=SignalSource.GH_REVIEW,
        payload={"state": "changes_requested", "body": "needs more tests"},
    )
    assert feedback_text(s) == "needs more tests"


def test_feedback_text_empty_for_unsupported_source():
    s = _signal(source=SignalSource.GH_COMMENT, payload={"body": "hi"})
    assert feedback_text(s) == ""


# ─── abort_explanation ──────────────────────────────────────────────────────


def test_abort_explanation_no_run_includes_target():
    s = _signal(source=SignalSource.GLIMMUNG_UI, payload={"kind": "reject"}, target_id="42")
    text = abort_explanation(TriageDecision.ABORT_NO_RUN, None, s)
    assert "ambience" in text
    assert "#42" in text


def test_abort_explanation_budget_cost_includes_amounts():
    s = _signal(source=SignalSource.GLIMMUNG_UI, payload={"kind": "reject"})
    run = _run(cumulative_cost=30.0, total=25.0)
    text = abort_explanation(TriageDecision.ABORT_BUDGET_COST, run, s)
    assert "$30.00" in text
    assert "$25.00" in text


def test_abort_explanation_budget_attempts_includes_counts():
    s = _signal(source=SignalSource.GLIMMUNG_UI, payload={"kind": "reject"})
    run = _run(attempts=3)
    wf = _workflow(max_attempts=3)
    text = abort_explanation(TriageDecision.ABORT_BUDGET_ATTEMPTS, run, s, wf)
    assert "3" in text
    assert "max_attempts" in text

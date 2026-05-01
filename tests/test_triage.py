"""Triage decision engine — pure-function unit tests.

Mirrors `tests/test_decision.py`'s style: run + signal fixtures,
explicit cases for every branch of `decide_triage`. No I/O.
"""

from datetime import UTC, datetime

from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    Run,
    RunPhase,
    Signal,
    SignalSource,
    SignalState,
    SignalTargetType,
    TriageDecision,
    VerificationResult,
    VerificationStatus,
)
from glimmung.triage import abort_explanation, decide_triage, feedback_text


def _now() -> datetime:
    return datetime.now(UTC)


def _run(*, attempts: int = 1, cumulative_cost: float = 2.0,
         max_attempts: int = 3, max_cost: float = 25.0,
         pr_number: int | None = 100) -> Run:
    phase_attempts = [
        PhaseAttempt(
            attempt_index=i,
            phase=RunPhase.INITIAL if i == 0 else RunPhase.RETRY,
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
        budget=BudgetConfig(max_attempts=max_attempts, max_cost_usd=max_cost),
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
    assert decide_triage(signal=s, run=None) == TriageDecision.ABORT_NO_RUN


# ─── DISPATCH_TRIAGE ────────────────────────────────────────────────────────


def test_dispatch_for_glimmung_ui_reject_with_feedback():
    s = _signal(
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "the date format is wrong"},
    )
    assert decide_triage(signal=s, run=_run()) == TriageDecision.DISPATCH_TRIAGE


def test_dispatch_for_gh_review_changes_requested_with_body():
    s = _signal(
        source=SignalSource.GH_REVIEW,
        payload={"state": "changes_requested", "body": "missing test for the regex case"},
    )
    assert decide_triage(signal=s, run=_run()) == TriageDecision.DISPATCH_TRIAGE


# ─── IGNORE branches ────────────────────────────────────────────────────────


def test_ignore_for_glimmung_ui_non_reject_kind():
    """UI sends signals for things other than reject (future: re-run, cancel).
    Triage only acts on `kind == "reject"`."""
    s = _signal(source=SignalSource.GLIMMUNG_UI, payload={"kind": "something_else"})
    assert decide_triage(signal=s, run=_run()) == TriageDecision.IGNORE


def test_ignore_for_gh_review_approved():
    s = _signal(source=SignalSource.GH_REVIEW, payload={"state": "approved"})
    assert decide_triage(signal=s, run=_run()) == TriageDecision.IGNORE


def test_ignore_for_gh_review_commented_state():
    """A 'commented' review is a no-action review — ignore."""
    s = _signal(
        source=SignalSource.GH_REVIEW,
        payload={"state": "commented", "body": "looks ok"},
    )
    assert decide_triage(signal=s, run=_run()) == TriageDecision.IGNORE


def test_ignore_for_gh_review_changes_requested_with_empty_body():
    """If a reviewer requests changes without saying what, we have no
    feedback to feed back. Ignore — treat like a non-actionable signal."""
    s = _signal(
        source=SignalSource.GH_REVIEW,
        payload={"state": "changes_requested", "body": ""},
    )
    assert decide_triage(signal=s, run=_run()) == TriageDecision.IGNORE


def test_ignore_for_gh_review_comment_source():
    """GH_REVIEW_COMMENT and GH_COMMENT need disambiguation between
    chatter and feedback. Out of scope for #19's first PR — ignored
    until a keyword/mention parser lands."""
    s = _signal(
        source=SignalSource.GH_REVIEW_COMMENT,
        payload={"body": "this looks weird"},
    )
    assert decide_triage(signal=s, run=_run()) == TriageDecision.IGNORE


def test_ignore_for_gh_comment_source():
    s = _signal(source=SignalSource.GH_COMMENT, payload={"body": "thoughts?"})
    assert decide_triage(signal=s, run=_run()) == TriageDecision.IGNORE


# ─── budget gates ───────────────────────────────────────────────────────────


def test_abort_budget_cost_when_at_cap():
    s = _signal(
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "fix"},
    )
    run = _run(cumulative_cost=25.0, max_cost=25.0, attempts=1, max_attempts=5)
    assert decide_triage(signal=s, run=run) == TriageDecision.ABORT_BUDGET_COST


def test_abort_budget_attempts_when_at_cap():
    s = _signal(
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "fix"},
    )
    run = _run(cumulative_cost=2.0, max_cost=25.0, attempts=3, max_attempts=3)
    assert decide_triage(signal=s, run=run) == TriageDecision.ABORT_BUDGET_ATTEMPTS


def test_cost_gate_wins_over_attempts_gate():
    """Both gates trip at once — cost wins (harder cap)."""
    s = _signal(
        source=SignalSource.GLIMMUNG_UI,
        payload={"kind": "reject", "feedback": "fix"},
    )
    run = _run(cumulative_cost=30.0, max_cost=25.0, attempts=3, max_attempts=3)
    assert decide_triage(signal=s, run=run) == TriageDecision.ABORT_BUDGET_COST


def test_actionability_check_runs_before_budget():
    """An IGNORE-shape signal with a budget-exhausted Run still IGNORES,
    not ABORTs. Decision precedence: no-run → actionability → budget."""
    s = _signal(source=SignalSource.GH_REVIEW, payload={"state": "approved"})
    run = _run(cumulative_cost=30.0, max_cost=25.0)
    assert decide_triage(signal=s, run=run) == TriageDecision.IGNORE


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
    run = _run(cumulative_cost=30.0, max_cost=25.0)
    text = abort_explanation(TriageDecision.ABORT_BUDGET_COST, run, s)
    assert "$30.00" in text
    assert "$25.00" in text


def test_abort_explanation_budget_attempts_includes_counts():
    s = _signal(source=SignalSource.GLIMMUNG_UI, payload={"kind": "reject"})
    run = _run(attempts=3, max_attempts=3)
    text = abort_explanation(TriageDecision.ABORT_BUDGET_ATTEMPTS, run, s)
    assert "3" in text
    assert "max_attempts" in text

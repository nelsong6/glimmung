"""PR triage decision engine (#19).

Pure function over `(signal, run_for_pr)` → `TriageDecision`. Side
effects (workflow dispatch, comment, lock release) live at the call
site in `app.py`, mirroring the discipline #18's `decide()` set up.

Inputs:
- `signal`: the Signal the drain loop is processing.
- `run`: the Run linked to this PR (`Run.pr_number == signal.target_id`),
  or `None` if no agent-tracked Run exists for this PR (manual PR,
  out-of-band branch, etc.).

Outputs (one of `TriageDecision`):
- `DISPATCH_TRIAGE` — fire the triage workflow with the feedback as
  context, append a TRIAGE PhaseAttempt to the Run.
- `IGNORE` — non-actionable signal: an "approved" review, a comment
  with no actionable content, etc.
- `ABORT_NO_RUN` — PR isn't agent-tracked; nothing to triage.
- `ABORT_BUDGET_ATTEMPTS` / `ABORT_BUDGET_COST` — Run has hit the
  cumulative budget gate; can't dispatch another attempt.

Decision precedence (mirrors #18's ordering): no-run → budget-cost →
budget-attempts → source-specific actionability → default IGNORE.

Sources covered in #19's first PR:
- `GLIMMUNG_UI` with `kind == "reject"` — always actionable (subject
  to budget). Payload contains `feedback` text.
- `GH_REVIEW` with `state == "changes_requested"` — actionable. Body
  is the feedback.
- All other GH webhook sources (review_comment / issue_comment /
  approved-review) → IGNORE.
"""

from __future__ import annotations

from glimmung.models import (
    Run,
    Signal,
    SignalSource,
    TriageDecision,
)


def decide_triage(
    *,
    signal: Signal,
    run: Run | None,
) -> TriageDecision:
    if run is None:
        return TriageDecision.ABORT_NO_RUN

    # Source-specific actionability — bail early on signals that aren't
    # PR-feedback-shaped, so we don't penalize them with a budget gate.
    if not _is_actionable_feedback(signal):
        return TriageDecision.IGNORE

    # Budget gates. Cost first (harder cap) — mirrors decide() in
    # decision.py. The triage attempt itself adds no cost yet (we're
    # deciding *whether* to dispatch); the cost is realized post-hoc
    # when the workflow_run.completed handler records the attempt's
    # `cost_usd`. The check is: would dispatching another attempt
    # exceed the budget assuming zero additional cost? If we're
    # already at-or-over the cap, no.
    if run.cumulative_cost_usd >= run.budget.max_cost_usd:
        return TriageDecision.ABORT_BUDGET_COST
    if len(run.attempts) >= run.budget.max_attempts:
        return TriageDecision.ABORT_BUDGET_ATTEMPTS

    return TriageDecision.DISPATCH_TRIAGE


def _is_actionable_feedback(signal: Signal) -> bool:
    if signal.source == SignalSource.GLIMMUNG_UI:
        return signal.payload.get("kind") == "reject"
    if signal.source == SignalSource.GH_REVIEW:
        # GH review states: approved, changes_requested, commented, dismissed.
        # Only changes_requested with a non-empty body is feedback we act on.
        if signal.payload.get("state") != "changes_requested":
            return False
        body = signal.payload.get("body") or ""
        return bool(body.strip())
    # GH_REVIEW_COMMENT and GH_COMMENT need keyword/mention parsing to
    # disambiguate "this is feedback for the agent" from chatter. Out
    # of scope for #19's first PR; explicitly route through GLIMMUNG_UI
    # or GH_REVIEW for now.
    return False


def feedback_text(signal: Signal) -> str:
    """Extract the human-readable feedback from a signal payload. Used
    by the triage workflow dispatch to populate the prompt context."""
    if signal.source == SignalSource.GLIMMUNG_UI:
        return str(signal.payload.get("feedback", ""))
    if signal.source == SignalSource.GH_REVIEW:
        return str(signal.payload.get("body", ""))
    return ""


def abort_explanation(decision: TriageDecision, run: Run | None, signal: Signal) -> str:
    """Human-readable comment body for issue/PR when triage aborts.
    Mirrors `decision.abort_explanation`'s style for consistency on
    the comment surface."""
    if decision == TriageDecision.ABORT_NO_RUN:
        return (
            f"Glimmung received a triage signal on {signal.target_repo}#{signal.target_id} "
            f"but couldn't find an agent-tracked Run for it. The PR may have been opened "
            f"manually, or the run-to-PR linkage didn't land. No action taken."
        )
    if decision == TriageDecision.ABORT_BUDGET_COST and run is not None:
        return (
            f"Glimmung can't dispatch a triage attempt: cumulative cost "
            f"`${run.cumulative_cost_usd:.2f}` is at-or-over the budget cap "
            f"`${run.budget.max_cost_usd:.2f}`. Increase the budget label "
            f"(`agent-budget:NxM`) on the originating issue or accept the PR as-is."
        )
    if decision == TriageDecision.ABORT_BUDGET_ATTEMPTS and run is not None:
        return (
            f"Glimmung can't dispatch a triage attempt: attempt count "
            f"`{len(run.attempts)}` is at-or-over the budget cap "
            f"`max_attempts={run.budget.max_attempts}`. Increase the budget label "
            f"(`agent-budget:NxM`) on the originating issue or accept the PR as-is."
        )
    return f"Triage aborted: {decision.value}"

"""PR-feedback decision engine (#19, restructured under #69 phases).

Pure function over `(signal, run, workflow)` → `TriageDecision`. Side
effects (workflow dispatch, comment, lock release) live at the call site
in `app.py`, mirroring the discipline #18's `decide()` set up.

Under #69, "triage" is no longer a separate workflow with its own
filename; it's the PR primitive's `recycle_policy` firing — when an
actionable PR-feedback signal arrives, dispatch the workflow for the
phase that `recycle_policy.lands_at` names. The decision logic stays
here as a thin layer that decides whether to dispatch at all, and the
caller in `app.py` does the dispatch with the new schema.

Inputs:
- `signal`: the Signal the drain loop is processing.
- `run`: the Run linked to this PR (`Run.pr_number == signal.target_id`),
  or `None` if no agent-tracked Run exists for this PR.
- `workflow`: the typed `Workflow` model (post-#69 schema). Reads
  `workflow.pr.recycle_policy` for the policy, `workflow.budget.total`
  for the run-cumulative cost cap.

Outputs (one of `TriageDecision`):
- `DISPATCH_TRIAGE` — fire a recycle dispatch at the policy's `lands_at`
  phase, with the feedback as additional context.
- `IGNORE` — non-actionable signal, or the workflow doesn't have a PR
  recycle policy configured.
- `ABORT_NO_RUN` — PR isn't agent-tracked.
- `ABORT_BUDGET_ATTEMPTS` / `ABORT_BUDGET_COST` — Run has hit its caps.
"""

from __future__ import annotations

from glimmung.models import (
    Run,
    Signal,
    SignalSource,
    TriageDecision,
    Workflow,
)


def decide_triage(
    *,
    signal: Signal,
    run: Run | None,
    workflow: Workflow,
) -> TriageDecision:
    if run is None:
        return TriageDecision.ABORT_NO_RUN

    if not _is_actionable_feedback(signal):
        return TriageDecision.IGNORE

    pr_rp = workflow.pr.recycle_policy
    if pr_rp is None:
        # Workflow didn't opt into PR-feedback recycle; the signal is
        # noted but not actionable.
        return TriageDecision.IGNORE

    # Cost first (harder cap) — mirrors decide() in decision.py.
    if run.cumulative_cost_usd >= run.budget.total:
        return TriageDecision.ABORT_BUDGET_COST

    # Per-phase attempt cap on the lands_at target. v1 conflates verify-
    # loop retries on the same phase and PR-primitive recycle dispatches
    # that land at it; both increment the same counter.
    target_attempts = sum(1 for a in run.attempts if a.phase == pr_rp.lands_at)
    if target_attempts >= pr_rp.max_attempts:
        return TriageDecision.ABORT_BUDGET_ATTEMPTS

    return TriageDecision.DISPATCH_TRIAGE


def _is_actionable_feedback(signal: Signal) -> bool:
    if signal.source == SignalSource.GLIMMUNG_UI:
        return signal.payload.get("kind") == "reject"
    if signal.source == SignalSource.GH_REVIEW:
        if signal.payload.get("state") != "changes_requested":
            return False
        body = signal.payload.get("body") or ""
        return bool(body.strip())
    # GH_REVIEW_COMMENT and GH_COMMENT need keyword/mention parsing
    # (`/agent revise`) — out of scope for v1; see spirelens#173.
    return False


def feedback_text(signal: Signal) -> str:
    """Extract human-readable feedback from a signal payload. Used by the
    PR-recycle dispatch to populate prompt context."""
    if signal.source == SignalSource.GLIMMUNG_UI:
        return str(signal.payload.get("feedback", ""))
    if signal.source == SignalSource.GH_REVIEW:
        return str(signal.payload.get("body", ""))
    return ""


def abort_explanation(
    decision: TriageDecision,
    run: Run | None,
    signal: Signal,
    workflow: Workflow | None = None,
) -> str:
    """Human-readable comment body for issue/PR when triage aborts."""
    if decision == TriageDecision.ABORT_NO_RUN:
        return (
            f"Glimmung received a PR-feedback signal on {signal.target_repo}#{signal.target_id} "
            f"but couldn't find an agent-tracked Run for it. The PR may have been opened "
            f"manually, or the run-to-PR linkage didn't land. No action taken."
        )
    if decision == TriageDecision.ABORT_BUDGET_COST and run is not None:
        return (
            f"Glimmung can't dispatch a recycle: cumulative cost "
            f"`${run.cumulative_cost_usd:.2f}` is at-or-over the budget cap "
            f"`${run.budget.total:.2f}`. Increase the budget label "
            f"(`agent-budget:M`) on the originating issue or accept the PR as-is."
        )
    if decision == TriageDecision.ABORT_BUDGET_ATTEMPTS and run is not None:
        cap = (
            workflow.pr.recycle_policy.max_attempts
            if (workflow and workflow.pr.recycle_policy) else None
        )
        cap_text = f"max_attempts={cap}" if cap is not None else "the configured cap"
        return (
            f"Glimmung can't dispatch a recycle: attempts on the recycle target "
            f"have reached {cap_text}. Increase the budget label or accept the PR as-is."
        )
    return f"Triage aborted: {decision.value}"

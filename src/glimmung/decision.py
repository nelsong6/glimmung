"""Verify-loop decision engine (#18).

A pure function `(state, last_phase_result) -> next_action`. No I/O, no
state mutation. Side effects (dispatching the retry workflow, posting the
abort comment, updating Cosmos) live entirely at the call site in
`app.py`. This separation is the discipline the issue calls out
explicitly:

>   Discipline: pure function; side effects only at the call site.

The set of decisions is closed (`RunDecision` enum); every reachable
state in the run model maps to exactly one decision so the caller never
has to ad-hoc a tie-breaker.

Decision order (first match wins):

  1. No verification artifact parsed   -> ABORT_MALFORMED
     The producer's contract was violated. Retrying the same producer
     gets us the same broken artifact.
  2. status == PASS                    -> ADVANCE
     Verification passed; consumer's PR-open step proceeds. (Today the
     consumer workflow opens the PR itself; ADVANCE is informational.
     When the orchestrator owns PR opening — Sprint 4 — ADVANCE
     becomes the trigger for that step.)
  3. cumulative_cost_usd >= budget     -> ABORT_BUDGET_COST
  4. attempts so far >= max_attempts   -> ABORT_BUDGET_ATTEMPTS
  5. otherwise (FAIL or ERROR)         -> RETRY

Cost is checked before attempts because it's the harder cap — a single
attempt could blow past the $25 default while still well under the
3-attempt cap. Either gate triggering is terminal.

`ERROR` (producer ran but couldn't reach a verdict) is treated as a
failure for retry purposes, distinct from `verification is None`
(producer didn't produce a contract-shaped artifact at all). The former
is a substantive verdict — "I tried, I don't know"; the latter is a
contract violation that retry won't fix.
"""

from glimmung.models import Run, RunDecision, VerificationStatus


def decide(run: Run) -> RunDecision:
    if not run.attempts:
        raise ValueError("decide() called on run with no attempts")

    last = run.attempts[-1]

    if last.verification is None:
        return RunDecision.ABORT_MALFORMED

    if last.verification.status == VerificationStatus.PASS:
        return RunDecision.ADVANCE

    # Verification failed (FAIL or ERROR). Check budget gates.
    if run.cumulative_cost_usd >= run.budget.max_cost_usd:
        return RunDecision.ABORT_BUDGET_COST

    if len(run.attempts) >= run.budget.max_attempts:
        return RunDecision.ABORT_BUDGET_ATTEMPTS

    return RunDecision.RETRY


def abort_explanation(run: Run, decision: RunDecision) -> str:
    """Human-readable abort comment body. Kept here because the wording
    is part of the decision contract (the user-facing surface of an
    abort) — changing it shouldn't require touching the webhook
    handler."""
    last = run.attempts[-1] if run.attempts else None
    reasons = last.verification.reasons if (last and last.verification) else []
    detail = ""
    if reasons:
        detail = "\n\nMost recent verification reasons:\n" + "\n".join(f"- {r}" for r in reasons)

    if decision == RunDecision.ABORT_BUDGET_ATTEMPTS:
        return (
            f"Aborting verify-loop after {len(run.attempts)} attempt(s); "
            f"reached max_attempts={run.budget.max_attempts}.{detail}"
        )
    if decision == RunDecision.ABORT_BUDGET_COST:
        return (
            f"Aborting verify-loop after cumulative cost "
            f"${run.cumulative_cost_usd:.2f} >= budget "
            f"${run.budget.max_cost_usd:.2f}.{detail}"
        )
    if decision == RunDecision.ABORT_MALFORMED:
        return (
            "Aborting verify-loop: the latest workflow run did not produce a "
            "well-formed `verification.json` artifact. The decision engine "
            "cannot retry against a missing or invalid producer contract."
        )
    raise ValueError(f"abort_explanation called with non-abort decision {decision!r}")

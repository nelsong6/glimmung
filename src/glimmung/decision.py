"""Verify-loop decision engine (#18, restructured for #69 phases).

A pure function `(run, workflow) -> RunDecision`. No I/O, no state
mutation. Side effects (dispatching the retry workflow, posting the abort
comment, updating Cosmos) live at the call site in `app.py`. The
discipline holds across the phase refactor: the engine looks up the
current phase's `recycle_policy` on the workflow and routes accordingly,
but never mutates either model.

Decision order, on the latest attempt's phase (first match wins):

  Non-verify phase
    1. GHA conclusion == "success"      -> ADVANCE
    2. anything else                    -> ABORT_MALFORMED
       (Hard rule: non-verify phase failure ends the run. Recycle policy
       is invalid on non-verify phases — there's no contract to retry
       against.)

  Verify phase
    1. status == PASS                   -> ADVANCE
    2. cumulative_cost_usd >= budget    -> ABORT_BUDGET_COST
    3. trigger not in recycle.on        -> ABORT_MALFORMED / ABORT_BUDGET_ATTEMPTS
       (depending on whether the trigger is verify_malformed or verify_fail)
    4. attempts-in-phase >= max         -> ABORT_BUDGET_ATTEMPTS
    5. otherwise                        -> RETRY

The trigger for the latest attempt is derived from the verification
artifact: `None` -> "verify_malformed", FAIL/ERROR -> "verify_fail",
PASS short-circuits to ADVANCE before any trigger logic. ERROR is a
substantive verdict — "I tried, I don't know" — distinct from a missing
artifact, but for retry purposes both are governed by `recycle_policy.on`
which can opt into either trigger independently.

Cost is checked before the recycle gate because it's the harder cap — a
single expensive attempt could blow past the $25 default while still
well under the per-phase attempt cap.
"""

from glimmung.models import RecyclePolicy, Run, RunDecision, VerificationStatus, Workflow


def _trigger_for_attempt(attempt) -> str:
    """Map the latest attempt's verification state to a recycle trigger.
    Caller must already have ruled out PASS (which is ADVANCE, not a
    trigger)."""
    if attempt.verification is None:
        return "verify_malformed"
    return "verify_fail"  # FAIL or ERROR


def decide(run: Run, workflow: Workflow) -> RunDecision:
    if not run.attempts:
        raise ValueError("decide() called on run with no attempts")

    last = run.attempts[-1]
    phase_spec = next((p for p in workflow.phases if p.name == last.phase), None)
    if phase_spec is None:
        raise ValueError(
            f"attempt phase {last.phase!r} not found in workflow.phases "
            f"({[p.name for p in workflow.phases]})"
        )

    # Non-verify phase: GHA conclusion is the verdict. Any failure ends the run.
    if not phase_spec.verify:
        if last.conclusion == "success":
            return RunDecision.ADVANCE
        return RunDecision.ABORT_MALFORMED

    # Verify phase. PASS short-circuits.
    if last.verification is not None and last.verification.status == VerificationStatus.PASS:
        return RunDecision.ADVANCE

    # Cost cap is checked before the recycle gate.
    if run.cumulative_cost_usd >= run.budget.total:
        return RunDecision.ABORT_BUDGET_COST

    trigger = _trigger_for_attempt(last)
    rp: RecyclePolicy | None = phase_spec.recycle_policy

    if rp is None or trigger not in rp.on:
        # No recycle for this trigger. Surface the *reason* for the abort:
        # malformed artifacts get the malformed code; failures with no
        # retry path collapse to attempts-exhausted (semantically "no
        # retry available," kept under the existing enum to avoid churning
        # the abort-comment surface for v1).
        return RunDecision.ABORT_MALFORMED if trigger == "verify_malformed" else RunDecision.ABORT_BUDGET_ATTEMPTS

    attempts_in_phase = sum(1 for a in run.attempts if a.phase == last.phase)
    if attempts_in_phase >= rp.max_attempts:
        return RunDecision.ABORT_BUDGET_ATTEMPTS

    return RunDecision.RETRY


def abort_explanation(run: Run, workflow: Workflow, decision: RunDecision) -> str:
    """Human-readable abort comment body. Kept alongside the engine so the
    wording is part of the decision contract."""
    last = run.attempts[-1] if run.attempts else None
    reasons = last.verification.reasons if (last and last.verification) else []
    detail = ""
    if reasons:
        detail = "\n\nMost recent verification reasons:\n" + "\n".join(f"- {r}" for r in reasons)

    if decision == RunDecision.ABORT_BUDGET_ATTEMPTS:
        phase_spec = next((p for p in workflow.phases if last and p.name == last.phase), None)
        rp_max = phase_spec.recycle_policy.max_attempts if (phase_spec and phase_spec.recycle_policy) else None
        attempts_in_phase = sum(1 for a in run.attempts if last and a.phase == last.phase)
        if rp_max is not None:
            return (
                f"Aborting verify-loop after {attempts_in_phase} attempt(s) on phase "
                f"{last.phase!r}; reached max_attempts={rp_max}.{detail}"
            )
        return (
            f"Aborting verify-loop on phase {last.phase if last else '?'!r}: no retry "
            f"path available for the latest verification result.{detail}"
        )
    if decision == RunDecision.ABORT_BUDGET_COST:
        return (
            f"Aborting verify-loop after cumulative cost "
            f"${run.cumulative_cost_usd:.2f} >= budget ${run.budget.total:.2f}.{detail}"
        )
    if decision == RunDecision.ABORT_MALFORMED:
        return (
            "Aborting verify-loop: the latest workflow run did not produce a "
            "well-formed `verification.json` artifact, or the failure mode is "
            "not in this phase's recycle policy. The decision engine cannot "
            "retry against a missing or invalid producer contract."
        )
    raise ValueError(f"abort_explanation called with non-abort decision {decision!r}")

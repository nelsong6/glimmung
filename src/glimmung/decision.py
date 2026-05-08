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

from glimmung.models import Run, RunDecision, VerificationStatus, Workflow


def _next_phase_after(workflow: Workflow, phase_name: str):
    for i, p in enumerate(workflow.phases):
        if p.name == phase_name:
            if i + 1 < len(workflow.phases):
                return workflow.phases[i + 1]
            return None
    return None


def _trigger_for_attempt(attempt) -> str:
    """Map the latest attempt's verification state to a recycle trigger.
    Caller must already have ruled out PASS (which is ADVANCE, not a
    trigger)."""
    if attempt.verification is None:
        return "verify_malformed"
    return "verify_fail"  # FAIL or ERROR


def decide(
    run: Run,
    workflow: Workflow,
    attempt_index: int | None = None,
) -> RunDecision:
    """Decide what to do next based on a specific attempt's outcome.

    `attempt_index` selects which attempt the decision applies to.
    Defaults to attempts[-1] for the legacy single-in-flight model;
    pass an explicit index when concurrent dispatch means the just-
    completed attempt isn't necessarily the latest in the list."""
    if not run.attempts:
        raise ValueError("decide() called on run with no attempts")

    if attempt_index is not None:
        if attempt_index < 0 or attempt_index >= len(run.attempts):
            raise ValueError(
                f"decide() attempt_index={attempt_index} out of range "
                f"(0..{len(run.attempts) - 1})"
            )
        last = run.attempts[attempt_index]
    else:
        last = run.attempts[-1]
    phase_spec = next((p for p in workflow.phases if p.name == last.phase), None)
    if phase_spec is None:
        raise ValueError(
            f"attempt phase {last.phase!r} not found in workflow.phases "
            f"({[p.name for p in workflow.phases]})"
        )

    # Evidence verification gate (#296 follow-up). The gate is a glimmung-
    # owned phase that runs after a verify phase, reads the verification
    # artifact, and exits 0 if status==pass else 1. Routing is on
    # conclusion (no verification.status of its own), and recycle lands
    # back on the verify phase to re-run the agent on a fresh attempt.
    if phase_spec.evidence_verification_gate:
        if last.conclusion == "success":
            return RunDecision.ADVANCE
        if run.cumulative_cost_usd >= run.budget.total:
            return RunDecision.ABORT_BUDGET_COST
        rp = phase_spec.recycle_policy
        if rp is None or "verify_fail" not in rp.on:
            return RunDecision.ABORT_BUDGET_ATTEMPTS
        attempts_in_phase = sum(1 for a in run.attempts if a.phase == last.phase)
        if attempts_in_phase >= rp.max_attempts:
            return RunDecision.ABORT_BUDGET_ATTEMPTS
        return RunDecision.RETRY

    # Non-verify phase: GHA conclusion is the verdict. Any failure ends the run.
    if not phase_spec.verify:
        if last.conclusion == "success":
            return RunDecision.ADVANCE
        return RunDecision.ABORT_MALFORMED

    # Verify phase followed by an evidence_verification_gate: the gate is
    # the decider; the verify phase just emits the artifact and we ADVANCE
    # to the gate on conclusion success regardless of verification.status
    # (the artifact's verdict will be enforced one phase downstream).
    next_phase = _next_phase_after(workflow, phase_spec.name)
    if next_phase is not None and next_phase.evidence_verification_gate:
        if last.conclusion == "success":
            return RunDecision.ADVANCE
        return RunDecision.ABORT_MALFORMED

    # Legacy verify phase (no gate): route on verification.status. Kept so
    # workflows registered before the gate primitive shipped continue to
    # behave identically. New registrations are required by the validator
    # to declare a gate, so this branch fades out as projects migrate.
    if last.verification is not None and last.verification.status == VerificationStatus.PASS:
        return RunDecision.ADVANCE

    if run.cumulative_cost_usd >= run.budget.total:
        return RunDecision.ABORT_BUDGET_COST

    trigger = _trigger_for_attempt(last)
    rp = phase_spec.recycle_policy

    if rp is None or trigger not in rp.on:
        return RunDecision.ABORT_MALFORMED if trigger == "verify_malformed" else RunDecision.ABORT_BUDGET_ATTEMPTS

    attempts_in_phase = sum(1 for a in run.attempts if a.phase == last.phase)
    if attempts_in_phase >= rp.max_attempts:
        return RunDecision.ABORT_BUDGET_ATTEMPTS

    return RunDecision.RETRY


def abort_explanation(run: Run, workflow: Workflow, decision: RunDecision) -> str:
    """Human-readable abort comment body. Kept alongside the engine so the
    wording is part of the decision contract.

    The "primary" attempt for messaging is the most recent non-always-run
    attempt (always-run teardown phases run after the abort decision and
    don't carry the abort context). Falls back to the latest attempt
    overall when no non-always attempts exist (defensive — pre-teardown
    workflows will always hit this path)."""
    last = None
    for a in reversed(run.attempts):
        ph = next((p for p in workflow.phases if p.name == a.phase), None)
        if ph is None or not ph.always:
            last = a
            break
    if last is None and run.attempts:
        last = run.attempts[-1]
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

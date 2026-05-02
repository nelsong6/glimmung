"""Decision-engine replay for the smoke-test substrate (#111).

A pure-function smoke test for the verify-loop contract: take a Run, a
Workflow registration, and a synthetic completion payload (the body the
workflow's `/completed` callback would have posted), and return the
RunDecision the engine *would* make — without mutating any state, without
firing any GHA dispatch.

Motivated by the verify=true→false friction documented in
nelsong6/glimmung#111: a workflow registered with `verify: true` whose
underlying `/completed` callback omits the `verification` field returns
ABORT_MALFORMED. Catching that mismatch via a real agent dispatch costs
~20 minutes of agent runtime and a full env-prep cycle. Catching it via
this replay endpoint costs a single Cosmos read.

This module is the glue between `decide()` (already pure) and the
HTTP/MCP surface — the helpers here build a synthetic `Run` with the
posted completion fields applied to the latest attempt, then delegate
to the existing decision engine.
"""

from __future__ import annotations

from typing import Any

from pydantic import BaseModel

from glimmung.decision import abort_explanation, decide
from glimmung.models import (
    PhaseAttempt,
    Run,
    RunDecision,
    VerificationResult,
    Workflow,
)


class SyntheticCompletion(BaseModel):
    """Mirror of the real `/completed` callback body, replayed in-memory.

    Fields match `RunCompletedRequest` in app.py — same shape so an
    operator can copy-paste a real completion payload into the replay
    request and ask "what would happen now?"."""
    conclusion: str = "success"
    verification: dict[str, Any] | None = None
    phase_outputs: dict[str, str] | None = None


class ReplayResult(BaseModel):
    """What the decision engine would return for `synthetic` against
    `workflow`, plus a next-action hint so the caller can see the
    downstream consequence without having to re-derive it."""
    run_id: str
    applied_to_phase: str
    applied_to_attempt_index: int
    decision: str
    abort_reason: str | None = None
    would_advance_to_phase: str | None = None
    would_open_pr: bool = False
    would_retry_target_phase: str | None = None
    cumulative_cost_usd_after: float = 0.0
    attempts_in_phase_after: int = 0
    workflow_source: str = "registered"  # "registered" | "override"


def apply_synthetic_completion(
    run: Run,
    synthetic: SyntheticCompletion,
) -> Run:
    """Return a copy of `run` with `synthetic` applied to the latest
    attempt — same shape as `record_completion` produces, but in-memory.

    Mirrors `_apply_completion` in runs.py: stamps `conclusion`,
    `verification`, `phase_outputs`, and rolls cost into
    `cumulative_cost_usd`. No `completed_at` / `workflow_run_id` mutation
    here — the replay isn't asserting "this happened," it's asking
    "if this happened, what would the engine decide?"."""
    if not run.attempts:
        raise ValueError(f"run {run.id!r} has no attempts to replay against")

    verification: VerificationResult | None = None
    if synthetic.verification is not None:
        try:
            verification = VerificationResult.model_validate(synthetic.verification)
        except Exception:
            # Replay treats an unparseable verification the same way
            # `_process_run_completion` does — leave it None so the
            # decision engine routes through ABORT_MALFORMED.
            verification = None

    cost = verification.cost_usd if verification else 0.0
    replay_run = run.model_copy(deep=True)
    last = replay_run.attempts[-1]

    replay_run.attempts[-1] = PhaseAttempt(
        attempt_index=last.attempt_index,
        phase=last.phase,
        workflow_filename=last.workflow_filename,
        workflow_run_id=last.workflow_run_id,
        dispatched_at=last.dispatched_at,
        completed_at=last.completed_at,
        conclusion=synthetic.conclusion,
        verification=verification,
        cost_usd=cost if last.cost_usd is None else last.cost_usd,
        artifact_url=last.artifact_url,
        decision=last.decision,
        phase_outputs=synthetic.phase_outputs if synthetic.phase_outputs is not None else last.phase_outputs,
    )
    replay_run.cumulative_cost_usd = run.cumulative_cost_usd + cost
    return replay_run


def _next_phase_after(workflow: Workflow, phase_name: str) -> str | None:
    """Mirror of `app._next_phase_after` to keep this module dependency-
    free of the FastAPI app. Returns the next phase name, or None if
    `phase_name` is the last phase or absent."""
    for i, p in enumerate(workflow.phases):
        if p.name == phase_name:
            if i + 1 < len(workflow.phases):
                return workflow.phases[i + 1].name
            return None
    return None


def replay_decision(
    *,
    run: Run,
    workflow: Workflow,
    synthetic: SyntheticCompletion,
    workflow_source: str = "registered",
) -> ReplayResult:
    """Pure function. Apply `synthetic` to the latest attempt in-memory,
    run the decision engine, return the verdict + a next-action hint.

    `workflow_source` is "registered" when the caller passed the live
    registration, "override" when the caller supplied an alternate
    Workflow shape — the value is echoed back in the result so the
    operator sees which workflow drove the verdict.

    No Cosmos writes. No GHA dispatches. Safe to call against any Run,
    including terminal ones (PASSED / ABORTED) — useful for "given my
    fixed registration, what would have happened?" forensics."""
    replay_run = apply_synthetic_completion(run, synthetic)
    last = replay_run.attempts[-1]
    decision = decide(replay_run, workflow)

    result = ReplayResult(
        run_id=run.id,
        applied_to_phase=last.phase,
        applied_to_attempt_index=last.attempt_index,
        decision=decision.value,
        cumulative_cost_usd_after=replay_run.cumulative_cost_usd,
        attempts_in_phase_after=sum(1 for a in replay_run.attempts if a.phase == last.phase),
        workflow_source=workflow_source,
    )

    if decision == RunDecision.ADVANCE:
        next_name = _next_phase_after(workflow, last.phase)
        if next_name is not None:
            result.would_advance_to_phase = next_name
        else:
            result.would_open_pr = workflow.pr.enabled
    elif decision == RunDecision.RETRY:
        # Look up the failing phase's recycle target; mirrors
        # `_dispatch_retry`'s lookup path. `lands_at == "self"` resolves
        # to the failing phase name.
        failing_phase = next(
            (p for p in workflow.phases if p.name == last.phase), None,
        )
        if failing_phase and failing_phase.recycle_policy:
            target = failing_phase.recycle_policy.lands_at
            if target == "self":
                target = last.phase
            result.would_retry_target_phase = target
    else:
        # Any abort decision — surface the same human-readable reason
        # the live ABORT path would post as an issue comment.
        result.abort_reason = abort_explanation(replay_run, workflow, decision)

    return result

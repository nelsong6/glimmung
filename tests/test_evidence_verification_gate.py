"""Evidence verification gate primitive (#296 follow-up).

Glimmung-owned phase that runs after a verify phase, reads the verification
artifact via an inputs ref, and exits 0 if `status==pass` else 1. The
visible effect: a verify_fail surfaces as a red gate phase in the run
graph, not a buried artifact field on a green user-phase attempt.

Three layers covered here:
- Model validators: gate-shape constraints and verify↔gate ordering rules.
- Storage helpers: `_phase_to_doc` auto-fills the gate's jobs[]; the
  glimmung-supplied job spec is the python:3.12-slim entrypoint with the
  inline status-check script.
- Decision engine: gate phases route on conclusion + recycle; verify
  phases followed by a gate route on conclusion only (gate is the
  decider). Legacy verify phases (no gate) keep their verification.status
  routing for backward compat.
"""

from __future__ import annotations

from datetime import UTC, datetime

import pytest

from glimmung import app as glimmung_app
from glimmung.decision import decide
from glimmung.models import (
    BudgetConfig,
    NativeJobSpec,
    NativeStepSpec,
    PhaseAttempt,
    PhaseSpec,
    RecyclePolicy,
    Run,
    RunDecision,
    RunState,
    VerificationResult,
    VerificationStatus,
    Workflow,
    WorkflowRegister,
)


# ─── helpers ─────────────────────────────────────────────────────────────


def _phase(
    name: str,
    *,
    verify: bool = False,
    evidence_verification_gate: bool = False,
    always: bool = False,
    recycle_policy: RecyclePolicy | None = None,
    inputs: dict[str, str] | None = None,
    outputs: list[str] | None = None,
) -> PhaseSpec:
    return PhaseSpec(
        name=name,
        kind="k8s_job",
        verify=verify,
        evidence_verification_gate=evidence_verification_gate,
        always=always,
        recycle_policy=recycle_policy,
        inputs=inputs or {},
        outputs=outputs or [],
        jobs=[] if evidence_verification_gate else [
            NativeJobSpec(
                id=name, image="ghcr.io/example/runner:latest",
                command=["/bin/true"],
                steps=[NativeStepSpec(slug="run", title="run")],
            )
        ],
    )


def _good_workflow_phases() -> list[PhaseSpec]:
    """Canonical verify+gate shape: env-prep, agent-execute (verify),
    agent-verify-gate (gate), env-destroy (always)."""
    return [
        _phase("env-prep"),
        _phase("agent-execute", verify=True, outputs=["verification"]),
        _phase(
            "agent-verify-gate",
            evidence_verification_gate=True,
            inputs={"verification": "${{ phases.agent-execute.outputs.verification }}"},
            recycle_policy=RecyclePolicy(
                max_attempts=2,
                on=["verify_fail", "verify_malformed"],
                lands_at="agent-execute",
            ),
        ),
        _phase("env-destroy", always=True),
    ]


def _workflow(phases: list[PhaseSpec]) -> Workflow:
    return Workflow(
        id="agent-run", project="ambience", name="agent-run",
        phases=phases, budget=BudgetConfig(total=25.0),
        created_at=datetime.now(UTC),
    )


def _attempt(
    *, phase: str, conclusion: str | None = "success",
    verification: VerificationResult | None = None,
    decision: RunDecision | None = None,
) -> PhaseAttempt:
    return PhaseAttempt(
        attempt_index=0, phase=phase, phase_kind="k8s_job",
        workflow_filename=f"k8s_job:{phase}",
        dispatched_at=datetime.now(UTC),
        completed_at=datetime.now(UTC),
        conclusion=conclusion,
        verification=verification,
        decision=(decision.value if decision else None),
    )


def _run(*, attempts: list[PhaseAttempt], cumulative_cost_usd: float = 0.0) -> Run:
    now = datetime.now(UTC)
    return Run(
        id="01TESTRUN0000000000000000",
        project="ambience", workflow="agent-run",
        run_number=1, issue_number=172,
        attempts=attempts, cumulative_cost_usd=cumulative_cost_usd,
        budget=BudgetConfig(total=25.0),
        state=RunState.IN_PROGRESS,
        created_at=now, updated_at=now,
    )


# ─── model validators ────────────────────────────────────────────────────


def test_gate_default_is_false():
    p = _phase("agent-execute")
    assert p.evidence_verification_gate is False


def test_canonical_verify_gate_shape_is_accepted():
    WorkflowRegister(
        project="ambience", name="agent-run",
        phases=_good_workflow_phases(),
    )


def test_verify_phase_without_gate_rejected_with_pointer():
    with pytest.raises(ValueError) as excinfo:
        WorkflowRegister(
            project="ambience", name="agent-run",
            phases=[
                _phase("env-prep"),
                _phase("agent-execute", verify=True, outputs=["verification"]),
                _phase("env-destroy", always=True),
            ],
        )
    msg = str(excinfo.value)
    # Pointer-style help so the registrar knows what to add.
    assert "evidence_verification_gate" in msg
    assert "agent-execute-gate" in msg
    assert "kind: k8s_job" in msg


def test_verify_phase_as_last_phase_rejected():
    with pytest.raises(ValueError, match="is the last phase"):
        WorkflowRegister(
            project="ambience", name="agent-run",
            phases=[
                _phase("env-prep"),
                _phase("agent-execute", verify=True, outputs=["verification"]),
            ],
        )


def test_gate_without_preceding_verify_rejected():
    with pytest.raises(ValueError, match="not preceded by a verify=True phase"):
        WorkflowRegister(
            project="ambience", name="agent-run",
            phases=[
                _phase("env-prep"),
                _phase(
                    "stray-gate",
                    evidence_verification_gate=True,
                    inputs={"verification": "${{ phases.env-prep.outputs.x }}"},
                ),
            ],
        )


def test_gate_with_user_supplied_jobs_rejected():
    with pytest.raises(ValueError, match="gate jobs are glimmung-supplied"):
        WorkflowRegister(
            project="ambience", name="agent-run",
            phases=[
                _phase("env-prep"),
                _phase("agent-execute", verify=True, outputs=["verification"]),
                PhaseSpec(
                    name="agent-verify-gate",
                    kind="k8s_job",
                    evidence_verification_gate=True,
                    inputs={"verification": "${{ phases.agent-execute.outputs.verification }}"},
                    jobs=[NativeJobSpec(
                        id="my-gate", image="evil.example/gate:latest",
                        steps=[NativeStepSpec(slug="run", title="run")],
                    )],
                ),
            ],
        )


def test_gate_combined_with_verify_rejected():
    with pytest.raises(ValueError, match="cannot be both verify=True and"):
        WorkflowRegister(
            project="ambience", name="agent-run",
            phases=[
                _phase("env-prep"),
                _phase("agent-execute", verify=True, outputs=["verification"]),
                _phase(
                    "doubly-flagged",
                    verify=True,
                    evidence_verification_gate=True,
                    inputs={"verification": "${{ phases.agent-execute.outputs.verification }}"},
                ),
            ],
        )


def test_gate_combined_with_always_rejected():
    with pytest.raises(ValueError, match="mutually exclusive"):
        WorkflowRegister(
            project="ambience", name="agent-run",
            phases=[
                _phase("env-prep"),
                _phase("agent-execute", verify=True, outputs=["verification"]),
                _phase(
                    "weird",
                    evidence_verification_gate=True,
                    always=True,
                    inputs={"verification": "${{ phases.agent-execute.outputs.verification }}"},
                ),
            ],
        )


def test_gate_can_carry_recycle_policy():
    # Recycle on the gate is the canonical placement — the gate is the
    # decision point. No error on this shape.
    WorkflowRegister(
        project="ambience", name="agent-run",
        phases=_good_workflow_phases(),  # gate has recycle_policy
    )


# ─── storage helpers: _phase_to_doc auto-fill ──────────────────────────


def test_phase_to_doc_fills_glimmung_supplied_jobs_for_gate():
    gate = _phase(
        "agent-verify-gate",
        evidence_verification_gate=True,
        inputs={"verification": "${{ phases.agent-execute.outputs.verification }}"},
    )
    assert gate.jobs == []
    doc = glimmung_app._phase_to_doc(gate)
    assert doc["evidenceVerificationGate"] is True
    assert len(doc["jobs"]) == 1
    job = doc["jobs"][0]
    assert job["image"] == "python:3.12-slim"
    assert job["command"] == ["python", "-c"]
    # The script reads $VERIFICATION and exits based on .status
    assert "VERIFICATION" in job["args"][0]
    assert "status" in job["args"][0]
    assert job["steps"] == [{"slug": "evaluate-verdict", "title": "Evaluate verification verdict"}]


def test_phase_to_doc_does_not_overwrite_user_jobs_on_non_gate_phases():
    # Sanity: a normal k8s_job phase keeps its declared jobs.
    p = _phase("agent-execute", verify=True, outputs=["verification"])
    doc = glimmung_app._phase_to_doc(p)
    assert doc["evidenceVerificationGate"] is False
    assert doc["jobs"][0]["id"] == "agent-execute"
    assert doc["jobs"][0]["image"] == "ghcr.io/example/runner:latest"


def test_phase_round_trip_preserves_gate_flag():
    gate = _phase(
        "agent-verify-gate",
        evidence_verification_gate=True,
        inputs={"verification": "${{ phases.agent-execute.outputs.verification }}"},
    )
    doc = glimmung_app._phase_to_doc(gate)
    restored = glimmung_app._phase_from_doc(doc)
    assert restored.evidence_verification_gate is True
    assert len(restored.jobs) == 1
    assert restored.jobs[0].image == "python:3.12-slim"


# ─── decision engine: gate routing ─────────────────────────────────────


def test_gate_advances_on_success_conclusion():
    wf = _workflow(_good_workflow_phases())
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(phase="agent-execute", decision=RunDecision.ADVANCE,
                 verification=VerificationResult(
                     schema_version=1, status=VerificationStatus.PASS, reasons=[],
                 )),
        _attempt(phase="agent-verify-gate", conclusion="success"),
    ])
    assert decide(run, wf) == RunDecision.ADVANCE


def test_gate_retries_on_failure_when_recycle_attempts_remain():
    wf = _workflow(_good_workflow_phases())
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(phase="agent-execute", decision=RunDecision.ADVANCE,
                 verification=VerificationResult(
                     schema_version=1, status=VerificationStatus.FAIL,
                     reasons=["screenshot capture failed"],
                 )),
        _attempt(phase="agent-verify-gate", conclusion="failure"),
    ])
    assert decide(run, wf) == RunDecision.RETRY


def test_gate_aborts_on_failure_when_attempts_exhausted():
    # Construct a workflow where the gate has max_attempts=1 so the
    # first failure is terminal.
    phases = _good_workflow_phases()
    phases[2] = _phase(
        "agent-verify-gate",
        evidence_verification_gate=True,
        inputs={"verification": "${{ phases.agent-execute.outputs.verification }}"},
        recycle_policy=RecyclePolicy(
            max_attempts=1, on=["verify_fail"], lands_at="agent-execute",
        ),
    )
    wf = _workflow(phases)
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(phase="agent-execute", decision=RunDecision.ADVANCE,
                 verification=VerificationResult(
                     schema_version=1, status=VerificationStatus.FAIL, reasons=[],
                 )),
        _attempt(phase="agent-verify-gate", conclusion="failure"),
    ])
    assert decide(run, wf) == RunDecision.ABORT_BUDGET_ATTEMPTS


def test_gate_aborts_when_no_recycle_policy():
    phases = _good_workflow_phases()
    phases[2] = _phase(
        "agent-verify-gate",
        evidence_verification_gate=True,
        inputs={"verification": "${{ phases.agent-execute.outputs.verification }}"},
        recycle_policy=None,
    )
    wf = _workflow(phases)
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(phase="agent-execute", decision=RunDecision.ADVANCE,
                 verification=VerificationResult(
                     schema_version=1, status=VerificationStatus.FAIL, reasons=[],
                 )),
        _attempt(phase="agent-verify-gate", conclusion="failure"),
    ])
    assert decide(run, wf) == RunDecision.ABORT_BUDGET_ATTEMPTS


def test_verify_phase_followed_by_gate_advances_on_conclusion_success():
    """Verify phase no longer makes the verdict call when a gate
    follows — it just emits the artifact and routes on conclusion."""
    wf = _workflow(_good_workflow_phases())
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        # Verifier said FAIL but the verify phase still exited 0 (the
        # artifact was emitted cleanly). Decision engine should advance
        # to the gate, not abort here — the gate enforces.
        _attempt(phase="agent-execute", conclusion="success",
                 verification=VerificationResult(
                     schema_version=1, status=VerificationStatus.FAIL,
                     reasons=["verifier said no"],
                 )),
    ])
    assert decide(run, wf) == RunDecision.ADVANCE


def test_legacy_verify_phase_without_gate_still_routes_on_verification_status():
    """Backward compat: a workflow registered before this primitive
    shipped (no gate, verify=True with recycle) keeps its
    verification.status-driven routing untouched."""
    legacy_phases = [
        _phase("env-prep"),
        _phase("agent-execute", verify=True,
               recycle_policy=RecyclePolicy(
                   max_attempts=0,
                   on=["verify_fail", "verify_malformed"], lands_at="self",
               )),
    ]
    wf = _workflow(legacy_phases)
    # Verifier said FAIL; legacy routing aborts here.
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(phase="agent-execute", conclusion="success",
                 verification=VerificationResult(
                     schema_version=1, status=VerificationStatus.FAIL, reasons=[],
                 )),
    ])
    assert decide(run, wf) == RunDecision.ABORT_BUDGET_ATTEMPTS

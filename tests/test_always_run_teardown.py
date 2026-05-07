"""Always-run teardown phase support (#296).

Covers three layers:
- Model validators: always-phase shape constraints + ordering.
- Routing helpers: `_next_dispatch_target` + `_terminal_disposition`
  in app.py — pure functions over (workflow, run, decision).
- `abort_explanation` skipping teardown attempts when picking the
  primary attempt for messaging.
"""

from __future__ import annotations

from datetime import UTC, datetime

import pytest

from glimmung import app as glimmung_app
from glimmung.decision import abort_explanation
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


def _k8s_phase(
    name: str,
    *,
    verify: bool = False,
    always: bool = False,
    recycle_policy: RecyclePolicy | None = None,
    outputs: list[str] | None = None,
) -> PhaseSpec:
    return PhaseSpec(
        name=name,
        kind="k8s_job",
        verify=verify,
        always=always,
        recycle_policy=recycle_policy,
        outputs=outputs or [],
        jobs=[
            NativeJobSpec(
                id=name,
                image="ghcr.io/example/runner:latest",
                command=["/bin/true"],
                steps=[NativeStepSpec(slug="run", title="run")],
            )
        ],
    )


def _workflow(phases: list[PhaseSpec]) -> Workflow:
    return Workflow(
        id="agent-run",
        project="ambience",
        name="agent-run",
        phases=phases,
        budget=BudgetConfig(total=25.0),
        created_at=datetime.now(UTC),
    )


def _attempt(
    *,
    phase: str,
    phase_kind: str = "k8s_job",
    decision: RunDecision | None = None,
    verification: VerificationResult | None = None,
    conclusion: str | None = "success",
    attempt_index: int = 0,
) -> PhaseAttempt:
    return PhaseAttempt(
        attempt_index=attempt_index,
        phase=phase,
        phase_kind=phase_kind,
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
        project="ambience",
        workflow="agent-run",
        run_number=1,
        issue_number=172,
        attempts=attempts,
        cumulative_cost_usd=cumulative_cost_usd,
        budget=BudgetConfig(total=25.0),
        state=RunState.IN_PROGRESS,
        created_at=now,
        updated_at=now,
    )


# ─── model validators ────────────────────────────────────────────────────


def test_always_phase_default_is_false():
    p = _k8s_phase("env-prep")
    assert p.always is False


def test_always_phase_can_be_registered_after_regular_phase():
    WorkflowRegister(
        project="ambience",
        name="agent-run",
        phases=[
            _k8s_phase("env-prep"),
            _k8s_phase("agent-execute", verify=True, recycle_policy=RecyclePolicy(
                max_attempts=0, on=["verify_fail", "verify_malformed"], lands_at="self",
            )),
            _k8s_phase("env-destroy", always=True),
        ],
    )


def test_always_phase_rejects_verify_true():
    with pytest.raises(ValueError, match="always-run teardown phases cannot opt into the verify loop"):
        WorkflowRegister(
            project="ambience",
            name="agent-run",
            phases=[
                _k8s_phase("env-prep"),
                _k8s_phase("env-destroy", always=True, verify=True),
            ],
        )


def test_always_phase_rejects_recycle_policy():
    # The verify=False/recycle_policy gate fires first for a non-verify
    # phase; the always-specific gate is the safety net for the case where
    # someone tries to bolt verify+recycle onto a teardown phase.
    with pytest.raises(ValueError):
        WorkflowRegister(
            project="ambience",
            name="agent-run",
            phases=[
                _k8s_phase("env-prep"),
                _k8s_phase(
                    "env-destroy",
                    always=True,
                    recycle_policy=RecyclePolicy(max_attempts=2, on=[], lands_at="self"),
                ),
            ],
        )


def test_regular_phase_after_always_phase_rejected():
    with pytest.raises(ValueError, match="cannot follow an always=True"):
        WorkflowRegister(
            project="ambience",
            name="agent-run",
            phases=[
                _k8s_phase("env-prep"),
                _k8s_phase("env-destroy", always=True),
                _k8s_phase("post-destroy"),
            ],
        )


def test_workflow_with_only_always_phases_rejected():
    with pytest.raises(ValueError, match="at least one non-always phase"):
        WorkflowRegister(
            project="ambience",
            name="agent-run",
            phases=[_k8s_phase("env-destroy", always=True)],
        )


def test_recycle_target_pointing_at_always_phase_rejected():
    with pytest.raises(ValueError, match="targets an always-run teardown phase"):
        WorkflowRegister(
            project="ambience",
            name="agent-run",
            phases=[
                _k8s_phase("env-prep"),
                _k8s_phase(
                    "agent-execute",
                    verify=True,
                    recycle_policy=RecyclePolicy(
                        max_attempts=2,
                        on=["verify_fail"],
                        lands_at="env-destroy",
                    ),
                ),
                _k8s_phase("env-destroy", always=True),
            ],
        )


# ─── routing: _next_dispatch_target ──────────────────────────────────────


def test_advance_from_non_always_routes_to_next_phase():
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("agent-execute", verify=True, recycle_policy=RecyclePolicy(
            max_attempts=0, on=["verify_fail", "verify_malformed"], lands_at="self",
        )),
        _k8s_phase("env-destroy", always=True),
    ])
    run = _run(attempts=[_attempt(phase="env-prep", decision=RunDecision.ADVANCE)])
    target = glimmung_app._next_dispatch_target(wf, run, RunDecision.ADVANCE)
    assert target is not None
    assert target.name == "agent-execute"


def test_advance_from_last_non_always_routes_into_teardown():
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("agent-execute", verify=True, recycle_policy=RecyclePolicy(
            max_attempts=0, on=["verify_fail", "verify_malformed"], lands_at="self",
        )),
        _k8s_phase("env-destroy", always=True),
    ])
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(
            phase="agent-execute",
            decision=RunDecision.ADVANCE,
            verification=VerificationResult(
                schema_version=1,
                status=VerificationStatus.PASS,
                reasons=[],
            ),
        ),
    ])
    target = glimmung_app._next_dispatch_target(wf, run, RunDecision.ADVANCE)
    assert target is not None
    assert target.name == "env-destroy"
    assert target.always


def test_abort_from_non_always_skips_to_first_teardown_phase():
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("agent-execute", verify=True, recycle_policy=RecyclePolicy(
            max_attempts=0, on=["verify_fail", "verify_malformed"], lands_at="self",
        )),
        _k8s_phase("env-destroy", always=True),
    ])
    # agent-execute aborts on verify_fail
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(
            phase="agent-execute",
            decision=RunDecision.ABORT_BUDGET_ATTEMPTS,
            verification=VerificationResult(
                schema_version=1,
                status=VerificationStatus.FAIL,
                reasons=["screenshot capture failed"],
            ),
        ),
    ])
    target = glimmung_app._next_dispatch_target(
        wf, run, RunDecision.ABORT_BUDGET_ATTEMPTS,
    )
    assert target is not None
    assert target.name == "env-destroy"


def test_abort_with_no_teardown_returns_none_for_terminal():
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("agent-execute", verify=True, recycle_policy=RecyclePolicy(
            max_attempts=0, on=["verify_fail", "verify_malformed"], lands_at="self",
        )),
    ])
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(
            phase="agent-execute",
            decision=RunDecision.ABORT_BUDGET_ATTEMPTS,
            verification=VerificationResult(
                schema_version=1,
                status=VerificationStatus.FAIL,
                reasons=[],
            ),
        ),
    ])
    target = glimmung_app._next_dispatch_target(
        wf, run, RunDecision.ABORT_BUDGET_ATTEMPTS,
    )
    assert target is None


def test_advance_from_teardown_phase_chains_to_next_teardown():
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("env-destroy-1", always=True),
        _k8s_phase("env-destroy-2", always=True),
    ])
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(phase="env-destroy-1", decision=RunDecision.ADVANCE),
    ])
    target = glimmung_app._next_dispatch_target(wf, run, RunDecision.ADVANCE)
    assert target is not None
    assert target.name == "env-destroy-2"


def test_teardown_phase_failure_still_chains_to_next_teardown():
    """A teardown phase failing must NOT escalate — we keep cleaning up."""
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("env-destroy-1", always=True),
        _k8s_phase("env-destroy-2", always=True),
    ])
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(
            phase="env-destroy-1",
            decision=RunDecision.ABORT_MALFORMED,
            conclusion="failure",
        ),
    ])
    # Pass any decision — for an always-phase, routing ignores it.
    target = glimmung_app._next_dispatch_target(
        wf, run, RunDecision.ABORT_MALFORMED,
    )
    assert target is not None
    assert target.name == "env-destroy-2"


def test_advance_from_last_teardown_phase_returns_none():
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("env-destroy", always=True),
    ])
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(phase="env-destroy", decision=RunDecision.ADVANCE),
    ])
    target = glimmung_app._next_dispatch_target(wf, run, RunDecision.ADVANCE)
    assert target is None


# ─── routing: _terminal_disposition ──────────────────────────────────────


def test_terminal_disposition_passed_when_all_non_always_pass():
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("agent-execute", verify=True, recycle_policy=RecyclePolicy(
            max_attempts=0, on=["verify_fail"], lands_at="self",
        )),
        _k8s_phase("env-destroy", always=True),
    ])
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(phase="agent-execute", decision=RunDecision.ADVANCE),
        _attempt(phase="env-destroy", decision=RunDecision.ADVANCE),
    ])
    disposition, abort_decision = glimmung_app._terminal_disposition(wf, run)
    assert disposition == "passed"
    assert abort_decision is None


def test_terminal_disposition_aborted_when_non_always_aborts():
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("agent-execute", verify=True, recycle_policy=RecyclePolicy(
            max_attempts=0, on=["verify_fail"], lands_at="self",
        )),
        _k8s_phase("env-destroy", always=True),
    ])
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(phase="agent-execute", decision=RunDecision.ABORT_BUDGET_ATTEMPTS),
        _attempt(phase="env-destroy", decision=RunDecision.ADVANCE),
    ])
    disposition, abort_decision = glimmung_app._terminal_disposition(wf, run)
    assert disposition == "aborted"
    assert abort_decision == RunDecision.ABORT_BUDGET_ATTEMPTS.value


def test_terminal_disposition_ignores_teardown_failure_when_run_passed():
    """A teardown phase failing must NOT mark the run aborted — the run
    succeeded and the teardown failure is logged but not load-bearing."""
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("env-destroy", always=True),
    ])
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(phase="env-destroy", decision=RunDecision.ABORT_MALFORMED),
    ])
    disposition, abort_decision = glimmung_app._terminal_disposition(wf, run)
    assert disposition == "passed"
    assert abort_decision is None


# ─── abort_explanation: skips teardown attempts ─────────────────────────


def test_abort_explanation_uses_pre_teardown_attempt_for_messaging():
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("agent-execute", verify=True, recycle_policy=RecyclePolicy(
            max_attempts=0, on=["verify_fail", "verify_malformed"], lands_at="self",
        )),
        _k8s_phase("env-destroy", always=True),
    ])
    # agent-execute had verify=fail; teardown completed successfully after.
    run = _run(attempts=[
        _attempt(phase="env-prep", decision=RunDecision.ADVANCE),
        _attempt(
            phase="agent-execute",
            decision=RunDecision.ABORT_BUDGET_ATTEMPTS,
            verification=VerificationResult(
                schema_version=1,
                status=VerificationStatus.FAIL,
                reasons=["screenshot capture failed against ambience-slot-1"],
            ),
        ),
        _attempt(phase="env-destroy", decision=RunDecision.ADVANCE),
    ])
    msg = abort_explanation(run, wf, RunDecision.ABORT_BUDGET_ATTEMPTS)
    assert "agent-execute" in msg
    assert "screenshot capture failed against ambience-slot-1" in msg
    # teardown-attempt details should NOT leak into the abort message
    assert "env-destroy" not in msg


def test_abort_explanation_falls_back_when_no_non_always_attempts():
    # Defensive — in pre-teardown workflows every attempt is non-always,
    # so this fallback should never fire there. Verify the fallback works
    # if somehow a run only has an always-phase attempt recorded.
    wf = _workflow([
        _k8s_phase("env-prep"),
        _k8s_phase("env-destroy", always=True),
    ])
    run = _run(attempts=[_attempt(phase="env-destroy", decision=RunDecision.ABORT_MALFORMED)])
    # Should not raise; should produce some explanation.
    msg = abort_explanation(run, wf, RunDecision.ABORT_MALFORMED)
    assert isinstance(msg, str)
    assert len(msg) > 0

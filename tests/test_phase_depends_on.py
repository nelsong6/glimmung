"""Phase depends_on field — explicit DAG edges between phases.

Stage 2A of the spirelens-style parallel-LLM-stages refactor: introduce
the `depends_on: list[str]` field on PhaseSpec, with validator rules and
back-compat default inference (sequential when nothing's declared).

Routing isn't DAG-aware yet (still index-based) — that's stage 2B.
"""

from __future__ import annotations

from datetime import UTC, datetime

import pytest

from glimmung.models import (
    BudgetConfig,
    NativeJobSpec,
    NativeStepSpec,
    PhaseSpec,
    RecyclePolicy,
    WorkflowRegister,
)


def _phase(name, *, always=False, verify=False, evidence_verification_gate=False, depends_on=None, recycle_policy=None, inputs=None, outputs=None):
    return PhaseSpec(
        name=name,
        kind="k8s_job",
        always=always,
        verify=verify,
        evidence_verification_gate=evidence_verification_gate,
        depends_on=depends_on if depends_on is not None else [],
        recycle_policy=recycle_policy,
        inputs=inputs or {},
        outputs=outputs or [],
        jobs=[] if evidence_verification_gate else [
            NativeJobSpec(
                id=name, image="ghcr.io/example:latest",
                command=["/bin/true"],
                steps=[NativeStepSpec(slug="run", title="run")],
            )
        ],
    )


# ─── back-compat default inference ─────────────────────────────────────


def test_undeclared_depends_on_infers_sequential():
    """When NO phase declares explicit depends_on, glimmung infers
    sequential deps (each phase depends on the previous one). Existing
    workflows registered before this primitive shipped continue to
    behave identically."""
    wr = WorkflowRegister(
        project="p", name="w",
        phases=[
            _phase("env-prep"),
            _phase("agent-execute", verify=True, outputs=["verification"]),
            _phase("agent-gate", evidence_verification_gate=True,
                   inputs={"verification": "${{ phases.agent-execute.outputs.verification }}"}),
            _phase("env-destroy", always=True),
        ],
    )
    assert wr.phases[0].depends_on == []                    # entry
    assert wr.phases[1].depends_on == ["env-prep"]
    assert wr.phases[2].depends_on == ["agent-execute"]
    # always-phase: depends on all preceding non-always phases
    assert wr.phases[3].depends_on == ["env-prep", "agent-execute", "agent-gate"]


def test_undeclared_depends_on_for_single_phase_is_empty():
    wr = WorkflowRegister(
        project="p", name="w",
        phases=[_phase("only-phase")],
    )
    assert wr.phases[0].depends_on == []


# ─── explicit DAG mode ────────────────────────────────────────────────


def test_explicit_depends_on_for_parallel_phases():
    """spirelens-style parallel: test-plan + implement both depend on
    env-prep, neither on each other; verify joins both."""
    wr = WorkflowRegister(
        project="p", name="w",
        phases=[
            _phase("env-prep"),
            _phase("test-plan", depends_on=["env-prep"]),
            _phase("implement", depends_on=["env-prep"]),
            _phase("verify", verify=True, outputs=["verification"],
                   depends_on=["test-plan", "implement"]),
            _phase("verify-gate", evidence_verification_gate=True,
                   inputs={"verification": "${{ phases.verify.outputs.verification }}"},
                   depends_on=["verify"]),
        ],
    )
    assert wr.phases[1].depends_on == ["env-prep"]
    assert wr.phases[2].depends_on == ["env-prep"]
    assert wr.phases[3].depends_on == ["test-plan", "implement"]


def test_always_phase_in_explicit_mode_auto_fills_to_all_non_always():
    """When the user has opted into explicit DAG mode, always-phases
    still get auto-filled deps on all non-always phases — they're
    unconditional teardown."""
    wr = WorkflowRegister(
        project="p", name="w",
        phases=[
            _phase("env-prep"),
            _phase("test-plan", depends_on=["env-prep"]),
            _phase("implement", depends_on=["env-prep"]),
            _phase("env-destroy", always=True),  # no explicit deps
        ],
    )
    assert sorted(wr.phases[3].depends_on) == sorted(["env-prep", "test-plan", "implement"])


# ─── validator: reference + ordering rules ─────────────────────────────


def test_depends_on_self_reference_rejected():
    with pytest.raises(ValueError, match="cannot reference itself"):
        WorkflowRegister(
            project="p", name="w",
            phases=[
                _phase("env-prep"),
                _phase("circular", depends_on=["circular"]),
            ],
        )


def test_depends_on_unknown_phase_rejected():
    with pytest.raises(ValueError, match="not a phase name"):
        WorkflowRegister(
            project="p", name="w",
            phases=[
                _phase("env-prep"),
                _phase("agent", depends_on=["does-not-exist"]),
            ],
        )


def test_depends_on_forward_ref_rejected():
    """The phase list IS the topological order — depends_on can only
    reference phases earlier in the list."""
    with pytest.raises(ValueError, match="appears later"):
        WorkflowRegister(
            project="p", name="w",
            phases=[
                _phase("first", depends_on=["second"]),  # forward ref
                _phase("second"),
            ],
        )


# ─── round-trip ───────────────────────────────────────────────────────


def test_depends_on_round_trips_through_phase_doc():
    """Stored doc preserves depends_on and the field comes back through
    _phase_from_doc unchanged."""
    from glimmung import app as glimmung_app

    wr = WorkflowRegister(
        project="p", name="w",
        phases=[
            _phase("env-prep"),
            _phase("test-plan", depends_on=["env-prep"]),
            _phase("implement", depends_on=["env-prep"]),
            _phase("verify", verify=True, outputs=["verification"],
                   depends_on=["test-plan", "implement"]),
            _phase("verify-gate", evidence_verification_gate=True,
                   inputs={"verification": "${{ phases.verify.outputs.verification }}"},
                   depends_on=["verify"]),
        ],
    )
    docs = [glimmung_app._phase_to_doc(p) for p in wr.phases]
    restored = [glimmung_app._phase_from_doc(d) for d in docs]
    assert restored[1].depends_on == ["env-prep"]
    assert restored[2].depends_on == ["env-prep"]
    assert restored[3].depends_on == ["test-plan", "implement"]
    assert restored[4].depends_on == ["verify"]

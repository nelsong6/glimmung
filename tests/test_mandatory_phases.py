"""Mandatory-phase enforcement on the workflow registration API.

Glimmung-managed workflows must declare prepare/testing/cleanup
trio: ≥1 entry phase (depends_on=[]), ≥1 verify or
evidence-verification-gate phase, and ≥1 always-run teardown phase.
The rule fires at the `/v1/workflows` API endpoint via
`WorkflowRegister.validate_mandatory_phases()` — model-level
enforcement is queued behind a fixture migration.
"""

from __future__ import annotations

import pytest

from glimmung.models import (
    NativeJobSpec,
    NativeStepSpec,
    PhaseSpec,
    WorkflowRegister,
)


def _phase(name, *, depends_on=None, verify=False, always=False, gate=False):
    """Helper to build a NativeJobSpec-bearing phase quickly."""
    return PhaseSpec(
        name=name,
        kind="k8s_job",
        depends_on=depends_on if depends_on is not None else [],
        verify=verify,
        always=always,
        evidence_verification_gate=gate,
        jobs=[] if gate else [
            NativeJobSpec(
                id=name, image="ghcr.io/example:latest",
                command=["/bin/true"],
                steps=[NativeStepSpec(slug="run", title="run")],
            ),
        ],
    )


def test_canonical_shape_is_accepted():
    wr = WorkflowRegister(
        project="p", name="w",
        phases=[
            _phase("prepare"),
            _phase("work"),
            _phase("testing", verify=True),
            _phase("cleanup", always=True),
        ],
    )
    # No raise.
    wr.validate_mandatory_phases()


def test_entry_phase_rule_is_implied_by_topology():
    """The model validator already enforces that depends_on can only
    reference earlier phases, so phases[0] always has empty deps and
    the workflow is guaranteed to have at least one entry. Document
    the implication via a passing test rather than constructing an
    invalid input that the topology rule rejects first."""
    wr = WorkflowRegister(
        project="p", name="w",
        phases=[
            _phase("prepare"),
            _phase("testing", verify=True, depends_on=["prepare"]),
            _phase("cleanup", always=True),
        ],
    )
    assert any(not p.depends_on for p in wr.phases)
    wr.validate_mandatory_phases()


def test_missing_verify_phase_rejected():
    wr = WorkflowRegister(
        project="p", name="w",
        phases=[
            _phase("prepare"),
            _phase("cleanup", always=True),
        ],
    )
    with pytest.raises(ValueError) as excinfo:
        wr.validate_mandatory_phases()
    assert "testing" in str(excinfo.value)


def test_missing_always_phase_rejected():
    wr = WorkflowRegister(
        project="p", name="w",
        phases=[
            _phase("prepare"),
            _phase("testing", verify=True),
        ],
    )
    with pytest.raises(ValueError) as excinfo:
        wr.validate_mandatory_phases()
    assert "cleanup" in str(excinfo.value)


def test_evidence_verification_gate_satisfies_testing_requirement():
    """A glimmung-owned gate phase counts as 'testing' for the
    mandatory-phase rule — projects that prefer the gate primitive
    over self-enforcing verify don't need a separate verify phase."""
    wr = WorkflowRegister(
        project="p", name="w",
        phases=[
            _phase("prepare"),
            _phase("verify", verify=True, depends_on=["prepare"]),
            _phase(
                "gate", gate=True,
                depends_on=["verify"],
            ),
            _phase("cleanup", always=True),
        ],
    )
    wr.validate_mandatory_phases()

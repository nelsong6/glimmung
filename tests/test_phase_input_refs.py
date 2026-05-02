"""Cross-phase input ref schema tests (glimmung#101 — multi-phase runtime PR 1).

Covers the `inputs` / `outputs` fields on `PhaseSpec` and the ref validator
that wires them up at registration time. Forward + self refs are rejected
with phase-named errors so a misregistered workflow surfaces the bad
phase, not just a generic "couldn't resolve" failure.

Note: `WorkflowRegister`'s v1 single-phase enforcement is intentionally
LAST in the validator order so 2-phase fixtures still exercise the ref
validator. Tests that build 2-phase fixtures assert the ref-error
message, not the single-phase one — the ref validator must catch the
bad ref before the single-phase gate runs.
"""

from __future__ import annotations

import pytest

from glimmung.models import (
    PhaseSpec,
    WorkflowRegister,
    parse_phase_input_ref,
    substitute_phase_inputs,
    validate_phase_input_refs,
)


# ─── parse_phase_input_ref — pure parser ──────────────────────────────────


def test_parse_basic():
    assert parse_phase_input_ref(
        "${{ phases.env-prep.outputs.validation_url }}"
    ) == ("env-prep", "validation_url")


def test_parse_no_inner_whitespace():
    assert parse_phase_input_ref(
        "${{phases.a.outputs.b}}"
    ) == ("a", "b")


def test_parse_underscores_and_hyphens():
    assert parse_phase_input_ref(
        "${{ phases.agent_phase-1.outputs.image_tag-x }}"
    ) == ("agent_phase-1", "image_tag-x")


@pytest.mark.parametrize("bad", [
    "validation_url",                              # bare string
    "${{ phases.a.b }}",                           # missing .outputs.
    "${{ phases.a.outputs }}",                     # missing key
    "${{ phases..outputs.b }}",                    # empty phase
    "${{ phases.a.outputs. }}",                    # empty key
    "${{ inputs.a.outputs.b }}",                   # wrong namespace
    "${{ phases.a.outputs.b }} extra",             # trailing junk
    "phases.a.outputs.b",                          # missing ${{ }}
    "${{ phases.a$.outputs.b }}",                  # disallowed char in name
])
def test_parse_rejects_malformed(bad):
    assert parse_phase_input_ref(bad) is None


# ─── validate_phase_input_refs — cross-phase validator ────────────────────


def _phase(name: str, *, inputs=None, outputs=None) -> PhaseSpec:
    return PhaseSpec(
        name=name,
        workflow_filename=f"{name}.yml",
        inputs=inputs or {},
        outputs=outputs or [],
    )


def test_validator_accepts_valid_forward_ref():
    phases = [
        _phase("env-prep", outputs=["validation_url", "image_tag"]),
        _phase(
            "agent-execute",
            inputs={"validation_url": "${{ phases.env-prep.outputs.validation_url }}"},
        ),
    ]
    validate_phase_input_refs(phases)  # must not raise


def test_validator_rejects_self_ref():
    phases = [
        _phase(
            "agent",
            inputs={"x": "${{ phases.agent.outputs.x }}"},
            outputs=["x"],
        ),
    ]
    with pytest.raises(ValueError, match="refs itself"):
        validate_phase_input_refs(phases)


def test_validator_rejects_forward_ref():
    """A phase referencing a phase that comes LATER in the order is a
    forward ref. v1 is forward-dispatch only — the value isn't available
    when this phase runs."""
    phases = [
        _phase(
            "agent",
            inputs={"x": "${{ phases.cleanup.outputs.x }}"},
        ),
        _phase("cleanup", outputs=["x"]),
    ]
    with pytest.raises(ValueError, match="doesn't appear earlier"):
        validate_phase_input_refs(phases)


def test_validator_rejects_unknown_phase():
    phases = [
        _phase("a", outputs=["x"]),
        _phase("b", inputs={"x": "${{ phases.nonexistent.outputs.x }}"}),
    ]
    with pytest.raises(ValueError, match="doesn't appear earlier"):
        validate_phase_input_refs(phases)


def test_validator_rejects_undeclared_output():
    """`a` declares `validation_url` but not `image_tag`; the consumer
    error must name both the bad key and the actually-declared keys so
    the consumer's typo is obvious."""
    phases = [
        _phase("a", outputs=["validation_url"]),
        _phase("b", inputs={"image_tag": "${{ phases.a.outputs.image_tag }}"}),
    ]
    with pytest.raises(ValueError, match="declared:"):
        validate_phase_input_refs(phases)


def test_validator_rejects_malformed_expression():
    phases = [
        _phase("a", outputs=["x"]),
        _phase("b", inputs={"x": "literal-not-a-ref"}),
    ]
    with pytest.raises(ValueError, match="not a valid phase ref"):
        validate_phase_input_refs(phases)


# ─── WorkflowRegister wiring ──────────────────────────────────────────────


def test_register_accepts_single_phase_with_outputs():
    """Single-phase workflows can declare `outputs` even though no
    downstream consumes them yet — future-proofs for the PR primitive's
    eventual consumption of phase outputs."""
    WorkflowRegister(
        project="p",
        name="w",
        phases=[
            PhaseSpec(
                name="agent",
                workflow_filename="agent.yml",
                outputs=["validation_url"],
            ),
        ],
    )


def test_register_rejects_single_phase_with_inputs():
    """A first phase has no upstream — declaring `inputs` is always wrong."""
    with pytest.raises(ValueError, match="doesn't appear earlier"):
        WorkflowRegister(
            project="p",
            name="w",
            phases=[
                PhaseSpec(
                    name="agent",
                    workflow_filename="agent.yml",
                    inputs={"x": "${{ phases.upstream.outputs.x }}"},
                ),
            ],
        )


def test_register_2phase_with_bad_ref_surfaces_ref_error_first():
    """The ref validator must run BEFORE the v1 single-phase gate so
    consumers see the actionable ref error, not a generic "v1 only
    supports one phase" message that hides the typo."""
    with pytest.raises(ValueError, match="not a valid phase ref"):
        WorkflowRegister(
            project="p",
            name="w",
            phases=[
                PhaseSpec(name="a", workflow_filename="a.yml", outputs=["x"]),
                PhaseSpec(
                    name="b",
                    workflow_filename="b.yml",
                    inputs={"x": "garbage"},
                ),
            ],
        )


# ─── substitute_phase_inputs — runtime substitution path ─────────────────


def test_substitute_resolves_against_prior_outputs():
    next_phase = _phase(
        "agent-execute",
        inputs={
            "validation_url": "${{ phases.env-prep.outputs.validation_url }}",
            "image_tag": "${{ phases.env-prep.outputs.image_tag }}",
        },
    )
    prior = {
        "env-prep": {
            "validation_url": "https://x.glimmung.dev",
            "image_tag": "issue-123-abc",
        },
    }
    assert substitute_phase_inputs(next_phase, prior) == {
        "validation_url": "https://x.glimmung.dev",
        "image_tag": "issue-123-abc",
    }


def test_substitute_empty_inputs_returns_empty():
    next_phase = _phase("a")
    assert substitute_phase_inputs(next_phase, {}) == {}


def test_substitute_raises_on_missing_phase():
    """Reaching this state means the upstream phase ran but had no
    captured outputs (or registration validation slipped). Raise loudly
    instead of silently substituting empty strings."""
    next_phase = _phase("b", inputs={"x": "${{ phases.a.outputs.x }}"})
    with pytest.raises(KeyError, match="no captured outputs"):
        substitute_phase_inputs(next_phase, {})


def test_substitute_raises_on_missing_key():
    next_phase = _phase("b", inputs={"x": "${{ phases.a.outputs.x }}"})
    with pytest.raises(KeyError, match="phase posted outputs"):
        substitute_phase_inputs(next_phase, {"a": {"y": "v"}})


# ─── WorkflowRegister wiring ──────────────────────────────────────────────


def test_register_2phase_valid_refs_accepted():
    """Multi-phase workflows with valid refs register cleanly. The v1
    single-phase gate dropped when the runtime landed (PR 3 of #101);
    earlier PRs in the series kept it on as a safety rail."""
    reg = WorkflowRegister(
        project="p",
        name="w",
        phases=[
            PhaseSpec(name="a", workflow_filename="a.yml", outputs=["x"]),
            PhaseSpec(
                name="b",
                workflow_filename="b.yml",
                inputs={"x": "${{ phases.a.outputs.x }}"},
            ),
        ],
    )
    assert [p.name for p in reg.phases] == ["a", "b"]

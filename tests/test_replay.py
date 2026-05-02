"""Decision-engine replay endpoint (#111 smoke-test substrate).

The replay endpoint is the cheap path for catching the verify=true→false-
class registration bugs that caused the prior session's iteration tax —
each real dispatch was ~20 minutes of agent runtime. Replay returns the
decision engine's verdict against a synthetic completion + (optionally)
an alternative workflow shape, with no Cosmos writes and no GHA
dispatches.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from fastapi import HTTPException

from glimmung.app import (
    RunReplayRequest,
    WorkflowReplayOverride,
    replay_run_decision,
)
from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    PhaseSpec,
    PrPrimitiveSpec,
    Run,
    RunState,
)
from glimmung.replay import SyntheticCompletion

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        projects=FakeContainer("projects", "/name"),
        workflows=FakeContainer("workflows", "/project"),
        hosts=FakeContainer("hosts", "/name"),
        leases=FakeContainer("leases", "/project"),
        runs=FakeContainer("runs", "/project"),
        locks=FakeContainer("locks", "/scope"),
        issues=FakeContainer("issues", "/project"),
    )


@pytest.fixture
def app_state(cosmos):
    state = SimpleNamespace(cosmos=cosmos, settings=None, gh_minter=None)
    return SimpleNamespace(state=state)


async def _seed_workflow_verify_true(cosmos, project: str = "ambience") -> None:
    """Two-phase workflow: env-prep (no verify) + agent-execute (verify=true,
    recycle on verify_fail). Mirrors the prior-session shape that caused the
    ABORT_MALFORMED with verify=true mismatch."""
    await cosmos.workflows.create_item({
        "id": "agent-run",
        "project": project,
        "name": "agent-run",
        "phases": [
            {
                "name": "env-prep",
                "kind": "gha_dispatch",
                "workflowFilename": "env-prep.yml",
                "workflowRef": "main",
                "requirements": None,
                "verify": False,
                "recyclePolicy": None,
                "inputs": {},
                "outputs": ["validation_url", "namespace"],
            },
            {
                "name": "agent-execute",
                "kind": "gha_dispatch",
                "workflowFilename": "agent-execute.yml",
                "workflowRef": "main",
                "requirements": None,
                "verify": True,
                "recyclePolicy": {
                    "maxAttempts": 3, "on": ["verify_fail"], "landsAt": "self",
                },
                "inputs": {
                    "validation_url": "${{ phases.env-prep.outputs.validation_url }}",
                    "namespace": "${{ phases.env-prep.outputs.namespace }}",
                },
                "outputs": [],
            },
        ],
        "pr": {"enabled": True, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": "agent:run",
        "defaultRequirements": {"project": project, "role": "agent"},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


async def _seed_run_at_agent_execute(
    cosmos,
    *,
    run_id: str = "01KQTEST_RUN_RPL",
    project: str = "ambience",
) -> Run:
    """Run that's already past env-prep and is on the agent-execute phase
    (matches the state the prior session's failing run was in)."""
    now = datetime.now(UTC)
    run = Run(
        id=run_id,
        project=project,
        workflow="agent-run",
        issue_id="01HZZZTESTISSUE",
        issue_repo="nelsong6/ambience",
        issue_number=116,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0,
                phase="env-prep",
                workflow_filename="env-prep.yml",
                dispatched_at=now,
                completed_at=now,
                conclusion="success",
                phase_outputs={
                    "validation_url": "https://abc123.romaine.life",
                    "namespace": "preview-abc123",
                },
            ),
            PhaseAttempt(
                attempt_index=1,
                phase="agent-execute",
                workflow_filename="agent-execute.yml",
                dispatched_at=now,
            ),
        ],
        cumulative_cost_usd=0.0,
        created_at=now,
        updated_at=now,
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))
    return run


# ─── Verify=true→false case (the motivating bug) ──────────────────────────


@pytest.mark.asyncio
async def test_replay_verify_true_no_artifact_aborts_malformed(cosmos, app_state):
    """The exact prior-session failure: registered verify=true, /completed
    posts no verification field. decide() returns ABORT_MALFORMED."""
    await _seed_workflow_verify_true(cosmos)
    run = await _seed_run_at_agent_execute(cosmos)

    req = RunReplayRequest(
        synthetic_completion=SyntheticCompletion(
            conclusion="success",
            verification=None,
        ),
    )
    with patch("glimmung.app.app", app_state):
        result = await replay_run_decision(req, project=run.project, run_id=run.id)

    assert result.decision == "abort_malformed"
    assert result.applied_to_phase == "agent-execute"
    assert result.applied_to_attempt_index == 1
    assert result.workflow_source == "registered"
    assert result.abort_reason is not None and "verification.json" in result.abort_reason


@pytest.mark.asyncio
async def test_replay_override_verify_false_advances_to_pr(cosmos, app_state):
    """The fix path: override_workflow flips agent-execute to verify=false.
    decide() now treats GHA conclusion as authoritative; ADVANCE on
    success, and since it's the last phase, would_open_pr=True."""
    await _seed_workflow_verify_true(cosmos)
    run = await _seed_run_at_agent_execute(cosmos)

    req = RunReplayRequest(
        synthetic_completion=SyntheticCompletion(
            conclusion="success",
            verification=None,
        ),
        override_workflow=WorkflowReplayOverride(
            phases=[
                PhaseSpec(
                    name="env-prep",
                    workflow_filename="env-prep.yml",
                    verify=False,
                    outputs=["validation_url", "namespace"],
                ),
                PhaseSpec(
                    name="agent-execute",
                    workflow_filename="agent-execute.yml",
                    verify=False,  # the fix: drop verify so missing artifact isn't fatal
                    inputs={
                        "validation_url": "${{ phases.env-prep.outputs.validation_url }}",
                        "namespace": "${{ phases.env-prep.outputs.namespace }}",
                    },
                ),
            ],
            pr=PrPrimitiveSpec(enabled=True),
            budget=BudgetConfig(total=25.0),
        ),
    )
    with patch("glimmung.app.app", app_state):
        result = await replay_run_decision(req, project=run.project, run_id=run.id)

    assert result.decision == "advance"
    assert result.would_open_pr is True
    assert result.would_advance_to_phase is None  # last phase
    assert result.workflow_source == "override"


# ─── Other decision paths ─────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_replay_pass_advances_to_next_phase(cosmos, app_state):
    """Non-terminal ADVANCE: PASS verification on a non-final phase; the
    next phase in declared order should be the would_advance_to_phase."""
    # Register a 2-phase workflow where both phases verify, so a PASS on
    # the first phase routes through ADVANCE→next-phase.
    await cosmos.workflows.create_item({
        "id": "two-verify",
        "project": "ambience",
        "name": "two-verify",
        "phases": [
            {
                "name": "p1", "kind": "gha_dispatch",
                "workflowFilename": "p1.yml", "workflowRef": "main",
                "requirements": None, "verify": True,
                "recyclePolicy": {"maxAttempts": 3, "on": ["verify_fail"], "landsAt": "self"},
                "inputs": {}, "outputs": [],
            },
            {
                "name": "p2", "kind": "gha_dispatch",
                "workflowFilename": "p2.yml", "workflowRef": "main",
                "requirements": None, "verify": True,
                "recyclePolicy": {"maxAttempts": 3, "on": ["verify_fail"], "landsAt": "self"},
                "inputs": {}, "outputs": [],
            },
        ],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": "agent:run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })
    now = datetime.now(UTC)
    run = Run(
        id="01KQTEST_RUN_NXT",
        project="ambience",
        workflow="two-verify",
        issue_id="01HZTEST",
        issue_repo="nelsong6/ambience",
        issue_number=200,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0, phase="p1",
                workflow_filename="p1.yml", dispatched_at=now,
            ),
        ],
        created_at=now, updated_at=now,
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))

    req = RunReplayRequest(
        synthetic_completion=SyntheticCompletion(
            conclusion="success",
            verification={"schema_version": 1, "status": "pass"},
        ),
    )
    with patch("glimmung.app.app", app_state):
        result = await replay_run_decision(req, project=run.project, run_id=run.id)

    assert result.decision == "advance"
    assert result.would_advance_to_phase == "p2"
    assert result.would_open_pr is False


@pytest.mark.asyncio
async def test_replay_verify_fail_under_budget_retries(cosmos, app_state):
    """Recycle path: verification.status=fail, attempts under max, in
    recycle.on. decide() returns RETRY; replay reports the lands_at
    target."""
    await _seed_workflow_verify_true(cosmos)
    run = await _seed_run_at_agent_execute(cosmos)

    req = RunReplayRequest(
        synthetic_completion=SyntheticCompletion(
            conclusion="success",  # GHA succeeded, but verification said fail
            verification={
                "schema_version": 1, "status": "fail",
                "reasons": ["screenshot didn't render"], "cost_usd": 1.5,
            },
        ),
    )
    with patch("glimmung.app.app", app_state):
        result = await replay_run_decision(req, project=run.project, run_id=run.id)

    assert result.decision == "retry"
    assert result.would_retry_target_phase == "agent-execute"
    assert result.cumulative_cost_usd_after == pytest.approx(1.5)


@pytest.mark.asyncio
async def test_replay_does_not_mutate_run(cosmos, app_state):
    """Pure-function discipline: the Run document in Cosmos is unchanged
    after replay, regardless of the decision the engine returned."""
    await _seed_workflow_verify_true(cosmos)
    run = await _seed_run_at_agent_execute(cosmos)
    pre = run.model_dump(mode="json")

    req = RunReplayRequest(
        synthetic_completion=SyntheticCompletion(
            conclusion="success",
            verification={"schema_version": 1, "status": "fail", "cost_usd": 5.0},
        ),
    )
    with patch("glimmung.app.app", app_state):
        await replay_run_decision(req, project=run.project, run_id=run.id)

    post_doc = await cosmos.runs.read_item(item=run.id, partition_key=run.project)
    # Strip cosmos meta + drop the etag the fake stamps, compare the rest.
    post = {k: v for k, v in post_doc.items() if not k.startswith("_")}
    assert post == pre


# ─── Error paths ──────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_replay_404_for_unknown_run(cosmos, app_state):
    await _seed_workflow_verify_true(cosmos)
    req = RunReplayRequest(
        synthetic_completion=SyntheticCompletion(conclusion="success"),
    )
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await replay_run_decision(req, project="ambience", run_id="01_NO_SUCH")
        assert exc.value.status_code == 404


@pytest.mark.asyncio
async def test_replay_404_when_workflow_missing_and_no_override(cosmos, app_state):
    """Run exists but its workflow registration is gone (a real failure
    mode the existing _process_run_completion logs + aborts on). Replay
    should 404 with a hint pointing at override_workflow."""
    run = await _seed_run_at_agent_execute(cosmos)  # no workflow seeded
    req = RunReplayRequest(
        synthetic_completion=SyntheticCompletion(conclusion="success"),
    )
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await replay_run_decision(req, project=run.project, run_id=run.id)
        assert exc.value.status_code == 404
        assert "override_workflow" in str(exc.value.detail)


@pytest.mark.asyncio
async def test_replay_422_on_override_with_bad_input_ref(cosmos, app_state):
    """Override workflow with a typo in the cross-phase ref should 422
    with the same validation error register_workflow returns."""
    await _seed_workflow_verify_true(cosmos)
    run = await _seed_run_at_agent_execute(cosmos)

    req = RunReplayRequest(
        synthetic_completion=SyntheticCompletion(conclusion="success"),
        override_workflow=WorkflowReplayOverride(
            phases=[
                PhaseSpec(
                    name="env-prep",
                    workflow_filename="env-prep.yml",
                    verify=False,
                    outputs=["validation_url"],
                ),
                PhaseSpec(
                    name="agent-execute",
                    workflow_filename="agent-execute.yml",
                    verify=False,
                    inputs={
                        # Typo: declared output is `validation_url`, ref says `validatoin_url`.
                        "validation_url": "${{ phases.env-prep.outputs.validatoin_url }}",
                    },
                ),
            ],
        ),
    )
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await replay_run_decision(req, project=run.project, run_id=run.id)
        assert exc.value.status_code == 422
        assert "validatoin_url" in str(exc.value.detail)


@pytest.mark.asyncio
async def test_replay_422_when_run_attempt_phase_not_in_workflow(cosmos, app_state):
    """If the run's latest attempt names a phase that doesn't exist on
    the (override or registered) workflow, surface a readable 422 — beats
    a 500 from inside decide()."""
    await _seed_workflow_verify_true(cosmos)
    run = await _seed_run_at_agent_execute(cosmos)

    req = RunReplayRequest(
        synthetic_completion=SyntheticCompletion(conclusion="success"),
        override_workflow=WorkflowReplayOverride(
            phases=[
                # Note: doesn't include `agent-execute`, the run's latest phase.
                PhaseSpec(
                    name="env-prep",
                    workflow_filename="env-prep.yml",
                    verify=False,
                    outputs=["validation_url"],
                ),
            ],
        ),
    )
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await replay_run_decision(req, project=run.project, run_id=run.id)
        assert exc.value.status_code == 422
        assert "agent-execute" in str(exc.value.detail)

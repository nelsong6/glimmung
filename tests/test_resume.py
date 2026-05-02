"""Resume primitive (#111) tests.

Covers `runs.create_resumed_run` (pure synthesis) + `dispatch.dispatch_
resumed_run` (lock + run + dispatch orchestration) + the HTTP endpoint
surface. Exercises the prior-session friction case end-to-end: an
agent-execute attempt died on a verify=true→false mismatch; resume from
agent-execute re-uses env-prep's captured outputs and dispatches a fresh
agent-execute attempt.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from fastapi import HTTPException

from glimmung import runs as run_ops
from glimmung.app import (
    RunResumeRequest,
    resume_run,
)
from glimmung.dispatch import dispatch_resumed_run
from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    Run,
    RunState,
)

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
        signals=FakeContainer("signals", "/target_repo"),
        issues=FakeContainer("issues", "/project"),
    )


@pytest.fixture
def settings():
    """Stub settings with the two values the dispatch path reads."""
    return SimpleNamespace(
        lease_default_ttl_seconds=14400,  # 4h, matches prod default
        sweep_interval_seconds=60,
    )


@pytest.fixture
def app_state(cosmos, settings):
    state = SimpleNamespace(
        cosmos=cosmos,
        settings=settings,
        gh_minter=None,  # no GH; _dispatch_next_phase logs and returns
    )
    return SimpleNamespace(state=state)


# ─── Fixtures: workflow + prior runs ───────────────────────────────────────


async def _seed_workflow_two_phase(cosmos, project: str = "ambience") -> None:
    """env-prep (no verify, declares outputs) + agent-execute (verify=false
    after the v=t→v=f fix)."""
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
                "verify": False,
                "recyclePolicy": None,
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


async def _seed_aborted_run_at_agent_execute(
    cosmos,
    *,
    run_id: str = "01KQTEST_PRIOR_AAA",
    project: str = "ambience",
) -> Run:
    """Prior run that completed env-prep successfully (with phase_outputs
    captured) and then aborted on agent-execute. Mirrors the shape the
    prior session left behind."""
    now = datetime.now(UTC)
    run = Run(
        id=run_id,
        project=project,
        workflow="agent-run",
        issue_id="01HZRSMRESUMETEST",
        issue_repo="nelsong6/ambience",
        issue_number=116,
        state=RunState.ABORTED,
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
                workflow_run_id=99999,
                dispatched_at=now,
                completed_at=now,
                conclusion="success",
                # No verification field — verify=true mismatch returned
                # ABORT_MALFORMED.
                decision="abort_malformed",
            ),
        ],
        cumulative_cost_usd=0.0,
        abort_reason="verify=true mismatch (the bug #111 documents)",
        created_at=now,
        updated_at=now,
        validation_url="https://abc123.romaine.life",
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))
    return run


async def _seed_host(cosmos, name: str = "ambience-slot-1") -> None:
    await cosmos.hosts.create_item({
        "id": name,
        "name": name,
        "capabilities": {"project": "ambience", "role": "agent"},
        "current_lease_id": None,
        "last_heartbeat": None,
        "last_used_at": None,
        "drained": False,
        "created_at": datetime.now(UTC).isoformat(),
    })


# ─── create_resumed_run (pure synthesis) ──────────────────────────────────


@pytest.mark.asyncio
async def test_create_resumed_run_synthesizes_skipped_attempts(cosmos):
    """The basic synthesis: skipped phases get PhaseAttempts with carried
    phase_outputs, conclusion=success, skipped_from_run_id set, and a
    completed_at stamp so multi-phase substitution sees them as done."""
    await _seed_workflow_two_phase(cosmos)
    prior = await _seed_aborted_run_at_agent_execute(cosmos)

    workflow_doc = await cosmos.workflows.read_item(
        item="agent-run", partition_key="ambience",
    )
    from glimmung.app import _doc_to_workflow
    workflow = _doc_to_workflow(workflow_doc)

    new_run, etag = await run_ops.create_resumed_run(
        cosmos,
        prior_run=prior,
        workflow=workflow,
        entrypoint_phase="agent-execute",
    )

    assert new_run.cloned_from_run_id == prior.id
    assert new_run.entrypoint_phase == "agent-execute"
    assert new_run.state == RunState.IN_PROGRESS
    assert new_run.cumulative_cost_usd == 0.0
    assert new_run.validation_url == prior.validation_url

    # One skipped attempt for env-prep.
    assert len(new_run.attempts) == 1
    skipped = new_run.attempts[0]
    assert skipped.phase == "env-prep"
    assert skipped.skipped_from_run_id == prior.id
    assert skipped.conclusion == "success"
    assert skipped.workflow_run_id is None
    assert skipped.completed_at is not None
    assert skipped.phase_outputs == {
        "validation_url": "https://abc123.romaine.life",
        "namespace": "preview-abc123",
    }
    # etag was returned (Cosmos meta) so the caller can chain into
    # _dispatch_next_phase without re-reading.
    assert etag


@pytest.mark.asyncio
async def test_create_resumed_run_rejects_invalid_entrypoint(cosmos):
    await _seed_workflow_two_phase(cosmos)
    prior = await _seed_aborted_run_at_agent_execute(cosmos)
    workflow_doc = await cosmos.workflows.read_item(
        item="agent-run", partition_key="ambience",
    )
    from glimmung.app import _doc_to_workflow
    workflow = _doc_to_workflow(workflow_doc)

    with pytest.raises(ValueError, match="not declared on workflow"):
        await run_ops.create_resumed_run(
            cosmos,
            prior_run=prior,
            workflow=workflow,
            entrypoint_phase="ghost-phase",
        )


@pytest.mark.asyncio
async def test_create_resumed_run_rejects_skipped_without_outputs(cosmos):
    """Resume should fail loudly if a skipped phase has no captured
    outputs on the prior Run — the multi-phase substitution would 500
    on the missing ref otherwise."""
    await _seed_workflow_two_phase(cosmos)
    workflow_doc = await cosmos.workflows.read_item(
        item="agent-run", partition_key="ambience",
    )
    from glimmung.app import _doc_to_workflow
    workflow = _doc_to_workflow(workflow_doc)

    # Prior run that never even completed env-prep.
    now = datetime.now(UTC)
    prior = Run(
        id="01KQTEST_PRIOR_BAD",
        project="ambience",
        workflow="agent-run",
        issue_id="01HZRSMTEST",
        issue_repo="nelsong6/ambience",
        issue_number=200,
        state=RunState.ABORTED,
        budget=BudgetConfig(total=25.0),
        attempts=[],  # nothing captured
        created_at=now, updated_at=now,
    )
    await cosmos.runs.create_item(prior.model_dump(mode="json"))

    with pytest.raises(ValueError, match="no attempts on prior run"):
        await run_ops.create_resumed_run(
            cosmos,
            prior_run=prior,
            workflow=workflow,
            entrypoint_phase="agent-execute",
        )


@pytest.mark.asyncio
async def test_create_resumed_run_first_phase_no_skipped_attempts(cosmos):
    """Resume from phases[0]: nothing to skip; new Run starts empty,
    ready for the dispatcher to append a fresh attempt."""
    await _seed_workflow_two_phase(cosmos)
    prior = await _seed_aborted_run_at_agent_execute(cosmos)
    workflow_doc = await cosmos.workflows.read_item(
        item="agent-run", partition_key="ambience",
    )
    from glimmung.app import _doc_to_workflow
    workflow = _doc_to_workflow(workflow_doc)

    new_run, _ = await run_ops.create_resumed_run(
        cosmos,
        prior_run=prior,
        workflow=workflow,
        entrypoint_phase="env-prep",
    )

    assert new_run.attempts == []
    assert new_run.entrypoint_phase == "env-prep"
    assert new_run.cloned_from_run_id == prior.id


# ─── dispatch_resumed_run (orchestration + endpoint) ──────────────────────


@pytest.mark.asyncio
async def test_dispatch_resumed_run_happy_path(cosmos, app_state):
    """End-to-end: aborted prior, no host conflict, resume succeeds.
    Without a GH minter the dispatch path logs and returns; the new
    Run is persisted with skipped+entrypoint attempts."""
    await _seed_workflow_two_phase(cosmos)
    await _seed_host(cosmos)
    prior = await _seed_aborted_run_at_agent_execute(cosmos)

    with patch("glimmung.app.app", app_state):
        result = await dispatch_resumed_run(
            app_state,
            project=prior.project,
            prior_run_id=prior.id,
            entrypoint_phase="agent-execute",
            trigger_source={"kind": "resume_via_admin_api"},
        )

    assert result.state in ("dispatched", "pending")  # depends on host availability
    assert result.new_run_id != prior.id
    assert result.prior_run_id == prior.id
    assert result.issue_lock_holder_id

    # New run persisted with the right shape.
    found = await run_ops.read_run(
        cosmos, project=prior.project, run_id=result.new_run_id,
    )
    assert found is not None
    new_run, _ = found
    assert new_run.cloned_from_run_id == prior.id
    assert new_run.entrypoint_phase == "agent-execute"
    # 1 skipped (env-prep) + 1 fresh entrypoint (agent-execute).
    assert len(new_run.attempts) == 2
    assert new_run.attempts[0].phase == "env-prep"
    assert new_run.attempts[0].skipped_from_run_id == prior.id
    assert new_run.attempts[1].phase == "agent-execute"
    assert new_run.attempts[1].skipped_from_run_id is None
    assert new_run.attempts[1].completed_at is None  # awaiting /completed callback

    # Issue lock was claimed for the resumed run.
    lock_doc = await cosmos.locks.read_item(
        item="issue::nelsong6%2Fambience%23116", partition_key="issue",
    )
    assert lock_doc["state"] == "held"
    assert lock_doc["held_by"] == result.issue_lock_holder_id


@pytest.mark.asyncio
async def test_dispatch_resumed_run_refuses_in_progress_prior(cosmos, app_state):
    """Resuming from an IN_PROGRESS run would race the live dispatch's
    lock + lease. Refuse loudly."""
    await _seed_workflow_two_phase(cosmos)
    now = datetime.now(UTC)
    prior = Run(
        id="01KQTEST_INPROG",
        project="ambience",
        workflow="agent-run",
        issue_id="01HZTEST",
        issue_repo="nelsong6/ambience",
        issue_number=42,
        state=RunState.IN_PROGRESS,  # the disqualifier
        budget=BudgetConfig(total=25.0),
        attempts=[PhaseAttempt(
            attempt_index=0, phase="env-prep",
            workflow_filename="env-prep.yml", dispatched_at=now,
        )],
        created_at=now, updated_at=now,
    )
    await cosmos.runs.create_item(prior.model_dump(mode="json"))

    result = await dispatch_resumed_run(
        app_state,
        project=prior.project,
        prior_run_id=prior.id,
        entrypoint_phase="agent-execute",
        trigger_source={"kind": "test"},
    )

    assert result.state == "prior_in_progress"
    assert result.new_run_id is None


@pytest.mark.asyncio
async def test_dispatch_resumed_run_refuses_when_issue_locked(cosmos, app_state):
    """Another in-flight run holds the issue lock — resume must refuse
    rather than step on it."""
    from glimmung import locks as lock_ops
    await _seed_workflow_two_phase(cosmos)
    prior = await _seed_aborted_run_at_agent_execute(cosmos)

    # Different run holding the issue lock right now.
    await lock_ops.claim_lock(
        cosmos, scope="issue",
        key="nelsong6/ambience#116",
        holder_id="01OTHER_RUN_HOLDS_LOCK",
        ttl_seconds=3600,
        metadata={"trigger_source": {"kind": "test"}},
    )

    with patch("glimmung.app.app", app_state):
        result = await dispatch_resumed_run(
            app_state,
            project=prior.project,
            prior_run_id=prior.id,
            entrypoint_phase="agent-execute",
            trigger_source={"kind": "test"},
        )

    assert result.state == "already_running"
    assert "01OTHER_RUN_HOLDS_LOCK" in (result.detail or "")


@pytest.mark.asyncio
async def test_dispatch_resumed_run_returns_phase_invalid(cosmos, app_state):
    await _seed_workflow_two_phase(cosmos)
    prior = await _seed_aborted_run_at_agent_execute(cosmos)

    with patch("glimmung.app.app", app_state):
        result = await dispatch_resumed_run(
            app_state,
            project=prior.project,
            prior_run_id=prior.id,
            entrypoint_phase="ghost-phase",
            trigger_source={"kind": "test"},
        )

    assert result.state == "phase_invalid"
    assert "ghost-phase" in (result.detail or "")


@pytest.mark.asyncio
async def test_dispatch_resumed_run_returns_workflow_missing(cosmos, app_state):
    """Workflow registration was deleted between prior run and resume.
    Surface as workflow_missing so the operator re-registers first."""
    prior = await _seed_aborted_run_at_agent_execute(cosmos)
    # Don't seed workflow.

    with patch("glimmung.app.app", app_state):
        result = await dispatch_resumed_run(
            app_state,
            project=prior.project,
            prior_run_id=prior.id,
            entrypoint_phase="agent-execute",
            trigger_source={"kind": "test"},
        )

    assert result.state == "workflow_missing"


# ─── HTTP endpoint ────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_resume_endpoint_404_on_prior_missing(cosmos, app_state):
    await _seed_workflow_two_phase(cosmos)
    req = RunResumeRequest(entrypoint_phase="agent-execute")
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await resume_run(req, project="ambience", run_id="01_NO_PRIOR")
        assert exc.value.status_code == 404


@pytest.mark.asyncio
async def test_resume_endpoint_409_on_in_progress_prior(cosmos, app_state):
    await _seed_workflow_two_phase(cosmos)
    now = datetime.now(UTC)
    prior = Run(
        id="01KQTEST_INPROG2",
        project="ambience",
        workflow="agent-run",
        issue_id="01HZTEST",
        issue_repo="nelsong6/ambience",
        issue_number=44,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[PhaseAttempt(
            attempt_index=0, phase="env-prep",
            workflow_filename="env-prep.yml", dispatched_at=now,
        )],
        created_at=now, updated_at=now,
    )
    await cosmos.runs.create_item(prior.model_dump(mode="json"))

    req = RunResumeRequest(entrypoint_phase="agent-execute")
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await resume_run(req, project=prior.project, run_id=prior.id)
        assert exc.value.status_code == 409
        assert "abort the prior run" in (exc.value.detail or "")


@pytest.mark.asyncio
async def test_resume_endpoint_422_on_invalid_phase(cosmos, app_state):
    await _seed_workflow_two_phase(cosmos)
    prior = await _seed_aborted_run_at_agent_execute(cosmos)
    req = RunResumeRequest(entrypoint_phase="ghost-phase")
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await resume_run(req, project=prior.project, run_id=prior.id)
        assert exc.value.status_code == 422


@pytest.mark.asyncio
async def test_resume_endpoint_happy_path_returns_dispatched(cosmos, app_state):
    """End-to-end through the HTTP layer: prior aborted → resume →
    new Run id returned, lock claimed, attempts shaped right."""
    await _seed_workflow_two_phase(cosmos)
    await _seed_host(cosmos)
    prior = await _seed_aborted_run_at_agent_execute(cosmos)

    req = RunResumeRequest(
        entrypoint_phase="agent-execute",
        trigger_source={"kind": "test_via_http", "actor": "harness"},
    )
    with patch("glimmung.app.app", app_state):
        result = await resume_run(req, project=prior.project, run_id=prior.id)

    assert result.state in ("dispatched", "pending")
    assert result.new_run_id and result.new_run_id != prior.id
    assert result.prior_run_id == prior.id

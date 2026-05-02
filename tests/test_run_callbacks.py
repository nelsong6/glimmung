"""Workflow → glimmung run-state callbacks (`/v1/runs/{p}/{run_id}/started`
and `/v1/runs/{p}/{run_id}/completed`).

These replaced the workflow_run webhook handler. GitHub's `workflow_run`
payload doesn't echo workflow_dispatch inputs, so glimmung can't map an
inbound webhook to a Run without help; the workflow now reports
lifecycle directly via curl.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace
from unittest.mock import patch

import pytest

from glimmung import runs as run_ops
from glimmung.app import (
    RunCompletedRequest,
    RunStartedRequest,
    run_completed,
    run_started,
)
from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    Run,
    RunState,
    VerificationStatus,
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
        issues=FakeContainer("issues", "/project"),
    )


@pytest.fixture
def app_state(cosmos):
    state = SimpleNamespace(cosmos=cosmos, settings=None, gh_minter=None)
    return SimpleNamespace(state=state)


async def _register_project(cosmos, name: str, repo: str) -> None:
    await cosmos.projects.create_item({
        "id": name,
        "name": name,
        "githubRepo": repo,
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


async def _register_workflow_with_recycle(
    cosmos, project: str, name: str = "agent",
) -> None:
    """Workflow that opts into the verify loop (single phase + recycle)."""
    await cosmos.workflows.create_item({
        "id": name,
        "name": name,
        "project": project,
        "phases": [{
            "name": "agent",
            "kind": "gha_dispatch",
            "workflowFilename": "agent-run.yml",
            "workflowRef": "main",
            "requirements": None,
            "verify": True,
            "recyclePolicy": {
                "maxAttempts": 3, "on": ["verify_fail"], "landsAt": "self",
            },
        }],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": "agent-run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


async def _seed_run(
    cosmos,
    *,
    run_id: str,
    project: str,
    issue_repo: str,
    issue_number: int,
    issue_lock_holder_id: str | None = None,
) -> Run:
    """Mint a Run with a single dispatched-but-uncompleted attempt — the
    state immediately after `_maybe_dispatch_workflow` returns."""
    run = Run(
        id=run_id,
        project=project,
        workflow="agent",
        issue_id="01HZZZTESTISSUE",
        issue_repo=issue_repo,
        issue_number=issue_number,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[PhaseAttempt(
            attempt_index=0,
            phase="agent",
            workflow_filename="agent-run.yml",
            dispatched_at=datetime.now(UTC),
        )],
        cumulative_cost_usd=0.0,
        issue_lock_holder_id=issue_lock_holder_id,
        created_at=datetime.now(UTC),
        updated_at=datetime.now(UTC),
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))
    return run


# ─── /started ──────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_started_stamps_workflow_run_id(cosmos, app_state):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_AAA", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=42,
    )
    with patch("glimmung.app.app", app_state):
        result = await run_started(
            RunStartedRequest(workflow_run_id=25255513874),
            project="ambience", run_id=run.id,
        )
    assert result.run_id == run.id

    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    assert found is not None
    updated, _ = found
    assert updated.attempts[-1].workflow_run_id == 25255513874


@pytest.mark.asyncio
async def test_started_is_idempotent_on_redelivery(cosmos, app_state):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_BBB", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=43,
    )
    with patch("glimmung.app.app", app_state):
        await run_started(
            RunStartedRequest(workflow_run_id=99999),
            project="ambience", run_id=run.id,
        )
        # A duplicate call with the SAME id — no-op.
        await run_started(
            RunStartedRequest(workflow_run_id=99999),
            project="ambience", run_id=run.id,
        )
    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    updated, _ = found  # type: ignore[misc]
    assert updated.attempts[-1].workflow_run_id == 99999


@pytest.mark.asyncio
async def test_started_404_for_unknown_run(cosmos, app_state):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    with patch("glimmung.app.app", app_state):
        from fastapi import HTTPException
        with pytest.raises(HTTPException) as exc:
            await run_started(
                RunStartedRequest(workflow_run_id=1),
                project="ambience", run_id="01KQ_DOES_NOT_EXIST",
            )
        assert exc.value.status_code == 404


# ─── /completed ────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_completed_pass_advances_run(cosmos, app_state):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_recycle(cosmos, "ambience")
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_CCC", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=44,
    )

    body = RunCompletedRequest(
        workflow_run_id=25255513874,
        conclusion="success",
        verification={
            "schema_version": 1,
            "status": "pass",
            "reasons": [],
            "evidence_refs": [],
            "cost_usd": 0.42,
            "prompt_version": "ambience-v1",
            "metadata": {},
        },
    )
    with patch("glimmung.app.app", app_state):
        result = await run_completed(
            body, project="ambience", run_id=run.id,
        )
    assert result.decision == "advance"

    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    final, _ = found  # type: ignore[misc]
    assert final.state == RunState.PASSED
    assert final.attempts[-1].workflow_run_id == 25255513874
    assert final.attempts[-1].conclusion == "success"
    assert final.attempts[-1].verification.status == VerificationStatus.PASS
    assert final.cumulative_cost_usd == pytest.approx(0.42)


@pytest.mark.asyncio
async def test_completed_releases_issue_lock_on_terminal(cosmos, app_state):
    """Run with an issue_lock_holder_id should release that lock when the
    decision is terminal (ADVANCE / ABORT_*). RETRY does not release."""
    from glimmung import locks as lock_ops
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_recycle(cosmos, "ambience")

    holder = "01HZZZHOLDER000000000000"
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_DDD", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=45,
        issue_lock_holder_id=holder,
    )
    await lock_ops.claim_lock(
        cosmos, scope="issue", key="nelsong6/ambience#45",
        holder_id=holder, ttl_seconds=14400, metadata={},
    )

    body = RunCompletedRequest(
        workflow_run_id=1, conclusion="success",
        verification={
            "schema_version": 1, "status": "pass",
            "reasons": [], "evidence_refs": [], "cost_usd": 0.0,
        },
    )
    with patch("glimmung.app.app", app_state):
        result = await run_completed(
            body, project="ambience", run_id=run.id,
        )
    assert result.decision == "advance"
    assert result.issue_lock_released is True


@pytest.mark.asyncio
async def test_completed_malformed_verification_aborts(cosmos, app_state):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_recycle(cosmos, "ambience")
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_EEE", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=46,
    )

    body = RunCompletedRequest(
        workflow_run_id=1, conclusion="failure",
        verification=None,  # producer crashed before emitting verification.json
    )
    with patch("glimmung.app.app", app_state):
        result = await run_completed(
            body, project="ambience", run_id=run.id,
        )
    assert result.decision == "abort_malformed"

    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    final, _ = found  # type: ignore[misc]
    assert final.state == RunState.ABORTED


@pytest.mark.asyncio
async def test_completed_404_for_unknown_run(cosmos, app_state):
    with patch("glimmung.app.app", app_state):
        from fastapi import HTTPException
        body = RunCompletedRequest(workflow_run_id=1, conclusion="success")
        with pytest.raises(HTTPException) as exc:
            await run_completed(
                body, project="ambience", run_id="01KQ_NOPE",
            )
        assert exc.value.status_code == 404


# ─── /completed: phase output capture (#101) ──────────────────────────────


async def _register_workflow_with_outputs(
    cosmos,
    project: str,
    *,
    outputs: list[str],
    name: str = "agent",
) -> None:
    """Workflow whose single phase declares the given outputs. Used by
    the #101 output-capture tests; verify=True stays on so the existing
    decision-engine path still drives terminal state."""
    await cosmos.workflows.create_item({
        "id": name,
        "name": name,
        "project": project,
        "phases": [{
            "name": "agent",
            "kind": "gha_dispatch",
            "workflowFilename": "agent-run.yml",
            "workflowRef": "main",
            "requirements": None,
            "verify": True,
            "recyclePolicy": {
                "maxAttempts": 3, "on": ["verify_fail"], "landsAt": "self",
            },
            "inputs": {},
            "outputs": outputs,
        }],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": "agent-run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


def _pass_verification() -> dict:
    return {
        "schema_version": 1,
        "status": "pass",
        "reasons": [],
        "evidence_refs": [],
        "cost_usd": 0.0,
        "metadata": {},
    }


@pytest.mark.asyncio
async def test_completed_persists_phase_outputs(cosmos, app_state):
    """Posted outputs whose keys match the phase's declared `outputs`
    are persisted on the latest PhaseAttempt. The runtime substitution
    path (PR 3) reads from this field."""
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_outputs(
        cosmos, "ambience", outputs=["validation_url", "image_tag"],
    )
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_OUT1", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=100,
    )

    body = RunCompletedRequest(
        workflow_run_id=1234,
        conclusion="success",
        verification=_pass_verification(),
        outputs={
            "validation_url": "https://issue-100-1234-abc.glimmung.dev.romaine.life",
            "image_tag": "issue-100-1234-abc",
        },
    )
    with patch("glimmung.app.app", app_state):
        await run_completed(body, project="ambience", run_id=run.id)

    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    final, _ = found  # type: ignore[misc]
    assert final.attempts[-1].phase_outputs == {
        "validation_url": "https://issue-100-1234-abc.glimmung.dev.romaine.life",
        "image_tag": "issue-100-1234-abc",
    }


@pytest.mark.asyncio
async def test_completed_400_on_missing_output_key(cosmos, app_state):
    """Phase declares two outputs; the request omits one. Contract
    violation → 400, run state untouched (no completion recorded)."""
    from fastapi import HTTPException
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_outputs(
        cosmos, "ambience", outputs=["validation_url", "image_tag"],
    )
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_OUT2", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=101,
    )

    body = RunCompletedRequest(
        workflow_run_id=1, conclusion="success",
        verification=_pass_verification(),
        outputs={"validation_url": "https://x"},  # image_tag missing
    )
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await run_completed(body, project="ambience", run_id=run.id)
    assert exc.value.status_code == 400
    assert "missing" in exc.value.detail
    assert "image_tag" in exc.value.detail

    # Run state unchanged — no completion recorded on bad payload.
    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    final, _ = found  # type: ignore[misc]
    assert final.attempts[-1].completed_at is None
    assert final.attempts[-1].phase_outputs is None


@pytest.mark.asyncio
async def test_completed_400_on_extra_output_key(cosmos, app_state):
    """Posted key not in the phase's declared `outputs` → 400. The
    consumer's workflow has drifted from registration; safer to fail
    loud than silently drop the extra value."""
    from fastapi import HTTPException
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_outputs(
        cosmos, "ambience", outputs=["validation_url"],
    )
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_OUT3", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=102,
    )

    body = RunCompletedRequest(
        workflow_run_id=1, conclusion="success",
        verification=_pass_verification(),
        outputs={"validation_url": "https://x", "rogue_extra": "y"},
    )
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await run_completed(body, project="ambience", run_id=run.id)
    assert exc.value.status_code == 400
    assert "unexpected" in exc.value.detail
    assert "rogue_extra" in exc.value.detail


@pytest.mark.asyncio
async def test_completed_400_when_outputs_posted_against_phase_with_none(
    cosmos, app_state,
):
    """Phase declares no `outputs`; consumer posts some anyway. Same
    "extra key" failure mode."""
    from fastapi import HTTPException
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_outputs(cosmos, "ambience", outputs=[])
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_OUT4", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=103,
    )

    body = RunCompletedRequest(
        workflow_run_id=1, conclusion="success",
        verification=_pass_verification(),
        outputs={"surprise": "value"},
    )
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await run_completed(body, project="ambience", run_id=run.id)
    assert exc.value.status_code == 400


@pytest.mark.asyncio
async def test_completed_400_when_outputs_omitted_against_declared_phase(
    cosmos, app_state,
):
    """Phase declares outputs; the request omits the `outputs` field
    entirely. Treated as "consumer claims success without producing
    declared outputs" — contract violation."""
    from fastapi import HTTPException
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_outputs(
        cosmos, "ambience", outputs=["validation_url"],
    )
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_OUT5", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=104,
    )

    body = RunCompletedRequest(
        workflow_run_id=1, conclusion="success",
        verification=_pass_verification(),
        # outputs omitted entirely
    )
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await run_completed(body, project="ambience", run_id=run.id)
    assert exc.value.status_code == 400
    assert "missing" in exc.value.detail


@pytest.mark.asyncio
async def test_completed_legacy_phase_with_no_outputs_still_works(
    cosmos, app_state,
):
    """Regression: existing single-phase workflows that declare no
    outputs and post no outputs continue to work unchanged. Output
    capture is opt-in via PhaseSpec.outputs."""
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_outputs(cosmos, "ambience", outputs=[])
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_OUT6", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=105,
    )

    body = RunCompletedRequest(
        workflow_run_id=1, conclusion="success",
        verification=_pass_verification(),
    )
    with patch("glimmung.app.app", app_state):
        result = await run_completed(body, project="ambience", run_id=run.id)
    assert result.decision == "advance"
    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    final, _ = found  # type: ignore[misc]
    assert final.state == RunState.PASSED
    assert final.attempts[-1].phase_outputs is None

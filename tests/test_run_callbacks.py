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

from glimmung import locks as lock_ops
from glimmung import playbooks as playbook_ops
from glimmung import reports as report_ops
from glimmung import runs as run_ops
from glimmung.app import (
    LinkExistingPrRequest,
    RunCompletedRequest,
    RunStartedRequest,
    _open_pr_primitive,
    link_existing_pr_endpoint,
    run_completed,
    run_started,
)
from glimmung.dispatch import DispatchResult
from glimmung.github_app import PRCreateNoDiff
from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    PhaseSpec,
    PlaybookCreate,
    PlaybookEntry,
    PlaybookEntryState,
    PlaybookIntegrationStrategy,
    PlaybookIssueSpec,
    PrPrimitiveSpec,
    Run,
    RunState,
    VerificationStatus,
    Workflow,
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
        playbooks=FakeContainer("playbooks", "/project"),
        reports=FakeContainer("reports", "/project"),
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


async def _register_workflow_with_pr(cosmos, project: str, name: str = "agent") -> None:
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
            "recyclePolicy": None,
        }],
        "pr": {"enabled": True, "recyclePolicy": None},
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


async def _seed_issue_for_run(
    cosmos,
    run: Run,
    title: str = "Fix the ambience picker",
    metadata: dict[str, object] | None = None,
) -> None:
    await cosmos.issues.create_item({
        "id": run.issue_id,
        "number": run.issue_number,
        "project": run.project,
        "title": title,
        "body": "",
        "state": "open",
        "labels": [],
        "metadata": metadata or {},
        "source": {"kind": "github", "repo": run.issue_repo, "number": run.issue_number},
        "created_at": datetime.now(UTC).isoformat(),
        "updated_at": datetime.now(UTC).isoformat(),
    })


def _workflow_with_pr(project: str, name: str = "agent") -> Workflow:
    return Workflow(
        id=name,
        name=name,
        project=project,
        phases=[
            PhaseSpec(
                name="agent",
                kind="gha_dispatch",
                workflow_filename="agent-run.yml",
                workflow_ref="main",
                verify=True,
            ),
        ],
        pr=PrPrimitiveSpec(enabled=True),
        budget=BudgetConfig(total=25.0),
        trigger_label="agent-run",
        created_at=datetime.now(UTC),
    )


# ─── PR primitive ─────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_pr_primitive_registers_rich_glimmung_pr_and_thin_github_body(cosmos):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    run = await _seed_run(
        cosmos,
        run_id="01KQTEST_RUN_PR1",
        project="ambience",
        issue_repo="nelsong6/ambience",
        issue_number=117,
    )
    run.attempts[-1].workflow_run_id = 25255513874
    run.validation_url = "https://issue-117.preview.example"
    await cosmos.runs.replace_item(
        item=run.id,
        body=run.model_dump(mode="json"),
        etag="1",
    )
    await cosmos.issues.create_item({
        "id": run.issue_id,
        "number": run.issue_number,
        "project": run.project,
        "title": "Fix the ambience picker",
        "body": "",
        "state": "open",
        "labels": [],
        "source": {"kind": "github", "repo": run.issue_repo, "number": run.issue_number},
        "created_at": datetime.now(UTC).isoformat(),
        "updated_at": datetime.now(UTC).isoformat(),
    })

    open_calls: list[dict[str, str]] = []
    update_calls: list[dict[str, str | int]] = []

    async def fake_open_pull_request(_minter, **kwargs):
        open_calls.append(kwargs)
        return 77, "https://github.com/nelsong6/ambience/pull/77"

    async def fake_update_pull_request_body(_minter, **kwargs):
        update_calls.append(kwargs)

    app_state = SimpleNamespace(
        state=SimpleNamespace(
            cosmos=cosmos,
            settings=SimpleNamespace(glimmung_base_url="https://glimmung.test"),
            gh_minter=object(),
        ),
    )
    with (
        patch("glimmung.app.app", app_state),
        patch("glimmung.app.open_pull_request", fake_open_pull_request),
        patch("glimmung.app.update_pull_request_body", fake_update_pull_request_body),
    ):
        await _open_pr_primitive(run=run, workflow=_workflow_with_pr("ambience"))

    assert open_calls == [{
        "repo": "nelsong6/ambience",
        "head": "glimmung/01KQTEST_RUN_PR1",
        "base": "main",
        "title": "Fix the ambience picker",
        "body": (
            "Closes nelsong6/ambience#117\n\n"
            "Canonical context is being prepared in Glimmung."
        ),
    }]
    assert update_calls == [{
        "repo": "nelsong6/ambience",
        "number": 77,
        "body": (
            "Closes nelsong6/ambience#117\n\n"
            "Canonical context: https://glimmung.test/touchpoints/nelsong6/ambience/77"
        ),
    }]

    found_pr = await report_ops.find_report_by_repo_number(
        cosmos,
        repo="nelsong6/ambience",
        number=77,
    )
    assert found_pr is not None
    pr, _ = found_pr
    assert pr.title == "Fix the ambience picker"
    assert pr.body != open_calls[0]["body"]
    assert "## Preview" in pr.body
    assert "https://issue-117.preview.example/_styleguide" in pr.body
    assert pr.linked_issue_id == run.issue_id
    assert pr.linked_run_id == run.id
    assert pr.html_url == "https://github.com/nelsong6/ambience/pull/77"

    found_run = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    assert found_run is not None
    updated_run, _ = found_run
    assert updated_run.pr_number == 77
    assert updated_run.pr_branch == "glimmung/01KQTEST_RUN_PR1"


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
async def test_completed_advances_owning_playbook(cosmos, app_state, monkeypatch):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_recycle(cosmos, "ambience")
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_PLAYBOOK", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=144,
    )
    playbook = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="ambience",
            title="shared feature",
            entries=[
                PlaybookEntry(
                    id="one",
                    issue=PlaybookIssueSpec(title="first", body="first"),
                ),
                PlaybookEntry(
                    id="two",
                    issue=PlaybookIssueSpec(title="second", body="second"),
                ),
            ],
            concurrency_limit=1,
            integration_strategy=PlaybookIntegrationStrategy.SHARED_FEATURE_BRANCH,
        ),
    )
    context = {
        "id": f"playbook:{playbook.id}:shared",
        "strategy": "shared_feature_branch",
        "branch": f"glimmung/playbooks/{playbook.id[:8]}",
        "base_ref": "main",
        "owner_playbook_id": playbook.id,
        "current_entry_id": "one",
        "state": "in_use",
    }
    holder_id = f"playbook:{playbook.id}:entry:one"
    await lock_ops.claim_lock(
        cosmos,
        scope="work_context",
        key=context["id"],
        holder_id=holder_id,
        ttl_seconds=14400,
        metadata={},
    )
    playbook.entries[0].state = PlaybookEntryState.RUNNING
    playbook.entries[0].created_issue_id = run.issue_id
    playbook.entries[0].run_id = run.id
    playbook.entries[0].metadata = {
        "work_context": context,
        "work_context_lock_state": "held",
        "work_context_lock_holder_id": holder_id,
    }
    found_playbook = await playbook_ops.read_playbook(
        cosmos,
        project="ambience",
        playbook_id=playbook.id,
    )
    assert found_playbook is not None
    _, playbook_etag = found_playbook
    await playbook_ops.replace_playbook(cosmos, playbook=playbook, etag=playbook_etag)
    await _seed_issue_for_run(
        cosmos,
        run,
        metadata={
            "playbook_id": playbook.id,
            "playbook_entry_id": "one",
        },
    )
    dispatches: list[str] = []

    async def fake_dispatch_run(app, **kwargs):
        dispatches.append(kwargs["issue_id"])
        return DispatchResult(state="pending", run_id="run-two")

    monkeypatch.setattr("glimmung.app.dispatch_run", fake_dispatch_run)

    body = RunCompletedRequest(
        workflow_run_id=25255513874,
        conclusion="success",
        verification={
            "schema_version": 1,
            "status": "pass",
            "reasons": [],
            "evidence_refs": [],
            "cost_usd": 0.42,
        },
    )
    with patch("glimmung.app.app", app_state):
        result = await run_completed(
            body, project="ambience", run_id=run.id,
        )

    assert result.decision == "advance"
    found = await playbook_ops.read_playbook(
        cosmos,
        project="ambience",
        playbook_id=playbook.id,
    )
    assert found is not None
    advanced, _ = found
    assert advanced.entries[0].state == PlaybookEntryState.SUCCEEDED
    assert advanced.entries[0].metadata["work_context_lock_state"] == "released"
    assert advanced.entries[1].state == PlaybookEntryState.RUNNING
    assert advanced.entries[1].run_id == "run-two"
    assert len(dispatches) == 1


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
async def test_completed_releases_native_issue_lock_on_terminal(cosmos, app_state):
    from glimmung import locks as lock_ops
    await _register_project(cosmos, "glimmung", "nelsong6/glimmung")
    await _register_workflow_with_recycle(cosmos, "glimmung")

    holder = "01HZZZHOLDER000000000000"
    run = await _seed_run(
        cosmos, run_id="01KQTEST_RUN_NATIVE", project="glimmung",
        issue_repo="nelsong6/glimmung", issue_number=0,
        issue_lock_holder_id=holder,
    )
    await lock_ops.claim_lock(
        cosmos, scope="issue", key=f"glimmung/{run.issue_id}",
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
            body, project="glimmung", run_id=run.id,
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


# ─── /completed: multi-phase forward dispatch (#101) ──────────────────────


async def _register_2phase_workflow(cosmos, project: str, name: str = "agent-run") -> None:
    """Two non-verify phases plumbed via inputs/outputs. env-prep emits
    validation_url + image_tag; agent-execute consumes both via
    `${{ phases.env-prep.outputs.X }}`. Mirrors the pilot (#102) shape."""
    await cosmos.workflows.create_item({
        "id": name,
        "name": name,
        "project": project,
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
                "outputs": ["validation_url", "image_tag"],
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
                    "image_tag": "${{ phases.env-prep.outputs.image_tag }}",
                },
                "outputs": [],
            },
        ],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": "agent-run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


async def _register_2phase_native_retry_workflow(
    cosmos,
    project: str,
    name: str = "agent-run",
) -> None:
    await cosmos.workflows.create_item({
        "id": name,
        "name": name,
        "project": project,
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
                "outputs": ["validation_url", "namespace", "image_tag"],
            },
            {
                "name": "agent-execute",
                "kind": "k8s_job",
                "workflowFilename": "",
                "workflowRef": "main",
                "requirements": None,
                "verify": True,
                "recyclePolicy": {
                    "maxAttempts": 3,
                    "on": ["verify_fail"],
                    "landsAt": "self",
                },
                "inputs": {
                    "validation_url": "${{ phases.env-prep.outputs.validation_url }}",
                    "namespace": "${{ phases.env-prep.outputs.namespace }}",
                    "image_tag": "${{ phases.env-prep.outputs.image_tag }}",
                },
                "outputs": [],
                "jobs": [{
                    "id": "agent",
                    "name": None,
                    "image": "runner:latest",
                    "command": [],
                    "args": [],
                    "env": {},
                    "steps": [
                        {"slug": "clone-repo", "title": None},
                        {"slug": "run-agent", "title": None},
                    ],
                    "timeoutSeconds": None,
                }],
            },
        ],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": "agent-run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


async def _seed_run_for_phase(
    cosmos,
    *,
    run_id: str,
    project: str,
    issue_repo: str,
    issue_number: int,
    phase: str,
    workflow_filename: str,
    workflow: str = "agent-run",
) -> Run:
    """Variant of `_seed_run` that lets the phase + workflow_filename
    be set so multi-phase tests can seed a run mid-flight on phase 1."""
    run = Run(
        id=run_id,
        project=project,
        workflow=workflow,
        issue_id="01HZZZTESTISSUE",
        issue_repo=issue_repo,
        issue_number=issue_number,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[PhaseAttempt(
            attempt_index=0,
            phase=phase,
            workflow_filename=workflow_filename,
            dispatched_at=datetime.now(UTC),
        )],
        cumulative_cost_usd=0.0,
        issue_lock_holder_id="01HOLDER",
        created_at=datetime.now(UTC),
        updated_at=datetime.now(UTC),
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))
    return run


@pytest.fixture
def app_state_with_settings(cosmos):
    """Variant of `app_state` that wires settings and gh_minter=None.
    Forward-dispatch tests need lease_ops.acquire to read TTL config;
    the no-minter path is what the test fixture relies on to stop short
    of an actual workflow_dispatch."""
    state = SimpleNamespace(
        cosmos=cosmos,
        settings=SimpleNamespace(
            lease_default_ttl_seconds=14400,
            sweep_interval_seconds=60,
        ),
        gh_minter=None,
    )
    return SimpleNamespace(state=state)


@pytest.mark.asyncio
async def test_completed_dispatches_next_phase_on_advance(cosmos, app_state_with_settings):
    """Phase 1 (env-prep) completes successfully with declared outputs.
    Glimmung's /completed handler dispatches phase 2 (agent-execute)
    instead of going terminal: a new PhaseAttempt is appended for the
    next phase, the run stays IN_PROGRESS, and the new lease's metadata
    carries the substituted phase inputs for the dispatch."""
    from glimmung.db import query_all
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_2phase_workflow(cosmos, "ambience")
    run = await _seed_run_for_phase(
        cosmos, run_id="01KQTEST_RUN_FW1", project="ambience",
        issue_repo="nelsong6/ambience", issue_number=200,
        phase="env-prep", workflow_filename="env-prep.yml",
    )

    body = RunCompletedRequest(
        workflow_run_id=999,
        conclusion="success",
        outputs={
            "validation_url": "https://issue-200-999-abc.glimmung.dev.romaine.life",
            "image_tag": "issue-200-999-abc",
        },
    )
    with patch("glimmung.app.app", app_state_with_settings):
        result = await run_completed(body, project="ambience", run_id=run.id)
    # Non-terminal — run continues into phase 2.
    assert result.decision == "advance_phase"

    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    final, _ = found  # type: ignore[misc]
    assert final.state == RunState.IN_PROGRESS
    assert len(final.attempts) == 2
    # Phase 1's outputs persisted on the prior attempt.
    assert final.attempts[0].phase_outputs == {
        "validation_url": "https://issue-200-999-abc.glimmung.dev.romaine.life",
        "image_tag": "issue-200-999-abc",
    }
    # Phase 2's attempt is queued (not yet completed).
    assert final.attempts[1].phase == "agent-execute"
    assert final.attempts[1].workflow_filename == "agent-execute.yml"
    assert final.attempts[1].completed_at is None

    # New lease for phase 2 with the substituted inputs in metadata.
    leases = await query_all(
        cosmos.leases,
        "SELECT * FROM c WHERE c.project = @p",
        parameters=[{"name": "@p", "value": "ambience"}],
    )
    phase2_leases = [
        lease for lease in leases
        if (lease.get("metadata") or {}).get("phase_name") == "agent-execute"
    ]
    assert len(phase2_leases) == 1
    md = phase2_leases[0]["metadata"]
    assert md["phase_inputs"] == {
        "validation_url": "https://issue-200-999-abc.glimmung.dev.romaine.life",
        "image_tag": "issue-200-999-abc",
    }
    assert md["run_id"] == run.id
    assert md["attempt_index"] == "1"


@pytest.mark.asyncio
async def test_retry_dispatch_carries_phase_inputs_to_native_phase(
    cosmos,
    app_state_with_settings,
):
    from glimmung.db import query_all
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_2phase_native_retry_workflow(cosmos, "ambience")
    now = datetime.now(UTC)
    run = Run(
        id="01KQTEST_RUN_RETRY_NATIVE",
        project="ambience",
        workflow="agent-run",
        issue_id="01HZZZTESTISSUE",
        issue_repo="nelsong6/ambience",
        issue_number=202,
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
                    "validation_url": "https://preview.example",
                    "namespace": "glim-run-01-test-0",
                    "image_tag": "glim-run-01-test-0",
                },
            ),
            PhaseAttempt(
                attempt_index=1,
                phase="agent-execute",
                phase_kind="k8s_job",
                workflow_filename="k8s_job:agent-execute",
                dispatched_at=now,
            ),
        ],
        cumulative_cost_usd=0.0,
        issue_lock_holder_id="01HOLDER",
        created_at=now,
        updated_at=now,
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))

    body = RunCompletedRequest(
        workflow_run_id=123,
        conclusion="success",
        verification={
            "schema_version": 1,
            "status": VerificationStatus.FAIL.value,
            "reasons": ["screenshot_capture_failed"],
            "evidence_refs": [],
            "cost_usd": 0.01,
        },
    )
    with patch("glimmung.app.app", app_state_with_settings):
        result = await run_completed(body, project="ambience", run_id=run.id)
    assert result.decision == "retry"

    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    final, _ = found  # type: ignore[misc]
    assert final.state == RunState.ABORTED
    assert final.abort_reason is not None
    assert final.abort_reason.startswith("recycled into run ")
    assert len(final.attempts) == 2

    runs = await query_all(
        cosmos.runs,
        "SELECT * FROM c WHERE c.project = @p",
        parameters=[{"name": "@p", "value": "ambience"}],
    )
    child_docs = [
        doc for doc in runs
        if doc.get("cloned_from_run_id") == run.id
    ]
    assert len(child_docs) == 1
    child = Run.model_validate(child_docs[0])
    assert child.state == RunState.IN_PROGRESS
    assert child.issue_lock_holder_id == "01HOLDER"
    assert child.entrypoint_phase == "agent-execute"
    assert child.trigger_source == {
        "kind": "native_recycle",
        "cloned_from_run_id": run.id,
        "phase_name": "agent-execute",
        "target_phase": "agent-execute",
        "decision": "retry",
    }
    assert len(child.attempts) == 2
    assert child.attempts[0].phase == "env-prep"
    assert child.attempts[0].skipped_from_run_id == run.id
    assert child.attempts[0].phase_outputs == {
        "validation_url": "https://preview.example",
        "namespace": "glim-run-01-test-0",
        "image_tag": "glim-run-01-test-0",
    }
    assert child.attempts[1].phase == "agent-execute"
    assert child.attempts[1].phase_kind == "k8s_job"

    leases = await query_all(
        cosmos.leases,
        "SELECT * FROM c WHERE c.project = @p",
        parameters=[{"name": "@p", "value": "ambience"}],
    )
    retry_leases = [
        lease for lease in leases
        if (lease.get("metadata") or {}).get("run_id") == child.id
    ]
    assert len(retry_leases) == 1
    assert retry_leases[0]["metadata"]["attempt_index"] == "1"
    assert retry_leases[0]["metadata"]["phase_inputs"] == {
        "validation_url": "https://preview.example",
        "namespace": "glim-run-01-test-0",
        "image_tag": "glim-run-01-test-0",
    }


@pytest.mark.asyncio
async def test_completed_marks_passed_after_last_phase(cosmos, app_state_with_settings):
    """After agent-execute (phase 2, the terminal phase) completes
    successfully, the run goes terminal — same flow as today's
    single-phase ADVANCE."""
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_2phase_workflow(cosmos, "ambience")
    # Seed a run that's already past phase 1; phase 2 is in flight.
    run_doc = Run(
        id="01KQTEST_RUN_FW2",
        project="ambience",
        workflow="agent-run",
        issue_id="01HZZZTESTISSUE",
        issue_repo="nelsong6/ambience",
        issue_number=201,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0, phase="env-prep",
                workflow_filename="env-prep.yml",
                dispatched_at=datetime.now(UTC),
                completed_at=datetime.now(UTC),
                conclusion="success",
                phase_outputs={"validation_url": "https://x", "image_tag": "t"},
            ),
            PhaseAttempt(
                attempt_index=1, phase="agent-execute",
                workflow_filename="agent-execute.yml",
                dispatched_at=datetime.now(UTC),
            ),
        ],
        cumulative_cost_usd=0.0,
        issue_lock_holder_id="01HOLDER",
        created_at=datetime.now(UTC),
        updated_at=datetime.now(UTC),
    )
    await cosmos.runs.create_item(run_doc.model_dump(mode="json"))

    body = RunCompletedRequest(
        workflow_run_id=1000,
        conclusion="success",
    )
    with patch("glimmung.app.app", app_state_with_settings):
        result = await run_completed(body, project="ambience", run_id=run_doc.id)
    assert result.decision == "advance"

    found = await run_ops.read_run(cosmos, project="ambience", run_id=run_doc.id)
    final, _ = found  # type: ignore[misc]
    assert final.state == RunState.PASSED
    assert len(final.attempts) == 2  # No new attempt — last phase is terminal.


@pytest.mark.asyncio
async def test_completed_pr_primitive_no_diff_marks_review_required(cosmos):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_pr(cosmos, "ambience")
    run = await _seed_run(
        cosmos,
        run_id="01KQTEST_RUN_NODIFF",
        project="ambience",
        issue_repo="nelsong6/ambience",
        issue_number=218,
    )
    await _seed_issue_for_run(cosmos, run)

    async def fake_open_pull_request(_minter, **_kwargs):
        raise PRCreateNoDiff("No commits between main and glimmung/01KQTEST_RUN_NODIFF")

    app_state = SimpleNamespace(
        state=SimpleNamespace(
            cosmos=cosmos,
            settings=SimpleNamespace(glimmung_base_url="https://glimmung.test"),
            gh_minter=object(),
        ),
    )
    body = RunCompletedRequest(
        workflow_run_id=1000,
        conclusion="success",
        verification=_pass_verification(),
    )
    with (
        patch("glimmung.app.app", app_state),
        patch("glimmung.app.open_pull_request", fake_open_pull_request),
    ):
        result = await run_completed(body, project="ambience", run_id=run.id)

    assert result.decision == "review_required_no_diff"
    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    final, _ = found  # type: ignore[misc]
    assert final.state == RunState.REVIEW_REQUIRED
    assert final.abort_reason == (
        "PR primitive: no diff between glimmung/01KQTEST_RUN_NODIFF and base"
    )


@pytest.mark.asyncio
async def test_completed_pr_primitive_failure_records_actionable_reason(cosmos):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    await _register_workflow_with_pr(cosmos, "ambience")
    run = await _seed_run(
        cosmos,
        run_id="01KQTEST_RUN_PRFAIL",
        project="ambience",
        issue_repo="nelsong6/ambience",
        issue_number=219,
    )
    await _seed_issue_for_run(cosmos, run)

    async def fake_open_pull_request(_minter, **_kwargs):
        raise RuntimeError("422 Validation Failed: head branch was not found")

    app_state = SimpleNamespace(
        state=SimpleNamespace(
            cosmos=cosmos,
            settings=SimpleNamespace(glimmung_base_url="https://glimmung.test"),
            gh_minter=object(),
        ),
    )
    body = RunCompletedRequest(
        workflow_run_id=1001,
        conclusion="success",
        verification=_pass_verification(),
    )
    with (
        patch("glimmung.app.app", app_state),
        patch("glimmung.app.open_pull_request", fake_open_pull_request),
    ):
        result = await run_completed(body, project="ambience", run_id=run.id)

    assert result.decision == "abort_pr_create_failed"
    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    final, _ = found  # type: ignore[misc]
    assert final.state == RunState.ABORTED
    assert final.abort_reason == (
        "PR primitive: gh pr create failed: "
        "422 Validation Failed: head branch was not found"
    )


@pytest.mark.asyncio
async def test_link_existing_pr_endpoint_repairs_run_and_touchpoint(cosmos):
    run = await _seed_run(
        cosmos,
        run_id="01KQTEST_RUN_REPAIR",
        project="ambience",
        issue_repo="nelsong6/ambience",
        issue_number=220,
    )
    await _seed_issue_for_run(cosmos, run, title="Repair PR primitive")
    app_state = SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))

    with patch("glimmung.app.app", app_state):
        result = await link_existing_pr_endpoint(
            LinkExistingPrRequest(number=88),
            project="ambience",
            run_id=run.id,
        )

    assert result.state == "linked"
    assert result.repo == "nelsong6/ambience"
    assert result.pr_number == 88

    found = await run_ops.read_run(cosmos, project="ambience", run_id=run.id)
    repaired, _ = found  # type: ignore[misc]
    assert repaired.pr_number == 88
    assert repaired.pr_branch == f"glimmung/{run.id}"

    found_pr = await report_ops.find_report_by_repo_number(
        cosmos, repo="nelsong6/ambience", number=88,
    )
    assert found_pr is not None
    pr, _ = found_pr
    assert pr.linked_run_id == run.id
    assert pr.linked_issue_id == run.issue_id
    assert pr.title == "Repair PR primitive"


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

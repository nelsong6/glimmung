"""`dispatch_run` rollback-on-failure tests.

Companion to `test_dispatch_inputs_filter.py`: that one prevents the
specific 422-on-undeclared-input failure mode at the `_maybe_dispatch_workflow`
level. This one is the catch-all — for *any* dispatch failure
(`dispatch_workflow` raising), the lease + issue lock are released and
no Run record is created. Eliminates the orphan Run shape (`state=in_progress`
+ `attempts[0].workflow_run_id=null`) that motivated `_abort_run`.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

import pytest

from glimmung import issues as issue_ops
from glimmung.dispatch import dispatch_run
from glimmung.models import IssueSource, LeaseState, RunState

from tests.cosmos_fake import FakeContainer


def _cosmos() -> SimpleNamespace:
    return SimpleNamespace(
        projects=FakeContainer("projects", "/name"),
        workflows=FakeContainer("workflows", "/project"),
        hosts=FakeContainer("hosts", "/name"),
        leases=FakeContainer("leases", "/project"),
        runs=FakeContainer("runs", "/project"),
        locks=FakeContainer("locks", "/scope"),
        issues=FakeContainer("issues", "/project"),
        reports=FakeContainer("reports", "/project"),
    )


def _settings() -> SimpleNamespace:
    return SimpleNamespace(
        lease_default_ttl_seconds=14400,
        sweep_interval_seconds=60,
    )


class _StubMinter:
    """Sentinel non-None gh_minter so `_maybe_dispatch_workflow` reaches
    the dispatch_workflow call (where we can monkeypatch failure)."""
    pass


@pytest.fixture
def app():
    state = SimpleNamespace(
        cosmos=_cosmos(),
        settings=_settings(),
        gh_minter=_StubMinter(),
    )
    return SimpleNamespace(state=state)


async def _register_project(app, name: str, repo: str) -> None:
    await app.state.cosmos.projects.create_item({
        "id": name, "name": name, "githubRepo": repo,
        "metadata": {}, "createdAt": datetime.now(UTC).isoformat(),
    })


async def _register_workflow(app, *, project: str, name: str,
                              retry_workflow_filename: str = "") -> None:
    """retry_workflow_filename kwarg preserved for legacy test signatures —
    truthy = phase opts into verify+recycle, falsy = phase is non-verify."""
    has_recycle = bool(retry_workflow_filename)
    await app.state.cosmos.workflows.create_item({
        "id": name, "name": name, "project": project,
        "phases": [{
            "name": "agent",
            "kind": "gha_dispatch",
            "workflowFilename": "agent-run.yml",
            "workflowRef": "main",
            "requirements": None,
            "verify": has_recycle,
            "recyclePolicy": (
                {"maxAttempts": 3, "on": ["verify_fail"], "landsAt": "self"}
                if has_recycle else None
            ),
        }],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": "agent:run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


async def _register_host(app, name: str) -> None:
    await app.state.cosmos.hosts.create_item({
        "id": name, "name": name, "capabilities": {},
        "drained": False, "createdAt": datetime.now(UTC).isoformat(),
    })


async def _register_issue(app, *, project: str, repo: str, number: int) -> str:
    issue = await issue_ops.create_issue(
        app.state.cosmos,
        project=project,
        title=f"{repo}#{number}",
        body="",
        labels=[],
        source=IssueSource.MANUAL,
        github_issue_url=issue_ops.github_issue_url_for(repo, number),
        github_issue_repo=repo,
        github_issue_number=number,
    )
    return issue.id


# ─── rollback on dispatch failure ────────────────────────────────────


@pytest.mark.asyncio
async def test_dispatch_failure_rolls_back_lease_lock_and_aborts_run(
    app, monkeypatch,
):
    """Simulates the orphan-producing path: GH `workflow_dispatch` raises
    (422 on undeclared input is the realistic case). Under #69 the Run is
    pre-created (so run_id can flow into workflow_dispatch inputs); on
    dispatch failure the Run is marked ABORTED rather than skipped — the
    orphan-state symptom #20 was preventing is still avoided because the
    Run doesn't sit in IN_PROGRESS forever."""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="agent-run",
        retry_workflow_filename="agent-run.yml",  # opt into Run substrate
    )
    await _register_host(app, "ambience-slot-1")
    await _register_issue(
        app, project="ambience", repo="nelsong6/ambience", number=124,
    )

    async def fake_dispatch(*args, **kwargs):
        raise RuntimeError("422 Unexpected inputs provided: foo, bar")
    monkeypatch.setattr("glimmung.app.dispatch_workflow", fake_dispatch)

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=124,
        trigger_source={"kind": "glimmung_ui"},
    )

    assert result.state == "dispatch_failed"
    assert result.lease_id is not None  # lease was claimed before backout
    assert result.run_id is not None    # Run was pre-created

    # Lease released.
    lease_doc = await app.state.cosmos.leases.read_item(
        item=result.lease_id, partition_key="ambience",
    )
    assert lease_doc["state"] == LeaseState.RELEASED.value

    # Issue lock released.
    lock_doc = await app.state.cosmos.locks.read_item(
        item="issue::ambience%23124", partition_key="issue",
    )
    assert lock_doc["state"] == "released"

    # Run document exists but is ABORTED — not orphaned IN_PROGRESS.
    runs = [d async for d in app.state.cosmos.runs.query_items(
        "SELECT * FROM c WHERE c.issue_number = @n",
        parameters=[{"name": "@n", "value": 124}],
    )]
    assert len(runs) == 1
    assert runs[0]["id"] == result.run_id
    assert runs[0]["state"] == "aborted"
    assert "dispatch_failed" in (runs[0].get("abort_reason") or "")


@pytest.mark.asyncio
async def test_dispatch_failure_idempotent_under_retry(app, monkeypatch):
    """A second dispatch after a failed first should succeed cleanly —
    the lock-released state from the rollback means the second call
    isn't blocked by a stale lock."""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="agent-run",
        retry_workflow_filename="agent-run.yml",
    )
    await _register_host(app, "ambience-slot-1")
    await _register_issue(
        app, project="ambience", repo="nelsong6/ambience", number=124,
    )

    call_count = {"n": 0}
    async def flaky_dispatch(*args, **kwargs):
        call_count["n"] += 1
        if call_count["n"] == 1:
            raise RuntimeError("transient")
    monkeypatch.setattr("glimmung.app.dispatch_workflow", flaky_dispatch)

    first = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=124,
        trigger_source={"kind": "glimmung_ui"},
    )
    assert first.state == "dispatch_failed"

    second = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=124,
        trigger_source={"kind": "glimmung_ui"},
    )
    assert second.state == "dispatched"
    assert second.run_id is not None
    assert second.run_id != first.run_id  # fresh run, not the aborted one

    # Two Runs persist: the aborted one from the first dispatch and the
    # in-progress one from the retry.
    runs = sorted(
        [d async for d in app.state.cosmos.runs.query_items(
            "SELECT * FROM c WHERE c.issue_number = @n",
            parameters=[{"name": "@n", "value": 124}],
        )],
        key=lambda d: d["created_at"],
    )
    assert len(runs) == 2
    assert runs[0]["state"] == "aborted"
    assert runs[0]["id"] == first.run_id
    assert runs[1]["state"] == RunState.IN_PROGRESS.value
    assert runs[1]["id"] == second.run_id


@pytest.mark.asyncio
async def test_successful_dispatch_creates_run_and_keeps_lock_held(
    app, monkeypatch,
):
    """Sanity check: the rollback path doesn't fire when dispatch
    succeeds. Run is created, lock stays HELD (released later by the
    workflow_run.completed terminal handler)."""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="agent-run",
        retry_workflow_filename="agent-run.yml",
    )
    await _register_host(app, "ambience-slot-1")
    await _register_issue(
        app, project="ambience", repo="nelsong6/ambience", number=124,
    )

    async def fake_dispatch(*args, **kwargs):
        return None  # success
    monkeypatch.setattr("glimmung.app.dispatch_workflow", fake_dispatch)

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=124,
        trigger_source={"kind": "glimmung_ui"},
    )

    assert result.state == "dispatched"
    assert result.run_id is not None

    lock_doc = await app.state.cosmos.locks.read_item(
        item="issue::ambience%23124", partition_key="issue",
    )
    assert lock_doc["state"] == "held"

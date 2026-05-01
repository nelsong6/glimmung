"""dispatch_run integration tests.

Both the GitHub label webhook and the glimmung UI route through the
same `dispatch_run` function (#20). These tests exercise the unified
path: project + workflow resolution, per-issue lock claim, lease
acquire, Run creation when the workflow opts into the verify-loop,
plus the `already_running` / `no_project` / `no_workflow` outcomes.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

import pytest

from glimmung.dispatch import dispatch_run
from glimmung.models import RunState

from tests.cosmos_fake import FakeContainer


# ─── fixtures: minimal app stand-in ──────────────────────────────────────────


def _settings() -> SimpleNamespace:
    return SimpleNamespace(
        lease_default_ttl_seconds=14400,
        sweep_interval_seconds=60,
    )


def _cosmos() -> SimpleNamespace:
    return SimpleNamespace(
        projects=FakeContainer("projects", "/name"),
        workflows=FakeContainer("workflows", "/project"),
        hosts=FakeContainer("hosts", "/name"),
        leases=FakeContainer("leases", "/project"),
        runs=FakeContainer("runs", "/project"),
        locks=FakeContainer("locks", "/scope"),
    )


@pytest.fixture
def app():
    """Stand-in for a FastAPI app object: dispatch_run only touches
    `app.state.cosmos`, `app.state.settings`, `app.state.gh_minter`.
    Setting gh_minter=None makes `_maybe_dispatch_workflow` a no-op so
    we don't have to mock GH HTTP calls."""
    state = SimpleNamespace(
        cosmos=_cosmos(),
        settings=_settings(),
        gh_minter=None,
    )
    return SimpleNamespace(state=state)


async def _register_project(app, name: str, repo: str) -> None:
    await app.state.cosmos.projects.create_item({
        "id": name,
        "name": name,
        "githubRepo": repo,
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


async def _register_workflow(
    app,
    *,
    project: str,
    name: str,
    workflow_filename: str = "issue-agent.yml",
    trigger_label: str = "agent-run",
    retry_workflow_filename: str = "",
    requirements: dict | None = None,
) -> None:
    await app.state.cosmos.workflows.create_item({
        "id": name,
        "name": name,
        "project": project,
        "workflowFilename": workflow_filename,
        "workflowRef": "main",
        "triggerLabel": trigger_label,
        "defaultRequirements": requirements or {},
        "retryWorkflowFilename": retry_workflow_filename,
        "defaultBudget": None,
        "createdAt": datetime.now(UTC).isoformat(),
    })


async def _register_host(app, name: str, capabilities: dict | None = None) -> None:
    await app.state.cosmos.hosts.create_item({
        "id": name,
        "name": name,
        "capabilities": capabilities or {},
        "drained": False,
        "createdAt": datetime.now(UTC).isoformat(),
    })


# ─── happy paths ─────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_dispatch_creates_lock_lease_and_run_when_workflow_opts_in(app):
    """Verify-loop-tracked workflow: dispatch_run claims the issue lock,
    acquires a lease (host gets assigned), and creates a Run."""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
    )

    assert result.state == "dispatched"
    assert result.host == "runner-1"
    assert result.lease_id is not None
    assert result.run_id is not None
    assert result.issue_lock_holder_id is not None

    # Lock is held
    lock_doc = await app.state.cosmos.locks.read_item(
        item="issue::nelsong6%2Fambience%2342", partition_key="issue",
    )
    assert lock_doc["state"] == "held"
    assert lock_doc["held_by"] == result.issue_lock_holder_id

    # Run is in_progress with the holder id stamped on it
    run_docs = [d async for d in app.state.cosmos.runs.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": result.run_id}],
    )]
    assert len(run_docs) == 1
    assert run_docs[0]["state"] == RunState.IN_PROGRESS.value
    assert run_docs[0]["issue_lock_holder_id"] == result.issue_lock_holder_id
    assert run_docs[0]["trigger_source"] == {"kind": "glimmung_ui"}


@pytest.mark.asyncio
async def test_dispatch_skips_run_creation_for_non_verify_loop_workflow(app):
    """Workflow with no retry_workflow_filename: lease is acquired, but
    no Run is created. The lock is still claimed (so two webhook
    deliveries on the same issue don't double-dispatch)."""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(app, project="ambience", name="issue-agent")
    await _register_host(app, "runner-1")

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=7,
        trigger_source={"kind": "label_webhook"},
    )

    assert result.state == "dispatched"
    assert result.run_id is None
    assert result.lease_id is not None
    assert result.issue_lock_holder_id is not None

    # Lock held; no Run created.
    lock = await app.state.cosmos.locks.read_item(
        item="issue::nelsong6%2Fambience%237", partition_key="issue",
    )
    assert lock["held_by"] == result.issue_lock_holder_id


@pytest.mark.asyncio
async def test_dispatch_returns_pending_when_no_host(app):
    """No registered hosts → lease is created in PENDING state, no
    workflow_dispatch fires, but the lock and Run are still created
    (dispatch_run is the source of truth for "this issue's run started")."""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    # No host registered.

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=99,
        trigger_source={"kind": "glimmung_ui"},
    )

    assert result.state == "pending"
    assert result.host is None
    assert result.lease_id is not None
    assert result.run_id is not None  # Run is still created on PENDING


# ─── per-issue serialization ─────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_concurrent_dispatch_on_same_issue_returns_already_running(app):
    """Second dispatch on same issue while first is in flight → second
    sees `already_running` without acquiring a lease or firing
    workflow_dispatch. This is the bug the lock primitive fixes (today's
    label-trigger path can double-dispatch on re-label)."""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")

    first = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
    )
    second = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "label_webhook", "label": "agent-run"},
    )

    assert first.state == "dispatched"
    assert second.state == "already_running"
    assert second.lease_id is None
    assert second.run_id is None
    assert "already" in (second.detail or "").lower() or "held" in (second.detail or "").lower()

    # Only one Run exists.
    runs = [d async for d in app.state.cosmos.runs.query_items(
        "SELECT * FROM c WHERE c.issue_number = @n",
        parameters=[{"name": "@n", "value": 42}],
    )]
    assert len(runs) == 1
    assert runs[0]["id"] == first.run_id


@pytest.mark.asyncio
async def test_dispatch_succeeds_after_lock_release(app):
    """After a Run terminates and releases the issue lock, a new dispatch
    on the same issue starts fresh. (Lease release is independent;
    second host is registered so capacity isn't the bottleneck.)"""
    from glimmung import leases as lease_ops
    from glimmung import locks as lock_ops

    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")
    await _register_host(app, "runner-2")

    first = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
    )
    assert first.state == "dispatched"

    # Simulate Run completion: release the lease + the issue lock.
    await lease_ops.release(app.state.cosmos, first.lease_id, "ambience")
    await lock_ops.release_lock(
        app.state.cosmos, scope="issue", key="nelsong6/ambience#42",
        holder_id=first.issue_lock_holder_id,
    )

    second = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
    )
    assert second.state == "dispatched"
    assert second.run_id != first.run_id


@pytest.mark.asyncio
async def test_dispatch_on_different_issues_does_not_serialize(app):
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")
    await _register_host(app, "runner-2")

    a = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=1,
        trigger_source={"kind": "glimmung_ui"},
    )
    b = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=2,
        trigger_source={"kind": "glimmung_ui"},
    )

    assert a.state == "dispatched"
    assert b.state == "dispatched"
    assert a.run_id != b.run_id
    assert a.issue_lock_holder_id != b.issue_lock_holder_id


# ─── resolution failures ─────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_dispatch_returns_no_project_for_unregistered_repo(app):
    result = await dispatch_run(
        app, repo="someone-else/private-repo", issue_number=1,
        trigger_source={"kind": "glimmung_ui"},
    )
    assert result.state == "no_project"
    assert "private-repo" in (result.detail or "")


@pytest.mark.asyncio
async def test_dispatch_returns_no_workflow_when_project_has_none(app):
    await _register_project(app, "ambience", "nelsong6/ambience")
    # No workflows registered.

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=1,
        trigger_source={"kind": "glimmung_ui"},
    )
    assert result.state == "no_workflow"
    assert "no workflows" in (result.detail or "").lower()


@pytest.mark.asyncio
async def test_dispatch_returns_no_workflow_when_ambiguous(app):
    """Two workflows registered + no explicit pick → caller has to
    disambiguate. (The webhook path always provides an explicit
    workflow_name from its label match; the UI path defaults to the
    only-one-workflow case.)"""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(app, project="ambience", name="issue-agent")
    await _register_workflow(app, project="ambience", name="other-agent")

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=1,
        trigger_source={"kind": "glimmung_ui"},
    )
    assert result.state == "no_workflow"
    assert "issue-agent" in (result.detail or "")
    assert "other-agent" in (result.detail or "")


@pytest.mark.asyncio
async def test_dispatch_with_explicit_workflow_disambiguates(app):
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_workflow(app, project="ambience", name="other-agent")
    await _register_host(app, "runner-1")

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=1,
        trigger_source={"kind": "glimmung_ui"},
        workflow_name="issue-agent",
    )
    assert result.state == "dispatched"
    assert result.workflow == "issue-agent"


@pytest.mark.asyncio
async def test_dispatch_with_unknown_workflow_returns_no_workflow(app):
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(app, project="ambience", name="issue-agent")

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=1,
        trigger_source={"kind": "glimmung_ui"},
        workflow_name="not-a-real-workflow",
    )
    assert result.state == "no_workflow"


# ─── budget resolution from labels ───────────────────────────────────────────


@pytest.mark.asyncio
async def test_dispatch_honors_agent_budget_label_at_run_creation(app):
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
        issue_labels=["bug", "agent-budget:5x50"],
    )
    assert result.state == "dispatched"

    runs = [d async for d in app.state.cosmos.runs.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": result.run_id}],
    )]
    assert len(runs) == 1
    assert runs[0]["budget"] == {"max_attempts": 5, "max_cost_usd": 50.0}


@pytest.mark.asyncio
async def test_dispatch_uses_workflow_default_budget_when_no_label(app):
    await app.state.cosmos.projects.create_item({
        "id": "ambience",
        "name": "ambience",
        "githubRepo": "nelsong6/ambience",
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })
    await app.state.cosmos.workflows.create_item({
        "id": "issue-agent",
        "name": "issue-agent",
        "project": "ambience",
        "workflowFilename": "issue-agent.yml",
        "workflowRef": "main",
        "triggerLabel": "agent-run",
        "defaultRequirements": {},
        "retryWorkflowFilename": "agent-retry.yml",
        "defaultBudget": {"max_attempts": 7, "max_cost_usd": 100.0},
        "createdAt": datetime.now(UTC).isoformat(),
    })
    await _register_host(app, "runner-1")

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
        issue_labels=["bug"],
    )
    runs = [d async for d in app.state.cosmos.runs.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": result.run_id}],
    )]
    assert runs[0]["budget"] == {"max_attempts": 7, "max_cost_usd": 100.0}

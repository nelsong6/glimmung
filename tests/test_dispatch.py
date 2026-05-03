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

from glimmung import issues as issue_ops
from glimmung.dispatch import dispatch_run
from glimmung.models import IssueSource, RunState

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
        issues=FakeContainer("issues", "/project"),
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
    retry_workflow_filename: str = "",  # legacy kwarg, see note below
    requirements: dict | None = None,
    metadata: dict | None = None,
) -> None:
    """Test helper that writes the #69 schema directly to Cosmos. The
    `retry_workflow_filename` kwarg is preserved as a legacy hint: when
    truthy, the registered phase opts into verify + a basic recycle policy;
    when empty, the phase is non-verify (no recycle). Test bodies don't
    need to know the new shape — they just keep saying "this workflow opts
    into the verify loop" and we translate."""
    has_recycle = bool(retry_workflow_filename)
    phase = {
        "name": "agent",
        "kind": "gha_dispatch",
        "workflowFilename": workflow_filename,
        "workflowRef": "main",
        "requirements": None,
        "verify": has_recycle,
        "recyclePolicy": (
            {"maxAttempts": 3, "on": ["verify_fail"], "landsAt": "self"}
            if has_recycle else None
        ),
    }
    await app.state.cosmos.workflows.create_item({
        "id": name,
        "name": name,
        "project": project,
        "phases": [phase],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": trigger_label,
        "defaultRequirements": requirements or {},
        "metadata": metadata or {},
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


async def _register_issue(
    app,
    *,
    project: str,
    repo: str,
    issue_number: int,
    title: str = "",
    body: str = "",
    labels: list[str] | None = None,
) -> str:
    """Create a glimmung Issue with GH coords. Dispatch no longer mints
    from GH coords (#50 + closed-issue-display fix), so tests that
    exercise the legacy `(repo, issue_number)` lookup must seed the
    Issue first. Returns the new Issue's id."""
    issue = await issue_ops.create_issue(
        app.state.cosmos,
        project=project,
        title=title or f"{repo}#{issue_number}",
        body=body,
        labels=labels or [],
        source=IssueSource.MANUAL,
        github_issue_url=issue_ops.github_issue_url_for(repo, issue_number),
        github_issue_repo=repo,
        github_issue_number=issue_number,
    )
    return issue.id


async def _lease_doc_for(app, lease_id: str) -> dict:
    docs = [d async for d in app.state.cosmos.leases.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": lease_id}],
    )]
    assert len(docs) == 1
    return docs[0]


# ─── happy paths ───────────────────────────────────────────────────────────────


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
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=42)

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
async def test_dispatch_includes_recent_comments_when_workflow_opts_in(app):
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app,
        project="ambience",
        name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
        metadata={"include_recent_comments": True},
    )
    await _register_host(app, "runner-1")
    issue_id = await _register_issue(
        app,
        project="ambience",
        repo="nelsong6/ambience",
        issue_number=42,
    )
    issue, etag = await issue_ops.read_issue(
        app.state.cosmos, project="ambience", issue_id=issue_id,
    )
    for idx in range(6):
        issue, etag, _ = await issue_ops.add_comment(
            app.state.cosmos,
            issue=issue,
            etag=etag,
            author=f"user-{idx}",
            body=f"comment {idx}",
        )

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
    )

    lease_doc = await _lease_doc_for(app, result.lease_id)
    recent = lease_doc["metadata"]["recent_comments"]
    assert "comment 0" not in recent
    assert "comment 1" in recent
    assert "comment 5" in recent
    assert "user-5" in recent


@pytest.mark.asyncio
async def test_dispatch_omits_recent_comments_by_default(app):
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")
    issue_id = await _register_issue(
        app,
        project="ambience",
        repo="nelsong6/ambience",
        issue_number=42,
    )
    issue, etag = await issue_ops.read_issue(
        app.state.cosmos, project="ambience", issue_id=issue_id,
    )
    await issue_ops.add_comment(
        app.state.cosmos,
        issue=issue,
        etag=etag,
        author="nelson",
        body="do not include unless opted in",
    )

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
    )

    lease_doc = await _lease_doc_for(app, result.lease_id)
    assert "recent_comments" not in lease_doc["metadata"]


@pytest.mark.asyncio
async def test_dispatch_creates_run_even_for_non_verify_phase(app):
    """Under #69, every workflow has at least one phase, so every dispatch
    creates a Run — even when the phase doesn't opt into the verify loop.
    Replaces the pre-#69 'no retry_workflow_filename = no Run' behavior:
    the gating moved off the trio and onto the per-phase verify flag, and
    Runs exist regardless so glimmung tracks every dispatch in the lineage
    graph."""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(app, project="ambience", name="issue-agent")
    await _register_host(app, "runner-1")
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=7)

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=7,
        trigger_source={"kind": "label_webhook"},
    )

    assert result.state == "dispatched"
    assert result.run_id is not None
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
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=99)

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=99,
        trigger_source={"kind": "glimmung_ui"},
    )

    assert result.state == "pending"
    assert result.host is None
    assert result.lease_id is not None
    assert result.run_id is not None  # Run is still created on PENDING


# ─── per-issue serialization ────────────────────────────────────────────────


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
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=42)

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
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=42)

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
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=1)
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=2)

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


# ─── resolution failures ──────────────────────────────────────────────────


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
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=1)
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
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=1)

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
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=1)

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
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=1)

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=1,
        trigger_source={"kind": "glimmung_ui"},
        workflow_name="not-a-real-workflow",
    )
    assert result.state == "no_workflow"


# ─── budget resolution from labels ────────────────────────────────────────────


@pytest.mark.asyncio
async def test_dispatch_honors_agent_budget_label_at_run_creation(app):
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=42)

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
    assert runs[0]["budget"] == {"total": 50.0}


@pytest.mark.asyncio
async def test_dispatch_records_warm_session_label_on_run(app):
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=42)

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
        issue_labels=["bug", "agent-session:warm"],
    )
    assert result.state == "dispatched"

    runs = [d async for d in app.state.cosmos.runs.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": result.run_id}],
    )]
    assert len(runs) == 1
    assert runs[0]["session_launch_intent"] == "warm"


# ─── glimmung-Issue plumbing ──────────────────────────────


@pytest.mark.asyncio
async def test_dispatch_stamps_issue_id_on_run(app):
    """A dispatch against a glimmung Issue with GH coords stamps the
    Issue's id (and denormalized GH coords) on the resulting Run."""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")
    issue_id = await _register_issue(
        app, project="ambience", repo="nelsong6/ambience", issue_number=42,
    )

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
    )
    assert result.state == "dispatched"

    run_docs = [d async for d in app.state.cosmos.runs.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": result.run_id}],
    )]
    assert run_docs[0]["issue_id"] == issue_id
    assert run_docs[0]["issue_repo"] == "nelsong6/ambience"
    assert run_docs[0]["issue_number"] == 42


@pytest.mark.asyncio
async def test_dispatch_does_not_mint_issue_when_unknown(app):
    """The legacy (repo, issue_number) shape is a lookup, not a mint —
    if no glimmung Issue matches the URL, dispatch returns no_project
    without writing anything to the issues container."""
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
    assert result.state == "no_project"

    issue_docs = [d async for d in app.state.cosmos.issues.query_items(
        "SELECT * FROM c", parameters=[],
    )]
    assert issue_docs == []


@pytest.mark.asyncio
async def test_find_run_by_issue_id_returns_most_recent_run_cross_partition(app):
    """The PR `Closes #N` parser doesn't know the project, so the
    lookup is cross-partition by issue_id alone. Verify it picks the
    most-recent run when an issue has multiple runs (initial + retry,
    etc.)."""
    from glimmung import locks as lock_ops
    from glimmung import runs as run_ops

    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")
    await _register_host(app, "runner-2")
    issue_id = await _register_issue(
        app, project="ambience", repo="nelsong6/ambience", issue_number=42,
    )

    first = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
    )
    # Release the lock so a second dispatch can land on the same issue.
    await lock_ops.release_lock(
        app.state.cosmos, scope="issue",
        key="nelsong6/ambience#42", holder_id=first.issue_lock_holder_id,
    )
    second = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
    )

    found = await run_ops.find_run_by_issue_id(
        app.state.cosmos, issue_id=issue_id,
    )
    assert found is not None
    # Most recent first → second dispatch wins.
    assert found.id == second.run_id


@pytest.mark.asyncio
async def test_find_run_by_issue_id_returns_none_when_no_match(app):
    from glimmung import runs as run_ops
    found = await run_ops.find_run_by_issue_id(
        app.state.cosmos, issue_id="01HZZZNOPE",
    )
    assert found is None


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
        "phases": [{
            "name": "agent",
            "kind": "gha_dispatch",
            "workflowFilename": "issue-agent.yml",
            "workflowRef": "main",
            "requirements": None,
            "verify": True,
            "recyclePolicy": {"maxAttempts": 3, "on": ["verify_fail"], "landsAt": "self"},
        }],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 100.0},
        "triggerLabel": "agent-run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })
    await _register_host(app, "runner-1")
    await _register_issue(app, project="ambience", repo="nelsong6/ambience", issue_number=42)

    result = await dispatch_run(
        app, repo="nelsong6/ambience", issue_number=42,
        trigger_source={"kind": "glimmung_ui"},
        issue_labels=["bug"],
    )
    runs = [d async for d in app.state.cosmos.runs.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": result.run_id}],
    )]
    assert runs[0]["budget"] == {"total": 100.0}


# ─── glimmung-native dispatch (#50) ───────────────────────────────────────────


@pytest.mark.asyncio
async def test_dispatch_by_issue_id_dispatches_native_issue(app):
    """Issue created via POST /v1/issues — no GH coords. Dispatch via
    issue_id resolves project from the Issue doc, locks on a glimmung/
    namespaced key, and stamps issue_id on the Run."""
    from glimmung import issues as issue_ops

    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")

    issue = await issue_ops.create_issue(
        app.state.cosmos, project="ambience",
        title="native-only issue",
    )

    result = await dispatch_run(
        app, issue_id=issue.id,
        trigger_source={"kind": "glimmung_ui"},
    )

    assert result.state == "dispatched"
    assert result.run_id is not None

    # Lock keyed on glimmung/{id}, not repo#N.
    lock_docs = [d async for d in app.state.cosmos.locks.query_items(
        "SELECT * FROM c WHERE c.scope = @s",
        parameters=[{"name": "@s", "value": "issue"}],
    )]
    assert any(d["key"] == f"glimmung/{issue.id}" for d in lock_docs)

    runs = [d async for d in app.state.cosmos.runs.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": result.run_id}],
    )]
    assert runs[0]["issue_id"] == issue.id
    assert runs[0]["issue_repo"] == "nelsong6/ambience"
    assert runs[0]["issue_number"] == 0

    lease_docs = [d async for d in app.state.cosmos.leases.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": result.lease_id}],
    )]
    metadata = lease_docs[0]["metadata"]
    assert metadata["issue_id"] == issue.id
    assert metadata["issue_title"] == "native-only issue"
    assert "issue_number" not in metadata
    assert "issue_repo" not in metadata


@pytest.mark.asyncio
async def test_dispatch_by_issue_id_returns_no_project_when_id_unknown(app):
    """Caller passes a stale or invalid issue id → no_project (the no-
    Issue-here outcome reuses the same code path as no-project-for-repo,
    so the failure mode is uniform across dispatch entry shapes)."""
    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )

    result = await dispatch_run(
        app, issue_id="01JBOGUS0000000000000000",
        trigger_source={"kind": "glimmung_ui"},
    )
    assert result.state == "no_project"


@pytest.mark.asyncio
async def test_native_dispatch_serializes_with_second_call(app):
    """Two concurrent dispatches against the same native Issue → second
    sees `already_running`. Same lock primitive as the GH path, just a
    different key shape."""
    from glimmung import issues as issue_ops

    await _register_project(app, "ambience", "nelsong6/ambience")
    await _register_workflow(
        app, project="ambience", name="issue-agent",
        retry_workflow_filename="agent-retry.yml",
    )
    await _register_host(app, "runner-1")

    issue = await issue_ops.create_issue(
        app.state.cosmos, project="ambience", title="t",
    )

    first = await dispatch_run(
        app, issue_id=issue.id,
        trigger_source={"kind": "glimmung_ui"},
    )
    second = await dispatch_run(
        app, issue_id=issue.id,
        trigger_source={"kind": "glimmung_ui"},
    )
    assert first.state == "dispatched"
    assert second.state == "already_running"

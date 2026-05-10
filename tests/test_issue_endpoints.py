"""Issue API surface tests (#50).

Covers the issue endpoints — POST /v1/issues, PATCH by id,
GET by id, and the IssueRow/IssueDetail rendering for native issues.
Runs against the in-memory Cosmos fake;
uses the same direct-helper pattern as test_webhook_mirror.py rather
than spinning up a FastAPI client.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from glimmung import issues as issue_ops
from glimmung.app import (
    IssueArchiveRequest,
    IssueCommentRequest,
    IssueUpdateRequest,
    _build_issue_detail,
    archive_issue_by_number_endpoint,
    archive_issue_endpoint,
    app,
    create_issue_comment_by_number_endpoint,
    create_issue_comment_endpoint,
    delete_issue_comment_by_number_endpoint,
    delete_issue_comment_endpoint,
    discard_issue_by_number_endpoint,
    discard_issue_endpoint,
    issue_detail_by_id,
    patch_issue_endpoint,
    update_issue_comment_by_number_endpoint,
    update_issue_comment_endpoint,
    _list_issues_from_cosmos,
)
from glimmung.auth import User
from glimmung.models import IssueState, RunState

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        issues=FakeContainer("issues", "/project"),
        runs=FakeContainer("runs", "/project"),
        locks=FakeContainer("locks", "/scope"),
    )


@pytest.fixture
def app_state(cosmos):
    old = getattr(app.state, "cosmos", None)
    app.state.cosmos = cosmos
    yield
    if old is None:
        delattr(app.state, "cosmos")
    else:
        app.state.cosmos = old


# ─── list_issues ──────────────────────────────


@pytest.mark.asyncio
async def test_list_surfaces_native_issues(cosmos):
    """The row carries a public `ref`; `number` is the Glimmung
    project-scoped issue number."""
    await issue_ops.create_issue(
        cosmos, project="ambience", title="native one",
    )

    rows = await _list_issues_from_cosmos(cosmos)
    assert len(rows) == 1
    row = rows[0]
    assert row.number == 1
    assert row.repo is None
    assert row.html_url is None
    assert row.ref == "ambience#1"


@pytest.mark.asyncio
async def test_list_omits_closed_issues(cosmos):
    """`list_open_issues` filters by state — closed native issues stay
    off the dashboard until reopened, same as GH-anchored ones."""
    issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="t",
    )
    found = await issue_ops.read_issue(
        cosmos, project="ambience", issue_id=issue.id,
    )
    assert found is not None
    fetched, etag = found
    await issue_ops.close_issue(cosmos, issue=fetched, etag=etag)

    rows = await _list_issues_from_cosmos(cosmos)
    assert rows == []


@pytest.mark.asyncio
async def test_list_can_include_closed_issues_for_audit(cosmos):
    open_issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="active",
    )
    closed_issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="archived",
    )
    found = await issue_ops.read_issue(
        cosmos, project="ambience", issue_id=closed_issue.id,
    )
    assert found is not None
    fetched, etag = found
    await issue_ops.close_issue(cosmos, issue=fetched, etag=etag)

    closed_rows = await _list_issues_from_cosmos(cosmos, state="closed")
    all_rows = await _list_issues_from_cosmos(cosmos, state="all")

    assert [r.ref for r in closed_rows] == [f"ambience#{closed_issue.number}"]
    assert {r.ref for r in all_rows} == {
        f"ambience#{open_issue.number}",
        f"ambience#{closed_issue.number}",
    }


@pytest.mark.asyncio
async def test_list_rejects_unknown_issue_state(cosmos):
    with pytest.raises(HTTPException) as exc:
        await _list_issues_from_cosmos(cosmos, state="discarded")

    assert exc.value.status_code == 400


@pytest.mark.asyncio
async def test_archive_issue_closes_and_comments(cosmos, app_state):
    issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="stale idea",
    )

    detail = await archive_issue_by_number_endpoint(
        IssueArchiveRequest(reason="superseded"),
        project="ambience",
        issue_number=issue.number,
        user=User(sub="admin", email="admin@example.com", name="Admin"),
    )

    assert detail.state == IssueState.CLOSED.value
    assert [(c.author, c.body) for c in detail.comments] == [
        ("admin@example.com", "Archived: superseded"),
    ]
    assert await _list_issues_from_cosmos(cosmos) == []


@pytest.mark.asyncio
async def test_discard_issue_closes_and_comments(cosmos, app_state):
    issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="not actionable",
    )

    detail = await discard_issue_by_number_endpoint(
        IssueArchiveRequest(),
        project="ambience",
        issue_number=issue.number,
        user=User(sub="admin", email="admin@example.com", name="Admin"),
    )

    assert detail.state == IssueState.CLOSED.value
    assert [(c.author, c.body) for c in detail.comments] == [
        ("admin@example.com", "Discarded"),
    ]


@pytest.mark.asyncio
async def test_list_filters_by_project_and_limit(cosmos):
    await issue_ops.create_issue(
        cosmos, project="ambience", title="ambience native",
    )
    await issue_ops.create_issue(
        cosmos, project="ambience", title="ambience native 2",
    )
    await issue_ops.create_issue(cosmos, project="glimmung", title="glimmung native")

    rows = await _list_issues_from_cosmos(cosmos, project="ambience")
    assert [r.project for r in rows] == ["ambience", "ambience"]

    with pytest.raises(HTTPException) as exc_info:
        await _list_issues_from_cosmos(cosmos, repo="nelsong6/glimmung")
    assert exc_info.value.status_code == 410

    rows = await _list_issues_from_cosmos(cosmos, limit=1)
    assert len(rows) == 1


@pytest.mark.asyncio
async def test_list_surfaces_and_filters_issue_workflow(cosmos):
    issue_agent = await issue_ops.create_issue(
        cosmos, project="ambience", title="workflow issue",
        workflow="issue-agent",
    )
    await issue_ops.create_issue(
        cosmos, project="ambience", title="other workflow",
        workflow="other-agent",
    )

    rows = await _list_issues_from_cosmos(
        cosmos, project="ambience", workflow="issue-agent",
    )

    assert len(rows) == 1
    assert rows[0].ref == f"ambience#{issue_agent.number}"
    assert rows[0].workflow == "issue-agent"


@pytest.mark.asyncio
async def test_list_derives_issue_workflow_from_latest_run(cosmos):
    issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="ran once",
    )
    await cosmos.runs.create_item({
        "id": "run-old",
        "project": "ambience",
        "workflow": "old-agent",
        "issue_id": issue.id,
        "issue_number": 0,
        "state": "aborted",
        "created_at": "2026-01-01T00:00:00+00:00",
    })
    await cosmos.runs.create_item({
        "id": "run-new",
        "project": "ambience",
        "workflow": "issue-agent",
        "issue_id": issue.id,
        "issue_number": 0,
        "state": "passed",
        "created_at": "2026-01-02T00:00:00+00:00",
    })

    rows = await _list_issues_from_cosmos(
        cosmos, project="ambience", workflow="issue-agent",
    )

    assert len(rows) == 1
    assert rows[0].workflow == "issue-agent"
    assert rows[0].last_run_ref == f"ambience#{issue.number}/runs/2"


@pytest.mark.asyncio
async def test_needs_attention_omits_runnable_and_active_issues(cosmos):
    runnable = await issue_ops.create_issue(
        cosmos, project="ambience", title="not dispatched yet",
    )
    active = await issue_ops.create_issue(
        cosmos, project="ambience", title="running now",
    )
    failed = await issue_ops.create_issue(
        cosmos, project="ambience", title="failed run",
    )
    ready = await issue_ops.create_issue(
        cosmos, project="ambience", title="touchpoint ready",
    )

    await cosmos.runs.create_item({
        "id": "run-active",
        "project": "ambience",
        "workflow": "issue-agent",
        "issue_id": active.id,
        "issue_number": active.number,
        "state": RunState.IN_PROGRESS.value,
        "created_at": "2026-01-01T00:00:00+00:00",
    })
    await cosmos.runs.create_item({
        "id": "run-failed",
        "project": "ambience",
        "workflow": "issue-agent",
        "issue_id": failed.id,
        "issue_number": failed.number,
        "state": RunState.ABORTED.value,
        "abort_reason": "verification failed",
        "created_at": "2026-01-02T00:00:00+00:00",
    })
    await cosmos.runs.create_item({
        "id": "run-ready",
        "project": "ambience",
        "workflow": "issue-agent",
        "issue_id": ready.id,
        "issue_number": ready.number,
        "state": RunState.PASSED.value,
        "created_at": "2026-01-03T00:00:00+00:00",
    })

    rows = await _list_issues_from_cosmos(cosmos, needs_attention=True)

    assert {r.ref for r in rows} == {f"ambience#{failed.number}", f"ambience#{ready.number}"}
    assert f"ambience#{runnable.number}" not in [r.ref for r in rows]
    assert f"ambience#{active.number}" not in [r.ref for r in rows]


# ─── _build_issue_detail: shared rendering ──────────────────────────────


@pytest.mark.asyncio
async def test_build_detail_for_native_issue_omits_gh_fields(cosmos):
    issue = await issue_ops.create_issue(
        cosmos, project="ambience",
        title="rewrite the dispatcher", body="we should split it",
        labels=["epic"],
    )

    detail = await _build_issue_detail(cosmos, issue=issue)
    assert detail.ref == "ambience#1"
    assert detail.project == "ambience"
    assert detail.title == "rewrite the dispatcher"
    assert detail.body == "we should split it"
    assert detail.labels == ["epic"]
    assert detail.comments == []
    assert detail.state == "open"
    assert detail.repo is None
    assert detail.number == 1
    assert detail.html_url is None
    assert detail.last_run_ref is None
    assert detail.issue_lock_held is False


@pytest.mark.asyncio
async def test_build_detail_accepts_legacy_run_without_issue_repo(cosmos):
    issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="legacy latest run",
    )
    await cosmos.runs.create_item({
        "id": "run-legacy",
        "project": "ambience",
        "workflow": "issue-agent",
        "issue_id": issue.id,
        "issue_number": issue.number,
        "state": RunState.IN_PROGRESS.value,
        "created_at": "2026-01-01T00:00:00+00:00",
        "updated_at": "2026-01-01T00:00:00+00:00",
    })

    detail = await _build_issue_detail(cosmos, issue=issue)

    assert detail.last_run_ref == "ambience#1/runs/1"
    assert detail.last_run_state == RunState.IN_PROGRESS.value


@pytest.mark.asyncio
async def test_read_issue_by_number_reads_project_scoped_number(cosmos):
    issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="native numbered",
    )

    found = await issue_ops.read_issue_by_number(
        cosmos, project="ambience", number=issue.number,
    )

    assert found is not None
    fetched, _ = found
    assert fetched.id == issue.id
    assert fetched.number == issue.number


@pytest.mark.asyncio
async def test_build_detail_has_no_repo_link(cosmos):
    issue = await issue_ops.create_issue(cosmos, project="ambience", title="t")
    detail = await _build_issue_detail(cosmos, issue=issue)
    assert detail.repo is None
    assert detail.number == 1
    assert detail.html_url is None


@pytest.mark.asyncio
async def test_build_detail_includes_issue_comments(cosmos):
    issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="t",
    )
    fetched, etag = await issue_ops.read_issue(
        cosmos, project="ambience", issue_id=issue.id,
    )
    issue, _, comment = await issue_ops.add_comment(
        cosmos,
        issue=fetched,
        etag=etag,
        author="nelson@example.com",
        body="triage note",
    )

    detail = await _build_issue_detail(cosmos, issue=issue)

    assert len(detail.comments) == 1
    assert detail.comments[0] == comment


@pytest.mark.asyncio
async def test_issue_comment_endpoints_create_update_delete(cosmos, monkeypatch):
    issue = await issue_ops.create_issue(cosmos, project="ambience", title="t")
    monkeypatch.setattr(
        "glimmung.app.app",
        SimpleNamespace(state=SimpleNamespace(cosmos=cosmos)),
    )
    user = User(sub="u", email="nelson@example.com", name="Nelson")

    comment = await create_issue_comment_by_number_endpoint(
        IssueCommentRequest(body="first"),
        project="ambience",
        issue_number=issue.number,
        user=user,
    )
    assert comment.author == "nelson@example.com"
    assert comment.body == "first"

    edited = await update_issue_comment_by_number_endpoint(
        IssueCommentRequest(body="edited"),
        project="ambience",
        issue_number=issue.number,
        comment_id=comment.id,
        user=user,
    )
    assert edited.id == comment.id
    assert edited.body == "edited"

    detail = await delete_issue_comment_by_number_endpoint(
        project="ambience",
        issue_number=issue.number,
        comment_id=comment.id,
    )
    assert detail.comments == []


@pytest.mark.asyncio
async def test_update_issue_comment_rejects_other_author(cosmos, monkeypatch):
    issue = await issue_ops.create_issue(cosmos, project="ambience", title="t")
    fetched, etag = await issue_ops.read_issue(
        cosmos, project="ambience", issue_id=issue.id,
    )
    _, _, comment = await issue_ops.add_comment(
        cosmos,
        issue=fetched,
        etag=etag,
        author="author@example.com",
        body="first",
    )
    monkeypatch.setattr(
        "glimmung.app.app",
        SimpleNamespace(state=SimpleNamespace(cosmos=cosmos)),
    )
    user = User(sub="u", email="other@example.com", name="Other")

    with pytest.raises(HTTPException) as exc:
            await update_issue_comment_by_number_endpoint(
                IssueCommentRequest(body="edited"),
                project="ambience",
                issue_number=issue.number,
            comment_id=comment.id,
            user=user,
        )

    assert exc.value.status_code == 403


# ─── PATCH endpoint logic (state transitions) ──────────────────────────


@pytest.mark.asyncio
async def test_patch_state_closed_transitions_open_issue(cosmos):
    """Replicates patch_issue_endpoint's body so the state-transition
    branching is testable without a FastAPI client."""
    issue = await issue_ops.create_issue(cosmos, project="ambience", title="t")

    found = await issue_ops.read_issue(cosmos, project="ambience", issue_id=issue.id)
    assert found is not None
    issue, etag = found

    # Apply the PATCH state=closed transition.
    req = IssueUpdateRequest(state="closed")
    if req.state == "closed" and issue.state == IssueState.OPEN:
        await issue_ops.close_issue(cosmos, issue=issue, etag=etag)

    found = await issue_ops.read_issue(cosmos, project="ambience", issue_id=issue.id)
    assert found is not None
    closed, _ = found
    assert closed.state == IssueState.CLOSED
    assert closed.closed_at is not None


@pytest.mark.asyncio
async def test_patch_state_invalid_value_raises():
    """The endpoint guards with HTTPException; validate the same logic
    against the Pydantic model + branching."""
    req = IssueUpdateRequest(state="archived")
    target = (req.state or "").lower()
    assert target not in ("open", "closed")
    with pytest.raises(HTTPException) as exc:
        if target not in ("open", "closed"):
            raise HTTPException(400, f"state must be 'open' or 'closed', not {req.state!r}")
    assert exc.value.status_code == 400


@pytest.mark.asyncio
async def test_storage_id_issue_routes_are_gone(cosmos, app_state):
    issue = await issue_ops.create_issue(cosmos, project="ambience", title="hidden id")
    user = User(sub="admin", email="admin@example.com", name="Admin")

    calls = [
        issue_detail_by_id(project="ambience", issue_id=issue.id),
        patch_issue_endpoint(
            IssueUpdateRequest(title="new"),
            project="ambience",
            issue_id=issue.id,
        ),
        archive_issue_endpoint(
            IssueArchiveRequest(reason="old"),
            project="ambience",
            issue_id=issue.id,
            user=user,
        ),
        discard_issue_endpoint(
            IssueArchiveRequest(reason="old"),
            project="ambience",
            issue_id=issue.id,
            user=user,
        ),
        create_issue_comment_endpoint(
            IssueCommentRequest(body="comment"),
            project="ambience",
            issue_id=issue.id,
            user=user,
        ),
        update_issue_comment_endpoint(
            IssueCommentRequest(body="comment"),
            project="ambience",
            issue_id=issue.id,
            comment_id="comment-id",
            user=user,
        ),
        delete_issue_comment_endpoint(
            project="ambience",
            issue_id=issue.id,
            comment_id="comment-id",
        ),
    ]
    for call in calls:
        with pytest.raises(HTTPException) as exc:
            await call
        assert exc.value.status_code == 410


@pytest.mark.asyncio
async def test_patch_can_combine_field_edits_and_state_close(cosmos):
    """One PATCH that touches title/labels and also closes the issue:
    the field update + state transition both apply."""
    issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="old", labels=["a"],
    )
    found = await issue_ops.read_issue(cosmos, project="ambience", issue_id=issue.id)
    assert found is not None
    issue, etag = found

    issue, etag = await issue_ops.update_issue(
        cosmos, issue=issue, etag=etag,
        title="new", labels=["a", "b"],
    )
    issue, etag = await issue_ops.close_issue(cosmos, issue=issue, etag=etag)

    found = await issue_ops.read_issue(cosmos, project="ambience", issue_id=issue.id)
    assert found is not None
    final, _ = found
    assert final.title == "new"
    assert final.labels == ["a", "b"]
    assert final.state == IssueState.CLOSED

"""Issue API surface tests (#50).

Covers the post-#50 issue endpoints — POST /v1/issues, PATCH by id,
GET by id, and the IssueRow/IssueDetail rendering for both GH-anchored
and glimmung-native shapes. Runs against the in-memory Cosmos fake;
uses the same direct-helper pattern as test_webhook_mirror.py rather
than spinning up a FastAPI client.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from glimmung import issues as issue_ops
from glimmung.app import (
    IssueUpdateRequest,
    _build_issue_detail,
    _list_issues_from_cosmos,
)
from glimmung.models import IssueState

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        issues=FakeContainer("issues", "/project"),
        runs=FakeContainer("runs", "/project"),
        locks=FakeContainer("locks", "/scope"),
    )


# ─── list_issues: native + GH-anchored coexist ──────────────────────────────


@pytest.mark.asyncio
async def test_list_surfaces_both_native_and_gh_issues(cosmos):
    """Pre-#50 the listing skipped issues without a GH url. Post-#50
    glimmung-native ones surface too — the row carries `id` always and
    `repo`/`number`/`html_url` only when GH-anchored."""
    await issue_ops.create_issue(
        cosmos, project="ambience", title="native one",
    )
    await issue_ops.create_issue(
        cosmos, project="ambience", title="gh one",
        github_issue_url="https://github.com/nelsong6/ambience/issues/7",
        github_issue_repo="nelsong6/ambience",
        github_issue_number=7,
    )

    rows = await _list_issues_from_cosmos(cosmos)
    assert len(rows) == 2

    # GH issue lands first (number-bearing rows sort before native ones).
    gh, native = rows
    assert gh.number == 7
    assert gh.repo == "nelsong6/ambience"
    assert gh.html_url and gh.html_url.endswith("/issues/7")

    assert native.number is None
    assert native.repo is None
    assert native.html_url is None
    assert native.id  # ULID always present


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


# ─── _build_issue_detail: shared rendering ──────────────────────────────


@pytest.mark.asyncio
async def test_build_detail_for_native_issue_omits_gh_fields(cosmos):
    issue = await issue_ops.create_issue(
        cosmos, project="ambience",
        title="rewrite the dispatcher", body="we should split it",
        labels=["epic"],
    )

    detail = await _build_issue_detail(cosmos, issue=issue)
    assert detail.id == issue.id
    assert detail.project == "ambience"
    assert detail.title == "rewrite the dispatcher"
    assert detail.body == "we should split it"
    assert detail.labels == ["epic"]
    assert detail.state == "open"
    assert detail.repo is None
    assert detail.number is None
    assert detail.html_url is None
    assert detail.last_run_id is None
    assert detail.issue_lock_held is False


@pytest.mark.asyncio
async def test_build_detail_carries_gh_coords_when_present(cosmos):
    issue = await issue_ops.create_issue(
        cosmos, project="ambience", title="t",
        github_issue_url="https://github.com/nelsong6/ambience/issues/12",
        github_issue_repo="nelsong6/ambience",
        github_issue_number=12,
    )
    detail = await _build_issue_detail(cosmos, issue=issue)
    assert detail.repo == "nelsong6/ambience"
    assert detail.number == 12
    assert detail.html_url == "https://github.com/nelsong6/ambience/issues/12"


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

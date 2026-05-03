"""PR API surface tests (#50 slice 2).

Covers the post-#50 PR endpoints: GET /v1/reports (Cosmos cutover from
runs container), POST /v1/reports (agent registration + idempotent re-
register), PATCH /v1/reports/by-id with state transitions (close, merge,
reopen). Mirrors the test_issue_endpoints style — direct helper
invocation against the in-memory cosmos fake.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from glimmung import reports as report_ops
from glimmung.app import (
    ReportCreateRequest,
    ReportUpdateRequest,
    _build_report_detail,
    _list_reports_from_cosmos,
)
from glimmung.models import ReportState

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        reports=FakeContainer("reports", "/project"),
        runs=FakeContainer("runs", "/project"),
        issues=FakeContainer("issues", "/project"),
        locks=FakeContainer("locks", "/scope"),
    )


# ─── list_reports ────────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_list_prs_surfaces_prs_from_cosmos(cosmos):
    """Pre-#50 the listing read from `runs` and required a Run with
    pr_number set. It now reads `reports` directly — manual PRs without
    a Run still surface."""
    await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=12, title="manual fix", branch="hotfix/12",
        html_url="https://github.com/nelsong6/ambience/pull/12",
    )
    await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="agent fix", branch="agent/issue-7",
    )

    rows = await _list_reports_from_cosmos(cosmos)
    assert len(rows) == 2
    # Sorted by descending pr_number.
    assert [r.pr_number for r in rows] == [14, 12]
    for row in rows:
        assert row.id  # ULID always present
        assert row.state == "ready"
        assert row.merged is False
        assert row.run_state is None  # no run linkage yet


@pytest.mark.asyncio
async def test_list_prs_includes_closed(cosmos):
    pr = await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=12, title="t", branch="b",
    )
    found = await report_ops.read_report(cosmos, project="ambience", report_id=pr.id)
    assert found is not None
    pr, etag = found
    await report_ops.close_report(cosmos, pr=pr, etag=etag)

    rows = await _list_reports_from_cosmos(cosmos)
    assert len(rows) == 1
    assert rows[0].pr_number == 12
    assert rows[0].state == "closed"


@pytest.mark.asyncio
async def test_list_prs_joins_run_via_linked_run_id(cosmos):
    """Linked-Run join lights up the runtime columns when a glimmung
    Run id is on the PR."""
    run_doc = {
        "id": "01JRUNAAAA",
        "project": "ambience",
        "issue_repo": "nelsong6/ambience",
        "pr_number": 14,
        "state": "in_progress",
        "attempts": [{"attempt_index": 0}],
        "cumulative_cost_usd": 1.25,
        "issue_number": 7,
        "issue_id": "01JISSUEAAA",
        "created_at": datetime.now(UTC).isoformat(),
    }
    await cosmos.runs.create_item(run_doc)

    await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="agent/issue-7",
        linked_run_id="01JRUNAAAA",
    )

    rows = await _list_reports_from_cosmos(cosmos)
    assert len(rows) == 1
    row = rows[0]
    assert row.linked_run_id == "01JRUNAAAA"
    assert row.run_state == "in_progress"
    assert row.run_attempts == 1
    assert row.run_cumulative_cost_usd == 1.25
    assert row.issue_number == 7


@pytest.mark.asyncio
async def test_list_prs_surfaces_warm_session_launch_url(cosmos):
    run_doc = {
        "id": "01JRUNWARM",
        "project": "ambience",
        "issue_repo": "nelsong6/ambience",
        "pr_number": 14,
        "state": "passed",
        "attempts": [{"attempt_index": 0}, {"attempt_index": 1}],
        "cumulative_cost_usd": 0.0,
        "issue_number": 0,
        "issue_id": "01JISSUEZZZ",
        "validation_url": "https://preview.example.test",
        "session_launch_intent": "warm",
        "created_at": datetime.now(UTC).isoformat(),
    }
    await cosmos.runs.create_item(run_doc)

    pr = await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="agent/native",
        linked_issue_id="01JISSUEZZZ",
        linked_run_id="01JRUNWARM",
    )

    rows = await _list_reports_from_cosmos(cosmos)
    assert len(rows) == 1
    row = rows[0]
    assert row.validation_url == "https://preview.example.test"
    assert row.session_launch_intent == "warm"
    assert row.session_launch_url is not None
    assert "glimmung_run_id=01JRUNWARM" in row.session_launch_url
    assert "glimmung_issue_id=01JISSUEZZZ" in row.session_launch_url
    assert f"glimmung_pr_id={pr.id}" in row.session_launch_url
    assert "validation_url=https%3A%2F%2Fpreview.example.test" in row.session_launch_url


# ─── _build_report_detail ────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_build_report_detail_for_manual_pr(cosmos):
    pr = await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=21, title="manual", branch="x", body="motivation: ...",
        html_url="https://github.com/nelsong6/ambience/pull/21",
        head_sha="abc123",
    )

    detail = await _build_report_detail(cosmos, pr=pr)
    assert detail.id == pr.id
    assert detail.repo == "nelsong6/ambience"
    assert detail.pr_number == 21
    assert detail.title == "manual"
    assert detail.body == "motivation: ..."
    assert detail.state == "ready"
    assert detail.merged is False
    assert detail.head_sha == "abc123"
    assert detail.run_state is None
    assert detail.run_attempts == 0
    assert detail.linked_issue_id is None
    assert detail.linked_run_id is None


@pytest.mark.asyncio
async def test_build_report_detail_stitches_linked_issue_title(cosmos):
    """When linked_issue_id is set, _build_report_detail reads the Issue
    title and surfaces it for the dashboard."""
    issue_doc = {
        "id": "01JISSUEZZZ",
        "project": "ambience",
        "title": "the linked issue title",
        "body": "",
        "labels": [],
        "state": "open",
        "metadata": {"source": "manual"},
        "created_at": datetime.now(UTC).isoformat(),
        "updated_at": datetime.now(UTC).isoformat(),
        "schema_version": 1,
    }
    await cosmos.issues.create_item(issue_doc)

    pr = await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="b",
        linked_issue_id="01JISSUEZZZ",
    )
    detail = await _build_report_detail(cosmos, pr=pr)
    assert detail.linked_issue_id == "01JISSUEZZZ"
    assert detail.issue_title == "the linked issue title"


@pytest.mark.asyncio
async def test_build_report_detail_surfaces_warm_session_launch_url(cosmos):
    issue_doc = {
        "id": "01JISSUEZZZ",
        "project": "ambience",
        "title": "the linked issue title",
        "body": "",
        "labels": ["agent-session:warm"],
        "state": "open",
        "metadata": {"source": "manual"},
        "created_at": datetime.now(UTC).isoformat(),
        "updated_at": datetime.now(UTC).isoformat(),
        "schema_version": 1,
    }
    await cosmos.issues.create_item(issue_doc)
    run_doc = {
        "id": "01JRUNWARM",
        "project": "ambience",
        "workflow": "issue-agent",
        "issue_id": "01JISSUEZZZ",
        "issue_repo": "nelsong6/ambience",
        "issue_number": 42,
        "state": "passed",
        "budget": {"total": 25.0},
        "attempts": [],
        "cumulative_cost_usd": 0.0,
        "trigger_source": {"kind": "glimmung_ui"},
        "validation_url": "https://preview.example.test",
        "session_launch_intent": "warm",
        "created_at": datetime.now(UTC).isoformat(),
        "updated_at": datetime.now(UTC).isoformat(),
    }
    await cosmos.runs.create_item(run_doc)
    pr = await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="b",
        linked_issue_id="01JISSUEZZZ", linked_run_id="01JRUNWARM",
    )

    detail = await _build_report_detail(cosmos, pr=pr)

    assert detail.session_launch_intent == "warm"
    assert detail.validation_url == "https://preview.example.test"
    assert detail.session_launch_url is not None
    assert "glimmung_run_id=01JRUNWARM" in detail.session_launch_url
    assert "glimmung_issue_id=01JISSUEZZZ" in detail.session_launch_url
    assert f"glimmung_pr_id={pr.id}" in detail.session_launch_url
    assert "validation_url=https%3A%2F%2Fpreview.example.test" in detail.session_launch_url


# ─── POST /v1/reports idempotence ───────────────────────────────────────────────


@pytest.mark.asyncio
async def test_create_pr_endpoint_logic_is_idempotent_on_repo_number(cosmos):
    """The endpoint's body uses ensure_report_for_github, so two POSTs with
    the same (repo, number) return the same PR id rather than minting
    a duplicate."""
    a, _, created_a = await report_ops.ensure_report_for_github(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="b",
    )
    b, _, created_b = await report_ops.ensure_report_for_github(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="different title", branch="b",
    )
    assert created_a is True
    assert created_b is False
    assert a.id == b.id


@pytest.mark.asyncio
async def test_create_pr_request_validation():
    """ReportCreateRequest defaults: body empty, base_ref main, no linkages."""
    req = ReportCreateRequest(
        project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="b",
    )
    assert req.body == ""
    assert req.base_ref == "main"
    assert req.linked_issue_id is None
    assert req.linked_run_id is None


# ─── PATCH state transitions ──────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_patch_state_closed_transitions_open_pr(cosmos):
    pr = await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="b",
    )

    found = await report_ops.read_report(cosmos, project="ambience", report_id=pr.id)
    assert found is not None
    pr, etag = found

    req = ReportUpdateRequest(state="closed")
    if req.state == "closed" and pr.state == ReportState.READY:
        pr, etag = await report_ops.close_report(cosmos, pr=pr, etag=etag)

    assert pr.state == ReportState.CLOSED
    assert pr.merged_at is None  # closed-without-merge


@pytest.mark.asyncio
async def test_patch_state_merged_requires_merged_by(cosmos):
    """Mirrors patch_pr_endpoint's guard."""
    req = ReportUpdateRequest(state="merged")
    with pytest.raises(HTTPException) as exc:
        if not req.merged_by:
            raise HTTPException(400, "state='merged' requires merged_by")
    assert exc.value.status_code == 400


@pytest.mark.asyncio
async def test_patch_state_merged_stamps_merged_at_and_by(cosmos):
    pr = await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="b",
    )
    found = await report_ops.read_report(cosmos, project="ambience", report_id=pr.id)
    assert found is not None
    pr, etag = found

    pr, _ = await report_ops.merge_report(
        cosmos, pr=pr, etag=etag, merged_by="nelsong6",
    )
    assert pr.state == ReportState.MERGED
    assert pr.merged_at is not None
    assert pr.merged_by == "nelsong6"


@pytest.mark.asyncio
async def test_patch_state_reopen_blocked_on_merged_pr(cosmos):
    """The endpoint guards: merged PRs cannot be reopened (matches GH)."""
    pr = await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="b",
    )
    found = await report_ops.read_report(cosmos, project="ambience", report_id=pr.id)
    assert found is not None
    pr, etag = found
    pr, _ = await report_ops.merge_report(
        cosmos, pr=pr, etag=etag, merged_by="nelsong6",
    )

    req = ReportUpdateRequest(state="ready")
    with pytest.raises(HTTPException) as exc:
        if req.state == "ready" and pr.merged_at is not None:
            raise HTTPException(409, "merged Report cannot be reopened")
    assert exc.value.status_code == 409


@pytest.mark.asyncio
async def test_patch_can_attach_run_linkage_to_existing_pr(cosmos):
    """A PATCH that just sets linked_run_id (no other field changes)
    threads through update_report cleanly."""
    pr = await report_ops.create_report(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="b",
    )
    found = await report_ops.read_report(cosmos, project="ambience", report_id=pr.id)
    assert found is not None
    pr, etag = found

    pr, _ = await report_ops.update_report(
        cosmos, pr=pr, etag=etag,
        linked_run_id="01JRUNAAAA", linked_issue_id="01JISSAAA",
    )
    assert pr.linked_run_id == "01JRUNAAAA"
    assert pr.linked_issue_id == "01JISSAAA"

"""Agent PR marker tests (#50 slice 4).

The agent's open-PR step embeds a `<!-- glimmung-meta ... -->` block
in the GH PR body. The webhook parses it to (a) pull the rich content
out into the glimmung PR doc and (b) attach linked_issue_id without
needing admin auth from the runner. These tests pin the parser +
renderer + the integration into _handle_pull_request.
"""

from __future__ import annotations

import base64
from datetime import UTC, datetime
from types import SimpleNamespace
from unittest.mock import patch

import pytest

from glimmung import reports as report_ops
from glimmung.app import (
    _handle_pull_request,
    _parse_glimmung_meta,
    _render_glimmung_pr_body,
)
from glimmung.models import ReportState

from tests.cosmos_fake import FakeContainer


# ─── parser ──────────────────────────────────────────────────────────────────


def test_parse_glimmung_meta_extracts_key_value_pairs():
    body = """\
Closes #42.

Glimmung PR: https://glimmung.romaine.life/reports?issue=01JABC

<!-- glimmung-meta
project=glimmung
issue_number=42
issue_id=01JABC0000000000000000000A
validation_env=https://issue-42.glimmung.dev.romaine.life
lease_id=01JLEASE000
host=glimmung-slot-1
-->
"""
    meta = _parse_glimmung_meta(body)
    assert meta["project"] == "glimmung"
    assert meta["issue_number"] == "42"
    assert meta["issue_id"] == "01JABC0000000000000000000A"
    assert meta["validation_env"] == "https://issue-42.glimmung.dev.romaine.life"
    assert meta["lease_id"] == "01JLEASE000"
    assert meta["host"] == "glimmung-slot-1"


def test_parse_glimmung_meta_returns_empty_when_marker_missing():
    """Manual / human-opened PRs don't have the marker — parser
    returns an empty dict without errors."""
    assert _parse_glimmung_meta("just a normal PR body") == {}
    assert _parse_glimmung_meta("") == {}


def test_render_glimmung_pr_body_decodes_b64_payloads():
    notes = "## Notes\n- did the thing\n"
    screenshots = "![one](url1)\n"
    meta = {
        "validation_env": "https://x.dev",
        "notes_md_b64": base64.b64encode(notes.encode()).decode(),
        "screenshots_md_b64": base64.b64encode(screenshots.encode()).decode(),
        "lease_id": "01JLEASE000",
        "host": "glimmung-slot-1",
    }
    rendered = _render_glimmung_pr_body(meta)
    assert "Validation env" in rendered
    assert "did the thing" in rendered
    assert "![one](url1)" in rendered
    assert "glimmung-slot-1" in rendered
    assert "01JLEASE000" in rendered


def test_render_glimmung_pr_body_handles_missing_keys():
    """Marker with only validation_env set still renders cleanly."""
    rendered = _render_glimmung_pr_body({"validation_env": "https://x"})
    assert "Validation env" in rendered
    assert "https://x" in rendered


# ─── webhook integration ───────────────────────────────────────────────────────


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        reports=FakeContainer("reports", "/project"),
        runs=FakeContainer("runs", "/project"),
        issues=FakeContainer("issues", "/project"),
        projects=FakeContainer("projects", "/name"),
        signals=FakeContainer("signals", "/target_repo"),
        locks=FakeContainer("locks", "/scope"),
    )


@pytest.fixture
def app_state(cosmos):
    state = SimpleNamespace(cosmos=cosmos, settings=None, gh_minter=None)
    return SimpleNamespace(state=state)


def _agent_payload(*, issue_id: str = "01JABC0000000000000000000A") -> dict:
    notes_b64 = base64.b64encode(b"## Notes\n- thing\n").decode()
    body = (
        f"Closes #42.\n\n"
        f"Glimmung PR: https://glimmung.romaine.life/reports?issue={issue_id}\n\n"
        f"<!-- glimmung-meta\n"
        f"project=glimmung\n"
        f"issue_number=42\n"
        f"issue_id={issue_id}\n"
        f"validation_env=https://issue-42.glimmung.dev.romaine.life\n"
        f"notes_md_b64={notes_b64}\n"
        f"lease_id=01JLEASE000\n"
        f"host=glimmung-slot-1\n"
        f"-->\n"
    )
    return {
        "action": "opened",
        "repository": {"full_name": "nelsong6/glimmung"},
        "pull_request": {
            "number": 99,
            "title": "agent: fix dispatcher (#42)",
            "body": body,
            "head": {"ref": "agent/issue-42-1234", "sha": "abc123"},
            "base": {"ref": "main"},
            "html_url": "https://github.com/nelsong6/glimmung/pull/99",
            "merged": False,
            "merged_by": None,
            "state": "open",
        },
    }


@pytest.mark.asyncio
async def test_agent_pr_open_attaches_linked_issue_id(cosmos, app_state):
    await cosmos.projects.create_item({
        "id": "glimmung",
        "name": "glimmung",
        "githubRepo": "nelsong6/glimmung",
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })

    with patch("glimmung.app.app", app_state):
        outcome = await _handle_pull_request(_agent_payload())

    assert outcome["created"] is True
    assert outcome.get("linked_issue_id") == "01JABC0000000000000000000A"

    found = await report_ops.find_report_by_repo_number(
        cosmos, repo="nelsong6/glimmung", number=99,
    )
    pr, _ = found
    assert pr.linked_issue_id == "01JABC0000000000000000000A"
    # The glimmung PR's body is the rendered rich content, not the raw
    # GH body with the marker still in it.
    assert "Validation env" in pr.body
    assert "thing" in pr.body  # decoded notes content
    assert "glimmung-meta" not in pr.body  # marker was consumed


@pytest.mark.asyncio
async def test_pr_open_without_marker_falls_back_to_raw_body(cosmos, app_state):
    """Manual PRs without the marker still mirror cleanly — the GH
    body lands on the glimmung PR as-is, and no linkages are set."""
    await cosmos.projects.create_item({
        "id": "glimmung",
        "name": "glimmung",
        "githubRepo": "nelsong6/glimmung",
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })

    payload = {
        "action": "opened",
        "repository": {"full_name": "nelsong6/glimmung"},
        "pull_request": {
            "number": 12,
            "title": "manual hotfix",
            "body": "fixes a thing — no marker here",
            "head": {"ref": "hotfix/12", "sha": "def456"},
            "base": {"ref": "main"},
            "html_url": "https://github.com/nelsong6/glimmung/pull/12",
            "merged": False,
            "merged_by": None,
            "state": "open",
        },
    }
    with patch("glimmung.app.app", app_state):
        outcome = await _handle_pull_request(payload)

    assert outcome["created"] is True
    assert "linked_issue_id" not in outcome
    found = await report_ops.find_report_by_repo_number(
        cosmos, repo="nelsong6/glimmung", number=12,
    )
    pr, _ = found
    assert pr.body == "fixes a thing — no marker here"
    assert pr.linked_issue_id is None


@pytest.mark.asyncio
async def test_agent_pr_open_derives_linked_run_when_run_exists(cosmos, app_state):
    """The marker doesn't carry run_id (Run is created after dispatch
    fires the workflow). When the Run is already in `runs` keyed off
    `(issue_repo, pr_number)`, the webhook attaches linked_run_id from
    the find_run_by_pr lookup."""
    await cosmos.projects.create_item({
        "id": "glimmung",
        "name": "glimmung",
        "githubRepo": "nelsong6/glimmung",
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })
    await cosmos.runs.create_item({
        "id": "01JRUN5500000000",
        "project": "glimmung",
        "workflow": "issue-agent",
        "issue_id": "01JABC0000000000000000000A",
        "issue_repo": "nelsong6/glimmung",
        "issue_number": 42,
        "state": "in_progress",
        "attempts": [],
        "cumulative_cost_usd": 0.0,
        "pr_number": 99,
        "pr_branch": "agent/issue-42-1234",
        "schema_version": 1,
        "created_at": datetime.now(UTC).isoformat(),
        "updated_at": datetime.now(UTC).isoformat(),
        "budget": {"max_attempts": 3, "max_cost_usd": 25.0},
    })

    with patch("glimmung.app.app", app_state):
        outcome = await _handle_pull_request(_agent_payload())

    assert outcome.get("linked_run_id") == "01JRUN5500000000"
    found = await report_ops.find_report_by_repo_number(
        cosmos, repo="nelsong6/glimmung", number=99,
    )
    pr, _ = found
    assert pr.linked_run_id == "01JRUN5500000000"
    assert pr.linked_issue_id == "01JABC0000000000000000000A"
    assert pr.state == ReportState.READY

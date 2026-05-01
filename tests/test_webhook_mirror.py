"""GH-issue → glimmung Issue mirror tests (#28 consumer-PR-3).

Exercises `_mirror_github_issue` end-to-end against the in-memory
Cosmos fake. The mirror is the canonical sync path: every `issues`
webhook event runs through it before any dispatch matching, so even
non-trigger actions (an edit, a label change that doesn't match a
workflow, a close) keep the glimmung `issues` container in sync with
GH.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from glimmung.app import _mirror_github_issue
from glimmung.issues import find_issue_by_github_url, read_issue
from glimmung.models import IssueSource, IssueState

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        issues=FakeContainer("issues", "/project"),
    )


def _payload(
    *,
    number: int = 42,
    title: str = "agent-run.yml triage misses prior feedback",
    body: str = "Repro: dispatch a run, reject the PR, observe…",
    labels: list[str] | None = None,
    state: str = "open",
) -> dict:
    return {
        "number": number,
        "title": title,
        "body": body,
        "labels": [{"name": n} for n in (labels or [])],
        "state": state,
    }


# ─── opened ─────────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_mirror_opened_creates_issue_with_full_payload(cosmos):
    outcome = await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="opened",
        issue_payload=_payload(labels=["bug", "issue-agent"]),
    )
    assert outcome["created"] is True

    found = await find_issue_by_github_url(
        cosmos, github_issue_url="https://github.com/nelsong6/ambience/issues/42",
    )
    assert found is not None
    issue, _ = found
    assert issue.title == "agent-run.yml triage misses prior feedback"
    assert "Repro" in issue.body
    assert issue.labels == ["bug", "issue-agent"]
    assert issue.state == IssueState.OPEN
    assert issue.metadata.source == IssueSource.GITHUB_WEBHOOK_IMPORT
    assert issue.metadata.github_issue_repo == "nelsong6/ambience"
    assert issue.metadata.github_issue_number == 42


@pytest.mark.asyncio
async def test_mirror_opened_after_dispatch_placeholder_overwrites_fields(cosmos):
    """When `dispatch_run` mints an Issue first (with placeholder title
    `repo#N`), a subsequent `issues.opened` webhook lands the real
    title/body/labels by patching the existing Issue."""
    from glimmung.issues import ensure_issue_for_github

    placeholder, _, created = await ensure_issue_for_github(
        cosmos, project="ambience",
        repo="nelsong6/ambience", issue_number=42,
    )
    assert created is True
    assert placeholder.title == "nelsong6/ambience#42"  # placeholder

    outcome = await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="opened",
        issue_payload=_payload(labels=["bug"]),
    )
    assert outcome["created"] is False  # Issue already existed
    assert outcome["patched"] is True

    fetched, _ = await read_issue(
        cosmos, project="ambience", issue_id=placeholder.id,
    )
    assert fetched.title == "agent-run.yml triage misses prior feedback"
    assert fetched.labels == ["bug"]


# ─── edited ─────────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_mirror_edited_patches_title_and_labels(cosmos):
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="opened", issue_payload=_payload(labels=["bug"]),
    )
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="edited",
        issue_payload=_payload(
            title="agent-run.yml: triage step lost feedback context",
            labels=["bug", "needs-triage"],
        ),
    )
    found = await find_issue_by_github_url(
        cosmos, github_issue_url="https://github.com/nelsong6/ambience/issues/42",
    )
    issue, _ = found
    assert issue.title == "agent-run.yml: triage step lost feedback context"
    assert issue.labels == ["bug", "needs-triage"]


# ─── labeled / unlabeled ────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_mirror_labeled_adds_to_label_list(cosmos):
    """Mirror reflects the full labels array as GH sends it (no diff —
    the payload is authoritative)."""
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="opened", issue_payload=_payload(labels=["bug"]),
    )
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="labeled", issue_payload=_payload(labels=["bug", "issue-agent"]),
    )
    found = await find_issue_by_github_url(
        cosmos, github_issue_url="https://github.com/nelsong6/ambience/issues/42",
    )
    issue, _ = found
    assert issue.labels == ["bug", "issue-agent"]


@pytest.mark.asyncio
async def test_mirror_unlabeled_removes_from_label_list(cosmos):
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="opened", issue_payload=_payload(labels=["bug", "issue-agent"]),
    )
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="unlabeled", issue_payload=_payload(labels=["bug"]),
    )
    found = await find_issue_by_github_url(
        cosmos, github_issue_url="https://github.com/nelsong6/ambience/issues/42",
    )
    issue, _ = found
    assert issue.labels == ["bug"]


# ─── closed / reopened ──────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_mirror_closed_transitions_state_and_stamps_closed_at(cosmos):
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="opened", issue_payload=_payload(),
    )
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="closed", issue_payload=_payload(state="closed"),
    )
    found = await find_issue_by_github_url(
        cosmos, github_issue_url="https://github.com/nelsong6/ambience/issues/42",
    )
    issue, _ = found
    assert issue.state == IssueState.CLOSED
    assert issue.closed_at is not None


@pytest.mark.asyncio
async def test_mirror_reopened_clears_closed_at(cosmos):
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="opened", issue_payload=_payload(),
    )
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="closed", issue_payload=_payload(state="closed"),
    )
    await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="reopened",
        issue_payload=_payload(state="open", labels=["bug"]),
    )
    found = await find_issue_by_github_url(
        cosmos, github_issue_url="https://github.com/nelsong6/ambience/issues/42",
    )
    issue, _ = found
    assert issue.state == IssueState.OPEN
    assert issue.closed_at is None
    # Patch on reopen also pulled in the new labels.
    assert issue.labels == ["bug"]


# ─── idempotence ────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_mirror_opened_twice_does_not_duplicate_issue(cosmos):
    """Webhook redeliveries shouldn't mint a second Issue. The `created`
    flag tells us the second call hit the existing-Issue path."""
    first = await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="opened", issue_payload=_payload(),
    )
    second = await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="opened", issue_payload=_payload(),
    )
    assert first["created"] is True
    assert second["created"] is False

    docs = [d async for d in cosmos.issues.query_items(
        "SELECT * FROM c", parameters=[],
    )]
    assert len(docs) == 1


# ─── missing fields guard ───────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_mirror_returns_ignored_when_payload_missing_number(cosmos):
    outcome = await _mirror_github_issue(
        cosmos, project="ambience", repo="nelsong6/ambience",
        action="opened", issue_payload={"title": "no number here"},
    )
    assert outcome == {"ignored": "no issue number"}
    docs = [d async for d in cosmos.issues.query_items(
        "SELECT * FROM c", parameters=[],
    )]
    assert docs == []

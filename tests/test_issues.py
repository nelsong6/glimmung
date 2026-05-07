"""Glimmung-native issues substrate tests (#28).

Backed by the in-memory Cosmos fake. Covers the issue lifecycle
(create → read → update → close → reopen) plus list-open filtering.
"""

from __future__ import annotations

import asyncio
from types import SimpleNamespace

import pytest

from glimmung.issues import (
    add_comment,
    close_issue,
    create_issue,
    ensure_issue_number_counter_at_least,
    list_open_issues,
    next_issue_number,
    read_issue,
    read_issue_by_number,
    remove_comment,
    reopen_issue,
    update_comment,
    update_issue,
)
from glimmung.models import IssueSource, IssueState

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        issues=FakeContainer("issues", "/project"),
    )


# ─── create / read ──────────────────────────────────


@pytest.mark.asyncio
async def test_create_issue_persists_with_open_state_and_defaults(cosmos):
    issue = await create_issue(
        cosmos, project="ambience",
        title="agent-run.yml triage step is missing prior-feedback context",
        body="Repro: …",
    )
    assert issue.project == "ambience"
    assert issue.state == IssueState.OPEN
    assert issue.metadata.source == IssueSource.MANUAL
    assert issue.labels == []
    assert issue.comments == []
    assert issue.closed_at is None

    # Round-trip: read back by id.
    result = await read_issue(cosmos, project="ambience", issue_id=issue.id)
    assert result is not None
    fetched, etag = result
    assert fetched.id == issue.id
    assert fetched.title == issue.title
    assert etag  # opaque, just non-empty


@pytest.mark.asyncio
async def test_create_issue_rejects_github_import_source(cosmos):
    with pytest.raises(ValueError, match="GitHub Issues are disabled"):
        await create_issue(
            cosmos, project="ambience",
            title="imported from GitHub",
            source=IssueSource.GITHUB_WEBHOOK_IMPORT,
        )


async def test_create_issue_allocates_native_number(cosmos):
    issue = await create_issue(cosmos, project="ambience", title="native")
    assert issue.number == 1


@pytest.mark.asyncio
async def test_next_issue_number_seeds_counter_from_top_level_numbers_only(cosmos):
    await create_issue(cosmos, project="ambience", number=8, title="migrated")
    await cosmos.issues.create_item({
        "id": "legacy-without-number",
        "project": "ambience",
        "title": "legacy",
        "body": "",
        "labels": [],
        "state": "open",
        "metadata": {"source": "manual"},
        "comments": [],
        "created_at": "2026-01-01T00:00:00+00:00",
        "updated_at": "2026-01-01T00:00:00+00:00",
    })

    assert await next_issue_number(cosmos, project="ambience") == 9
    assert await next_issue_number(cosmos, project="ambience") == 10


@pytest.mark.asyncio
async def test_create_issue_with_explicit_number_advances_existing_counter(cosmos):
    first = await create_issue(cosmos, project="ambience", title="first")
    explicit = await create_issue(cosmos, project="ambience", number=8, title="imported")
    next_auto = await create_issue(cosmos, project="ambience", title="next")

    assert first.number == 1
    assert explicit.number == 8
    assert next_auto.number == 9


@pytest.mark.asyncio
async def test_ensure_issue_number_counter_at_least_does_not_rewind(cosmos):
    await create_issue(cosmos, project="ambience", title="first")
    await ensure_issue_number_counter_at_least(cosmos, project="ambience", number=1)

    assert await next_issue_number(cosmos, project="ambience") == 2


@pytest.mark.asyncio
async def test_next_issue_number_allocates_unique_values_concurrently(cosmos):
    numbers = await asyncio.gather(
        *[next_issue_number(cosmos, project="ambience") for _ in range(10)]
    )

    assert sorted(numbers) == list(range(1, 11))


@pytest.mark.asyncio
async def test_read_issue_by_number_uses_project_scoped_number(cosmos):
    issue = await create_issue(cosmos, project="ambience", title="native")
    found = await read_issue_by_number(cosmos, project="ambience", number=issue.number)
    assert found is not None
    fetched, _ = found
    assert fetched.id == issue.id


@pytest.mark.asyncio
async def test_read_issue_returns_none_for_missing(cosmos):
    assert await read_issue(cosmos, project="ambience", issue_id="01HZZZNOPE") is None


# ─── update ─────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_update_issue_patches_specified_fields_only(cosmos):
    issue = await create_issue(
        cosmos, project="ambience", title="orig", body="orig body", labels=["a"],
    )
    fetched, etag = await read_issue(cosmos, project="ambience", issue_id=issue.id)

    updated, new_etag = await update_issue(
        cosmos, issue=fetched, etag=etag,
        title="new title",
    )
    assert updated.title == "new title"
    assert updated.body == "orig body"          # unchanged
    assert updated.labels == ["a"]              # unchanged
    assert new_etag != etag


@pytest.mark.asyncio
@pytest.mark.asyncio
async def test_update_issue_replaces_labels_wholesale(cosmos):
    issue = await create_issue(
        cosmos, project="ambience", title="t", labels=["a", "b"],
    )
    fetched, etag = await read_issue(cosmos, project="ambience", issue_id=issue.id)
    updated, _ = await update_issue(cosmos, issue=fetched, etag=etag, labels=["c"])
    assert updated.labels == ["c"]


@pytest.mark.asyncio
async def test_update_issue_recovers_from_stale_etag_via_retry(cosmos):
    """If a caller arrives with a stale etag (someone else mutated the
    row in between), the retry loop re-reads and succeeds on the second
    attempt — the call shouldn't surface a conflict to the caller."""
    issue = await create_issue(cosmos, project="ambience", title="t")
    fetched, stale_etag = await read_issue(cosmos, project="ambience", issue_id=issue.id)

    # Concurrent mutator advances the etag.
    await update_issue(cosmos, issue=fetched, etag=stale_etag, title="winner")

    # Now retry with the original (stale) etag — the apply-side fields
    # collide, but the retry loop re-reads and writes anyway. Behavioral
    # contract: returns the post-retry state.
    second, _ = await update_issue(cosmos, issue=fetched, etag=stale_etag, title="second")
    assert second.title == "second"


# ─── close / reopen ─────────────────────────────────────────────


@pytest.mark.asyncio
async def test_add_comment_appends_to_issue(cosmos):
    issue = await create_issue(cosmos, project="ambience", title="t")
    fetched, etag = await read_issue(cosmos, project="ambience", issue_id=issue.id)

    updated, new_etag, comment = await add_comment(
        cosmos,
        issue=fetched,
        etag=etag,
        author="nelson@example.com",
        body="first note",
    )

    assert new_etag != etag
    assert len(updated.comments) == 1
    assert updated.comments[0] == comment
    assert comment.author == "nelson@example.com"
    assert comment.body == "first note"


@pytest.mark.asyncio
async def test_update_comment_replaces_body(cosmos):
    issue = await create_issue(cosmos, project="ambience", title="t")
    fetched, etag = await read_issue(cosmos, project="ambience", issue_id=issue.id)
    issue, etag, comment = await add_comment(
        cosmos, issue=fetched, etag=etag, author="a", body="old",
    )

    result = await update_comment(
        cosmos,
        issue=issue,
        etag=etag,
        comment_id=comment.id,
        body="new",
    )

    assert result is not None
    updated, _, updated_comment = result
    assert len(updated.comments) == 1
    assert updated.comments[0].body == "new"
    assert updated_comment.id == comment.id
    assert updated_comment.created_at == comment.created_at
    assert updated_comment.updated_at >= comment.updated_at


@pytest.mark.asyncio
async def test_remove_comment_deletes_by_id(cosmos):
    issue = await create_issue(cosmos, project="ambience", title="t")
    fetched, etag = await read_issue(cosmos, project="ambience", issue_id=issue.id)
    issue, etag, comment = await add_comment(
        cosmos, issue=fetched, etag=etag, author="a", body="first",
    )
    issue, etag, keep = await add_comment(
        cosmos, issue=issue, etag=etag, author="a", body="second",
    )

    result = await remove_comment(
        cosmos,
        issue=issue,
        etag=etag,
        comment_id=comment.id,
    )

    assert result is not None
    updated, _ = result
    assert [c.id for c in updated.comments] == [keep.id]


@pytest.mark.asyncio
async def test_comment_ops_return_none_for_missing_comment(cosmos):
    issue = await create_issue(cosmos, project="ambience", title="t")
    fetched, etag = await read_issue(cosmos, project="ambience", issue_id=issue.id)

    assert await update_comment(
        cosmos, issue=fetched, etag=etag, comment_id="missing", body="x",
    ) is None
    assert await remove_comment(
        cosmos, issue=fetched, etag=etag, comment_id="missing",
    ) is None


@pytest.mark.asyncio
async def test_close_issue_transitions_state_and_stamps_closed_at(cosmos):
    issue = await create_issue(cosmos, project="ambience", title="t")
    fetched, etag = await read_issue(cosmos, project="ambience", issue_id=issue.id)
    closed, _ = await close_issue(cosmos, issue=fetched, etag=etag)
    assert closed.state == IssueState.CLOSED
    assert closed.closed_at is not None


@pytest.mark.asyncio
async def test_close_issue_is_idempotent(cosmos):
    issue = await create_issue(cosmos, project="ambience", title="t")
    fetched, etag = await read_issue(cosmos, project="ambience", issue_id=issue.id)
    closed_once, etag1 = await close_issue(cosmos, issue=fetched, etag=etag)
    closed_twice, _ = await close_issue(cosmos, issue=closed_once, etag=etag1)
    assert closed_twice.state == IssueState.CLOSED
    assert closed_twice.closed_at == closed_once.closed_at  # preserved on re-close


@pytest.mark.asyncio
async def test_reopen_issue_clears_closed_at(cosmos):
    issue = await create_issue(cosmos, project="ambience", title="t")
    fetched, etag = await read_issue(cosmos, project="ambience", issue_id=issue.id)
    closed, etag = await close_issue(cosmos, issue=fetched, etag=etag)
    reopened, _ = await reopen_issue(cosmos, issue=closed, etag=etag)
    assert reopened.state == IssueState.OPEN
    assert reopened.closed_at is None


# ─── list_open ───────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_list_open_issues_filters_by_state(cosmos):
    a = await create_issue(cosmos, project="ambience", title="a")
    b = await create_issue(cosmos, project="ambience", title="b")
    fetched, etag = await read_issue(cosmos, project="ambience", issue_id=a.id)
    await close_issue(cosmos, issue=fetched, etag=etag)

    open_issues = await list_open_issues(cosmos, project="ambience")
    ids = [i.id for i in open_issues]
    assert ids == [b.id]


@pytest.mark.asyncio
async def test_list_open_issues_filters_by_project(cosmos):
    a = await create_issue(cosmos, project="ambience", title="ambience-1")
    await create_issue(cosmos, project="kill-me", title="killme-1")

    ambience_open = await list_open_issues(cosmos, project="ambience")
    assert [i.id for i in ambience_open] == [a.id]


@pytest.mark.asyncio
async def test_list_open_issues_no_project_returns_all_open(cosmos):
    a = await create_issue(cosmos, project="ambience", title="ambience-1")
    b = await create_issue(cosmos, project="kill-me", title="killme-1")
    open_all = await list_open_issues(cosmos)
    assert sorted(i.id for i in open_all) == sorted([a.id, b.id])


@pytest.mark.asyncio
async def test_list_open_issues_orders_oldest_first(cosmos):
    a = await create_issue(cosmos, project="ambience", title="first")
    b = await create_issue(cosmos, project="ambience", title="second")
    c = await create_issue(cosmos, project="ambience", title="third")
    open_issues = await list_open_issues(cosmos, project="ambience")
    assert [i.id for i in open_issues] == [a.id, b.id, c.id]

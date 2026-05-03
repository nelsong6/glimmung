"""Glimmung-native PRs substrate tests (#41).

Backed by the in-memory Cosmos fake. Covers the PR lifecycle (create →
read → update → close/merge → reopen) plus list filtering, the
`find_pr_by_repo_number` helper used by the webhook mirror, and the
`append_pr_comment` / `append_pr_review` primitives the consumer PR will
call on inbound conversation events.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

import pytest
from ulid import ULID

from glimmung.models import PRComment, PRReview, PRReviewState, PRState
from glimmung.prs import (
    append_pr_comment,
    append_pr_review,
    close_pr,
    create_pr,
    ensure_pr_for_github,
    find_pr_by_repo_number,
    github_pr_url_for,
    list_prs,
    list_open_prs,
    merge_pr,
    read_pr,
    reopen_pr,
    update_pr,
)

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        prs=FakeContainer("prs", "/project"),
    )


# ─── create / read ───────────────────────────────────────────────


@pytest.mark.asyncio
async def test_create_pr_persists_with_open_state_and_defaults(cosmos):
    pr = await create_pr(
        cosmos, project="ambience",
        repo="nelsong6/ambience", number=42,
        title="impl: triage step", branch="claude/triage-fix",
    )
    assert pr.project == "ambience"
    assert pr.state == PRState.OPEN
    assert pr.repo == "nelsong6/ambience"
    assert pr.number == 42
    assert pr.title == "impl: triage step"
    assert pr.branch == "claude/triage-fix"
    assert pr.base_ref == "main"
    assert pr.body == ""
    assert pr.head_sha == ""
    assert pr.html_url == ""
    assert pr.comments == []
    assert pr.reviews == []
    assert pr.merged_at is None
    assert pr.merged_by is None

    result = await read_pr(cosmos, project="ambience", pr_id=pr.id)
    assert result is not None
    fetched, etag = result
    assert fetched.id == pr.id
    assert fetched.title == pr.title
    assert etag


@pytest.mark.asyncio
async def test_create_pr_records_optional_fields(cosmos):
    pr = await create_pr(
        cosmos, project="ambience",
        repo="nelsong6/ambience", number=42,
        title="impl",
        branch="b",
        body="closes #41",
        base_ref="release",
        head_sha="abc123",
        html_url="https://github.com/nelsong6/ambience/pull/42",
    )
    assert pr.body == "closes #41"
    assert pr.base_ref == "release"
    assert pr.head_sha == "abc123"
    assert pr.html_url == "https://github.com/nelsong6/ambience/pull/42"


@pytest.mark.asyncio
async def test_read_pr_returns_none_for_missing(cosmos):
    assert await read_pr(cosmos, project="ambience", pr_id="01HZZZNOPE") is None


# ─── update ──────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_update_pr_patches_specified_fields_only(cosmos):
    pr = await create_pr(
        cosmos, project="ambience",
        repo="nelsong6/ambience", number=1,
        title="orig", branch="b", body="orig body", head_sha="aaa",
    )
    fetched, etag = await read_pr(cosmos, project="ambience", pr_id=pr.id)

    updated, new_etag = await update_pr(
        cosmos, pr=fetched, etag=etag,
        head_sha="bbb",
    )
    assert updated.head_sha == "bbb"      # changed
    assert updated.title == "orig"        # untouched
    assert updated.body == "orig body"    # untouched
    assert updated.branch == "b"          # untouched
    assert new_etag != etag


@pytest.mark.asyncio
async def test_update_pr_recovers_from_stale_etag_via_retry(cosmos):
    """If a caller arrives with a stale etag (concurrent mutation), the
    retry loop re-reads and succeeds on the second attempt."""
    pr = await create_pr(
        cosmos, project="ambience",
        repo="r/n", number=1, title="t", branch="b",
    )
    fetched, stale_etag = await read_pr(cosmos, project="ambience", pr_id=pr.id)
    await update_pr(cosmos, pr=fetched, etag=stale_etag, title="winner")

    second, _ = await update_pr(cosmos, pr=fetched, etag=stale_etag, title="second")
    assert second.title == "second"


# ─── close / merge / reopen ──────────────────────────────────────


@pytest.mark.asyncio
async def test_close_pr_transitions_state_without_merge(cosmos):
    pr = await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=pr.id)
    closed, _ = await close_pr(cosmos, pr=fetched, etag=etag)
    assert closed.state == PRState.CLOSED
    assert closed.merged_at is None        # close-without-merge
    assert closed.merged_by is None


@pytest.mark.asyncio
async def test_close_pr_is_idempotent(cosmos):
    pr = await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=pr.id)
    closed_once, etag1 = await close_pr(cosmos, pr=fetched, etag=etag)
    closed_twice, _ = await close_pr(cosmos, pr=closed_once, etag=etag1)
    assert closed_twice.state == PRState.CLOSED


@pytest.mark.asyncio
async def test_merge_pr_stamps_merged_at_and_merged_by(cosmos):
    pr = await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=pr.id)

    merge_time = datetime(2026, 5, 1, 12, 0, tzinfo=UTC)
    merged, _ = await merge_pr(
        cosmos, pr=fetched, etag=etag,
        merged_by="claude[bot]",
        merged_at=merge_time,
    )
    assert merged.state == PRState.CLOSED
    assert merged.merged_at == merge_time
    assert merged.merged_by == "claude[bot]"


@pytest.mark.asyncio
async def test_merge_pr_preserves_existing_merge_metadata_on_redelivery(cosmos):
    """Webhook re-delivery must not overwrite the original merge
    timestamp / author with a later one."""
    pr = await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=pr.id)

    first_merge = datetime(2026, 5, 1, 12, 0, tzinfo=UTC)
    merged_first, etag1 = await merge_pr(
        cosmos, pr=fetched, etag=etag,
        merged_by="alice", merged_at=first_merge,
    )
    second_merge = datetime(2026, 5, 1, 12, 5, tzinfo=UTC)
    merged_second, _ = await merge_pr(
        cosmos, pr=merged_first, etag=etag1,
        merged_by="bob", merged_at=second_merge,
    )
    assert merged_second.merged_at == first_merge
    assert merged_second.merged_by == "alice"
    assert merged_second.state == PRState.CLOSED


@pytest.mark.asyncio
async def test_reopen_pr_transitions_back_to_open(cosmos):
    pr = await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=pr.id)
    closed, etag = await close_pr(cosmos, pr=fetched, etag=etag)
    reopened, _ = await reopen_pr(cosmos, pr=closed, etag=etag)
    assert reopened.state == PRState.OPEN


# ─── list_open ────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_list_prs_includes_closed_prs(cosmos):
    a = await create_pr(cosmos, project="a", repo="r/n", number=1, title="a", branch="b1")
    b = await create_pr(cosmos, project="a", repo="r/n", number=2, title="b", branch="b2")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=a.id)
    await close_pr(cosmos, pr=fetched, etag=etag)

    prs = await list_prs(cosmos, project="a")
    assert sorted(p.id for p in prs) == sorted([a.id, b.id])
    assert {p.id: p.state for p in prs}[a.id] == PRState.CLOSED


@pytest.mark.asyncio
async def test_list_open_prs_filters_by_state(cosmos):
    a = await create_pr(cosmos, project="a", repo="r/n", number=1, title="a", branch="b1")
    b = await create_pr(cosmos, project="a", repo="r/n", number=2, title="b", branch="b2")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=a.id)
    await close_pr(cosmos, pr=fetched, etag=etag)

    open_prs = await list_open_prs(cosmos, project="a")
    assert [p.id for p in open_prs] == [b.id]


@pytest.mark.asyncio
async def test_list_open_prs_filters_by_project(cosmos):
    a = await create_pr(cosmos, project="a", repo="r/n", number=1, title="a", branch="b1")
    await create_pr(cosmos, project="other", repo="r/n", number=2, title="o", branch="b2")
    open_a = await list_open_prs(cosmos, project="a")
    assert [p.id for p in open_a] == [a.id]


@pytest.mark.asyncio
async def test_list_open_prs_no_project_returns_all_open(cosmos):
    a = await create_pr(cosmos, project="a", repo="r/n", number=1, title="a", branch="b1")
    b = await create_pr(cosmos, project="b", repo="r/n", number=2, title="b", branch="b2")
    open_all = await list_open_prs(cosmos)
    assert sorted(p.id for p in open_all) == sorted([a.id, b.id])


# ─── find_pr_by_repo_number ────────────────────────────────────


@pytest.mark.asyncio
async def test_find_pr_by_repo_number_returns_matching(cosmos):
    expected = await create_pr(
        cosmos, project="a", repo="nelsong6/ambience", number=42,
        title="t", branch="b",
    )
    # decoy: same repo, different number
    await create_pr(
        cosmos, project="a", repo="nelsong6/ambience", number=99,
        title="d", branch="b2",
    )
    result = await find_pr_by_repo_number(cosmos, repo="nelsong6/ambience", number=42)
    assert result is not None
    pr, etag = result
    assert pr.id == expected.id
    assert etag


@pytest.mark.asyncio
async def test_find_pr_by_repo_number_crosses_partitions(cosmos):
    """Webhook payload doesn't carry the glimmung project name, so the
    lookup must scan across `/project` partitions."""
    expected = await create_pr(
        cosmos, project="kill-me", repo="nelsong6/kill-me", number=7,
        title="killme PR", branch="b",
    )
    await create_pr(
        cosmos, project="ambience", repo="nelsong6/ambience", number=7,
        title="unrelated same number", branch="b",
    )
    result = await find_pr_by_repo_number(cosmos, repo="nelsong6/kill-me", number=7)
    assert result is not None
    pr, _ = result
    assert pr.id == expected.id
    assert pr.project == "kill-me"


@pytest.mark.asyncio
async def test_find_pr_by_repo_number_returns_none_for_missing(cosmos):
    await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    result = await find_pr_by_repo_number(cosmos, repo="r/n", number=9999)
    assert result is None


# ─── ensure_pr_for_github ─────────────────────────────────────


@pytest.mark.asyncio
async def test_ensure_pr_for_github_creates_when_missing(cosmos):
    pr, etag, created = await ensure_pr_for_github(
        cosmos, project="ambience",
        repo="nelsong6/ambience", number=42,
        title="impl",
        branch="claude/fix",
    )
    assert created is True
    assert etag
    assert pr.repo == "nelsong6/ambience"
    assert pr.number == 42
    assert pr.title == "impl"
    assert pr.branch == "claude/fix"


@pytest.mark.asyncio
async def test_ensure_pr_for_github_returns_existing(cosmos):
    """Second call on the same (repo, number) returns the existing PR
    with `created=False` — keeps the webhook-mirror path idempotent
    across redeliveries."""
    first, _, created_first = await ensure_pr_for_github(
        cosmos, project="ambience",
        repo="nelsong6/ambience", number=42,
        title="impl", branch="b",
    )
    assert created_first is True

    second, etag, created_second = await ensure_pr_for_github(
        cosmos, project="ambience",
        repo="nelsong6/ambience", number=42,
        title="ignored — only applies on create",
        branch="b",
    )
    assert created_second is False
    assert second.id == first.id
    assert second.title == "impl"          # original title preserved
    assert etag


@pytest.mark.asyncio
async def test_ensure_pr_for_github_uses_default_title_when_omitted(cosmos):
    pr, _, _ = await ensure_pr_for_github(
        cosmos, project="ambience",
        repo="nelsong6/ambience", number=42,
        branch="b",
    )
    assert pr.title == "nelsong6/ambience#42"


# ─── append_pr_comment ────────────────────────────────────────


def _comment(gh_id: int | None, body: str = "looks good") -> PRComment:
    return PRComment(
        id=str(ULID()),
        gh_id=gh_id,
        author="alice",
        body=body,
        created_at=datetime.now(UTC),
    )


@pytest.mark.asyncio
async def test_append_pr_comment_appends(cosmos):
    pr = await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=pr.id)
    updated, _ = await append_pr_comment(
        cosmos, pr=fetched, etag=etag, comment=_comment(gh_id=100),
    )
    assert len(updated.comments) == 1
    assert updated.comments[0].gh_id == 100


@pytest.mark.asyncio
async def test_append_pr_comment_dedupes_on_gh_id(cosmos):
    """Webhook re-delivery for the same `gh_id` must not double-write
    the comment."""
    pr = await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=pr.id)
    once, etag1 = await append_pr_comment(
        cosmos, pr=fetched, etag=etag, comment=_comment(gh_id=100, body="first"),
    )
    twice, _ = await append_pr_comment(
        cosmos, pr=once, etag=etag1, comment=_comment(gh_id=100, body="re-delivery"),
    )
    assert len(twice.comments) == 1
    assert twice.comments[0].body == "first"   # original wins


@pytest.mark.asyncio
async def test_append_pr_comment_allows_multiple_without_gh_id(cosmos):
    """Comments without `gh_id` are glimmung-internal annotations; the
    dedupe check is `gh_id is not None`, so they always append."""
    pr = await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=pr.id)
    once, etag1 = await append_pr_comment(
        cosmos, pr=fetched, etag=etag, comment=_comment(gh_id=None, body="first"),
    )
    twice, _ = await append_pr_comment(
        cosmos, pr=once, etag=etag1, comment=_comment(gh_id=None, body="second"),
    )
    assert len(twice.comments) == 2


# ─── append_pr_review ─────────────────────────────────────────


def _review(gh_id: int | None, state: PRReviewState = PRReviewState.APPROVED) -> PRReview:
    return PRReview(
        id=str(ULID()),
        gh_id=gh_id,
        author="bob",
        state=state,
        submitted_at=datetime.now(UTC),
    )


@pytest.mark.asyncio
async def test_append_pr_review_appends(cosmos):
    pr = await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=pr.id)
    updated, _ = await append_pr_review(
        cosmos, pr=fetched, etag=etag, review=_review(gh_id=200),
    )
    assert len(updated.reviews) == 1
    assert updated.reviews[0].state == PRReviewState.APPROVED


@pytest.mark.asyncio
async def test_append_pr_review_dedupes_on_gh_id(cosmos):
    pr = await create_pr(cosmos, project="a", repo="r/n", number=1, title="t", branch="b")
    fetched, etag = await read_pr(cosmos, project="a", pr_id=pr.id)
    once, etag1 = await append_pr_review(
        cosmos, pr=fetched, etag=etag, review=_review(gh_id=200, state=PRReviewState.APPROVED),
    )
    twice, _ = await append_pr_review(
        cosmos, pr=once, etag=etag1, review=_review(gh_id=200, state=PRReviewState.CHANGES_REQUESTED),
    )
    assert len(twice.reviews) == 1
    assert twice.reviews[0].state == PRReviewState.APPROVED


# ─── github_pr_url_for ───────────────────────────────────────


def test_github_pr_url_for_renders_canonical_form():
    assert github_pr_url_for("nelsong6/ambience", 42) == "https://github.com/nelsong6/ambience/pull/42"

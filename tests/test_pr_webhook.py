"""PR webhook + signal retargeting tests (#50 slice 3).

Pre-#50 the `pull_request.*` handler parsed `Closes #N` from PR bodies
to link Runs to PRs by GH coords. Post-#50 the handler mirrors GH's
PR lifecycle into the glimmung `prs` container and the run/issue
linkage is set explicitly by the agent (slice 4) — these tests pin
the new mirror behavior + the post-#50 signal shape (target_repo =
project name, target_id = glimmung PR id).
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace
from unittest.mock import patch

import pytest

from glimmung import prs as pr_ops
from glimmung.app import (
    _handle_pull_request,
    _handle_pull_request_review,
    _resolve_signal_pr,
)
from glimmung.models import (
    PRReviewState,
    PRState,
    Signal,
    SignalSource,
    SignalState,
    SignalTargetType,
)

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        prs=FakeContainer("prs", "/project"),
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


async def _register_project(cosmos, name: str, repo: str) -> None:
    await cosmos.projects.create_item({
        "id": name,
        "name": name,
        "githubRepo": repo,
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


def _pr_payload(
    *,
    action: str = "opened",
    repo: str = "nelsong6/ambience",
    number: int = 14,
    title: str = "agent: fix the dispatcher",
    body: str = "spec link: https://glimmung.romaine.life/prs/ambience/01JABC",
    branch: str = "agent/issue-7",
    base: str = "main",
    head_sha: str = "abc123",
    merged: bool = False,
    merged_by: str | None = None,
    state: str = "open",
) -> dict:
    return {
        "action": action,
        "repository": {"full_name": repo},
        "pull_request": {
            "number": number,
            "title": title,
            "body": body,
            "head": {"ref": branch, "sha": head_sha},
            "base": {"ref": base},
            "html_url": f"https://github.com/{repo}/pull/{number}",
            "merged": merged,
            "merged_by": {"login": merged_by} if merged_by else None,
            "state": state,
        },
    }


# ─── _handle_pull_request: opened / reopened / closed ──────────────────────


@pytest.mark.asyncio
async def test_pr_opened_creates_glimmung_pr_doc(cosmos, app_state):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    with patch("glimmung.app.app", app_state):
        outcome = await _handle_pull_request(_pr_payload(action="opened"))

    assert outcome["created"] is True
    found = await pr_ops.find_pr_by_repo_number(
        cosmos, repo="nelsong6/ambience", number=14,
    )
    assert found is not None
    pr, _ = found
    assert pr.title == "agent: fix the dispatcher"
    assert pr.branch == "agent/issue-7"
    assert pr.base_ref == "main"
    assert pr.head_sha == "abc123"
    assert pr.html_url == "https://github.com/nelsong6/ambience/pull/14"
    assert pr.state == PRState.OPEN
    # Linkage stays unset — the agent's POST /v1/prs (slice 4) is the
    # authoritative source for linked_run_id / linked_issue_id.
    assert pr.linked_run_id is None
    assert pr.linked_issue_id is None


@pytest.mark.asyncio
async def test_pr_opened_twice_does_not_duplicate(cosmos, app_state):
    """Webhook redeliveries are idempotent — same `(repo, number)`
    surfaces the existing PR instead of minting a second."""
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    with patch("glimmung.app.app", app_state):
        first = await _handle_pull_request(_pr_payload(action="opened"))
        second = await _handle_pull_request(_pr_payload(action="opened", title="renamed"))

    assert first["created"] is True
    assert second["created"] is False
    docs = [d async for d in cosmos.prs.query_items("SELECT * FROM c", parameters=[])]
    assert len(docs) == 1
    # The second call patches title to match GH.
    assert docs[0]["title"] == "renamed"


@pytest.mark.asyncio
async def test_pr_synchronize_refreshes_head_sha(cosmos, app_state):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    with patch("glimmung.app.app", app_state):
        await _handle_pull_request(_pr_payload(action="opened", head_sha="aaa111"))
        outcome = await _handle_pull_request(_pr_payload(
            action="synchronize", head_sha="bbb222",
        ))

    assert outcome["patched"] is True
    found = await pr_ops.find_pr_by_repo_number(
        cosmos, repo="nelsong6/ambience", number=14,
    )
    pr, _ = found
    assert pr.head_sha == "bbb222"


@pytest.mark.asyncio
async def test_pr_closed_without_merge_transitions_to_closed(cosmos, app_state):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    with patch("glimmung.app.app", app_state):
        await _handle_pull_request(_pr_payload(action="opened"))
        outcome = await _handle_pull_request(_pr_payload(
            action="closed", merged=False, state="closed",
        ))

    assert outcome["closed"] is True
    found = await pr_ops.find_pr_by_repo_number(
        cosmos, repo="nelsong6/ambience", number=14,
    )
    pr, _ = found
    assert pr.state == PRState.CLOSED
    assert pr.merged_at is None


@pytest.mark.asyncio
async def test_pr_closed_with_merge_stamps_merged_metadata(cosmos, app_state):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    with patch("glimmung.app.app", app_state):
        await _handle_pull_request(_pr_payload(action="opened"))
        outcome = await _handle_pull_request(_pr_payload(
            action="closed", merged=True, merged_by="nelsong6", state="closed",
        ))

    assert outcome["merged"] is True
    found = await pr_ops.find_pr_by_repo_number(
        cosmos, repo="nelsong6/ambience", number=14,
    )
    pr, _ = found
    assert pr.state == PRState.CLOSED
    assert pr.merged_at is not None
    assert pr.merged_by == "nelsong6"


@pytest.mark.asyncio
async def test_pr_reopened_on_merged_pr_is_a_no_op(cosmos, app_state):
    """GH doesn't fire `reopened` for merged PRs in practice but the
    handler guards anyway — merged PRs cannot be reopened."""
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    with patch("glimmung.app.app", app_state):
        await _handle_pull_request(_pr_payload(action="opened"))
        await _handle_pull_request(_pr_payload(
            action="closed", merged=True, merged_by="nelsong6", state="closed",
        ))
        outcome = await _handle_pull_request(_pr_payload(
            action="reopened", state="open",
        ))

    assert outcome.get("reopen_ignored") == "merged"
    found = await pr_ops.find_pr_by_repo_number(
        cosmos, repo="nelsong6/ambience", number=14,
    )
    pr, _ = found
    # State stays CLOSED (merged) — no transition.
    assert pr.state == PRState.CLOSED
    assert pr.merged_at is not None


@pytest.mark.asyncio
async def test_pr_reopened_after_close_without_merge_transitions_to_open(
    cosmos, app_state,
):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    with patch("glimmung.app.app", app_state):
        await _handle_pull_request(_pr_payload(action="opened"))
        await _handle_pull_request(_pr_payload(
            action="closed", merged=False, state="closed",
        ))
        outcome = await _handle_pull_request(_pr_payload(
            action="reopened", state="open",
        ))

    assert outcome.get("reopened") is True
    found = await pr_ops.find_pr_by_repo_number(
        cosmos, repo="nelsong6/ambience", number=14,
    )
    pr, _ = found
    assert pr.state == PRState.OPEN


@pytest.mark.asyncio
async def test_pr_event_for_unregistered_repo_is_ignored(cosmos, app_state):
    with patch("glimmung.app.app", app_state):
        outcome = await _handle_pull_request(_pr_payload(
            action="opened", repo="someone-else/private",
        ))
    assert outcome == {"ignored": "no project for repo"}


# ─── _handle_pull_request_review: post-#50 signal targeting ────────────────


@pytest.mark.asyncio
async def test_review_signal_targets_glimmung_pr_id_when_pr_exists(
    cosmos, app_state,
):
    """The post-#50 signal carries `(project, glimmung_pr_id)` — the
    drain looks up the glimmung PR doc directly rather than going
    through GH coords."""
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    pr = await pr_ops.create_pr(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=42, title="t", branch="b",
    )
    review_payload = {
        "action": "submitted",
        "repository": {"full_name": "nelsong6/ambience"},
        "pull_request": {"number": 42},
        "review": {
            "state": "changes_requested",
            "body": "this needs another iteration",
            "user": {"login": "nelsong6"},
            "id": 99999,
        },
    }
    with patch("glimmung.app.app", app_state):
        outcome = await _handle_pull_request_review(review_payload)

    assert outcome["mirrored_review"] is True
    sig_id = outcome["enqueued_signal"]
    docs = [d async for d in cosmos.signals.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": sig_id}],
    )]
    assert len(docs) == 1
    sig = docs[0]
    assert sig["target_type"] == "pr"
    assert sig["target_repo"] == "ambience"        # project name, not GH repo
    assert sig["target_id"] == pr.id               # glimmung PR id, not GH number
    fetched, _ = await pr_ops.read_pr(cosmos, project="ambience", pr_id=pr.id)
    assert len(fetched.reviews) == 1
    mirrored = fetched.reviews[0]
    assert mirrored.gh_id == 99999
    assert mirrored.author == "nelsong6"
    assert mirrored.state == PRReviewState.CHANGES_REQUESTED
    assert mirrored.body == "this needs another iteration"


@pytest.mark.asyncio
async def test_review_mirror_dedupes_on_redelivery(cosmos, app_state):
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    pr = await pr_ops.create_pr(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=42, title="t", branch="b",
    )
    review_payload = {
        "action": "submitted",
        "repository": {"full_name": "nelsong6/ambience"},
        "pull_request": {"number": 42},
        "review": {
            "state": "approved",
            "body": "ship it",
            "user": {"login": "reviewer"},
            "id": 12345,
            "submitted_at": "2026-05-03T02:00:00Z",
        },
    }
    with patch("glimmung.app.app", app_state):
        first = await _handle_pull_request_review(review_payload)
        second = await _handle_pull_request_review({
            **review_payload,
            "review": {
                **review_payload["review"],
                "state": "changes_requested",
                "body": "redelivery should not replace original",
            },
        })

    assert first["mirrored_review"] is True
    assert second["mirrored_review"] is True
    fetched, _ = await pr_ops.read_pr(cosmos, project="ambience", pr_id=pr.id)
    assert len(fetched.reviews) == 1
    assert fetched.reviews[0].state == PRReviewState.APPROVED
    assert fetched.reviews[0].body == "ship it"


@pytest.mark.asyncio
async def test_review_signal_falls_back_to_gh_coords_when_no_glimmung_pr(
    cosmos, app_state,
):
    """If the webhook handler races and the glimmung PR isn't there
    yet (rare; ensure_pr_for_github usually wins), the signal falls
    back to the legacy `(repo, gh_pr_number)` shape so it's not lost.
    The drain accepts both shapes."""
    await _register_project(cosmos, "ambience", "nelsong6/ambience")
    review_payload = {
        "action": "submitted",
        "repository": {"full_name": "nelsong6/ambience"},
        "pull_request": {"number": 99},
        "review": {"state": "changes_requested", "user": {"login": "x"}, "id": 1},
    }
    with patch("glimmung.app.app", app_state):
        outcome = await _handle_pull_request_review(review_payload)

    assert outcome["mirrored_review"] is False
    sig_id = outcome["enqueued_signal"]
    docs = [d async for d in cosmos.signals.query_items(
        "SELECT * FROM c WHERE c.id = @id",
        parameters=[{"name": "@id", "value": sig_id}],
    )]
    sig = docs[0]
    assert sig["target_repo"] == "nelsong6/ambience"  # legacy shape
    assert sig["target_id"] == "99"


# ─── _resolve_signal_pr: dual-shape lookup ──────────────────────────────


def _signal(*, target_repo: str, target_id: str) -> Signal:
    return Signal(
        id="01JSIGNAL0",
        target_type=SignalTargetType.PR,
        target_repo=target_repo,
        target_id=target_id,
        source=SignalSource.GH_REVIEW,
        payload={},
        state=SignalState.PENDING,
        enqueued_at=datetime.now(UTC),
    )


@pytest.mark.asyncio
async def test_resolve_signal_pr_handles_glimmung_id_shape(cosmos):
    """ULID-shaped target_id resolves through `prs.read_pr` and pulls
    in the linked Run when present."""
    await pr_ops.create_pr(
        cosmos, project="ambience", repo="nelsong6/ambience",
        number=14, title="t", branch="b", linked_run_id="01JRUN00000",
    )
    run_doc = {
        "id": "01JRUN00000",
        "project": "ambience",
        "issue_repo": "nelsong6/ambience",
        "issue_number": 7,
        "issue_id": "01JISS00000",
        "workflow": "issue-agent",
        "state": "passed",
        "attempts": [],
        "cumulative_cost_usd": 0.0,
        "schema_version": 1,
        "created_at": datetime.now(UTC).isoformat(),
        "updated_at": datetime.now(UTC).isoformat(),
        "budget": {"max_attempts": 3, "max_cost_usd": 25.0},
    }
    await cosmos.runs.create_item(run_doc)
    found = await pr_ops.find_pr_by_repo_number(
        cosmos, repo="nelsong6/ambience", number=14,
    )
    pr, _ = found

    signal = _signal(target_repo="ambience", target_id=pr.id)
    resolved = await _resolve_signal_pr(cosmos, signal)
    assert resolved is not None
    repo, pr_number, run, etag = resolved
    assert repo == "nelsong6/ambience"
    assert pr_number == 14
    assert run is not None
    assert run.id == "01JRUN00000"
    assert etag is not None


@pytest.mark.asyncio
async def test_resolve_signal_pr_handles_legacy_gh_number_shape(cosmos):
    """Numeric target_id with a GH-repo target_repo resolves through
    the legacy `find_run_by_pr` lookup. Pre-#50 in-flight signals
    drain cleanly through this branch."""
    run_doc = {
        "id": "01JRUNLEGACY",
        "project": "ambience",
        "issue_repo": "nelsong6/ambience",
        "issue_number": 7,
        "issue_id": "",
        "workflow": "issue-agent",
        "state": "passed",
        "attempts": [],
        "pr_number": 14,
        "pr_branch": "agent/issue-7",
        "cumulative_cost_usd": 0.0,
        "schema_version": 1,
        "created_at": datetime.now(UTC).isoformat(),
        "updated_at": datetime.now(UTC).isoformat(),
        "budget": {"max_attempts": 3, "max_cost_usd": 25.0},
    }
    await cosmos.runs.create_item(run_doc)

    signal = _signal(target_repo="nelsong6/ambience", target_id="14")
    resolved = await _resolve_signal_pr(cosmos, signal)
    assert resolved is not None
    repo, pr_number, run, etag = resolved
    assert repo == "nelsong6/ambience"
    assert pr_number == 14
    assert run is not None
    assert run.id == "01JRUNLEGACY"


@pytest.mark.asyncio
async def test_resolve_signal_pr_returns_none_for_unknown_glimmung_id(cosmos):
    signal = _signal(target_repo="ambience", target_id="01JNOTREALPRIDX0000000000")
    resolved = await _resolve_signal_pr(cosmos, signal)
    assert resolved is None

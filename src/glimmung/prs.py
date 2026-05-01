"""Glimmung-native PRs (#41).

Cosmos-backed CRUD for the `prs` container. Mirrors the Issue substrate
(#28): glimmung is the source of truth for PR conversation (title, body,
state, reviews, comments), GitHub is one syndication target. The detail
read path will source from this container with no live-GH stitch; the
iteration-graph viewer (#43) will key its PR-conversation nodes off the
docs stored here.

API:
- `create_pr(...)` — mint a new PR.
- `read_pr(...)` — point-read by `(project, pr_id)`. Returns `(PR, etag)`
  so callers can chain into a write without re-reading.
- `update_pr(...)` — patch title / body / branch / base_ref / head_sha /
  html_url. Etag-validated.
- `close_pr(...)` — state OPEN → CLOSED without merge (e.g. PR rejected).
- `merge_pr(...)` — state OPEN → CLOSED with `merged_at` + `merged_by` set.
- `reopen_pr(...)` — CLOSED → OPEN. Only meaningful for never-merged PRs;
  GH does not allow reopening a merged PR, so callers should not call this
  on a PR with `merged_at` set. The function does not enforce that — the
  contract belongs at the webhook-mirror call site, where we know whether
  the inbound `pull_request.reopened` event refers to a merged PR.
- `list_open_prs(project=None)` — list open PRs, optionally project-scoped.
- `find_pr_by_repo_number(...)` — cross-partition lookup by `(repo, number)`.
  Used by the webhook mirror in the consumer PR to find the glimmung PR
  for an inbound GH event.
- `ensure_pr_for_github(...)` — find or mint a glimmung PR mirroring a GH
  PR. Returns `(pr, etag, created)`. Used by the webhook mirror to
  guarantee a PR exists before appending comments/reviews.
- `append_pr_comment(...)`, `append_pr_review(...)` — append to the
  embedded conversation lists, deduping on `gh_id` so webhook re-deliveries
  are idempotent.

`find_pr_by_run_id` is intentionally not in this PR — that helper requires
`Run.pr_id`, which lands in the consumer PR that wires Run ↔ PR linkage
on `Closes #N` parsing. Adding the lookup here without a working linkage
field would be a stub.

Concurrency model: same shape as `runs.py` / `issues.py` — `_etag` +
`IfNotModified` on every mutating write, with a small retry loop on the
rare conflict.
"""

from __future__ import annotations

import logging
from datetime import UTC, datetime
from typing import Any, Callable

from azure.core import MatchConditions
from azure.cosmos.exceptions import CosmosAccessConditionFailedError, CosmosResourceNotFoundError
from ulid import ULID

from glimmung.db import Cosmos, query_all
from glimmung.models import PR, PRComment, PRReview, PRState

log = logging.getLogger(__name__)


_MAX_CONFLICT_RETRIES = 3


def _now() -> datetime:
    return datetime.now(UTC)


def _strip_meta(doc: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in doc.items() if not k.startswith("_")}


async def create_pr(
    cosmos: Cosmos,
    *,
    project: str,
    repo: str,
    number: int,
    title: str,
    branch: str,
    body: str = "",
    base_ref: str = "main",
    head_sha: str = "",
    html_url: str = "",
) -> PR:
    """Mint a new PR in OPEN state. Returns the persisted PR.

    `repo` and `number` are GH coords (PRs are inherently a GH concept; see
    the model banner). The webhook-mirror consumer PR threads them through
    on `pull_request.opened`. Direct callers (none today, but glimmung-side
    PR creation is on the long-term roadmap) supply them too."""
    now = _now()
    pr = PR(
        id=str(ULID()),
        project=project,
        repo=repo,
        number=number,
        title=title,
        body=body,
        state=PRState.OPEN,
        branch=branch,
        base_ref=base_ref,
        head_sha=head_sha,
        html_url=html_url,
        created_at=now,
        updated_at=now,
    )
    await cosmos.prs.create_item(pr.model_dump(mode="json"))
    log.info(
        "created PR %s in project=%s (%s#%d)",
        pr.id, project, repo, number,
    )
    return pr


async def read_pr(
    cosmos: Cosmos,
    *,
    project: str,
    pr_id: str,
) -> tuple[PR, str] | None:
    """Point-read a PR. Returns `(pr, etag)` or `None` if missing."""
    try:
        doc = await cosmos.prs.read_item(item=pr_id, partition_key=project)
    except CosmosResourceNotFoundError:
        return None
    return PR.model_validate(_strip_meta(doc)), doc["_etag"]


async def update_pr(
    cosmos: Cosmos,
    *,
    pr: PR,
    etag: str,
    title: str | None = None,
    body: str | None = None,
    branch: str | None = None,
    base_ref: str | None = None,
    head_sha: str | None = None,
    html_url: str | None = None,
) -> tuple[PR, str]:
    """Patch fields on a PR. `None` means "don't change"; pass an empty
    string to actually clear a field. State transitions (close / merge /
    reopen) go through their dedicated functions so the timestamp
    invariants stay obvious at the call site."""
    def apply(p: PR) -> PR:
        updates: dict[str, Any] = {"updated_at": _now()}
        if title is not None:
            updates["title"] = title
        if body is not None:
            updates["body"] = body
        if branch is not None:
            updates["branch"] = branch
        if base_ref is not None:
            updates["base_ref"] = base_ref
        if head_sha is not None:
            updates["head_sha"] = head_sha
        if html_url is not None:
            updates["html_url"] = html_url
        return p.model_copy(update=updates)

    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def close_pr(
    cosmos: Cosmos,
    *,
    pr: PR,
    etag: str,
) -> tuple[PR, str]:
    """Transition OPEN → CLOSED without merge. For "PR rejected" / author-
    closed flows. `merged_at` and `merged_by` stay `None`. Idempotent:
    closing an already-closed PR is a no-op state-wise (still bumps
    `updated_at` so the caller gets a fresh etag). Use `merge_pr` instead
    if the close is the result of a merge."""
    def apply(p: PR) -> PR:
        if p.state == PRState.CLOSED:
            return p.model_copy(update={"updated_at": _now()})
        return p.model_copy(update={
            "state": PRState.CLOSED,
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def merge_pr(
    cosmos: Cosmos,
    *,
    pr: PR,
    etag: str,
    merged_by: str,
    merged_at: datetime | None = None,
) -> tuple[PR, str]:
    """Transition OPEN → CLOSED with `merged_at` + `merged_by` stamped.
    `merged_at` defaults to now, but the webhook-mirror call site should
    pass the GH-event timestamp instead so the audit trail matches what
    GitHub recorded. Idempotent: if the PR is already merged, the existing
    merge metadata is preserved (we don't overwrite earlier `merged_at`
    with a later re-delivery)."""
    def apply(p: PR) -> PR:
        if p.merged_at is not None:
            return p.model_copy(update={"updated_at": _now()})
        return p.model_copy(update={
            "state": PRState.CLOSED,
            "merged_at": merged_at or _now(),
            "merged_by": merged_by,
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def reopen_pr(
    cosmos: Cosmos,
    *,
    pr: PR,
    etag: str,
) -> tuple[PR, str]:
    """Transition CLOSED → OPEN. Caller's responsibility to ensure the PR
    was not merged (GH does not allow reopening merged PRs, so an inbound
    `pull_request.reopened` event for a merged PR is a webhook anomaly to
    log + drop, not a state transition)."""
    def apply(p: PR) -> PR:
        if p.state == PRState.OPEN:
            return p.model_copy(update={"updated_at": _now()})
        return p.model_copy(update={
            "state": PRState.OPEN,
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def list_open_prs(
    cosmos: Cosmos,
    *,
    project: str | None = None,
) -> list[PR]:
    """Return all OPEN PRs, oldest-first. Single-partition if `project` is
    set; cross-partition otherwise (used by the global PRs view)."""
    if project is not None:
        docs = await query_all(
            cosmos.prs,
            "SELECT * FROM c WHERE c.project = @p AND c.state = @s ORDER BY c.created_at ASC",
            parameters=[
                {"name": "@p", "value": project},
                {"name": "@s", "value": PRState.OPEN.value},
            ],
        )
    else:
        docs = await query_all(
            cosmos.prs,
            "SELECT * FROM c WHERE c.state = @s ORDER BY c.created_at ASC",
            parameters=[{"name": "@s", "value": PRState.OPEN.value}],
        )
    return [PR.model_validate(_strip_meta(d)) for d in docs]


async def find_pr_by_repo_number(
    cosmos: Cosmos,
    *,
    repo: str,
    number: int,
) -> tuple[PR, str] | None:
    """Cross-partition lookup keyed off `(repo, number)`. The webhook
    mirror uses this to find the glimmung PR for an inbound GH event
    (the event payload carries `repo` + `number`, not the glimmung id).
    Returns `(pr, etag)` or `None`."""
    docs = await query_all(
        cosmos.prs,
        "SELECT * FROM c WHERE c.repo = @r AND c.number = @n",
        parameters=[
            {"name": "@r", "value": repo},
            {"name": "@n", "value": number},
        ],
    )
    if not docs:
        return None
    if len(docs) > 1:
        # Two glimmung PRs pointing at the same GH PR is a semantic error
        # — log loudly and pick the oldest. The webhook-import path in the
        # consumer PR should de-duplicate on import via this same lookup.
        log.warning(
            "multiple glimmung PRs link to %s#%d: %s",
            repo, number, [d["id"] for d in docs],
        )
        docs.sort(key=lambda d: d.get("created_at", ""))
    doc = docs[0]
    return PR.model_validate(_strip_meta(doc)), doc["_etag"]


async def ensure_pr_for_github(
    cosmos: Cosmos,
    *,
    project: str,
    repo: str,
    number: int,
    title: str = "",
    branch: str = "",
    body: str = "",
    base_ref: str = "main",
    head_sha: str = "",
    html_url: str = "",
) -> tuple[PR, str, bool]:
    """Find or mint a glimmung PR mirroring a GH PR. Returns `(pr, etag,
    created)`; `created=True` if this call minted the PR.

    The webhook mirror calls this on every relevant `pull_request.*` event
    so subsequent comment / review appends always have a target. Title /
    body / branch / etc. only apply on create — the mirror's update path
    is the right tool for refreshing existing fields."""
    existing = await find_pr_by_repo_number(cosmos, repo=repo, number=number)
    if existing is not None:
        pr, etag = existing
        return pr, etag, False

    pr = await create_pr(
        cosmos,
        project=project,
        repo=repo,
        number=number,
        title=title or f"{repo}#{number}",
        branch=branch,
        body=body,
        base_ref=base_ref,
        head_sha=head_sha,
        html_url=html_url,
    )
    refreshed = await read_pr(cosmos, project=project, pr_id=pr.id)
    if refreshed is None:
        raise RuntimeError(f"ensure_pr_for_github: just-created PR {pr.id} not readable")
    return refreshed[0], refreshed[1], True


async def append_pr_comment(
    cosmos: Cosmos,
    *,
    pr: PR,
    etag: str,
    comment: PRComment,
) -> tuple[PR, str]:
    """Append a comment to a PR. Dedupes on `comment.gh_id` so webhook
    re-deliveries are idempotent — if a comment with the same `gh_id`
    already exists, the existing entry is left in place and the call is a
    no-op state-wise (still bumps `updated_at` so the caller gets a fresh
    etag). Comments without `gh_id` (e.g. glimmung-internal annotations)
    are always appended; the dedupe check is `gh_id is not None`."""
    def apply(p: PR) -> PR:
        if comment.gh_id is not None and any(
            c.gh_id == comment.gh_id for c in p.comments
        ):
            return p.model_copy(update={"updated_at": _now()})
        return p.model_copy(update={
            "comments": [*p.comments, comment],
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def append_pr_review(
    cosmos: Cosmos,
    *,
    pr: PR,
    etag: str,
    review: PRReview,
) -> tuple[PR, str]:
    """Append a review to a PR. Dedupes on `review.gh_id` like
    `append_pr_comment`."""
    def apply(p: PR) -> PR:
        if review.gh_id is not None and any(
            r.gh_id == review.gh_id for r in p.reviews
        ):
            return p.model_copy(update={"updated_at": _now()})
        return p.model_copy(update={
            "reviews": [*p.reviews, review],
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, pr, etag, apply)


def github_pr_url_for(repo: str, number: int) -> str:
    """Canonical glimmung-side rendering of a GH PR URL. Mirrors
    `github_issue_url_for` so any code path that stitches a URL gets the
    same shape; future cross-lookup helpers can rely on the format being
    deterministic."""
    return f"https://github.com/{repo}/pull/{number}"


async def _retry_on_conflict(
    cosmos: Cosmos,
    pr: PR,
    etag: str,
    apply: Callable[[PR], PR],
) -> tuple[PR, str]:
    """Apply `apply(pr) -> pr` with optimistic concurrency. On `_etag`
    mismatch, re-read and retry. Returns `(updated_pr, new_etag)` so
    callers can chain ops without an extra read."""
    current = pr
    current_etag = etag
    for attempt in range(_MAX_CONFLICT_RETRIES):
        updated = apply(current)
        try:
            response = await cosmos.prs.replace_item(
                item=updated.id,
                body=updated.model_dump(mode="json"),
                etag=current_etag,
                match_condition=MatchConditions.IfNotModified,
            )
            return updated, response.get("_etag", current_etag)
        except CosmosAccessConditionFailedError:
            if attempt == _MAX_CONFLICT_RETRIES - 1:
                raise
            log.info("PR %s replace_item conflict; re-reading and retrying", current.id)
            doc = await cosmos.prs.read_item(item=current.id, partition_key=current.project)
            current = PR.model_validate(_strip_meta(doc))
            current_etag = doc["_etag"]
    raise RuntimeError("unreachable")

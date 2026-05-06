"""Glimmung-native issues (#28).

Cosmos-backed CRUD for the `issues` container. Glimmung is the
orchestrator / source of truth for issues; GitHub is one of N possible
syndication targets. Listing, dispatching, and signal-bus references
all key off a glimmung issue id, never a GH issue number.

A glimmung Issue *can* carry a GH reference
(`metadata.github_issue_url`), but the glimmung object exists and is
dispatchable whether or not a GH counterpart exists. This substrate is
the foundation; consumer PRs rewire `dispatch_run`, `/v1/issues`, and
the GH webhook handlers to flow through it.

API:
- `create_issue(...)` — mint a new Issue.
- `read_issue(...)` — point-read by `(project, issue_id)`. Returns
  `(Issue, etag)` so callers can chain into a write without re-reading.
- `update_issue(...)` — patch title / body / labels / metadata. Etag-
  validated via optimistic concurrency.
- `close_issue(...)` — state transition OPEN → CLOSED, sets `closed_at`.
- `reopen_issue(...)` — state transition CLOSED → OPEN, clears `closed_at`.
- `list_open_issues(project=None)` — list open issues, optionally
  scoped to a single project (single-partition).
- `find_issue_by_github_url(...)` — cross-partition lookup used by the
  PR `Closes #N` parser in the consumer PR; returns the glimmung Issue
  whose `metadata.github_issue_url` matches.

Concurrency model: same shape as runs.py — `_etag` + `IfNotModified`
on every mutating write, with a small retry loop on the rare conflict.
Conflicts here will be even rarer than for runs (issues mutate at
human-speed, not webhook-speed) but the pattern is uniform across
substrates so consumers can plug in without surprise.
"""

from __future__ import annotations

import logging
from datetime import UTC, datetime
from typing import Any, Callable

from azure.core import MatchConditions
from azure.cosmos.exceptions import (
    CosmosAccessConditionFailedError,
    CosmosResourceExistsError,
    CosmosResourceNotFoundError,
)
from ulid import ULID

from glimmung.db import Cosmos, query_all
from glimmung.models import Issue, IssueComment, IssueMetadata, IssueSource, IssueState

log = logging.getLogger(__name__)


_MAX_CONFLICT_RETRIES = 3
_COUNTER_PREFIX = "__counter:issue-number:"


def _now() -> datetime:
    return datetime.now(UTC)


def _strip_meta(doc: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in doc.items() if not k.startswith("_")}


async def create_issue(
    cosmos: Cosmos,
    *,
    project: str,
    number: int | None = None,
    title: str,
    body: str = "",
    labels: list[str] | None = None,
    source: IssueSource = IssueSource.MANUAL,
    workflow: str | None = None,
    ui_validation_requested: bool = False,
    github_issue_url: str | None = None,
    github_issue_repo: str | None = None,
    github_issue_number: int | None = None,
) -> Issue:
    """Mint a new Issue in OPEN state. Returns the persisted Issue.

    `source` defaults to MANUAL — UI/CLI creation. The GH-anchored
    paths (webhook import + dispatch shim) pass all three GH fields
    so the denormalized coords land on the Issue at creation. Issues
    minted from non-GH sources (Slack, scheduled) leave them None."""
    now = _now()
    issue_number = number
    if issue_number is None:
        issue_number = await next_issue_number(cosmos, project=project)
    else:
        await ensure_issue_number_counter_at_least(cosmos, project=project, number=issue_number)
    issue = Issue(
        id=str(ULID()),
        number=issue_number,
        project=project,
        title=title,
        body=body,
        labels=labels or [],
        state=IssueState.OPEN,
        metadata=IssueMetadata(
            source=source,
            workflow=workflow,
            ui_validation_requested=ui_validation_requested,
            github_issue_url=github_issue_url,
            github_issue_repo=github_issue_repo,
            github_issue_number=github_issue_number,
        ),
        created_at=now,
        updated_at=now,
    )
    await cosmos.issues.create_item(issue.model_dump(mode="json"))
    log.info(
        "created issue %s in project=%s (source=%s github_url=%s)",
        issue.id, project, source.value, github_issue_url or "-",
    )
    return issue


async def next_issue_number(cosmos: Cosmos, *, project: str) -> int:
    """Allocate the next project-scoped Glimmung issue number.

    The counter document lives in the same partition as the project issues so
    allocation is guarded by Cosmos create/etag conflicts instead of a
    read-max/write race. If the counter is absent, seed it from the migrated
    top-level `Issue.number` values in that project.
    """
    counter_id = _counter_id(project)
    for attempt in range(_MAX_CONFLICT_RETRIES):
        try:
            doc = await cosmos.issues.read_item(item=counter_id, partition_key=project)
        except CosmosResourceNotFoundError:
            try:
                seed = await _seed_issue_number_counter(cosmos, project=project)
                return int(seed["last_allocated"])
            except CosmosResourceExistsError:
                continue

        current_next = int(doc.get("next_issue_number") or 1)
        updated = {
            **doc,
            "next_issue_number": current_next + 1,
            "updated_at": _now().isoformat(),
        }
        try:
            await cosmos.issues.replace_item(
                item=counter_id,
                body=_strip_meta(updated),
                etag=doc["_etag"],
                match_condition=MatchConditions.IfNotModified,
            )
            return current_next
        except CosmosAccessConditionFailedError:
            if attempt == _MAX_CONFLICT_RETRIES - 1:
                raise
            log.info("issue-number counter conflict for project=%s; retrying", project)
    raise RuntimeError("unreachable")


async def ensure_issue_number_counter_at_least(
    cosmos: Cosmos,
    *,
    project: str,
    number: int,
) -> None:
    """Advance the project issue-number counter past an explicit number.

    Most callers should let `create_issue` allocate numbers. Migration and
    import paths sometimes pass an explicit project-scoped number; once the
    counter exists, those writes must still move `next_issue_number` forward
    so future auto-allocation cannot collide.
    """
    if number < 1:
        raise ValueError("issue number must be >= 1")
    counter_id = _counter_id(project)
    desired_next = number + 1
    for attempt in range(_MAX_CONFLICT_RETRIES):
        try:
            doc = await cosmos.issues.read_item(item=counter_id, partition_key=project)
        except CosmosResourceNotFoundError:
            try:
                seed = await _seed_issue_number_counter(cosmos, project=project)
                if int(seed["next_issue_number"]) >= desired_next:
                    return
            except CosmosResourceExistsError:
                continue
            continue

        current_next = int(doc.get("next_issue_number") or 1)
        if current_next >= desired_next:
            return
        updated = {
            **doc,
            "next_issue_number": desired_next,
            "updated_at": _now().isoformat(),
        }
        try:
            await cosmos.issues.replace_item(
                item=counter_id,
                body=_strip_meta(updated),
                etag=doc["_etag"],
                match_condition=MatchConditions.IfNotModified,
            )
            return
        except CosmosAccessConditionFailedError:
            if attempt == _MAX_CONFLICT_RETRIES - 1:
                raise
            log.info("issue-number counter conflict for project=%s; retrying", project)
    raise RuntimeError("unreachable")


def _counter_id(project: str) -> str:
    return f"{_COUNTER_PREFIX}{project}"


async def _seed_issue_number_counter(cosmos: Cosmos, *, project: str) -> dict[str, Any]:
    highest = await _highest_issue_number(cosmos, project=project)
    first = highest + 1
    now = _now().isoformat()
    doc = {
        "id": _counter_id(project),
        "project": project,
        "kind": "issue_number_counter",
        "last_allocated": first,
        "next_issue_number": first + 1,
        "created_at": now,
        "updated_at": now,
    }
    await cosmos.issues.create_item(doc)
    return doc


async def _highest_issue_number(cosmos: Cosmos, *, project: str) -> int:
    docs = await query_all(
        cosmos.issues,
        "SELECT * FROM c WHERE c.project = @p AND IS_DEFINED(c.number)",
        parameters=[{"name": "@p", "value": project}],
    )
    highest = 0
    for doc in docs:
        candidate = doc.get("number")
        try:
            n = int(candidate)
        except (TypeError, ValueError):
            continue
        highest = max(highest, n)
    return highest


async def read_issue(
    cosmos: Cosmos,
    *,
    project: str,
    issue_id: str,
) -> tuple[Issue, str] | None:
    """Point-read an Issue. Returns `(issue, etag)` or `None` if missing.
    The etag lets the caller chain into a write op without re-reading."""
    try:
        doc = await cosmos.issues.read_item(item=issue_id, partition_key=project)
    except CosmosResourceNotFoundError:
        return None
    return Issue.model_validate(_strip_meta(doc)), doc["_etag"]


async def read_issue_by_number(
    cosmos: Cosmos,
    *,
    project: str,
    number: int,
) -> tuple[Issue, str] | None:
    """Read an Issue by its project-scoped Glimmung issue number."""
    docs = await query_all(
        cosmos.issues,
        "SELECT * FROM c WHERE c.project = @p AND c.number = @n",
        parameters=[
            {"name": "@p", "value": project},
            {"name": "@n", "value": number},
        ],
    )
    if not docs:
        return None
    if len(docs) > 1:
        log.warning(
            "multiple glimmung issues have project=%s number=%s: %s",
            project, number, [d["id"] for d in docs],
        )
        docs.sort(key=lambda d: d.get("created_at", ""))
    doc = docs[0]
    return Issue.model_validate(_strip_meta(doc)), doc["_etag"]


async def update_issue(
    cosmos: Cosmos,
    *,
    issue: Issue,
    etag: str,
    title: str | None = None,
    body: str | None = None,
    labels: list[str] | None = None,
    github_issue_url: str | None = None,
) -> tuple[Issue, str]:
    """Patch fields on an Issue. `None` means "don't change"; pass an
    empty string / empty list to actually clear a field. State
    transitions go through `close_issue` / `reopen_issue` instead so the
    timestamp invariants stay obvious at the call site.

    Etag-validated; retries up to `_MAX_CONFLICT_RETRIES` on conflict."""
    def apply(i: Issue) -> Issue:
        updates: dict[str, Any] = {"updated_at": _now()}
        if title is not None:
            updates["title"] = title
        if body is not None:
            updates["body"] = body
        if labels is not None:
            updates["labels"] = list(labels)
        if github_issue_url is not None:
            updates["metadata"] = i.metadata.model_copy(
                update={"github_issue_url": github_issue_url},
            )
        return i.model_copy(update=updates)

    return await _retry_on_conflict(cosmos, issue, etag, apply)


async def add_comment(
    cosmos: Cosmos,
    *,
    issue: Issue,
    etag: str,
    author: str,
    body: str,
) -> tuple[Issue, str, IssueComment]:
    """Append a glimmung-authored comment to an Issue."""
    now = _now()
    comment = IssueComment(
        id=str(ULID()),
        author=author,
        body=body,
        created_at=now,
        updated_at=now,
    )

    def apply(i: Issue) -> Issue:
        return i.model_copy(update={
            "comments": [*i.comments, comment],
            "updated_at": _now(),
        })

    updated, new_etag = await _retry_on_conflict(cosmos, issue, etag, apply)
    return updated, new_etag, comment


async def update_comment(
    cosmos: Cosmos,
    *,
    issue: Issue,
    etag: str,
    comment_id: str,
    body: str,
) -> tuple[Issue, str, IssueComment] | None:
    """Replace a comment body by id. Returns None if the comment is missing."""
    existing = next((c for c in issue.comments if c.id == comment_id), None)
    if existing is None:
        return None
    now = _now()
    updated_comment = existing.model_copy(update={"body": body, "updated_at": now})

    def apply(i: Issue) -> Issue:
        if not any(c.id == comment_id for c in i.comments):
            return i
        return i.model_copy(update={
            "comments": [
                updated_comment if c.id == comment_id else c
                for c in i.comments
            ],
            "updated_at": _now(),
        })

    updated, new_etag = await _retry_on_conflict(cosmos, issue, etag, apply)
    if not any(c.id == comment_id for c in updated.comments):
        return None
    return updated, new_etag, updated_comment


async def remove_comment(
    cosmos: Cosmos,
    *,
    issue: Issue,
    etag: str,
    comment_id: str,
) -> tuple[Issue, str] | None:
    """Remove a comment by id. Returns None if the comment is missing."""
    if not any(c.id == comment_id for c in issue.comments):
        return None

    def apply(i: Issue) -> Issue:
        if not any(c.id == comment_id for c in i.comments):
            return i
        return i.model_copy(update={
            "comments": [c for c in i.comments if c.id != comment_id],
            "updated_at": _now(),
        })

    updated, new_etag = await _retry_on_conflict(cosmos, issue, etag, apply)
    if any(c.id == comment_id for c in updated.comments):
        return None
    return updated, new_etag


async def close_issue(
    cosmos: Cosmos,
    *,
    issue: Issue,
    etag: str,
) -> tuple[Issue, str]:
    """Transition OPEN → CLOSED, stamp `closed_at`. Idempotent: closing
    an already-closed issue is a no-op (still does the etag write so the
    caller gets a fresh etag back, but `closed_at` is preserved)."""
    def apply(i: Issue) -> Issue:
        if i.state == IssueState.CLOSED:
            return i.model_copy(update={"updated_at": _now()})
        now = _now()
        return i.model_copy(update={
            "state": IssueState.CLOSED,
            "closed_at": now,
            "updated_at": now,
        })
    return await _retry_on_conflict(cosmos, issue, etag, apply)


async def reopen_issue(
    cosmos: Cosmos,
    *,
    issue: Issue,
    etag: str,
) -> tuple[Issue, str]:
    """Transition CLOSED → OPEN, clear `closed_at`. The GH-webhook
    consumer PR uses this when `issues.reopened` arrives."""
    def apply(i: Issue) -> Issue:
        if i.state == IssueState.OPEN:
            return i.model_copy(update={"updated_at": _now()})
        return i.model_copy(update={
            "state": IssueState.OPEN,
            "closed_at": None,
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, issue, etag, apply)


async def list_open_issues(
    cosmos: Cosmos,
    *,
    project: str | None = None,
) -> list[Issue]:
    """Return all OPEN issues, oldest-first. If `project` is set the
    query is single-partition; otherwise it scans across partitions
    (used by the global Issues view in the dashboard)."""
    if project is not None:
        docs = await query_all(
            cosmos.issues,
            "SELECT * FROM c WHERE c.project = @p AND c.state = @s ORDER BY c.created_at ASC",
            parameters=[
                {"name": "@p", "value": project},
                {"name": "@s", "value": IssueState.OPEN.value},
            ],
        )
    else:
        docs = await query_all(
            cosmos.issues,
            "SELECT * FROM c WHERE c.state = @s ORDER BY c.created_at ASC",
            parameters=[{"name": "@s", "value": IssueState.OPEN.value}],
        )
    return [Issue.model_validate(_strip_meta(d)) for d in docs]


def github_issue_url_for(repo: str, issue_number: int) -> str:
    """Canonical glimmung-side rendering of a GH issue URL. The webhook
    payload's html_url is what we'd ideally store, but `Closes #N`
    parsing only sees `(repo, N)` — so the dispatch shim and PR-link
    parser both stitch the URL the same way to keep `find_issue_by_
    github_url` lookups deterministic."""
    return f"https://github.com/{repo}/issues/{issue_number}"


async def find_issue_by_github_url(
    cosmos: Cosmos,
    *,
    github_issue_url: str,
) -> tuple[Issue, str] | None:
    """Cross-partition lookup keyed off the `metadata.github_issue_url`
    link. Returns `(issue, etag)` or `None`. The PR `Closes #N` parser
    uses this in the consumer PR to resolve a GH issue number (which it
    can stitch into a URL given the repo) back to a glimmung Issue id."""
    docs = await query_all(
        cosmos.issues,
        "SELECT * FROM c WHERE c.metadata.github_issue_url = @u",
        parameters=[{"name": "@u", "value": github_issue_url}],
    )
    if not docs:
        return None
    if len(docs) > 1:
        # Multiple glimmung Issues pointing at the same GH URL is a
        # semantic error — log loudly and pick the oldest. The webhook-
        # import path in the consumer PR should de-duplicate on import.
        log.warning(
            "multiple glimmung issues link to %s: %s",
            github_issue_url, [d["id"] for d in docs],
        )
        docs.sort(key=lambda d: d.get("created_at", ""))
    doc = docs[0]
    return Issue.model_validate(_strip_meta(doc)), doc["_etag"]


async def _retry_on_conflict(
    cosmos: Cosmos,
    issue: Issue,
    etag: str,
    apply: Callable[[Issue], Issue],
) -> tuple[Issue, str]:
    """Apply `apply(issue) -> issue` with optimistic concurrency. On
    `_etag` mismatch, re-read and retry. Returns `(updated_issue,
    new_etag)` so callers can chain ops without an extra read."""
    current = issue
    current_etag = etag
    for attempt in range(_MAX_CONFLICT_RETRIES):
        updated = apply(current)
        try:
            response = await cosmos.issues.replace_item(
                item=updated.id,
                body=updated.model_dump(mode="json"),
                etag=current_etag,
                match_condition=MatchConditions.IfNotModified,
            )
            return updated, response.get("_etag", current_etag)
        except CosmosAccessConditionFailedError:
            if attempt == _MAX_CONFLICT_RETRIES - 1:
                raise
            log.info("issue %s replace_item conflict; re-reading and retrying", current.id)
            doc = await cosmos.issues.read_item(item=current.id, partition_key=current.project)
            current = Issue.model_validate(_strip_meta(doc))
            current_etag = doc["_etag"]
    raise RuntimeError("unreachable")

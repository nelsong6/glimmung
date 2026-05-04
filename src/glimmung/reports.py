"""Cosmos-backed CRUD for Glimmung Reports.

Report is the canonical review object. GitHub pull requests remain a
syndication target, so this module keeps repo/number/branch metadata for
mirrored PRs while storing the source of truth in the `reports` container.
"""

from __future__ import annotations

import logging
from datetime import UTC, datetime
from typing import Any, Callable

from azure.core import MatchConditions
from azure.cosmos.exceptions import CosmosAccessConditionFailedError, CosmosResourceNotFoundError
from ulid import ULID

from glimmung.db import Cosmos, query_all
from glimmung.models import Report, ReportComment, ReportReview, ReportState, ReportVersion

log = logging.getLogger(__name__)


_MAX_CONFLICT_RETRIES = 3


def _now() -> datetime:
    return datetime.now(UTC)


def _strip_meta(doc: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in doc.items() if not k.startswith("_")}


async def create_report(
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
    linked_issue_id: str | None = None,
    linked_run_id: str | None = None,
) -> Report:
    """Mint a new Report in READY state. Returns the persisted Report.

    `repo` and `number` are GitHub PR coordinates when the Report has a
    GitHub syndication target.
    """
    now = _now()
    report_id = linked_issue_id or str(ULID())
    pr = Report(
        id=report_id,
        project=project,
        repo=repo,
        number=number,
        title=title,
        body=body,
        state=ReportState.READY,
        branch=branch,
        base_ref=base_ref,
        head_sha=head_sha,
        html_url=html_url,
        linked_issue_id=linked_issue_id,
        linked_run_id=linked_run_id,
        created_at=now,
        updated_at=now,
    )
    await cosmos.reports.create_item(pr.model_dump(mode="json"))
    log.info(
        "created Report %s in project=%s (%s#%d)",
        pr.id, project, repo, number,
    )
    return pr


async def read_report(
    cosmos: Cosmos,
    *,
    project: str,
    report_id: str,
) -> tuple[Report, str] | None:
    """Point-read a Report. Returns `(pr, etag)` or `None` if missing."""
    try:
        doc = await cosmos.reports.read_item(item=report_id, partition_key=project)
    except CosmosResourceNotFoundError:
        return None
    return Report.model_validate(_strip_meta(doc)), doc["_etag"]


async def update_report(
    cosmos: Cosmos,
    *,
    pr: Report,
    etag: str,
    title: str | None = None,
    body: str | None = None,
    branch: str | None = None,
    base_ref: str | None = None,
    head_sha: str | None = None,
    html_url: str | None = None,
    linked_issue_id: str | None = None,
    linked_run_id: str | None = None,
) -> tuple[Report, str]:
    """Patch fields on a Report. `None` means "don't change"; pass an empty
    string to actually clear a field. State transitions (close / merge /
    reopen) go through their dedicated functions so the timestamp
    invariants stay obvious at the call site."""
    def apply(p: Report) -> Report:
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
        if linked_issue_id is not None:
            updates["linked_issue_id"] = linked_issue_id or None
        if linked_run_id is not None:
            updates["linked_run_id"] = linked_run_id or None
        return p.model_copy(update=updates)

    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def close_report(
    cosmos: Cosmos,
    *,
    pr: Report,
    etag: str,
) -> tuple[Report, str]:
    """Transition a Report to CLOSED without merge.

    `merged_at` and `merged_by` stay `None`. Idempotent:
    closing an already-closed Report is a no-op state-wise (still bumps
    `updated_at` so the caller gets a fresh etag). Use `merge_report` instead
    if the close is the result of a merge."""
    def apply(p: Report) -> Report:
        if p.state == ReportState.CLOSED:
            return p.model_copy(update={"updated_at": _now()})
        return p.model_copy(update={
            "state": ReportState.CLOSED,
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def merge_report(
    cosmos: Cosmos,
    *,
    pr: Report,
    etag: str,
    merged_by: str,
    merged_at: datetime | None = None,
) -> tuple[Report, str]:
    """Transition a Report to MERGED with `merged_at` + `merged_by` stamped.
    `merged_at` defaults to now, but the webhook-mirror call site should
    pass the GH-event timestamp instead so the audit trail matches what
    GitHub recorded. Idempotent: if the Report is already merged, the existing
    merge metadata is preserved (we don't overwrite earlier `merged_at`
    with a later re-delivery)."""
    def apply(p: Report) -> Report:
        if p.merged_at is not None:
            return p.model_copy(update={"updated_at": _now()})
        return p.model_copy(update={
            "state": ReportState.MERGED,
            "merged_at": merged_at or _now(),
            "merged_by": merged_by,
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def reopen_report(
    cosmos: Cosmos,
    *,
    pr: Report,
    etag: str,
) -> tuple[Report, str]:
    """Transition a closed Report back to READY."""
    def apply(p: Report) -> Report:
        if p.state == ReportState.READY:
            return p.model_copy(update={"updated_at": _now()})
        return p.model_copy(update={
            "state": ReportState.READY,
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def set_report_state(
    cosmos: Cosmos,
    *,
    pr: Report,
    etag: str,
    state: ReportState,
) -> tuple[Report, str]:
    """Set non-GitHub terminal/review states such as NEEDS_REVIEW or FAILED."""
    def apply(p: Report) -> Report:
        return p.model_copy(update={"state": state, "updated_at": _now()})

    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def list_active_reports(
    cosmos: Cosmos,
    *,
    project: str | None = None,
) -> list[Report]:
    """Return active Reports, oldest-first."""
    if project is not None:
        docs = await query_all(
            cosmos.reports,
            "SELECT * FROM c WHERE c.project = @p AND (c.state = @ready OR c.state = @needs_review) ORDER BY c.created_at ASC",
            parameters=[
                {"name": "@p", "value": project},
                {"name": "@ready", "value": ReportState.READY.value},
                {"name": "@needs_review", "value": ReportState.NEEDS_REVIEW.value},
            ],
        )
    else:
        docs = await query_all(
            cosmos.reports,
            "SELECT * FROM c WHERE c.state = @ready OR c.state = @needs_review ORDER BY c.created_at ASC",
            parameters=[
                {"name": "@ready", "value": ReportState.READY.value},
                {"name": "@needs_review", "value": ReportState.NEEDS_REVIEW.value},
            ],
        )
    return [Report.model_validate(_strip_meta(d)) for d in docs]


async def list_reports(
    cosmos: Cosmos,
    *,
    project: str | None = None,
) -> list[Report]:
    """Return all Reports, newest-updated first."""
    if project is not None:
        docs = await query_all(
            cosmos.reports,
            "SELECT * FROM c WHERE c.project = @p ORDER BY c.updated_at DESC",
            parameters=[{"name": "@p", "value": project}],
        )
    else:
        docs = await query_all(
            cosmos.reports,
            "SELECT * FROM c ORDER BY c.updated_at DESC",
        )
    return [Report.model_validate(_strip_meta(d)) for d in docs]


async def create_report_version(
    cosmos: Cosmos,
    *,
    project: str,
    report_id: str,
    title: str,
    body: str = "",
    state: ReportState = ReportState.READY,
    linked_run_id: str | None = None,
    github_repo: str | None = None,
    github_pr_number: int | None = None,
    github_html_url: str | None = None,
    version: int | None = None,
) -> ReportVersion:
    """Append an immutable ReportVersion snapshot.

    If `version` is omitted, the next integer version for the Report is
    assigned from the current stored history. Callers that mirror an external
    numbering scheme can pass an explicit version; Cosmos will reject duplicate
    ids, preserving immutability.
    """
    if version is None:
        versions = await list_report_versions(
            cosmos, project=project, report_id=report_id,
        )
        version = (max((v.version for v in versions), default=0) + 1)
    if version < 0:
        raise ValueError("version must be >= 0")

    doc = ReportVersion(
        id=f"{report_id}.{version}",
        project=project,
        report_id=report_id,
        version=version,
        state=state,
        title=title,
        body=body,
        linked_run_id=linked_run_id,
        github_repo=github_repo,
        github_pr_number=github_pr_number,
        github_html_url=github_html_url,
        created_at=_now(),
    )
    await cosmos.report_versions.create_item(doc.model_dump(mode="json"))
    return doc


async def list_report_versions(
    cosmos: Cosmos,
    *,
    project: str,
    report_id: str,
) -> list[ReportVersion]:
    """Return immutable ReportVersion snapshots for a Report, newest first."""
    docs = await query_all(
        cosmos.report_versions,
        "SELECT * FROM c WHERE c.project = @p AND c.report_id = @r ORDER BY c.version DESC",
        parameters=[
            {"name": "@p", "value": project},
            {"name": "@r", "value": report_id},
        ],
    )
    return [ReportVersion.model_validate(_strip_meta(d)) for d in docs]


async def read_report_version(
    cosmos: Cosmos,
    *,
    project: str,
    report_id: str,
    version: int,
) -> ReportVersion | None:
    """Point-read one ReportVersion by parent Report and integer version."""
    try:
        doc = await cosmos.report_versions.read_item(
            item=f"{report_id}.{version}",
            partition_key=project,
        )
    except CosmosResourceNotFoundError:
        return None
    return ReportVersion.model_validate(_strip_meta(doc))


async def find_report_by_repo_number(
    cosmos: Cosmos,
    *,
    repo: str,
    number: int,
) -> tuple[Report, str] | None:
    """Cross-partition lookup keyed off `(repo, number)`. The webhook
    mirror uses this to find the glimmung Report for an inbound GH event
    (the event payload carries `repo` + `number`, not the glimmung id).
    Returns `(pr, etag)` or `None`."""
    docs = await query_all(
        cosmos.reports,
        "SELECT * FROM c WHERE c.repo = @r AND c.number = @n",
        parameters=[
            {"name": "@r", "value": repo},
            {"name": "@n", "value": number},
        ],
    )
    if not docs:
        return None
    if len(docs) > 1:
        # Two glimmung PRs pointing at the same GH Report is a semantic error
        # — log loudly and pick the oldest. The webhook-import path in the
        # consumer Report should de-duplicate on import via this same lookup.
        log.warning(
            "multiple glimmung PRs link to %s#%d: %s",
            repo, number, [d["id"] for d in docs],
        )
        docs.sort(key=lambda d: d.get("created_at", ""))
    doc = docs[0]
    return Report.model_validate(_strip_meta(doc)), doc["_etag"]


async def ensure_report_for_github(
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
    linked_issue_id: str | None = None,
    linked_run_id: str | None = None,
) -> tuple[Report, str, bool]:
    """Find or mint a Report mirroring a GitHub PR.

    The webhook mirror calls this on every relevant `pull_request.*` event
    so subsequent comment / review appends always have a target. Title /
    body / branch / etc. only apply on create — the mirror's update path
    is the right tool for refreshing existing fields."""
    existing = await find_report_by_repo_number(cosmos, repo=repo, number=number)
    if existing is not None:
        pr, etag = existing
        return pr, etag, False

    pr = await create_report(
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
        linked_issue_id=linked_issue_id,
        linked_run_id=linked_run_id,
    )
    refreshed = await read_report(cosmos, project=project, report_id=pr.id)
    if refreshed is None:
        raise RuntimeError(f"ensure_report_for_github: just-created Report {pr.id} not readable")
    return refreshed[0], refreshed[1], True


async def append_report_comment(
    cosmos: Cosmos,
    *,
    pr: Report,
    etag: str,
    comment: ReportComment,
) -> tuple[Report, str]:
    """Append a comment to a Report. Dedupes on `comment.gh_id` so webhook
    re-deliveries are idempotent — if a comment with the same `gh_id`
    already exists, the existing entry is left in place and the call is a
    no-op state-wise (still bumps `updated_at` so the caller gets a fresh
    etag). Comments without `gh_id` (e.g. glimmung-internal annotations)
    are always appended; the dedupe check is `gh_id is not None`."""
    def apply(p: Report) -> Report:
        if comment.gh_id is not None and any(
            c.gh_id == comment.gh_id for c in p.comments
        ):
            return p.model_copy(update={"updated_at": _now()})
        return p.model_copy(update={
            "comments": [*p.comments, comment],
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, pr, etag, apply)


async def append_report_review(
    cosmos: Cosmos,
    *,
    pr: Report,
    etag: str,
    review: ReportReview,
) -> tuple[Report, str]:
    """Append a review to a Report. Dedupes on `review.gh_id` like
    `append_report_comment`."""
    def apply(p: Report) -> Report:
        if review.gh_id is not None and any(
            r.gh_id == review.gh_id for r in p.reviews
        ):
            return p.model_copy(update={"updated_at": _now()})
        return p.model_copy(update={
            "reviews": [*p.reviews, review],
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, pr, etag, apply)


def github_pull_request_url_for(repo: str, number: int) -> str:
    """Canonical rendering of a GitHub pull request URL. Mirrors
    `github_issue_url_for` so any code path that stitches a URL gets the
    same shape; future cross-lookup helpers can rely on the format being
    deterministic."""
    return f"https://github.com/{repo}/pull/{number}"


async def _retry_on_conflict(
    cosmos: Cosmos,
    pr: Report,
    etag: str,
    apply: Callable[[Report], Report],
) -> tuple[Report, str]:
    """Apply `apply(pr) -> pr` with optimistic concurrency. On `_etag`
    mismatch, re-read and retry. Returns `(updated_pr, new_etag)` so
    callers can chain ops without an extra read."""
    current = pr
    current_etag = etag
    for attempt in range(_MAX_CONFLICT_RETRIES):
        updated = apply(current)
        try:
            response = await cosmos.reports.replace_item(
                item=updated.id,
                body=updated.model_dump(mode="json"),
                etag=current_etag,
                match_condition=MatchConditions.IfNotModified,
            )
            return updated, response.get("_etag", current_etag)
        except CosmosAccessConditionFailedError:
            if attempt == _MAX_CONFLICT_RETRIES - 1:
                raise
            log.info("Report %s replace_item conflict; re-reading and retrying", current.id)
            doc = await cosmos.reports.read_item(item=current.id, partition_key=current.project)
            current = Report.model_validate(_strip_meta(doc))
            current_etag = doc["_etag"]
    raise RuntimeError("unreachable")

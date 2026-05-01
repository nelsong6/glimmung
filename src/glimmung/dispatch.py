"""Unified dispatch path.

`dispatch_run` is the single function both the GitHub webhook
(label-triggered) and the glimmung UI (button-triggered) call to start
an agent run. Future trigger sources — scheduled re-runs, CLI, Slack,
signal drains — plug in the same way.

Per-issue serialization is built in: every dispatch claims the
`("issue", f"{repo}#{issue_number}")` lock. Concurrent dispatches on
the same issue (two webhook deliveries, two UI clicks, webhook + UI
race) all serialize cleanly — the second sees `state="already_running"`
without acquiring a lease or firing a workflow_dispatch.

The lock is held for the run's lifetime. Release happens at terminal
transition:
- For Run-tracked workflows (`retry_workflow_filename` set): the
  workflow_run.completed handler releases the lock when the Run reaches
  PASSED or ABORTED, using the `issue_lock_holder_id` recorded on the
  Run document.
- For non-Run-tracked workflows: the handler releases when the lease
  releases, using `issue_lock_holder_id` stashed in the lease metadata.

Lock TTL is set to `lease_default_ttl_seconds` (4h) — comfortably
longer than any single workflow's wall time. If the workflow_run
completion never fires (runner died, glimmung crash mid-flight), the
lock + lease both expire via TTL/sweep and a fresh dispatch can take
over after that grace window.
"""

from __future__ import annotations

import logging
from typing import Any

from fastapi import FastAPI
from pydantic import BaseModel
from ulid import ULID

from glimmung import issues as issue_ops
from glimmung import leases as lease_ops
from glimmung import locks as lock_ops
from glimmung import runs as run_ops
from glimmung.budget import resolve_budget
from glimmung.db import Cosmos, query_all
from glimmung.locks import LockBusy
from glimmung.models import BudgetConfig, IssueSource

log = logging.getLogger(__name__)


class DispatchResult(BaseModel):
    """The outcome of a dispatch_run call.

    `state` is the operational verdict:
      - `dispatched`: lease claimed, workflow_dispatch fired, host assigned.
      - `pending`: lease claimed in PENDING state (no host capacity); the
        promote loop will fire workflow_dispatch when capacity frees up.
      - `already_running`: a prior in-flight dispatch holds the issue
        lock. No new lease, no new workflow_dispatch. Caller can poll.
      - `no_project`: repo isn't registered with glimmung.
      - `no_workflow`: project has no matching workflow (caller passed
        none and the project has 0 or 2+ workflows).
    """
    state: str
    lease_id: str | None = None
    run_id: str | None = None
    host: str | None = None
    workflow: str | None = None
    issue_lock_holder_id: str | None = None
    detail: str | None = None


async def dispatch_run(
    app: FastAPI,
    *,
    repo: str | None = None,
    issue_number: int | None = None,
    issue_id: str | None = None,
    project: str | None = None,
    trigger_source: dict[str, Any],
    workflow_name: str | None = None,
    issue_labels: list[str] | None = None,
    extra_metadata: dict[str, Any] | None = None,
) -> DispatchResult:
    """Start an agent run on a glimmung Issue.

    Two ways to identify the target:
    - GH-anchored: pass `repo` + `issue_number`. The Issue is minted /
      reused via `ensure_issue_for_github` (source = GITHUB_WEBHOOK_IMPORT).
      This is the legacy path and the one any GH-driven trigger source
      still uses.
    - Glimmung-native (#50): pass `issue_id` (+ optional `project`,
      resolved from the doc when omitted). The Issue must exist; no
      mint happens here. Used by UI dispatch on issues created via
      `POST /v1/issues`.

    `trigger_source` is recorded on the Run for observability. Required
    fields by convention: `kind` (one of `label_webhook`, `glimmung_ui`,
    `scheduled`, `cli`, `signal_drain`), plus kind-specific extras
    (`label`, `actor`, etc.). The decision engine doesn't read it.

    `workflow_name`: if provided, that exact workflow is dispatched.
    If not provided, glimmung picks the project's only registered
    workflow; if there are 0 or 2+, returns `no_workflow` and the
    caller has to disambiguate.

    `issue_labels` are passed to budget resolution (`agent-budget:NxM`
    label support). Labels remain a courtesy syndication surface from
    glimmung, not the dispatch primitive.
    """
    cosmos: Cosmos = app.state.cosmos
    settings = app.state.settings

    # 1. Resolve the target Issue + project. Two entry shapes converge
    # to (issue, project_name) before workflow resolution.
    issue = None
    if issue_id is not None:
        # Native path: caller already has a glimmung issue id. Project
        # may be passed for a single-partition point read; if omitted,
        # we cross-partition find it by id.
        if project is None:
            issue = await _find_issue_anywhere(cosmos, issue_id=issue_id)
        else:
            found = await issue_ops.read_issue(
                cosmos, project=project, issue_id=issue_id,
            )
            issue = found[0] if found else None
        if issue is None:
            return DispatchResult(state="no_project", detail=f"no glimmung issue {issue_id!r}")
        project_name = issue.project
        repo = issue.metadata.github_issue_repo
        issue_number = issue.metadata.github_issue_number
    else:
        if repo is None or issue_number is None:
            raise ValueError("dispatch_run requires either issue_id or (repo + issue_number)")
        # GH-anchored path: project from repo lookup.
        project_doc = await _resolve_project(cosmos, repo)
        if project_doc is None:
            return DispatchResult(state="no_project", detail=f"no project for repo {repo!r}")
        project_name = project_doc["name"]

    # 2. Resolve workflow.
    workflow_doc, picker_detail = await _resolve_workflow(cosmos, project_name, workflow_name)
    if workflow_doc is None:
        return DispatchResult(state="no_workflow", detail=picker_detail)
    workflow_actual_name: str = workflow_doc["name"]

    # 2b. Ensure a glimmung Issue exists for this (repo, issue_number).
    # Skipped when the caller passed an explicit issue_id — that issue
    # already exists. Sequenced after project + workflow resolution so
    # failed dispatches (no_project / no_workflow) are no-ops on the
    # issues container — we only mint when we're committed to dispatching.
    if issue is None:
        assert repo is not None and issue_number is not None  # narrowed above
        issue, _issue_etag, issue_created = await issue_ops.ensure_issue_for_github(
            cosmos,
            project=project_name,
            repo=repo,
            issue_number=issue_number,
            source=IssueSource.GITHUB_WEBHOOK_IMPORT,
        )
        if issue_created:
            log.info(
                "dispatch_run: minted glimmung issue %s for %s#%d",
                issue.id, repo, issue_number,
            )

    # 3. Claim the per-issue lock. Native issues with no GH coords lock
    # on their glimmung id; GH-anchored issues use the repo#N key so
    # webhook-driven dispatches keep colliding with UI-driven ones.
    holder_id = str(ULID())
    lock_key = (
        f"{repo}#{issue_number}" if (repo and issue_number is not None)
        else f"glimmung/{issue.id}"
    )
    try:
        await lock_ops.claim_lock(
            cosmos,
            scope="issue",
            key=lock_key,
            holder_id=holder_id,
            ttl_seconds=settings.lease_default_ttl_seconds,
            metadata={
                "trigger_source": trigger_source,
                "workflow": workflow_actual_name,
            },
        )
    except LockBusy as busy:
        log.info(
            "dispatch_run: %s already running (lock holder=%s); skipping",
            lock_key, busy.lock.held_by,
        )
        return DispatchResult(
            state="already_running",
            workflow=workflow_actual_name,
            detail=f"issue lock held by {busy.lock.held_by} until {busy.lock.expires_at.isoformat()}",
        )

    # 4. Lease + workflow_dispatch.
    metadata: dict[str, Any] = {
        "issue_id": issue.id,
        "issue_lock_holder_id": holder_id,
        **(extra_metadata or {}),
    }
    if repo and issue_number is not None:
        metadata["issue_number"] = str(issue_number)
        metadata["issue_repo"] = repo
    requirements = workflow_doc.get("defaultRequirements", {}) or {}
    lease, host = await lease_ops.acquire(
        cosmos,
        settings,
        project=project_name,
        workflow=workflow_actual_name,
        requirements=requirements,
        metadata=metadata,
    )

    if host is not None:
        # Inline import to avoid the app.py ↔ dispatch.py circular dep:
        # dispatch.py is imported by app.py at module load time, so we
        # can't import the helper at the top.
        from glimmung.app import _maybe_dispatch_workflow

        lease_doc = {
            **lease_ops._lease_to_doc(lease),
            "id": lease.id,
            "project": lease.project,
            "workflow": lease.workflow,
        }
        await _maybe_dispatch_workflow(app, lease_doc, host)

    # 5. Run record (only if workflow opts into the verify-loop substrate).
    run_id: str | None = None
    retry_filename = workflow_doc.get("retryWorkflowFilename") or ""
    if retry_filename:
        budget = resolve_budget(
            issue_labels or [],
            _budget_from_doc(workflow_doc.get("defaultBudget")),
        )
        run = await run_ops.create_run(
            cosmos,
            project=project_name,
            workflow=workflow_actual_name,
            issue_id=issue.id,
            issue_repo=repo or "",
            issue_number=issue_number or 0,
            budget=budget,
            initial_workflow_filename=workflow_doc["workflowFilename"],
            issue_lock_holder_id=holder_id,
            trigger_source=trigger_source,
        )
        run_id = run.id

    return DispatchResult(
        state="dispatched" if host is not None else "pending",
        lease_id=lease.id,
        run_id=run_id,
        host=host.name if host is not None else None,
        workflow=workflow_actual_name,
        issue_lock_holder_id=holder_id,
    )


async def _find_issue_anywhere(cosmos: Cosmos, *, issue_id: str):
    """Cross-partition lookup of an Issue by id. Used when the dispatch
    caller has the id but not the project. Returns the `Issue` or None."""
    docs = await query_all(
        cosmos.issues,
        "SELECT * FROM c WHERE c.id = @i",
        parameters=[{"name": "@i", "value": issue_id}],
    )
    if not docs:
        return None
    from glimmung.models import Issue
    doc = {k: v for k, v in docs[0].items() if not k.startswith("_")}
    return Issue.model_validate(doc)


# ─── helpers ─────────────────────────────────────────────────────────────────


async def _resolve_project(cosmos: Cosmos, repo: str) -> dict[str, Any] | None:
    matching = await query_all(
        cosmos.projects,
        "SELECT * FROM c WHERE c.githubRepo = @r",
        parameters=[{"name": "@r", "value": repo}],
    )
    return matching[0] if matching else None


async def _resolve_workflow(
    cosmos: Cosmos,
    project_name: str,
    workflow_name: str | None,
) -> tuple[dict[str, Any] | None, str | None]:
    """Pick the workflow to dispatch.

    Explicit `workflow_name` wins. Otherwise: pick the project's only
    workflow if there's one. Anything else (zero workflows, or two+
    workflows + no explicit pick) returns `(None, detail)` so the
    caller can surface a meaningful error to the user.
    """
    if workflow_name:
        doc = await _read_workflow(cosmos, project_name, workflow_name)
        if doc is None:
            return None, f"workflow {project_name}/{workflow_name} not registered"
        return doc, None

    workflows = await query_all(
        cosmos.workflows,
        "SELECT * FROM c WHERE c.project = @p",
        parameters=[{"name": "@p", "value": project_name}],
    )
    if not workflows:
        return None, f"project {project_name!r} has no workflows registered"
    if len(workflows) > 1:
        names = sorted(w["name"] for w in workflows)
        return None, f"project {project_name!r} has multiple workflows; specify one of {names}"
    return workflows[0], None


async def _read_workflow(
    cosmos: Cosmos,
    project: str,
    name: str,
) -> dict[str, Any] | None:
    try:
        return await cosmos.workflows.read_item(item=name, partition_key=project)
    except Exception:
        return None


def _budget_from_doc(doc: dict[str, Any] | None) -> BudgetConfig | None:
    if not doc:
        return None
    return BudgetConfig.model_validate(doc)

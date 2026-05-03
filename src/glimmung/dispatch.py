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
from glimmung.models import BudgetConfig

log = logging.getLogger(__name__)

SESSION_WARM_LABEL = "agent-session:warm"


def session_launch_intent_from_labels(labels: list[str] | None) -> str:
    normalized = {label.strip().lower() for label in (labels or [])}
    return "warm" if SESSION_WARM_LABEL in normalized else "cold"


_RECENT_COMMENT_LIMIT = 5


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
      - `dispatch_failed`: the lease was claimed and a host was assigned,
        but `dispatch_workflow` raised (typically a 422 from GH on
        undeclared workflow inputs). Lease + lock are released and no
        Run record is created — caller can retry once the underlying
        cause is fixed.
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
    issue_id: str | None = None,
    project: str | None = None,
    repo: str | None = None,
    issue_number: int | None = None,
    trigger_source: dict[str, Any],
    workflow_name: str | None = None,
    issue_labels: list[str] | None = None,
    extra_metadata: dict[str, Any] | None = None,
) -> DispatchResult:
    """Start an agent run on a glimmung Issue.

    The target Issue must already exist in glimmung — dispatch never
    mints from GH coords. Two ways to identify the target:
    - `issue_id` (canonical, what the UI sends): cross-partition point
      read; `project` may be passed to make it a single-partition read.
    - `(repo, issue_number)` (legacy lookup shape): looks up by
      `metadata.github_issue_url`. Returns `no_project` if no glimmung
      Issue links to that URL.

    `trigger_source` is recorded on the Run for observability. Required
    fields by convention: `kind` (one of `glimmung_ui`, `scheduled`,
    `cli`, `signal_drain`), plus kind-specific extras (`actor`, etc.).
    The decision engine doesn't read it.

    `workflow_name`: if provided, that exact workflow is dispatched.
    If not provided, glimmung picks the project's only registered
    workflow; if there are 0 or 2+, returns `no_workflow` and the
    caller has to disambiguate.

    `issue_labels` can override the stored Issue labels for webhook
    callers that already have a fresh label set. UI/native callers omit
    it and dispatch uses the labels persisted on the Issue.
    """
    cosmos: Cosmos = app.state.cosmos
    settings = app.state.settings

    # 1. Resolve the target Issue + project.
    issue = None
    if issue_id is not None:
        if project is None:
            issue = await _find_issue_anywhere(cosmos, issue_id=issue_id)
        else:
            found = await issue_ops.read_issue(
                cosmos, project=project, issue_id=issue_id,
            )
            issue = found[0] if found else None
        if issue is None:
            return DispatchResult(state="no_project", detail=f"no glimmung issue {issue_id!r}")
    else:
        if repo is None or issue_number is None:
            raise ValueError("dispatch_run requires either issue_id or (repo + issue_number)")
        url = issue_ops.github_issue_url_for(repo, issue_number)
        found = await issue_ops.find_issue_by_github_url(
            cosmos, github_issue_url=url,
        )
        if found is None:
            return DispatchResult(
                state="no_project",
                detail=f"no glimmung issue for {repo}#{issue_number}",
            )
        issue, _ = found
    project_name = issue.project
    project_doc = await _read_project(cosmos, project_name)
    project_repo = str((project_doc or {}).get("githubRepo") or "")
    github_issue_repo = issue.metadata.github_issue_repo
    repo = github_issue_repo or project_repo
    issue_number = issue.metadata.github_issue_number

    # 2. Resolve workflow.
    workflow_doc, picker_detail = await _resolve_workflow(cosmos, project_name, workflow_name)
    if workflow_doc is None:
        return DispatchResult(state="no_workflow", detail=picker_detail)
    workflow_actual_name: str = workflow_doc["name"]
    effective_issue_labels = issue_labels if issue_labels is not None else list(issue.labels)

    # 3. Claim the per-issue lock. GH-anchored issues lock on the
    # repo#N key (so webhook-driven dispatches collide with UI-driven
    # ones); native issues with no GH coords lock on their glimmung id.
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

    # 4. Run record (#69): create BEFORE dispatch so run_id can flow into
    # workflow_dispatch inputs. Glimmung dictates the head ref via run_id
    # (`glimmung/<run_id>`) and the agent's workflow needs that value at
    # branch-resolution time. Pre-#69 the Run was created post-dispatch
    # (fine because branches were agent-named); post-#69 the order matters.
    run_id: str | None = None
    phases = workflow_doc.get("phases") or []
    if phases:
        initial_phase = phases[0]
        budget = resolve_budget(
            effective_issue_labels,
            _budget_from_doc(workflow_doc.get("budget")),
        )
        run = await run_ops.create_run(
            cosmos,
            project=project_name,
            workflow=workflow_actual_name,
            issue_id=issue.id,
            issue_repo=repo or "",
            issue_number=issue_number or 0,
            budget=budget,
            initial_phase_name=initial_phase["name"],
            initial_workflow_filename=initial_phase["workflowFilename"],
            issue_lock_holder_id=holder_id,
            trigger_source=trigger_source,
            session_launch_intent=session_launch_intent_from_labels(effective_issue_labels),
        )
        run_id = run.id

    # 5. Lease + workflow_dispatch.
    metadata: dict[str, Any] = {
        "issue_title": issue.title,
        "issue_lock_holder_id": holder_id,
        **(extra_metadata or {}),
    }
    workflow_metadata = workflow_doc.get("metadata") or {}
    if workflow_metadata.get("include_recent_comments"):
        metadata["recent_comments"] = _format_recent_comments(issue.comments)
    if run_id is not None:
        metadata["run_id"] = run_id
        metadata["attempt_index"] = "0"
    if github_issue_repo and issue_number is not None:
        metadata["issue_number"] = str(issue_number)
        metadata["issue_repo"] = github_issue_repo
    else:
        metadata["issue_id"] = issue.id
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
        dispatched = await _maybe_dispatch_workflow(app, lease_doc, host)
        if not dispatched:
            # GH refused the dispatch (typically a 422 on undeclared
            # inputs — see _DISPATCH_INPUT_KEYS in app.py). Roll back the
            # lease + lock + Run so the issue isn't held for the lock TTL
            # on a phantom run.
            try:
                await lease_ops.release(cosmos, lease.id, project_name)
            except Exception:
                log.exception(
                    "dispatch_run: lease release failed during backout for %s",
                    lease.id,
                )
            try:
                await lock_ops.release_lock(
                    cosmos, scope="issue", key=lock_key, holder_id=holder_id,
                )
            except Exception:
                log.exception(
                    "dispatch_run: lock release failed during backout for %s",
                    lock_key,
                )
            if run_id is not None:
                try:
                    # Mark the run aborted to avoid an orphan IN_PROGRESS row
                    # (the symptom #20 was supposed to prevent).
                    doc = await cosmos.runs.read_item(item=run_id, partition_key=project_name)
                    from glimmung.models import Run
                    run_obj = Run.model_validate({k: v for k, v in doc.items() if not k.startswith("_")})
                    await run_ops.mark_aborted(
                        cosmos, run=run_obj, etag=doc["_etag"],
                        reason="dispatch_failed: GitHub workflow_dispatch raised before run started",
                    )
                except Exception:
                    log.exception(
                        "dispatch_run: failed to mark run %s aborted during backout", run_id,
                    )
            return DispatchResult(
                state="dispatch_failed",
                lease_id=lease.id,
                run_id=run_id,
                workflow=workflow_actual_name,
                detail="GitHub workflow_dispatch raised; lease + lock + run rolled back",
            )

    return DispatchResult(
        state="dispatched" if host is not None else "pending",
        lease_id=lease.id,
        run_id=run_id,
        host=host.name if host is not None else None,
        workflow=workflow_actual_name,
        issue_lock_holder_id=holder_id,
    )


def _format_recent_comments(comments: list[Any]) -> str:
    recent = sorted(comments, key=lambda c: c.created_at)[-_RECENT_COMMENT_LIMIT:]
    return "\n\n".join(
        f"[{comment.created_at.isoformat()}] {comment.author}:\n{comment.body}"
        for comment in recent
    )


class ResumeResult(BaseModel):
    """The outcome of `dispatch_resumed_run`. Same shape philosophy as
    `DispatchResult`: `state` is the operational verdict, the rest is
    surface for the caller to track / surface.

    `state`:
      - `dispatched`: skipped attempts persisted, lease claimed, entrypoint
        workflow_dispatch fired.
      - `pending`: skipped attempts persisted, lease in PENDING (no host);
        the promote loop will fire workflow_dispatch when capacity frees.
      - `already_running`: prior run's issue is already locked by another
        run; resume rejected. Caller must abort the conflicting run first.
      - `dispatch_failed`: lease + lock were claimed but workflow_dispatch
        raised; lease + lock + new Run rolled back.
      - `prior_in_progress`: refusing to resume from an IN_PROGRESS prior
        run (would race the in-flight dispatch's lock + lease).
      - `prior_missing` / `workflow_missing` / `phase_invalid` /
        `outputs_missing`: pre-flight validation failures, no state
        mutation occurred.
    """
    state: str
    new_run_id: str | None = None
    prior_run_id: str | None = None
    lease_id: str | None = None
    host: str | None = None
    issue_lock_holder_id: str | None = None
    detail: str | None = None


async def dispatch_resumed_run(
    app: FastAPI,
    *,
    project: str,
    prior_run_id: str,
    entrypoint_phase: str,
    trigger_source: dict[str, Any],
) -> ResumeResult:
    """Spawn a new Run from a prior Run with phases preceding
    `entrypoint_phase` skipped (carrying their `phase_outputs` forward),
    and dispatch the entrypoint phase fresh. Sibling of `dispatch_run`,
    same lock-then-Run-then-lease shape.

    Refuses if the prior Run is still IN_PROGRESS — the operator should
    abort it first; resuming from a live Run would race its in-flight
    lock + lease.

    Refuses if the issue is currently locked by another Run — caller
    sees `state="already_running"` with the lock holder in `detail`,
    same response shape `dispatch_run` returns on collision.

    Pre-flight validation (workflow exists, entrypoint phase exists on
    workflow, all skipped phases have captured outputs on the prior
    Run) runs before any state mutation; failures return one of the
    `prior_missing` / `workflow_missing` / `phase_invalid` /
    `outputs_missing` states with no rollback needed.
    """
    cosmos: Cosmos = app.state.cosmos
    settings = app.state.settings

    # 1. Read prior run.
    found = await run_ops.read_run(cosmos, project=project, run_id=prior_run_id)
    if found is None:
        return ResumeResult(
            state="prior_missing",
            prior_run_id=prior_run_id,
            detail=f"no run {project}/{prior_run_id}",
        )
    prior_run, _ = found

    from glimmung.models import RunState
    if prior_run.state == RunState.IN_PROGRESS:
        return ResumeResult(
            state="prior_in_progress",
            prior_run_id=prior_run_id,
            detail=(
                "refusing to resume from an in-progress run; abort the prior "
                "run first (POST /v1/runs/{p}/{run_id}/abort) and retry"
            ),
        )

    # 2. Read workflow registration.
    workflow_doc = await _read_workflow(cosmos, project, prior_run.workflow)
    if workflow_doc is None:
        return ResumeResult(
            state="workflow_missing",
            prior_run_id=prior_run_id,
            detail=(
                f"workflow {project}/{prior_run.workflow!r} no longer registered; "
                "re-register before resuming"
            ),
        )
    # Inline import keeps dispatch.py free of the app.py runtime.
    from glimmung.app import _doc_to_workflow, _dispatch_next_phase
    workflow_model = _doc_to_workflow(workflow_doc)
    if workflow_model is None:
        return ResumeResult(
            state="workflow_missing",
            prior_run_id=prior_run_id,
            detail="workflow doc didn't validate to Workflow model",
        )

    # 3. Validate entrypoint phase exists on workflow.
    next_phase = next(
        (p for p in workflow_model.phases if p.name == entrypoint_phase), None,
    )
    if next_phase is None:
        return ResumeResult(
            state="phase_invalid",
            prior_run_id=prior_run_id,
            detail=(
                f"entrypoint_phase {entrypoint_phase!r} not on workflow "
                f"{project}/{prior_run.workflow!r} "
                f"(phases: {[p.name for p in workflow_model.phases]})"
            ),
        )

    # 4. Claim a fresh issue lock. Resume always claims with a new
    # holder_id — the prior run's lock was either released on its
    # terminal transition or never claimed. If a different run holds
    # the lock right now (a fresh dispatch came in), refuse.
    holder_id = str(ULID())
    lock_key = (
        f"{prior_run.issue_repo}#{prior_run.issue_number}"
        if (prior_run.issue_repo and prior_run.issue_number)
        else f"glimmung/{prior_run.issue_id}"
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
                "workflow": prior_run.workflow,
                "resumed_from": prior_run_id,
            },
        )
    except LockBusy as busy:
        return ResumeResult(
            state="already_running",
            prior_run_id=prior_run_id,
            detail=(
                f"issue lock held by {busy.lock.held_by} until "
                f"{busy.lock.expires_at.isoformat()}; abort the conflicting "
                "run before resuming"
            ),
        )

    # 5. Create the resumed Run with skipped attempts. Pre-flight
    # validation in `create_resumed_run` catches missing-phase-outputs;
    # roll back the lock if it raises.
    try:
        new_run, etag = await run_ops.create_resumed_run(
            cosmos,
            prior_run=prior_run,
            workflow=workflow_model,
            entrypoint_phase=entrypoint_phase,
            issue_lock_holder_id=holder_id,
            trigger_source=trigger_source,
        )
    except ValueError as e:
        try:
            await lock_ops.release_lock(
                cosmos, scope="issue", key=lock_key, holder_id=holder_id,
            )
        except Exception:
            log.exception(
                "dispatch_resumed_run: lock release failed during validation backout"
            )
        return ResumeResult(
            state="outputs_missing",
            prior_run_id=prior_run_id,
            detail=str(e),
        )

    # 6. Dispatch the entrypoint phase. _dispatch_next_phase appends a
    # fresh PhaseAttempt for `next_phase`, acquires a lease, and fires
    # workflow_dispatch with prior phases' outputs substituted in.
    # Errors inside _dispatch_next_phase log + mark the run aborted but
    # don't raise — pattern matches the rest of the dispatch code.
    await _dispatch_next_phase(
        run=new_run,
        etag=etag,
        repo=prior_run.issue_repo,
        workflow_model=workflow_model,
        next_phase=next_phase,
    )

    # Re-read for the freshest state (post-dispatch the lease + new
    # attempt are persisted; we want their ids in the response).
    post = await run_ops.read_run(cosmos, project=project, run_id=new_run.id)
    if post is None:
        # Defensive — _dispatch_next_phase shouldn't be deleting the run.
        return ResumeResult(
            state="dispatch_failed",
            new_run_id=new_run.id,
            prior_run_id=prior_run_id,
            issue_lock_holder_id=holder_id,
            detail="resumed run vanished mid-dispatch",
        )
    post_run, _ = post

    # If _dispatch_next_phase aborted (substitution failed, etc.), the
    # state will be ABORTED. Release the issue lock — the live
    # workflow_run.completed terminal handler that normally releases
    # the lock won't fire because no workflow_run exists. Mirrors the
    # rollback shape `dispatch_run` uses on its dispatch_failed path.
    if post_run.state == RunState.ABORTED:
        try:
            await lock_ops.release_lock(
                cosmos, scope="issue", key=lock_key, holder_id=holder_id,
            )
        except Exception:
            log.exception(
                "dispatch_resumed_run: lock release failed during dispatch-abort backout for %s",
                lock_key,
            )
        return ResumeResult(
            state="dispatch_failed",
            new_run_id=new_run.id,
            prior_run_id=prior_run_id,
            issue_lock_holder_id=holder_id,
            detail=post_run.abort_reason or "dispatch aborted; see glimmung logs",
        )

    # Find the lease the dispatch just claimed (latest attempt's
    # workflow_run_id won't be set yet — the started callback fills it).
    # The lease isn't recorded on the run; pull from leases container by
    # metadata.run_id. Best-effort — surface the run_id either way.
    lease_id: str | None = None
    host_name: str | None = None
    try:
        lease_docs = await query_all(
            cosmos.leases,
            "SELECT * FROM c WHERE c.project = @p AND c.metadata.run_id = @r",
            parameters=[
                {"name": "@p", "value": project},
                {"name": "@r", "value": new_run.id},
            ],
        )
        if lease_docs:
            # Most recently requested lease wins (the entrypoint dispatch
            # is the one we just fired).
            lease_docs.sort(key=lambda d: d.get("requestedAt", ""), reverse=True)
            lease_id = lease_docs[0]["id"]
            host_name = lease_docs[0].get("host")
    except Exception:
        log.exception(
            "dispatch_resumed_run: lease lookup failed for run %s; result will "
            "omit lease info (dispatch itself succeeded)",
            new_run.id,
        )

    return ResumeResult(
        state="dispatched" if host_name is not None else "pending",
        new_run_id=new_run.id,
        prior_run_id=prior_run_id,
        lease_id=lease_id,
        host=host_name,
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


async def _read_project(cosmos: Cosmos, project_name: str) -> dict[str, Any] | None:
    try:
        return await cosmos.projects.read_item(item=project_name, partition_key=project_name)
    except Exception:
        return None


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

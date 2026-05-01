import asyncio
import json
import logging
import os
from contextlib import asynccontextmanager
from datetime import UTC, datetime
from pathlib import Path as FsPath
from typing import Any

from fastapi import Depends, FastAPI, HTTPException, Path, Request
from fastapi.responses import FileResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field
from sse_starlette.sse import EventSourceResponse

from glimmung import leases as lease_ops
from glimmung import locks as lock_ops
from glimmung import runs as run_ops
from glimmung.dispatch import DispatchResult, dispatch_run
from glimmung.auth import require_admin_user
from glimmung.db import Cosmos, query_all
from glimmung.decision import abort_explanation, decide
from glimmung.github_app import (
    GitHubAppTokenMinter,
    dispatch_workflow,
    get_issue,
    list_open_issues,
    post_issue_comment,
    verify_webhook_signature,
)
from glimmung.models import (
    Host,
    Lease,
    LeaseRequest,
    LeaseResponse,
    LeaseState,
    Project,
    ProjectRegister,
    Run,
    RunDecision,
    StateSnapshot,
    Workflow,
    WorkflowRegister,
)
from glimmung.settings import Settings, get_settings
from glimmung.verification import fetch_verification

log = logging.getLogger(__name__)


@asynccontextmanager
async def lifespan(app: FastAPI):
    settings = get_settings()
    cosmos = Cosmos(settings)
    await cosmos.start()
    app.state.cosmos = cosmos
    app.state.settings = settings

    if settings.github_app_id and settings.github_app_private_key and settings.github_app_installation_id:
        app.state.gh_minter = GitHubAppTokenMinter(
            app_id=settings.github_app_id,
            installation_id=settings.github_app_installation_id,
            private_key=settings.github_app_private_key,
        )
        log.info("github app minter ready (app_id=%s)", settings.github_app_id)
    else:
        app.state.gh_minter = None
        log.warning("github app credentials not configured; webhook + dispatch disabled")

    sweep_task = asyncio.create_task(_sweep_loop(cosmos, settings))
    promote_task = asyncio.create_task(_promote_loop(app, settings))
    lock_sweep_task = asyncio.create_task(_lock_sweep_loop(cosmos, settings))
    try:
        yield
    finally:
        sweep_task.cancel()
        promote_task.cancel()
        lock_sweep_task.cancel()
        await asyncio.gather(
            sweep_task, promote_task, lock_sweep_task,
            return_exceptions=True,
        )
        await cosmos.stop()


async def _sweep_loop(cosmos: Cosmos, settings: Settings) -> None:
    while True:
        try:
            count = await lease_ops.sweep_expired(cosmos, settings)
            if count:
                log.info("sweep expired %d leases", count)
        except Exception:
            log.exception("sweep failed; will retry")
        await asyncio.sleep(settings.sweep_interval_seconds)


async def _lock_sweep_loop(cosmos: Cosmos, settings: Settings) -> None:
    """Mark expired locks as EXPIRED. Cosmetic — claim_lock can take over
    a HELD-but-time-expired lock directly — but keeps the dashboard
    honest about which scope/key pairs are truly held vs. abandoned."""
    while True:
        try:
            await lock_ops.sweep_expired_locks(cosmos)
        except Exception:
            log.exception("lock sweep failed; will retry")
        await asyncio.sleep(settings.sweep_interval_seconds)


async def _promote_loop(app: FastAPI, settings: Settings) -> None:
    """Periodically retry pending leases against current free capacity.
    Fires workflow_dispatch for each newly-assigned lease."""
    while True:
        try:
            assigned = await lease_ops.promote_pending(app.state.cosmos)
            for lease_doc, host in assigned:
                await _maybe_dispatch_workflow(app, lease_doc, host)
        except Exception:
            log.exception("promote_pending failed; will retry")
        await asyncio.sleep(settings.sweep_interval_seconds)


async def _maybe_dispatch_workflow(app: FastAPI, lease_doc: dict[str, Any], host: Host) -> None:
    """Fire workflow_dispatch for the lease's (project, workflow). Both must
    exist in Cosmos and the project must have a github_repo set."""
    minter: GitHubAppTokenMinter | None = app.state.gh_minter
    if minter is None:
        return

    cosmos: Cosmos = app.state.cosmos
    project_doc = await _read_project(cosmos, lease_doc["project"])
    if not project_doc or not project_doc.get("githubRepo"):
        return

    workflow_name = lease_doc.get("workflow")
    if not workflow_name:
        log.warning("lease %s has no workflow; skipping dispatch", lease_doc["id"])
        return

    workflow_doc = await _read_workflow(cosmos, lease_doc["project"], workflow_name)
    if not workflow_doc or not workflow_doc.get("workflowFilename"):
        return

    inputs = {
        "host": host.name,
        "lease_id": lease_doc["id"],
        **{k: str(v) for k, v in lease_doc.get("metadata", {}).items()},
    }
    try:
        await dispatch_workflow(
            minter,
            repo=project_doc["githubRepo"],
            workflow_filename=workflow_doc["workflowFilename"],
            ref=workflow_doc.get("workflowRef") or "main",
            inputs=inputs,
        )
        log.info(
            "dispatched %s on %s for lease %s (project=%s workflow=%s)",
            workflow_doc["workflowFilename"], host.name, lease_doc["id"],
            lease_doc["project"], workflow_name,
        )
    except Exception:
        log.exception("workflow_dispatch failed for lease %s", lease_doc["id"])


async def _handle_workflow_run(payload: dict[str, Any]) -> dict[str, Any]:
    """workflow_run.completed handler. Two responsibilities:

    1. **Lease release** (belt-and-suspenders). GitHub fires this event when
       a workflow run finishes for *any* reason (success, failure, cancel,
       runner died). We pull lease_id back out of the dispatch inputs and
       release. `release()` is idempotent — if the workflow's own release
       step already fired, this is a no-op.

    2. **Verify-loop substrate** (#18). If the completed run belongs to a
       tracked Run (workflow registered with `retry_workflow_filename`),
       we fetch the verification artifact, record the attempt, run the
       decision engine, and either dispatch the retry workflow or abort
       with an issue comment. The lease release in (1) still happens —
       the retry dispatch acquires its *own* lease.
    """
    if payload.get("action") != "completed":
        return {"ignored": f"workflow_run.{payload.get('action')}"}

    run_data = payload.get("workflow_run") or {}
    inputs = run_data.get("inputs") or {}
    lease_id = inputs.get("lease_id")
    if not lease_id:
        return {"ignored": "no lease_id in inputs"}

    repo = (payload.get("repository") or {}).get("full_name", "")
    cosmos: Cosmos = app.state.cosmos
    matching = await query_all(
        cosmos.projects,
        "SELECT * FROM c WHERE c.githubRepo = @r",
        parameters=[{"name": "@r", "value": repo}],
    )
    if not matching:
        return {"ignored": "no project for repo"}
    project = matching[0]["name"]

    result: dict[str, Any] = {}

    # (1) Lease release — always.
    issue_lock_holder_id: str | None = None
    try:
        released = await lease_ops.release(cosmos, lease_id, project)
        result["released"] = lease_id
        result["lease_state"] = released.state.value
        issue_lock_holder_id = (released.metadata or {}).get("issue_lock_holder_id")
    except Exception as e:
        log.exception("workflow_run release failed for %s", lease_id)
        result["error"] = str(e)
        result["lease_id"] = lease_id

    # (2) Verify-loop substrate — only if the completion lines up with an
    # in-progress Run for this issue.
    run_reached_terminal = False
    is_run_tracked = False
    issue_number_raw = inputs.get("issue_number")
    issue_number: int | None = None
    if issue_number_raw:
        try:
            issue_number = int(issue_number_raw)
        except ValueError:
            issue_number = None

    if issue_number is not None:
        run_lookup = await run_ops.get_active_run(
            cosmos, project=project, issue_number=issue_number,
        )
        if run_lookup is not None:
            is_run_tracked = True
            run, etag = run_lookup
            try:
                run_outcome = await _process_run_completion(
                    run=run, etag=etag, run_data=run_data, repo=repo,
                )
                result["run_id"] = run.id
                result["decision"] = run_outcome
                # The run-tracked lock holder lives on the Run document
                # (survives across retry attempts; lease metadata only
                # carries the latest attempt's lease).
                if run_outcome in (RunDecision.ADVANCE.value,
                                   RunDecision.ABORT_BUDGET_ATTEMPTS.value,
                                   RunDecision.ABORT_BUDGET_COST.value,
                                   RunDecision.ABORT_MALFORMED.value):
                    run_reached_terminal = True
                    issue_lock_holder_id = run.issue_lock_holder_id or issue_lock_holder_id
            except Exception:
                log.exception("verify-loop processing failed for run %s", run.id)
                result["run_error"] = "see logs"

    # (3) Issue-lock release — covers both terminations:
    #   - Run-tracked workflow reached a terminal decision (PASS / ABORT_*).
    #   - Non-Run-tracked workflow's lease released (no Run; treat as done).
    # RETRY decisions intentionally do NOT release: the lock spans the whole
    # verify-loop chain (initial + retries), not per-attempt.
    should_release_lock = (
        issue_lock_holder_id
        and issue_number is not None
        and (run_reached_terminal or not is_run_tracked)
    )
    if should_release_lock:
        try:
            released_lock = await lock_ops.release_lock(
                cosmos, scope="issue",
                key=f"{repo}#{issue_number}",
                holder_id=issue_lock_holder_id,
            )
            result["issue_lock_released"] = released_lock
        except Exception:
            log.exception(
                "issue lock release failed for %s#%s holder=%s",
                repo, issue_number, issue_lock_holder_id,
            )

    return result


async def _process_run_completion(
    *,
    run: Run,
    etag: str,
    run_data: dict[str, Any],
    repo: str,
) -> str:
    """Drive a Run from `workflow_run.completed` through one decision-engine
    cycle. Returns the decision value."""
    cosmos: Cosmos = app.state.cosmos
    minter: GitHubAppTokenMinter | None = app.state.gh_minter
    if minter is None:
        log.warning("no GH minter; cannot fetch verification artifact for run %s", run.id)
        return "skipped_no_minter"

    workflow_run_id = int(run_data.get("id") or 0)
    conclusion = str(run_data.get("conclusion") or "")

    verification_result, archive_url = await fetch_verification(
        minter, repo=repo, run_id=workflow_run_id,
    )

    run, etag = await run_ops.record_completion(
        cosmos,
        run=run,
        etag=etag,
        workflow_run_id=workflow_run_id,
        conclusion=conclusion,
        verification=verification_result,
        artifact_url=archive_url,
    )

    decision = decide(run)
    run, etag = await run_ops.record_decision(cosmos, run=run, etag=etag, decision=decision)

    if decision == RunDecision.ADVANCE:
        await run_ops.mark_passed(cosmos, run=run, etag=etag)
        log.info("run %s passed verification on attempt %d", run.id, len(run.attempts))
        return decision.value

    if decision == RunDecision.RETRY:
        await _dispatch_retry(run=run, etag=etag, repo=repo, archive_url=archive_url)
        return decision.value

    # Any abort decision.
    reason = abort_explanation(run, decision)
    await run_ops.mark_aborted(cosmos, run=run, etag=etag, reason=reason)
    try:
        await post_issue_comment(
            minter, repo=repo, issue_number=run.issue_number, body=reason,
        )
    except Exception:
        log.exception("failed to post abort comment on %s#%d", repo, run.issue_number)
    return decision.value


async def _dispatch_retry(
    *,
    run: Run,
    etag: str,
    repo: str,
    archive_url: str | None,
) -> None:
    """Dispatch the retry workflow for a Run. Acquires a fresh lease, then
    fires workflow_dispatch with `prior_verification_artifact_url` set
    so the retry workflow can pull the previous attempt's verification
    artifact for context."""
    cosmos: Cosmos = app.state.cosmos
    settings: Settings = app.state.settings
    minter: GitHubAppTokenMinter = app.state.gh_minter

    workflow_doc = await _read_workflow(cosmos, run.project, run.workflow)
    if not workflow_doc:
        log.warning("retry: workflow %s/%s vanished; cannot dispatch", run.project, run.workflow)
        return
    retry_filename = workflow_doc.get("retryWorkflowFilename") or workflow_doc.get("retry_workflow_filename")
    if not retry_filename:
        log.warning(
            "retry: workflow %s/%s has no retry_workflow_filename; cannot dispatch",
            run.project, run.workflow,
        )
        return

    # Append the retry attempt *before* dispatching so a webhook redelivery
    # of the previous completion can detect and skip the duplicate decision
    # cycle (record_completion no-ops on already-completed attempts).
    run, _ = await run_ops.append_retry_attempt(
        cosmos, run=run, etag=etag, retry_workflow_filename=retry_filename,
    )

    # Acquire a fresh lease for the retry. Reuses the workflow's
    # default_requirements.
    metadata = {
        "issue_number": str(run.issue_number),
        "issue_repo": run.issue_repo,
        "run_id": run.id,
        "phase": "retry",
        "attempt_index": str(len(run.attempts) - 1),
    }
    lease, host = await lease_ops.acquire(
        cosmos,
        settings,
        project=run.project,
        workflow=run.workflow,
        requirements=workflow_doc.get("defaultRequirements", {}),
        metadata=metadata,
    )

    if host is None:
        # No capacity. The promote_loop will dispatch when a host frees up;
        # but the retry workflow is a different filename than the initial,
        # so promote_loop's _maybe_dispatch_workflow won't know to use the
        # retry filename. For Sprint 1, log and accept — capacity rarely
        # binds at this scale; full pending-retry handling is W1 followup.
        log.warning(
            "retry: no host available for run %s; lease %s pending. "
            "Manual re-dispatch required (see #18 followup).",
            run.id, lease.id,
        )
        return

    inputs = {
        "host": host.name,
        "lease_id": lease.id,
        "issue_number": str(run.issue_number),
        "run_id": run.id,
        "prior_verification_artifact_url": archive_url or "",
        "attempt_index": str(len(run.attempts) - 1),
    }
    try:
        await dispatch_workflow(
            minter,
            repo=repo,
            workflow_filename=retry_filename,
            ref=workflow_doc.get("workflowRef") or "main",
            inputs=inputs,
        )
        log.info(
            "dispatched retry %s on %s for run %s (attempt %d)",
            retry_filename, host.name, run.id, len(run.attempts) - 1,
        )
    except Exception:
        log.exception("retry workflow_dispatch failed for run %s", run.id)


async def _read_project(cosmos: Cosmos, name: str) -> dict[str, Any] | None:
    try:
        return await cosmos.projects.read_item(item=name, partition_key=name)
    except Exception:
        return None


async def _read_workflow(cosmos: Cosmos, project: str, name: str) -> dict[str, Any] | None:
    try:
        return await cosmos.workflows.read_item(item=name, partition_key=project)
    except Exception:
        return None


def _project_to_doc(p: ProjectRegister) -> dict[str, Any]:
    return {
        "id": p.name,
        "name": p.name,
        "githubRepo": p.github_repo,
        "metadata": p.metadata,
        "createdAt": datetime.now(UTC).isoformat(),
    }


def _workflow_to_doc(w: WorkflowRegister) -> dict[str, Any]:
    return {
        "id": w.name,
        "project": w.project,
        "name": w.name,
        "workflowFilename": w.workflow_filename,
        "workflowRef": w.workflow_ref,
        "triggerLabel": w.trigger_label,
        "defaultRequirements": w.default_requirements,
        "retryWorkflowFilename": w.retry_workflow_filename,
        "defaultBudget": w.default_budget.model_dump() if w.default_budget else None,
        "createdAt": datetime.now(UTC).isoformat(),
    }


app = FastAPI(title="glimmung", version="0.1.0", lifespan=lifespan)


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "ok"}


@app.get("/v1/config")
async def public_config() -> dict[str, str]:
    """Public config consumed by the frontend at bootstrap. The client_id is
    not secret but is operationally managed (rotates on tofu re-create), so
    serve it from here instead of baking into the JS bundle.

    Frontend uses MSAL with the standard openid/profile/email scopes and
    sends the resulting ID token to the backend; backend validates it with
    audience=entra_client_id. No custom API scope needed (matches the
    tank-operator pattern exactly)."""
    settings = app.state.settings
    return {
        "entra_client_id": settings.entra_client_id,
        "authority": "https://login.microsoftonline.com/common",
    }


# ─── Lease lifecycle (capability-based via lease_id) ──────────────────────────


@app.post("/v1/lease", response_model=LeaseResponse)
async def create_lease(request: LeaseRequest) -> LeaseResponse:
    lease, host = await lease_ops.acquire(
        app.state.cosmos,
        app.state.settings,
        project=request.project,
        workflow=request.workflow,
        requirements=request.requirements,
        metadata=request.metadata,
        ttl_seconds=request.ttl_seconds,
    )
    return LeaseResponse(lease=lease, host=host)


@app.get("/v1/lease/{lease_id}", response_model=Lease)
async def read_lease(lease_id: str = Path(...), project: str = "") -> Lease:
    """Read a lease by id. Capability auth: possessing the (ULID) lease_id is
    the proof of authorization. The verify-lease step in consumer workflows
    hits this and asserts state=active + host matches inputs.host."""
    if not project:
        raise HTTPException(400, "project query param required")
    cosmos: Cosmos = app.state.cosmos
    try:
        doc = await cosmos.leases.read_item(item=lease_id, partition_key=project)
    except Exception:
        raise HTTPException(404, "lease not found")
    return Lease.model_validate(lease_ops._camel_to_snake(doc))


@app.post("/v1/lease/{lease_id}/heartbeat", response_model=Lease)
async def heartbeat_lease(lease_id: str = Path(...), project: str = "") -> Lease:
    if not project:
        raise HTTPException(400, "project query param required")
    try:
        return await lease_ops.heartbeat(app.state.cosmos, lease_id, project)
    except ValueError as e:
        raise HTTPException(409, str(e))


@app.post("/v1/lease/{lease_id}/release", response_model=Lease)
async def release_lease(lease_id: str = Path(...), project: str = "") -> Lease:
    if not project:
        raise HTTPException(400, "project query param required")
    return await lease_ops.release(app.state.cosmos, lease_id, project)


async def _compute_snapshot(cosmos: Cosmos) -> StateSnapshot:
    host_docs = await query_all(cosmos.hosts, "SELECT * FROM c")
    project_docs = await query_all(cosmos.projects, "SELECT * FROM c")
    workflow_docs = await query_all(cosmos.workflows, "SELECT * FROM c")
    pending_docs = await query_all(
        cosmos.leases,
        "SELECT * FROM c WHERE c.state = @s",
        parameters=[{"name": "@s", "value": LeaseState.PENDING.value}],
    )
    active_docs = await query_all(
        cosmos.leases,
        "SELECT * FROM c WHERE c.state = @s",
        parameters=[{"name": "@s", "value": LeaseState.ACTIVE.value}],
    )
    return StateSnapshot(
        hosts=[Host.model_validate(lease_ops._camel_to_snake(h)) for h in host_docs],
        pending_leases=[Lease.model_validate(lease_ops._camel_to_snake(p)) for p in pending_docs],
        active_leases=[Lease.model_validate(lease_ops._camel_to_snake(a)) for a in active_docs],
        projects=[Project.model_validate(lease_ops._camel_to_snake(d)) for d in project_docs],
        workflows=[Workflow.model_validate(lease_ops._camel_to_snake(d)) for d in workflow_docs],
    )


@app.get("/v1/state", response_model=StateSnapshot)
async def state() -> StateSnapshot:
    return await _compute_snapshot(app.state.cosmos)


@app.get("/v1/events")
async def events(request: Request):
    """SSE stream of state snapshots. Phase 3 v1: poll-and-push every
    snapshot_interval_seconds. A future revision can switch to event-driven
    fan-out (broadcast channel + Cosmos Change Feed) — same wire format."""
    async def gen():
        cosmos: Cosmos = app.state.cosmos
        try:
            while True:
                if await request.is_disconnected():
                    break
                snap = await _compute_snapshot(cosmos)
                yield {"event": "state", "data": snap.model_dump_json()}
                await asyncio.sleep(2)
        except asyncio.CancelledError:
            return
    return EventSourceResponse(gen())


# ─── Admin: projects + hosts ─────────────────────────────────────────────────


@app.post("/v1/projects", response_model=Project, dependencies=[Depends(require_admin_user)])
async def register_project(p: ProjectRegister) -> Project:
    doc = _project_to_doc(p)
    cosmos: Cosmos = app.state.cosmos
    try:
        existing = await cosmos.projects.read_item(item=p.name, partition_key=p.name)
        # Preserve createdAt on update.
        doc["createdAt"] = existing.get("createdAt", doc["createdAt"])
        await cosmos.projects.replace_item(item=p.name, body=doc)
    except Exception:
        await cosmos.projects.create_item(doc)
    return Project.model_validate(lease_ops._camel_to_snake(doc))


@app.get("/v1/projects", response_model=list[Project], dependencies=[Depends(require_admin_user)])
async def list_projects() -> list[Project]:
    docs = await query_all(app.state.cosmos.projects, "SELECT * FROM c")
    return [Project.model_validate(lease_ops._camel_to_snake(d)) for d in docs]


@app.post("/v1/workflows", response_model=Workflow, dependencies=[Depends(require_admin_user)])
async def register_workflow(w: WorkflowRegister) -> Workflow:
    cosmos: Cosmos = app.state.cosmos
    project_doc = await _read_project(cosmos, w.project)
    if not project_doc:
        raise HTTPException(400, f"project {w.project!r} does not exist; register it first")
    doc = _workflow_to_doc(w)
    try:
        existing = await cosmos.workflows.read_item(item=w.name, partition_key=w.project)
        doc["createdAt"] = existing.get("createdAt", doc["createdAt"])
        await cosmos.workflows.replace_item(item=w.name, body=doc)
    except Exception:
        await cosmos.workflows.create_item(doc)
    return Workflow.model_validate(lease_ops._camel_to_snake(doc))


@app.get("/v1/workflows", response_model=list[Workflow], dependencies=[Depends(require_admin_user)])
async def list_workflows() -> list[Workflow]:
    docs = await query_all(app.state.cosmos.workflows, "SELECT * FROM c")
    return [Workflow.model_validate(lease_ops._camel_to_snake(d)) for d in docs]


@app.post("/v1/hosts", response_model=Host, dependencies=[Depends(require_admin_user)])
async def register_host(host: dict[str, Any]) -> Host:
    name = host.get("name")
    if not name:
        raise HTTPException(400, "host.name required")
    cosmos: Cosmos = app.state.cosmos
    try:
        existing = await cosmos.hosts.read_item(item=name, partition_key=name)
        existing["capabilities"] = host.get("capabilities", existing.get("capabilities", {}))
        if "drained" in host:
            existing["drained"] = bool(host["drained"])
        await cosmos.hosts.replace_item(item=name, body=existing)
        return Host.model_validate(lease_ops._camel_to_snake(existing))
    except Exception:
        new_doc = {
            "id": name,
            "name": name,
            "capabilities": host.get("capabilities", {}),
            "currentLeaseId": None,
            "lastHeartbeat": None,
            "lastUsedAt": None,
            "drained": bool(host.get("drained", False)),
            "createdAt": datetime.now(UTC).isoformat(),
        }
        await cosmos.hosts.create_item(new_doc)
        return Host.model_validate(lease_ops._camel_to_snake(new_doc))


# ─── GitHub webhook ───────────────────────────────────────────────────────────


@app.post("/v1/webhook/github")
async def github_webhook(request: Request) -> dict[str, Any]:
    settings: Settings = app.state.settings
    if not settings.github_webhook_secret:
        raise HTTPException(503, "webhook disabled (no secret configured)")

    body = await request.body()
    sig = request.headers.get("X-Hub-Signature-256")
    if not verify_webhook_signature(settings.github_webhook_secret, body, sig):
        raise HTTPException(401, "invalid signature")

    event = request.headers.get("X-GitHub-Event", "")
    payload = json.loads(body)

    if event == "workflow_run":
        return await _handle_workflow_run(payload)
    if event != "issues":
        return {"ignored": event}
    action = payload.get("action")
    issue = payload.get("issue", {})
    repo = payload.get("repository", {}).get("full_name", "")
    label = (payload.get("label") or {}).get("name") if action == "labeled" else None

    if not repo or not issue or issue.get("number") is None:
        return {"ignored": "missing fields"}

    # Match a workflow whose trigger_label fires on this event. The
    # label-trigger path is preserved (#20: "the label-trigger path
    # keeps working unchanged") even though labels are no longer the
    # primary primitive — UI dispatch is the new first-class trigger
    # source. Both paths route through `dispatch_run` below.
    cosmos: Cosmos = app.state.cosmos
    matching = await query_all(
        cosmos.projects,
        "SELECT * FROM c WHERE c.githubRepo = @r",
        parameters=[{"name": "@r", "value": repo}],
    )
    if not matching:
        return {"ignored": "no project for repo"}
    project_name = matching[0]["name"]

    label_names = [lab["name"] for lab in issue.get("labels", []) if isinstance(lab, dict)]
    workflows_for_project = await query_all(
        cosmos.workflows,
        "SELECT * FROM c WHERE c.project = @p",
        parameters=[{"name": "@p", "value": project_name}],
    )
    matched_workflow_name: str | None = None
    for w in sorted(workflows_for_project, key=lambda d: d.get("name", "")):
        trigger = w.get("triggerLabel", "")
        if not trigger:
            continue
        fires = (
            (action == "labeled" and label == trigger)
            or (action in ("opened", "reopened") and trigger in label_names)
        )
        if fires:
            matched_workflow_name = w["name"]
            break

    if matched_workflow_name is None:
        return {"ignored": f"no workflow matched action={action} label={label}"}

    result = await dispatch_run(
        app,
        repo=repo,
        issue_number=int(issue["number"]),
        trigger_source={
            "kind": "label_webhook",
            "label": label,
            "action": action or "",
            "issue_title": str(issue.get("title", ""))[:200],
        },
        workflow_name=matched_workflow_name,
        issue_labels=label_names,
        extra_metadata={
            "issue_title": str(issue.get("title", ""))[:200],
            "gh_event": event,
            "gh_action": action or "",
        },
    )
    return result.model_dump()


# ─── Issues view + UI-initiated dispatch (#20) ────────────────────────────────


class IssueRow(BaseModel):
    """One row in the Issues view: a GH issue surfaced for the user to
    dispatch. Pulled live from the GH API per request — no caching;
    single-user scale doesn't merit it."""
    project: str
    repo: str
    number: int
    title: str
    labels: list[str] = Field(default_factory=list)
    html_url: str
    last_run_id: str | None = None
    last_run_state: str | None = None  # "in_progress" | "passed" | "aborted" | None
    last_run_abort_reason: str | None = None
    issue_lock_held: bool = False  # convenience: lock currently held → in flight


class IssueDetail(BaseModel):
    project: str
    repo: str
    number: int
    title: str
    body: str = ""
    labels: list[str] = Field(default_factory=list)
    html_url: str
    last_run_id: str | None = None
    last_run_state: str | None = None
    issue_lock_held: bool = False


class DispatchRequest(BaseModel):
    repo: str
    issue_number: int
    workflow: str | None = None  # optional; dispatch_run picks if omitted+unambiguous


@app.get(
    "/v1/issues",
    response_model=list[IssueRow],
    dependencies=[Depends(require_admin_user)],
)
async def list_issues() -> list[IssueRow]:
    """All open issues across registered repos. Live GH API call per
    request via the GH App installation token. Filters out PRs (the GH
    REST issues endpoint returns them by default).

    Labels are surfaced informationally only — they're a courtesy
    syndication surface in the post-#20 model, not a dispatch
    primitive. The dispatch button on each row is the trigger."""
    cosmos: Cosmos = app.state.cosmos
    minter: GitHubAppTokenMinter | None = app.state.gh_minter
    if minter is None:
        raise HTTPException(503, "github app credentials not configured")

    project_docs = await query_all(cosmos.projects, "SELECT * FROM c")
    rows: list[IssueRow] = []
    for project_doc in project_docs:
        repo = project_doc.get("githubRepo") or ""
        if not repo:
            continue
        try:
            issues = await list_open_issues(minter, repo=repo)
        except Exception:
            log.exception("list_open_issues failed for %s; skipping", repo)
            continue
        for issue in issues:
            number = int(issue["number"])
            labels = [lab["name"] for lab in issue.get("labels", []) if isinstance(lab, dict)]
            row = IssueRow(
                project=project_doc["name"],
                repo=repo,
                number=number,
                title=str(issue.get("title", "")),
                labels=labels,
                html_url=str(issue.get("html_url", "")),
            )
            latest_run = await run_ops.get_latest_run(
                cosmos, project=project_doc["name"], issue_number=number,
            )
            if latest_run is not None:
                row.last_run_id = latest_run.id
                row.last_run_state = latest_run.state.value
                row.last_run_abort_reason = latest_run.abort_reason
            existing_lock = await lock_ops.read_lock(
                cosmos, scope="issue", key=f"{repo}#{number}",
            )
            row.issue_lock_held = (
                existing_lock is not None
                and existing_lock.state.value == "held"
                and existing_lock.expires_at > datetime.now(UTC)
            )
            rows.append(row)

    rows.sort(key=lambda r: (r.project, -r.number))
    return rows


@app.get(
    "/v1/issues/{repo_owner}/{repo_name}/{issue_number}",
    response_model=IssueDetail,
    dependencies=[Depends(require_admin_user)],
)
async def issue_detail(
    repo_owner: str = Path(...),
    repo_name: str = Path(...),
    issue_number: int = Path(...),
) -> IssueDetail:
    """Detail view: title + body + last-run summary + lock state. Live
    GH API call. Three-segment path so the repo owner/name pair stays
    URL-friendly without a query param."""
    cosmos: Cosmos = app.state.cosmos
    minter: GitHubAppTokenMinter | None = app.state.gh_minter
    if minter is None:
        raise HTTPException(503, "github app credentials not configured")

    repo = f"{repo_owner}/{repo_name}"
    matching = await query_all(
        cosmos.projects,
        "SELECT * FROM c WHERE c.githubRepo = @r",
        parameters=[{"name": "@r", "value": repo}],
    )
    if not matching:
        raise HTTPException(404, f"no project registered for {repo!r}")
    project_name = matching[0]["name"]

    try:
        issue = await get_issue(minter, repo=repo, issue_number=issue_number)
    except Exception:
        log.exception("get_issue failed for %s#%d", repo, issue_number)
        raise HTTPException(502, "github api error fetching issue")

    detail = IssueDetail(
        project=project_name,
        repo=repo,
        number=issue_number,
        title=str(issue.get("title", "")),
        body=str(issue.get("body") or ""),
        labels=[lab["name"] for lab in issue.get("labels", []) if isinstance(lab, dict)],
        html_url=str(issue.get("html_url", "")),
    )
    latest_run = await run_ops.get_latest_run(
        cosmos, project=project_name, issue_number=issue_number,
    )
    if latest_run is not None:
        detail.last_run_id = latest_run.id
        detail.last_run_state = latest_run.state.value
    existing_lock = await lock_ops.read_lock(
        cosmos, scope="issue", key=f"{repo}#{issue_number}",
    )
    detail.issue_lock_held = (
        existing_lock is not None
        and existing_lock.state.value == "held"
        and existing_lock.expires_at > datetime.now(UTC)
    )
    return detail


@app.post(
    "/v1/runs/dispatch",
    response_model=DispatchResult,
    dependencies=[Depends(require_admin_user)],
)
async def dispatch_run_endpoint(req: DispatchRequest) -> DispatchResult:
    """UI-initiated dispatch. Same code path as the label-webhook handler:
    both call `dispatch_run` from glimmung.dispatch. The trigger source
    is recorded on the resulting Run for W6 observability."""
    return await dispatch_run(
        app,
        repo=req.repo,
        issue_number=req.issue_number,
        trigger_source={"kind": "glimmung_ui"},
        workflow_name=req.workflow,
    )


# ─── Static frontend ──────────────────────────────────────────────────────────
# Mounted last so the API routes win. Frontend is built into /app/static by
# the multi-stage Dockerfile; locally it lives at <repo>/frontend/dist.

_static_env = os.environ.get("GLIMMUNG_STATIC_DIR")
_static = FsPath(_static_env) if _static_env else FsPath(__file__).resolve().parent / "static"
if _static.exists():
    if (_static / "assets").exists():
        app.mount("/assets", StaticFiles(directory=_static / "assets"), name="assets")

    @app.get("/")
    async def serve_index() -> FileResponse:
        return FileResponse(_static / "index.html")

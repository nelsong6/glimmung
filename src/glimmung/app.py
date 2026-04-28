import asyncio
import json
import logging
from contextlib import asynccontextmanager
from datetime import UTC, datetime
from typing import Any

from fastapi import Depends, FastAPI, HTTPException, Path, Request

from glimmung import leases as lease_ops
from glimmung.auth import require_entra_user
from glimmung.db import Cosmos, query_all
from glimmung.github_app import (
    GitHubAppTokenMinter,
    dispatch_workflow,
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
    StateSnapshot,
)
from glimmung.settings import Settings, get_settings

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
    try:
        yield
    finally:
        sweep_task.cancel()
        promote_task.cancel()
        await asyncio.gather(sweep_task, promote_task, return_exceptions=True)
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
    """If the lease's project has a configured workflow, fire workflow_dispatch."""
    minter: GitHubAppTokenMinter | None = app.state.gh_minter
    if minter is None:
        return

    project_doc = await _read_project(app.state.cosmos, lease_doc["project"])
    if not project_doc or not project_doc.get("githubRepo") or not project_doc.get("workflowFilename"):
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
            workflow_filename=project_doc["workflowFilename"],
            ref=project_doc.get("workflowRef") or "main",
            inputs=inputs,
        )
        log.info("dispatched %s on %s for lease %s", project_doc["workflowFilename"], host.name, lease_doc["id"])
    except Exception:
        log.exception("workflow_dispatch failed for lease %s", lease_doc["id"])


async def _read_project(cosmos: Cosmos, name: str) -> dict[str, Any] | None:
    try:
        return await cosmos.projects.read_item(item=name, partition_key=name)
    except Exception:
        return None


def _project_to_doc(p: ProjectRegister) -> dict[str, Any]:
    return {
        "id": p.name,
        "name": p.name,
        "githubRepo": p.github_repo,
        "workflowFilename": p.workflow_filename,
        "workflowRef": p.workflow_ref,
        "triggerLabel": p.trigger_label,
        "defaultRequirements": p.default_requirements,
        "createdAt": datetime.now(UTC).isoformat(),
    }


app = FastAPI(title="glimmung", version="0.1.0", lifespan=lifespan)


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "ok"}


# ─── Lease lifecycle (capability-based via lease_id) ──────────────────────────


@app.post("/v1/lease", response_model=LeaseResponse)
async def create_lease(request: LeaseRequest) -> LeaseResponse:
    lease, host = await lease_ops.acquire(
        app.state.cosmos,
        app.state.settings,
        project=request.project,
        requirements=request.requirements,
        metadata=request.metadata,
        ttl_seconds=request.ttl_seconds,
    )
    return LeaseResponse(lease=lease, host=host)


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


@app.get("/v1/state", response_model=StateSnapshot)
async def state() -> StateSnapshot:
    cosmos: Cosmos = app.state.cosmos
    host_docs = await query_all(cosmos.hosts, "SELECT * FROM c")
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
    )


# ─── Admin: projects + hosts ─────────────────────────────────────────────────


@app.post("/v1/projects", response_model=Project, dependencies=[Depends(require_entra_user)])
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


@app.get("/v1/projects", response_model=list[Project], dependencies=[Depends(require_entra_user)])
async def list_projects() -> list[Project]:
    docs = await query_all(app.state.cosmos.projects, "SELECT * FROM c")
    return [Project.model_validate(lease_ops._camel_to_snake(d)) for d in docs]


@app.post("/v1/hosts", response_model=Host, dependencies=[Depends(require_entra_user)])
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
    if event != "issues":
        return {"ignored": event}

    payload = json.loads(body)
    action = payload.get("action")
    issue = payload.get("issue", {})
    repo = payload.get("repository", {}).get("full_name", "")
    label = (payload.get("label") or {}).get("name") if action == "labeled" else None

    if not repo or not issue:
        return {"ignored": "missing fields"}

    # Find project by github_repo. Cross-partition scan; tiny container.
    cosmos: Cosmos = app.state.cosmos
    matching = await query_all(
        cosmos.projects,
        "SELECT * FROM c WHERE c.githubRepo = @r",
        parameters=[{"name": "@r", "value": repo}],
    )
    if not matching:
        return {"ignored": "no project for repo"}
    project_doc = matching[0]

    # Decide whether this event triggers a lease.
    trigger_label = project_doc.get("triggerLabel", "issue-agent")
    label_names = {l["name"] for l in issue.get("labels", []) if isinstance(l, dict)}
    fires = (
        (action == "labeled" and label == trigger_label)
        or (action in ("opened", "reopened") and trigger_label in label_names)
    )
    if not fires:
        return {"ignored": f"action={action} label={label}"}

    metadata = {
        "issueNumber": issue.get("number"),
        "issueTitle": issue.get("title", "")[:200],
        "ghEvent": event,
        "ghAction": action,
    }

    lease, host = await lease_ops.acquire(
        cosmos,
        settings,
        project=project_doc["name"],
        requirements=project_doc.get("defaultRequirements", {}),
        metadata=metadata,
    )

    if host is not None:
        await _maybe_dispatch_workflow(
            app,
            {**lease_ops._lease_to_doc(lease), "id": lease.id, "project": lease.project},
            host,
        )

    return {"lease_id": lease.id, "state": lease.state.value, "host": host.name if host else None}

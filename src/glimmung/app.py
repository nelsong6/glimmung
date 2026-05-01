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

from glimmung import issues as issue_ops
from glimmung import leases as lease_ops
from glimmung import locks as lock_ops
from glimmung import runs as run_ops
from glimmung import signals as signal_ops
from glimmung.dispatch import DispatchResult, dispatch_run
from glimmung.auth import require_admin_user
from glimmung.db import Cosmos, query_all
from glimmung.decision import abort_explanation, decide
from glimmung.github_app import (
    GitHubAppTokenMinter,
    cancel_workflow_run,
    dispatch_workflow,
    list_open_issues as gh_list_open_issues,
    post_issue_comment,
    verify_webhook_signature,
)
from glimmung.locks import LockBusy
from glimmung.models import (
    Host,
    Issue,
    IssueState,
    Lease,
    LeaseRequest,
    LeaseResponse,
    LeaseState,
    Project,
    ProjectRegister,
    Run,
    RunDecision,
    Signal,
    SignalEnqueueRequest,
    SignalSource,
    SignalTargetType,
    StateSnapshot,
    TriageDecision,
    Workflow,
    WorkflowRegister,
)
from glimmung.triage import abort_explanation as triage_abort_explanation
from glimmung.triage import decide_triage, feedback_text
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
    drain_task = asyncio.create_task(_signal_drain_loop(app, settings))
    try:
        yield
    finally:
        sweep_task.cancel()
        promote_task.cancel()
        lock_sweep_task.cancel()
        drain_task.cancel()
        await asyncio.gather(
            sweep_task, promote_task, lock_sweep_task, drain_task,
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


async def _signal_drain_loop(app: FastAPI, settings: Settings) -> None:
    """Process the signal bus (#19). Each tick walks pending signals
    oldest-first, claims the per-target lock from #22, runs the
    triage decision engine, and applies the side effect (workflow
    dispatch, comment, no-op). Per-target serialization is free —
    signals on a target whose lock is held stay PENDING and re-evaluate
    next tick.

    Tick interval is fast (every 2s) so user actions feel responsive;
    cost is negligible (cross-partition pending query is one round-trip
    on a tiny container)."""
    drain_interval = max(2, settings.sweep_interval_seconds // 30)
    while True:
        try:
            await signal_ops.drain_signals(
                app.state.cosmos,
                settings=settings,
                decide_fn=lambda s: _triage_decide(app, s),
                apply_fn=lambda s, d, h: _triage_apply(app, s, d, h),
            )
        except Exception:
            log.exception("signal drain failed; will retry")
        await asyncio.sleep(drain_interval)


async def _triage_decide(app: FastAPI, signal: Signal) -> tuple[str, bool]:
    """Drain decide_fn for triage: look up the Run linked to the PR,
    invoke the pure decision engine, return (decision_value, hold_lock).
    `hold_lock=True` only for DISPATCH_TRIAGE — the triage workflow's
    terminal handler (`_handle_workflow_run`) releases the lock on
    Run terminal transition."""
    cosmos: Cosmos = app.state.cosmos

    run: Run | None = None
    if signal.target_type == SignalTargetType.PR:
        try:
            pr_number = int(signal.target_id)
        except ValueError:
            log.warning("triage_decide: signal %s target_id %r is not an int",
                        signal.id, signal.target_id)
            return (TriageDecision.ABORT_NO_RUN.value, False)
        lookup = await run_ops.find_run_by_pr(
            cosmos, issue_repo=signal.target_repo, pr_number=pr_number,
        )
        run = lookup[0] if lookup else None
    # Issue/Run scoped signals don't yet have triage decision logic;
    # IGNORE them so they don't sit in PENDING forever.
    elif signal.target_type != SignalTargetType.PR:
        return (TriageDecision.IGNORE.value, False)

    decision = decide_triage(signal=signal, run=run)
    hold_lock = decision == TriageDecision.DISPATCH_TRIAGE
    return (decision.value, hold_lock)


async def _triage_apply(
    app: FastAPI,
    signal: Signal,
    decision: str,
    holder_id: str,
) -> None:
    """Drain apply_fn for triage: side effects according to the decision.
    Lock release semantics are caller-managed: the drain releases for
    IGNORE/ABORT (since `_triage_decide` returned hold_lock=False); for
    DISPATCH_TRIAGE the lock stays held and the workflow_run.completed
    terminal handler releases it."""
    cosmos: Cosmos = app.state.cosmos

    if decision == TriageDecision.IGNORE.value:
        return

    # Look up the run + post a comment / dispatch as appropriate.
    run: Run | None = None
    etag: str | None = None
    if signal.target_type == SignalTargetType.PR:
        try:
            pr_number = int(signal.target_id)
        except ValueError:
            return
        lookup = await run_ops.find_run_by_pr(
            cosmos, issue_repo=signal.target_repo, pr_number=pr_number,
        )
        if lookup is not None:
            run, etag = lookup

    if decision == TriageDecision.DISPATCH_TRIAGE.value:
        if run is None or etag is None:
            log.warning("triage_apply: DISPATCH_TRIAGE but no run; signal %s", signal.id)
            return
        await _dispatch_triage(app, signal=signal, run=run, etag=etag, holder_id=holder_id)
        return

    # Any abort decision: post a comment to the PR explaining why no
    # action was taken. The lock has already been released by the drain.
    if decision in (
        TriageDecision.ABORT_NO_RUN.value,
        TriageDecision.ABORT_BUDGET_ATTEMPTS.value,
        TriageDecision.ABORT_BUDGET_COST.value,
    ):
        try:
            decision_enum = TriageDecision(decision)
        except ValueError:
            return
        body = triage_abort_explanation(decision_enum, run, signal)
        minter: GitHubAppTokenMinter | None = app.state.gh_minter
        if minter is None:
            log.info("triage_apply: no GH minter; would have posted: %s", body[:80])
            return
        try:
            pr_number = int(signal.target_id)
            await post_issue_comment(
                minter, repo=signal.target_repo,
                issue_number=pr_number, body=body,
            )
        except Exception:
            log.exception(
                "triage_apply: failed to post abort comment on %s#%s",
                signal.target_repo, signal.target_id,
            )


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

    # (4) PR-lock release — only fires when a triage cycle reached a
    # terminal Run state. The Run document carries pr_number +
    # pr_lock_holder_id (set by _dispatch_triage when re-opening for
    # triage). Release uses run.pr_lock_holder_id; idempotent.
    if run_reached_terminal and is_run_tracked and run_lookup is not None:
        run_for_locks, _ = run_lookup  # the original run, not the post-decision one
        # Re-read the run to get the *latest* pr_lock_holder_id (in
        # case the Run was re-opened for triage between when we read
        # it above and now). Cheap point-read.
        try:
            doc = await cosmos.runs.read_item(
                item=run_for_locks.id, partition_key=run_for_locks.project,
            )
            pr_lock_holder = doc.get("pr_lock_holder_id")
            pr_number = doc.get("pr_number")
        except Exception:
            pr_lock_holder = run_for_locks.pr_lock_holder_id
            pr_number = run_for_locks.pr_number

        if pr_lock_holder and pr_number:
            try:
                released_pr_lock = await lock_ops.release_lock(
                    cosmos, scope="pr",
                    key=f"{repo}#{pr_number}",
                    holder_id=pr_lock_holder,
                )
                result["pr_lock_released"] = released_pr_lock
            except Exception:
                log.exception(
                    "pr lock release failed for %s#%s holder=%s",
                    repo, pr_number, pr_lock_holder,
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


async def _dispatch_triage(
    app: FastAPI,
    *,
    signal: Signal,
    run: Run,
    etag: str,
    holder_id: str,
) -> None:
    """Re-open a Run for triage and fire the consumer's triage workflow.

    Triage state machine: PASSED → IN_PROGRESS, with a new TRIAGE
    PhaseAttempt appended. Both the issue lock and the PR lock are
    held with `holder_id` (the signal_id, set on the Run for terminal
    handler release). The triage workflow runs impl + verify with
    `feedback_text` as additional context; on terminal Run transition
    (PASS / ABORT_*) the workflow_run.completed handler releases both
    locks. RETRY decisions within a triage cycle dispatch the regular
    retry workflow and keep both locks held."""
    cosmos: Cosmos = app.state.cosmos
    settings: Settings = app.state.settings
    minter: GitHubAppTokenMinter | None = app.state.gh_minter

    workflow_doc = await _read_workflow(cosmos, run.project, run.workflow)
    if not workflow_doc:
        log.warning(
            "triage: workflow %s/%s vanished; cannot dispatch",
            run.project, run.workflow,
        )
        return
    triage_filename = (
        workflow_doc.get("triageWorkflowFilename")
        or workflow_doc.get("triage_workflow_filename")
        or ""
    )
    if not triage_filename:
        log.warning(
            "triage: workflow %s/%s has no triage_workflow_filename; cannot dispatch",
            run.project, run.workflow,
        )
        return

    # Claim the issue lock with the signal as holder. If the issue
    # lock is currently held (rare: original Run is still in-flight,
    # or a prior triage is still in flight on the same issue), bail
    # — drain will retry next tick.
    try:
        await lock_ops.claim_lock(
            cosmos, scope="issue",
            key=f"{run.issue_repo}#{run.issue_number}",
            holder_id=holder_id,
            ttl_seconds=settings.lease_default_ttl_seconds,
            metadata={"triage_signal_id": signal.id, "phase": "triage"},
        )
    except LockBusy as busy:
        log.warning(
            "triage: issue lock %s#%d is held by %s; deferring signal %s",
            run.issue_repo, run.issue_number, busy.lock.held_by, signal.id,
        )
        return

    # Re-open the Run + append the TRIAGE attempt before dispatching,
    # so a webhook redelivery of the previous completion can detect
    # and skip duplicate decision cycles.
    run, etag = await run_ops.reopen_for_triage(
        cosmos, run=run, etag=etag,
        triage_workflow_filename=triage_filename,
        pr_lock_holder_id=holder_id,
        issue_lock_holder_id=holder_id,
    )

    metadata = {
        "issue_number": str(run.issue_number),
        "issue_repo": run.issue_repo,
        "run_id": run.id,
        "phase": "triage",
        "attempt_index": str(len(run.attempts) - 1),
        "issue_lock_holder_id": holder_id,
    }
    lease, host = await lease_ops.acquire(
        cosmos, settings,
        project=run.project, workflow=run.workflow,
        requirements=workflow_doc.get("defaultRequirements", {}),
        metadata=metadata,
    )

    if host is None:
        log.warning(
            "triage: no host available for run %s; lease %s pending. "
            "Manual re-dispatch may be required.",
            run.id, lease.id,
        )
        return

    if minter is None:
        log.warning("triage: no GH minter; cannot dispatch workflow for run %s", run.id)
        return

    feedback = feedback_text(signal)
    inputs = {
        "host": host.name,
        "lease_id": lease.id,
        "issue_number": str(run.issue_number),
        "pr_number": str(run.pr_number) if run.pr_number is not None else "",
        "run_id": run.id,
        "attempt_index": str(len(run.attempts) - 1),
        "feedback": feedback,
        "prior_verification_artifact_url": "",
    }
    try:
        await dispatch_workflow(
            minter,
            repo=run.issue_repo,
            workflow_filename=triage_filename,
            ref=workflow_doc.get("workflowRef") or "main",
            inputs=inputs,
        )
        log.info(
            "dispatched triage %s on %s for run %s (attempt %d, signal %s)",
            triage_filename, host.name, run.id, len(run.attempts) - 1, signal.id,
        )
    except Exception:
        log.exception(
            "triage workflow_dispatch failed for run %s signal %s",
            run.id, signal.id,
        )


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
        "triageWorkflowFilename": w.triage_workflow_filename,
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


# ─── Lease lifecycle (capability-based via lease_id) ─────────────────────────────


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


class CancelLeaseResult(BaseModel):
    """Outcome of POST /v1/lease/{lease_id}/cancel.

    `state`:
      - `cancelled`: lease released and (if Run-tracked + GH-dispatched)
        a GH workflow_run cancel was POSTed. The actual GH-side state
        flip arrives later via `workflow_run.completed`; the handler is
        idempotent so this doesn't conflict.
      - `no_active_run`: lease released, but there was no associated Run
        with a GH workflow_run_id to cancel. Either a non-Run-tracked
        lease, or a Run that hadn't yet been dispatched at GH-side
        (lease still in PENDING when cancelled, or the dispatch_workflow
        call hadn't completed). Lease + locks still released.
      - `already_terminal`: lease was already RELEASED or EXPIRED, or the
        Run was already in a terminal state. No side effects beyond a
        re-read.
    """
    state: str
    lease_id: str
    run_id: str | None = None
    gh_run_cancelled: bool | None = None
    issue_lock_released: bool | None = None
    pr_lock_released: bool | None = None


async def _cancel_lease(
    cosmos: Cosmos,
    minter: GitHubAppTokenMinter | None,
    lease_id: str,
    project: str,
) -> CancelLeaseResult:
    """Operator-initiated cancel of an active lease (#30).

    Mirrors the release path of `_handle_workflow_run`: cancels the GH
    workflow_run (so the runner stops working a doomed job), marks the
    Run ABORTED with reason="cancelled_via_ui", releases the lease, and
    releases any locks the Run was holding (issue + PR scopes). All
    sub-steps are idempotent — safe under a race with the natural
    `workflow_run.completed` arrival.

    Free function (rather than a method on the endpoint) so the test
    suite can drive it directly with a `cosmos_fake`-backed cosmos and
    a stub minter, matching the existing test pattern around
    `_handle_workflow_run`.
    """
    # 1. Read the lease.
    try:
        lease_doc = await cosmos.leases.read_item(item=lease_id, partition_key=project)
    except Exception:
        raise HTTPException(404, f"lease {lease_id} not found in project {project!r}")

    if lease_doc["state"] in (LeaseState.RELEASED.value, LeaseState.EXPIRED.value):
        return CancelLeaseResult(state="already_terminal", lease_id=lease_id)

    metadata = lease_doc.get("metadata") or {}
    issue_repo: str | None = metadata.get("issue_repo")
    issue_number_raw = metadata.get("issue_number")
    issue_lock_holder_id: str | None = metadata.get("issue_lock_holder_id")
    issue_number: int | None = None
    if issue_number_raw:
        try:
            issue_number = int(issue_number_raw)
        except ValueError:
            issue_number = None

    # 2. Find the active Run for this lease's issue, if any.
    run = None
    run_etag: str | None = None
    if issue_number is not None:
        run_lookup = await run_ops.get_active_run(
            cosmos, project=project, issue_number=issue_number,
        )
        if run_lookup is not None:
            run, run_etag = run_lookup

    # 3. GH cancel + Run abort. Skipped if there's no Run, or the Run
    # has no dispatched GH workflow_run yet (e.g. PENDING lease).
    gh_cancelled: bool | None = None
    if run is not None and run.attempts and issue_repo and minter is not None:
        latest = run.attempts[-1]
        gh_run_id = latest.workflow_run_id
        if gh_run_id is not None:
            try:
                gh_cancelled = await cancel_workflow_run(
                    minter, repo=issue_repo, run_id=gh_run_id,
                )
            except Exception:
                log.exception(
                    "cancel_lease: GH cancel failed for run %s (workflow_run_id=%d); "
                    "proceeding with lease release",
                    run.id, gh_run_id,
                )
                gh_cancelled = False

    if run is not None and run_etag is not None:
        try:
            await run_ops.mark_aborted(
                cosmos, run=run, etag=run_etag, reason="cancelled_via_ui",
            )
        except Exception:
            log.exception("cancel_lease: failed to mark run %s aborted", run.id)

    # 4. Release the lease.
    try:
        await lease_ops.release(cosmos, lease_id, project)
    except Exception:
        log.exception("cancel_lease: lease release failed for %s", lease_id)
        raise HTTPException(500, f"lease release failed for {lease_id}")

    # 5. Release the issue lock (if held) and PR lock (if Run was holding one).
    issue_lock_released: bool | None = None
    if issue_lock_holder_id and issue_repo and issue_number is not None:
        try:
            issue_lock_released = bool(await lock_ops.release_lock(
                cosmos, scope="issue",
                key=f"{issue_repo}#{issue_number}",
                holder_id=issue_lock_holder_id,
            ))
        except Exception:
            log.exception(
                "cancel_lease: issue lock release failed for %s#%s holder=%s",
                issue_repo, issue_number, issue_lock_holder_id,
            )

    pr_lock_released: bool | None = None
    if run is not None and run.pr_lock_holder_id and run.pr_number and issue_repo:
        try:
            pr_lock_released = bool(await lock_ops.release_lock(
                cosmos, scope="pr",
                key=f"{issue_repo}#{run.pr_number}",
                holder_id=run.pr_lock_holder_id,
            ))
        except Exception:
            log.exception(
                "cancel_lease: pr lock release failed for %s#%s holder=%s",
                issue_repo, run.pr_number, run.pr_lock_holder_id,
            )

    state = "cancelled" if (run is not None and gh_cancelled is not None) else "no_active_run"
    return CancelLeaseResult(
        state=state,
        lease_id=lease_id,
        run_id=run.id if run is not None else None,
        gh_run_cancelled=gh_cancelled,
        issue_lock_released=issue_lock_released,
        pr_lock_released=pr_lock_released,
    )


@app.post(
    "/v1/lease/{lease_id}/cancel",
    response_model=CancelLeaseResult,
    dependencies=[Depends(require_admin_user)],
)
async def cancel_lease(lease_id: str = Path(...), project: str = "") -> CancelLeaseResult:
    """Admin-only endpoint that frees a host immediately by cancelling the
    GH workflow run and releasing the lease + locks. See `_cancel_lease`
    for the body of the operation."""
    if not project:
        raise HTTPException(400, "project query param required")
    return await _cancel_lease(
        app.state.cosmos,
        getattr(app.state, "gh_minter", None),
        lease_id,
        project,
    )


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


# ─── Admin: projects + hosts ──────────────────────────────────────────────────


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


# ─── GitHub webhook ───────────────────────────────────────────────────────


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
    if event == "pull_request":
        return await _handle_pull_request(payload)
    if event == "pull_request_review":
        return await _handle_pull_request_review(payload)
    # `issues` events used to mirror into the `issues` Cosmos container
    # and label-trigger workflow dispatch. Per #50, glimmung is the
    # source of truth for issues — GH issues are post-seed irrelevant.
    # The mirror helper survives for the one-shot backfill / seed paths;
    # the webhook just ignores the event here.
    return {"ignored": event}


async def _mirror_github_issue(
    cosmos: Cosmos,
    *,
    project: str,
    repo: str,
    action: str,
    issue_payload: dict[str, Any],
) -> dict[str, Any]:
    """Sync a GH issue's state into the glimmung `issues` container.

    Called for every `issues` webhook event. Actions that change the
    open/closed state (`closed`, `reopened`) drive the Issue state
    machine; `edited` / `labeled` / `unlabeled` patch fields; `opened`
    is treated as ensure-and-stamp-fields (the dispatch path may have
    minted a placeholder Issue earlier, so even on a fresh `opened` we
    still update title/body/labels in case they differ from the
    minted defaults).

    Other actions (`deleted`, `transferred`, `pinned`, `assigned`, …)
    are no-ops for now — the substrate's read path doesn't surface
    them, and tracking them would just bloat update churn.
    """
    issue_number = issue_payload.get("number")
    if issue_number is None:
        return {"ignored": "no issue number"}

    title = issue_payload.get("title") or ""
    body = issue_payload.get("body") or ""
    labels = [
        lab["name"] for lab in (issue_payload.get("labels") or [])
        if isinstance(lab, dict) and "name" in lab
    ]
    gh_state = issue_payload.get("state") or "open"

    # Ensure the Issue exists. ensure_issue_for_github populates the
    # denormalized GH coords; on a real `opened` event we want the
    # Issue's title/body/labels to match GH, not the placeholder
    # `repo#N` title that `ensure_issue_for_github` synthesizes when
    # the dispatch-path mints first. So we always patch after the
    # ensure, except for actions where there's nothing to patch.
    issue, etag, created = await issue_ops.ensure_issue_for_github(
        cosmos,
        project=project,
        repo=repo,
        issue_number=int(issue_number),
        title=title,
        body=body,
        labels=labels,
    )
    outcome: dict[str, Any] = {
        "issue_id": issue.id,
        "created": created,
    }

    if action in ("opened", "edited", "labeled", "unlabeled", "reopened"):
        # Patch the user-editable fields. ensure_issue_for_github only
        # honors title/body/labels on create; for an existing Issue we
        # need an explicit update to keep Cosmos in sync with GH edits.
        if not created:
            issue, etag = await issue_ops.update_issue(
                cosmos, issue=issue, etag=etag,
                title=title or None,
                body=body if body else None,
                labels=labels,
            )
            outcome["patched"] = True

    if action == "reopened" and gh_state == "open":
        issue, etag = await issue_ops.reopen_issue(cosmos, issue=issue, etag=etag)
        outcome["reopened"] = True
    elif action == "closed" or gh_state == "closed":
        issue, etag = await issue_ops.close_issue(cosmos, issue=issue, etag=etag)
        outcome["closed"] = True

    return outcome


# ─── PR webhook handlers (#19) ────────────────────────────────────────────────


_CLOSES_KEYWORDS_RE = None  # set on first call below
def _parse_issue_refs(body: str) -> list[int]:
    """Extract issue numbers from PR body 'Closes #N' / 'Fixes #N' /
    'Resolves #N' patterns (case-insensitive). Conservative — only
    same-repo references; cross-repo `owner/repo#N` is ignored
    because we only auto-link within the project."""
    import re
    global _CLOSES_KEYWORDS_RE
    if _CLOSES_KEYWORDS_RE is None:
        _CLOSES_KEYWORDS_RE = re.compile(
            r"\b(?:closes|fixes|resolves)\s+#(\d+)\b",
            re.IGNORECASE,
        )
    return [int(m.group(1)) for m in _CLOSES_KEYWORDS_RE.finditer(body)]


async def _handle_pull_request(payload: dict[str, Any]) -> dict[str, Any]:
    """`pull_request.opened` and `pull_request.reopened` — auto-link
    the new PR to a Run by parsing the PR body for `Closes #N`."""
    if payload.get("action") not in ("opened", "reopened"):
        return {"ignored": f"pull_request.{payload.get('action')}"}

    pr = payload.get("pull_request") or {}
    repo = (payload.get("repository") or {}).get("full_name", "")
    pr_number = pr.get("number")
    if not repo or not pr_number:
        return {"ignored": "missing fields"}

    pr_branch = ((pr.get("head") or {}).get("ref") or "")
    body = pr.get("body") or ""
    issue_refs = _parse_issue_refs(body)
    if not issue_refs:
        return {"ignored": "pr body has no issue ref"}

    cosmos: Cosmos = app.state.cosmos
    matching = await query_all(
        cosmos.projects,
        "SELECT * FROM c WHERE c.githubRepo = @r",
        parameters=[{"name": "@r", "value": repo}],
    )
    if not matching:
        return {"ignored": "no project for repo"}
    project = matching[0]["name"]

    linked: list[str] = []
    for issue_number in issue_refs:
        # `Closes #N` only carries the GH issue number. Resolve it
        # through the glimmung Issue first (#28-consumer-PR-1): match
        # by stitched github_issue_url, then look up the Run by the
        # canonical glimmung issue_id. Falls back to the legacy
        # `(project, issue_number)` query when no Issue exists for
        # this URL — covers Runs created before the dispatch shim
        # started minting Issues. The cleanup PR removes that branch
        # along with `Run.issue_number`.
        issue_url = issue_ops.github_issue_url_for(repo, issue_number)
        run = None
        issue_lookup = await issue_ops.find_issue_by_github_url(
            cosmos, github_issue_url=issue_url,
        )
        if issue_lookup is not None:
            issue, _issue_etag = issue_lookup
            run = await run_ops.find_run_by_issue_id(cosmos, issue_id=issue.id)
        if run is None:
            run = await run_ops.get_latest_run(
                cosmos, project=project, issue_number=issue_number,
            )
        if run is None:
            continue
        # Re-read with etag for the link mutation.
        try:
            doc = await cosmos.runs.read_item(item=run.id, partition_key=run.project)
        except Exception:
            continue
        from glimmung.runs import _strip_meta as _strip
        run, etag = (Run.model_validate(_strip(doc)), doc["_etag"])
        try:
            await run_ops.link_pr_to_run(
                cosmos, run=run, etag=etag,
                pr_number=int(pr_number), pr_branch=pr_branch,
            )
            linked.append(run.id)
            log.info(
                "linked PR %s#%d to run %s (issue #%d, branch %s)",
                repo, pr_number, run.id, issue_number, pr_branch,
            )
        except Exception:
            log.exception(
                "link_pr_to_run failed for run %s pr %s#%d",
                run.id, repo, pr_number,
            )

    return {"linked_runs": linked, "issue_refs": issue_refs}


async def _handle_pull_request_review(payload: dict[str, Any]) -> dict[str, Any]:
    """`pull_request_review.submitted` — enqueue a GH_REVIEW signal so
    the drain loop can route it through the triage decision engine.

    Other actions (`edited`, `dismissed`) are ignored — only the
    initial submission is decisional."""
    if payload.get("action") != "submitted":
        return {"ignored": f"pull_request_review.{payload.get('action')}"}

    pr = payload.get("pull_request") or {}
    review = payload.get("review") or {}
    repo = (payload.get("repository") or {}).get("full_name", "")
    pr_number = pr.get("number")
    if not repo or not pr_number:
        return {"ignored": "missing fields"}

    cosmos: Cosmos = app.state.cosmos
    sig = await signal_ops.enqueue_signal(
        cosmos,
        target_type=SignalTargetType.PR,
        target_repo=repo,
        target_id=str(pr_number),
        source=SignalSource.GH_REVIEW,
        payload={
            "state": review.get("state") or "",
            "body": review.get("body") or "",
            "reviewer": (review.get("user") or {}).get("login") or "",
            "review_id": review.get("id"),
        },
    )
    return {"enqueued_signal": sig.id}


# ─── Issues view + UI-initiated dispatch (#20) ───────────────────────────────────────


class IssueRow(BaseModel):
    """One row in the Issues view. After #50, issues live in glimmung's
    `issues` container; rows can be either GH-anchored (carry `repo` +
    `number` + `html_url` from `metadata.github_issue_*`) or glimmung-
    native (those three are None). The dashboard discriminates on
    `repo` to pick the dispatch payload shape."""
    id: str
    project: str
    repo: str | None = None
    number: int | None = None
    title: str
    state: str = "open"
    labels: list[str] = Field(default_factory=list)
    html_url: str | None = None
    last_run_id: str | None = None
    last_run_state: str | None = None  # "in_progress" | "passed" | "aborted" | None
    last_run_abort_reason: str | None = None
    issue_lock_held: bool = False  # convenience: lock currently held → in flight


class IssueDetail(BaseModel):
    id: str
    project: str
    repo: str | None = None
    number: int | None = None
    title: str
    body: str = ""
    state: str = "open"
    labels: list[str] = Field(default_factory=list)
    html_url: str | None = None
    last_run_id: str | None = None
    last_run_state: str | None = None
    issue_lock_held: bool = False


class IssueCreateRequest(BaseModel):
    """POST /v1/issues body — glimmung-native issue creation."""
    project: str
    title: str
    body: str = ""
    labels: list[str] = Field(default_factory=list)


class IssueUpdateRequest(BaseModel):
    """PATCH /v1/issues/by-id/{project}/{issue_id} body. All fields
    optional — None means "don't change". `state` is "open" or "closed";
    other transitions (e.g. label-only edits) leave it untouched."""
    title: str | None = None
    body: str | None = None
    labels: list[str] | None = None
    state: str | None = None


class GraphNode(BaseModel):
    """One node in the per-issue lineage graph (#42).

    `kind` discriminates rendering: an `issue` renders as a header card,
    a `run` as a column header, an `attempt` as a phase pill inside the
    column, a `pr` as the column footer, a `signal` as a sidebar event
    that may have a `re_dispatched` edge into a downstream Run.
    `metadata` carries kind-specific fields the renderer can show on
    expand (verification verdict, decision, signal payload, etc)."""
    id: str
    kind: str  # "issue" | "run" | "attempt" | "pr" | "signal"
    label: str
    state: str | None = None
    timestamp: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


class GraphEdge(BaseModel):
    source: str
    target: str
    kind: str  # "spawned" | "attempted" | "retried" | "opened" | "feedback" | "re_dispatched"


class IssueGraph(BaseModel):
    issue_id: str
    nodes: list[GraphNode] = Field(default_factory=list)
    edges: list[GraphEdge] = Field(default_factory=list)


class DispatchRequest(BaseModel):
    """One of two shapes:

    - GH-anchored: `repo` + `issue_number`. The pre-#50 path; the issue
      may exist in GH or be a glimmung-native one whose `metadata` was
      backfilled with the GH coords.
    - Native: `issue_id` (+ optional `project`; resolved from the Issue
      doc if omitted). For glimmung-native issues with no GH coords —
      created via `POST /v1/issues`.

    `workflow` is optional in both shapes; dispatch_run picks the
    project's only workflow if omitted and unambiguous.
    """
    repo: str | None = None
    issue_number: int | None = None
    issue_id: str | None = None
    project: str | None = None
    workflow: str | None = None


@app.get(
    "/v1/issues",
    response_model=list[IssueRow],
    dependencies=[Depends(require_admin_user)],
)
async def list_issues() -> list[IssueRow]:
    """All open glimmung Issues with a GH-syndication link, across
    registered projects. Sourced from the Cosmos `issues` container
    (#28-consumer-2 cutover) — populated by the GH-issue webhook
    mirror (#34) and the dispatch-path mint (#33). Glimmung-native
    Issues without a `metadata.github_issue_url` are excluded until
    there's UI for them.

    Labels are surfaced informationally only — they're a courtesy
    syndication surface in the post-#20 model, not a dispatch
    primitive. The dispatch button on each row is the trigger."""
    return await _list_issues_from_cosmos(app.state.cosmos)


async def _list_issues_from_cosmos(cosmos: Cosmos) -> list[IssueRow]:
    """Read-path for `/v1/issues`; lifted out so tests can drive it
    directly without standing up a FastAPI client. Returns the same
    `(project, -number)` ordering the UI table assumes.

    Bulk-loads `runs` and `locks` once instead of per-issue queries —
    with N issues the per-issue path was N cross-partition runs reads
    plus N lock point-reads (~70ms each on the runs side), so 67 open
    issues took ~5s. One cross-partition runs scan + one single-
    partition `scope=issue` locks scan keeps the endpoint sub-second
    at this scale; if/when the runs container grows large enough that
    a full scan stops fitting in budget, narrow it by `issue_id IN`
    over the open-issue set."""
    issues = await issue_ops.list_open_issues(cosmos)
    if not issues:
        return []

    run_docs = await query_all(cosmos.runs, "SELECT * FROM c")
    runs_by_issue_id: dict[str, dict[str, Any]] = {}
    runs_by_project_number: dict[tuple[str, int], dict[str, Any]] = {}
    for doc in run_docs:
        created = doc.get("created_at", "")
        issue_id = doc.get("issue_id") or ""
        if issue_id:
            cur = runs_by_issue_id.get(issue_id)
            if cur is None or created > cur.get("created_at", ""):
                runs_by_issue_id[issue_id] = doc
        project = doc.get("project")
        number = doc.get("issue_number")
        if project and number is not None:
            key = (project, int(number))
            cur = runs_by_project_number.get(key)
            if cur is None or created > cur.get("created_at", ""):
                runs_by_project_number[key] = doc

    lock_docs = await query_all(
        cosmos.locks,
        "SELECT * FROM c WHERE c.scope = @s",
        parameters=[{"name": "@s", "value": "issue"}],
    )
    locks_by_key = {doc["key"]: doc for doc in lock_docs}

    now = datetime.now(UTC)
    rows: list[IssueRow] = []
    for issue in issues:
        url = issue.metadata.github_issue_url
        repo = issue.metadata.github_issue_repo
        number = issue.metadata.github_issue_number
        row = IssueRow(
            id=issue.id,
            project=issue.project,
            repo=repo,
            number=number,
            title=issue.title,
            state=issue.state.value,
            labels=list(issue.labels),
            html_url=url,
        )
        # Pre-#33 Runs predate `Run.issue_id`; the (project, number)
        # fallback covers them so the Issues view keeps showing last-
        # run state. Cleanup-PR drops the fallback once those Runs are
        # migrated. Native issues have no number — `issue_id` is the
        # only joining key.
        run_doc = runs_by_issue_id.get(issue.id)
        if run_doc is None and number is not None:
            run_doc = runs_by_project_number.get((issue.project, number))
        if run_doc is not None:
            row.last_run_id = run_doc["id"]
            row.last_run_state = run_doc["state"]
            row.last_run_abort_reason = run_doc.get("abort_reason")
        lock_key = (
            f"{repo}#{number}" if (repo and number is not None)
            else f"glimmung/{issue.id}"
        )
        lock_doc = locks_by_key.get(lock_key)
        if lock_doc is not None and lock_doc.get("state") == "held":
            expires_at = datetime.fromisoformat(lock_doc["expires_at"])
            if expires_at > now:
                row.issue_lock_held = True
        rows.append(row)

    # Sort: project asc, then GH issues by descending number, then native
    # issues last (alphabetic by ulid suffix is fine — recency-ish).
    rows.sort(key=lambda r: (
        r.project,
        0 if r.number is not None else 1,
        -(r.number or 0),
        r.id,
    ))
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
    """Detail view: title + body + last-run summary + lock state.
    Sourced from the Cosmos `issues` container (#28-consumer-2). Three-
    segment path so the repo owner/name pair stays URL-friendly without
    a query param. 404 if no glimmung Issue mirrors the GH issue —
    rare post-#34 except for pre-existing untouched issues, which
    `/v1/admin/backfill-issues` lands."""
    repo = f"{repo_owner}/{repo_name}"
    return await _load_issue_detail(
        app.state.cosmos, repo=repo, issue_number=issue_number,
    )


async def _load_issue_detail(
    cosmos: Cosmos, *, repo: str, issue_number: int,
) -> IssueDetail:
    url = issue_ops.github_issue_url_for(repo, issue_number)
    found = await issue_ops.find_issue_by_github_url(cosmos, github_issue_url=url)
    if found is None:
        raise HTTPException(404, f"no glimmung issue mirrors {url}")
    issue, _ = found
    return await _build_issue_detail(cosmos, issue=issue)


async def _build_issue_detail(cosmos: Cosmos, *, issue: Issue) -> IssueDetail:
    """Render an `Issue` into the `IssueDetail` API view + stitch in the
    last-run summary and issue-lock state. Shared by both the URL-keyed
    (`/v1/issues/{owner}/{repo}/{n}`) and id-keyed (`/v1/issues/by-id/
    {project}/{id}`) detail endpoints."""
    repo = issue.metadata.github_issue_repo
    number = issue.metadata.github_issue_number
    detail = IssueDetail(
        id=issue.id,
        project=issue.project,
        repo=repo,
        number=number,
        title=issue.title,
        body=issue.body,
        state=issue.state.value,
        labels=list(issue.labels),
        html_url=issue.metadata.github_issue_url,
    )
    latest_run = await run_ops.find_run_by_issue_id(cosmos, issue_id=issue.id)
    if latest_run is None and number is not None:
        latest_run = await run_ops.get_latest_run(
            cosmos, project=issue.project, issue_number=number,
        )
    if latest_run is not None:
        detail.last_run_id = latest_run.id
        detail.last_run_state = latest_run.state.value
    lock_key = (
        f"{repo}#{number}" if (repo and number is not None)
        else f"glimmung/{issue.id}"
    )
    existing_lock = await lock_ops.read_lock(
        cosmos, scope="issue", key=lock_key,
    )
    detail.issue_lock_held = (
        existing_lock is not None
        and existing_lock.state.value == "held"
        and existing_lock.expires_at > datetime.now(UTC)
    )
    return detail


@app.get(
    "/v1/issues/by-id/{project}/{issue_id}",
    response_model=IssueDetail,
    dependencies=[Depends(require_admin_user)],
)
async def issue_detail_by_id(
    project: str = Path(...),
    issue_id: str = Path(...),
) -> IssueDetail:
    """Detail view keyed by glimmung issue id. Used for glimmung-native
    issues (which have no GH coords to slot into the URL-keyed path)
    and as the canonical handle for any caller that already has an id."""
    cosmos: Cosmos = app.state.cosmos
    found = await issue_ops.read_issue(cosmos, project=project, issue_id=issue_id)
    if found is None:
        raise HTTPException(404, f"no glimmung issue {project}/{issue_id}")
    issue, _ = found
    return await _build_issue_detail(cosmos, issue=issue)


@app.post(
    "/v1/issues",
    response_model=IssueDetail,
    dependencies=[Depends(require_admin_user)],
)
async def create_issue_endpoint(req: IssueCreateRequest) -> IssueDetail:
    """Mint a glimmung-native Issue. The dashboard issue-create form
    (#50) is the primary caller; CLI / scheduled paths can hit it too.

    No GH counterpart is created — glimmung is the source of truth.
    Source is `MANUAL`; the resulting Issue has no `metadata.github_*`
    fields set, so the URL-keyed detail endpoint can't find it. Use
    `/v1/issues/by-id/{project}/{id}` instead."""
    cosmos: Cosmos = app.state.cosmos
    project_doc = await _read_project(cosmos, req.project)
    if not project_doc:
        raise HTTPException(400, f"project {req.project!r} not registered")
    if not req.title.strip():
        raise HTTPException(400, "title required")
    issue = await issue_ops.create_issue(
        cosmos,
        project=req.project,
        title=req.title,
        body=req.body,
        labels=req.labels,
    )
    return await _build_issue_detail(cosmos, issue=issue)


@app.patch(
    "/v1/issues/by-id/{project}/{issue_id}",
    response_model=IssueDetail,
    dependencies=[Depends(require_admin_user)],
)
async def patch_issue_endpoint(
    req: IssueUpdateRequest,
    project: str = Path(...),
    issue_id: str = Path(...),
) -> IssueDetail:
    """Patch title / body / labels / state. State transitions go through
    `close_issue` / `reopen_issue` so `closed_at` is stamped consistently."""
    cosmos: Cosmos = app.state.cosmos
    found = await issue_ops.read_issue(cosmos, project=project, issue_id=issue_id)
    if found is None:
        raise HTTPException(404, f"no glimmung issue {project}/{issue_id}")
    issue, etag = found
    if req.title is not None or req.body is not None or req.labels is not None:
        issue, etag = await issue_ops.update_issue(
            cosmos, issue=issue, etag=etag,
            title=req.title,
            body=req.body,
            labels=req.labels,
        )
    if req.state is not None:
        target = req.state.lower()
        if target == "closed" and issue.state == IssueState.OPEN:
            issue, etag = await issue_ops.close_issue(cosmos, issue=issue, etag=etag)
        elif target == "open" and issue.state == IssueState.CLOSED:
            issue, etag = await issue_ops.reopen_issue(cosmos, issue=issue, etag=etag)
        elif target not in ("open", "closed"):
            raise HTTPException(400, f"state must be 'open' or 'closed', not {req.state!r}")
    return await _build_issue_detail(cosmos, issue=issue)


@app.get(
    "/v1/issues/{repo_owner}/{repo_name}/{issue_number}/graph",
    response_model=IssueGraph,
    dependencies=[Depends(require_admin_user)],
)
async def issue_graph(
    repo_owner: str = Path(...),
    repo_name: str = Path(...),
    issue_number: int = Path(...),
) -> IssueGraph:
    """Lineage graph for one Issue (#42): every Run dispatched against
    it, every PhaseAttempt inside each Run, the PR(s) opened, and the
    Signals that fed back. Bulk-loaded — one cross-partition runs query
    plus a legacy fallback plus one signals query, no per-row N+1."""
    repo = f"{repo_owner}/{repo_name}"
    return await _build_issue_graph(
        app.state.cosmos, repo=repo, issue_number=issue_number,
    )


async def _build_issue_graph(
    cosmos: Cosmos, *, repo: str, issue_number: int,
) -> IssueGraph:
    url = issue_ops.github_issue_url_for(repo, issue_number)
    found = await issue_ops.find_issue_by_github_url(cosmos, github_issue_url=url)
    if found is None:
        raise HTTPException(404, f"no glimmung issue mirrors {url}")
    issue, _ = found

    # All Runs targeting this Issue. Cover both #33's canonical
    # `issue_id` linkage and the legacy `(project, issue_number)` shape
    # for pre-#33 Runs; dedupe by id.
    by_issue_id = await query_all(
        cosmos.runs,
        "SELECT * FROM c WHERE c.issue_id = @i",
        parameters=[{"name": "@i", "value": issue.id}],
    )
    by_project_number = await query_all(
        cosmos.runs,
        "SELECT * FROM c WHERE c.project = @p AND c.issue_number = @n",
        parameters=[
            {"name": "@p", "value": issue.project},
            {"name": "@n", "value": issue_number},
        ],
    )
    seen_run_ids: set[str] = set()
    run_docs: list[dict[str, Any]] = []
    for doc in (*by_issue_id, *by_project_number):
        if doc["id"] not in seen_run_ids:
            run_docs.append(doc)
            seen_run_ids.add(doc["id"])
    run_docs.sort(key=lambda d: d.get("created_at", ""))

    pr_numbers = {
        int(d["pr_number"]) for d in run_docs
        if d.get("pr_number") is not None
    }
    run_ids = {d["id"] for d in run_docs}

    # All signals on this repo; filter in-memory to those targeting the
    # issue / one of its PRs / one of its Runs. Cross-partition but
    # signals is small so this is cheap.
    all_signals = await query_all(
        cosmos.signals,
        "SELECT * FROM c WHERE c.target_repo = @r",
        parameters=[{"name": "@r", "value": repo}],
    )
    relevant_signals: list[dict[str, Any]] = []
    for s in all_signals:
        target_type = s.get("target_type")
        target_id = s.get("target_id")
        if target_type == "pr":
            try:
                if int(target_id) in pr_numbers:
                    relevant_signals.append(s)
            except (TypeError, ValueError):
                pass
        elif target_type == "run":
            if target_id in run_ids:
                relevant_signals.append(s)
        elif target_type == "issue":
            try:
                if int(target_id) == issue_number:
                    relevant_signals.append(s)
            except (TypeError, ValueError):
                pass
    relevant_signals.sort(key=lambda s: s.get("created_at", ""))

    nodes: list[GraphNode] = []
    edges: list[GraphEdge] = []

    issue_node_id = f"issue:{issue.id}"
    nodes.append(GraphNode(
        id=issue_node_id,
        kind="issue",
        label=f"#{issue_number} {issue.title}",
        state=issue.state.value,
        timestamp=issue.created_at.isoformat(),
        metadata={
            "github_issue_url": issue.metadata.github_issue_url,
            "labels": list(issue.labels),
        },
    ))

    pr_node_by_number: dict[int, str] = {}
    for d in run_docs:
        prn = d.get("pr_number")
        if prn is None:
            continue
        prn_int = int(prn)
        if prn_int not in pr_node_by_number:
            pr_id = f"pr:{repo}#{prn_int}"
            pr_node_by_number[prn_int] = pr_id
            nodes.append(GraphNode(
                id=pr_id,
                kind="pr",
                label=f"PR #{prn_int}",
                state=None,  # rich PR state lands in #41
                timestamp=None,
                metadata={
                    "branch": d.get("pr_branch"),
                    "html_url": f"https://github.com/{repo}/pull/{prn_int}",
                },
            ))

    for d in run_docs:
        run_id = d["id"]
        run_node_id = f"run:{run_id}"
        nodes.append(GraphNode(
            id=run_node_id,
            kind="run",
            label=f"Run {run_id[:8]}",
            state=d.get("state"),
            timestamp=d.get("created_at"),
            metadata={
                "workflow": d.get("workflow"),
                "trigger_source": d.get("trigger_source"),
                "abort_reason": d.get("abort_reason"),
                "cumulative_cost_usd": d.get("cumulative_cost_usd"),
                "issue_lock_holder_id": d.get("issue_lock_holder_id"),
                "pr_number": d.get("pr_number"),
                "pr_branch": d.get("pr_branch"),
            },
        ))
        edges.append(GraphEdge(
            source=issue_node_id, target=run_node_id, kind="spawned",
        ))

        prev_attempt_node: str | None = None
        for a in d.get("attempts") or []:
            ai = a.get("attempt_index")
            attempt_node_id = f"attempt:{run_id}:{ai}"
            verification = a.get("verification") or {}
            nodes.append(GraphNode(
                id=attempt_node_id,
                kind="attempt",
                label=f"{a.get('phase', 'attempt')} #{ai}",
                state=verification.get("status") or (
                    "completed" if a.get("completed_at") else "pending"
                ),
                timestamp=a.get("dispatched_at"),
                metadata={
                    "phase": a.get("phase"),
                    "workflow_filename": a.get("workflow_filename"),
                    "workflow_run_id": a.get("workflow_run_id"),
                    "verification": verification or None,
                    "decision": a.get("decision"),
                    "completed_at": a.get("completed_at"),
                    "conclusion": a.get("conclusion"),
                },
            ))
            edges.append(GraphEdge(
                source=prev_attempt_node or run_node_id,
                target=attempt_node_id,
                kind="retried" if prev_attempt_node else "attempted",
            ))
            prev_attempt_node = attempt_node_id

        prn = d.get("pr_number")
        if prn is not None:
            edges.append(GraphEdge(
                source=run_node_id,
                target=pr_node_by_number[int(prn)],
                kind="opened",
            ))

    for s in relevant_signals:
        sig_node_id = f"signal:{s['id']}"
        target_type = s.get("target_type")
        target_id = s.get("target_id")
        payload = s.get("payload") or {}
        nodes.append(GraphNode(
            id=sig_node_id,
            kind="signal",
            label=str(payload.get("kind") or s.get("source") or "signal"),
            state=s.get("state"),
            timestamp=s.get("created_at"),
            metadata={
                "source": s.get("source"),
                "target_type": target_type,
                "target_id": target_id,
                "decision": s.get("decision"),
                "payload": payload,
                "failure_reason": s.get("failure_reason"),
            },
        ))
        if target_type == "pr":
            try:
                pn = int(target_id)
                if pn in pr_node_by_number:
                    edges.append(GraphEdge(
                        source=pr_node_by_number[pn],
                        target=sig_node_id,
                        kind="feedback",
                    ))
            except (TypeError, ValueError):
                pass
        elif target_type == "issue":
            edges.append(GraphEdge(
                source=issue_node_id, target=sig_node_id, kind="feedback",
            ))
        elif target_type == "run":
            if target_id in run_ids:
                edges.append(GraphEdge(
                    source=f"run:{target_id}",
                    target=sig_node_id,
                    kind="feedback",
                ))
        # Heuristic re_dispatched edge: if this signal preceded a
        # Run's creation, link it to the next Run on this issue. False
        # positives are tolerable — the renderer can color the edge as
        # "implied" rather than "explicit"; richer mapping (via the
        # Run's `trigger_source.signal_id`) waits on a future PR.
        sig_ts = s.get("created_at", "")
        for d in run_docs:
            if d.get("created_at", "") > sig_ts:
                edges.append(GraphEdge(
                    source=sig_node_id,
                    target=f"run:{d['id']}",
                    kind="re_dispatched",
                ))
                break

    return IssueGraph(issue_id=issue.id, nodes=nodes, edges=edges)


@app.post(
    "/v1/runs/dispatch",
    response_model=DispatchResult,
    dependencies=[Depends(require_admin_user)],
)
async def dispatch_run_endpoint(req: DispatchRequest) -> DispatchResult:
    """UI-initiated dispatch. Same code path as the legacy label-webhook
    handler used: both call `dispatch_run` from glimmung.dispatch. The
    trigger source is recorded on the resulting Run for W6 observability.

    Two payload shapes are accepted (#50): GH-anchored
    `{repo, issue_number}` or glimmung-native `{issue_id, project?}`.
    Native dispatch path looks up the existing Issue by id and skips the
    `ensure_issue_for_github` mint."""
    if req.issue_id is not None:
        return await dispatch_run(
            app,
            issue_id=req.issue_id,
            project=req.project,
            trigger_source={"kind": "glimmung_ui"},
            workflow_name=req.workflow,
        )
    if req.repo is None or req.issue_number is None:
        raise HTTPException(400, "dispatch payload requires either {issue_id} or {repo, issue_number}")
    return await dispatch_run(
        app,
        repo=req.repo,
        issue_number=req.issue_number,
        trigger_source={"kind": "glimmung_ui"},
        workflow_name=req.workflow,
    )


# ─── one-shot Issues backfill (#28-consumer-2) ──────────────────────────────────────


class BackfillIssuesResult(BaseModel):
    repos_processed: int
    issues_created: int
    issues_patched: int
    issues_unchanged: int
    skipped_repos: list[dict[str, str]] = Field(default_factory=list)


@app.post(
    "/v1/admin/backfill-issues",
    response_model=BackfillIssuesResult,
    dependencies=[Depends(require_admin_user)],
)
async def backfill_issues() -> BackfillIssuesResult:
    """Walk every registered repo's open GH issues and mirror them
    into the Cosmos `issues` container. Idempotent — `_mirror_github_
    issue` ensures-or-patches; safe to re-run. The post-#34 webhook
    mirror keeps ongoing GH activity in sync, but pre-existing issues
    that haven't seen any webhook event since the substrate landed
    won't surface in the Issues view until this runs once."""
    minter: GitHubAppTokenMinter | None = app.state.gh_minter
    if minter is None:
        raise HTTPException(503, "github app credentials not configured")
    return await _backfill_issues_impl(
        app.state.cosmos, list_open=gh_list_open_issues, minter=minter,
    )


async def _backfill_issues_impl(
    cosmos: Cosmos,
    *,
    list_open: Any,
    minter: Any,
) -> BackfillIssuesResult:
    """Backfill driver. `list_open` is a callable matching
    `github_app.list_open_issues`; tests pass a stub so we don't reach
    out to the real GH API."""
    project_docs = await query_all(cosmos.projects, "SELECT * FROM c")
    created = patched = unchanged = 0
    repos = 0
    skipped: list[dict[str, str]] = []
    for project_doc in project_docs:
        repo = project_doc.get("githubRepo") or ""
        if not repo:
            continue
        try:
            gh_issues = await list_open(minter, repo=repo)
        except Exception as exc:
            log.exception("backfill: list_open_issues failed for %s", repo)
            skipped.append({"repo": repo, "error": str(exc)})
            continue
        repos += 1
        for gh_issue in gh_issues:
            outcome = await _mirror_github_issue(
                cosmos,
                project=project_doc["name"],
                repo=repo,
                action="opened",
                issue_payload=gh_issue,
            )
            if outcome.get("created"):
                created += 1
            elif outcome.get("patched"):
                patched += 1
            else:
                unchanged += 1
    return BackfillIssuesResult(
        repos_processed=repos,
        issues_created=created,
        issues_patched=patched,
        issues_unchanged=unchanged,
        skipped_repos=skipped,
    )


# ─── PR view + reject signal (#19) ───────────────────────────────────────────────


class PrRow(BaseModel):
    """One row in the PR view: an agent-opened PR linked to a Run."""
    project: str
    repo: str
    pr_number: int
    pr_branch: str | None = None
    issue_number: int
    run_id: str
    run_state: str       # "in_progress" | "passed" | "aborted"
    run_attempts: int
    run_cumulative_cost_usd: float
    pr_lock_held: bool = False  # triage in flight


class PrDetail(BaseModel):
    project: str
    repo: str
    pr_number: int
    pr_branch: str | None = None
    issue_number: int
    issue_title: str | None = None
    pr_title: str | None = None
    pr_body: str | None = None
    pr_html_url: str | None = None
    run_id: str
    run_state: str
    run_attempts: int
    run_cumulative_cost_usd: float
    run_attempt_history: list[dict[str, Any]] = Field(default_factory=list)
    pr_lock_held: bool = False


@app.get(
    "/v1/prs",
    response_model=list[PrRow],
    dependencies=[Depends(require_admin_user)],
)
async def list_prs() -> list[PrRow]:
    """Agent-opened PRs across registered repos. Sourced from the
    `runs` container — each Run with `pr_number` set has a row.
    Multiple Runs on the same PR (rare) collapse to the most recent."""
    cosmos: Cosmos = app.state.cosmos
    docs = await query_all(
        cosmos.runs,
        "SELECT * FROM c WHERE IS_DEFINED(c.pr_number) AND c.pr_number != null",
    )
    rows: list[PrRow] = []
    seen: set[tuple[str, int]] = set()
    docs.sort(key=lambda d: d.get("created_at", ""), reverse=True)
    for d in docs:
        repo = d.get("issue_repo") or ""
        pr_num = d.get("pr_number")
        if not repo or pr_num is None:
            continue
        if (repo, pr_num) in seen:
            continue
        seen.add((repo, pr_num))

        existing_lock = await lock_ops.read_lock(
            cosmos, scope="pr", key=f"{repo}#{pr_num}",
        )
        held = (
            existing_lock is not None
            and existing_lock.state.value == "held"
            and existing_lock.expires_at > datetime.now(UTC)
        )
        rows.append(PrRow(
            project=d.get("project") or "",
            repo=repo,
            pr_number=int(pr_num),
            pr_branch=d.get("pr_branch"),
            issue_number=int(d.get("issue_number") or 0),
            run_id=d.get("id") or "",
            run_state=d.get("state") or "",
            run_attempts=len(d.get("attempts") or []),
            run_cumulative_cost_usd=float(d.get("cumulative_cost_usd") or 0.0),
            pr_lock_held=held,
        ))
    return rows


@app.get(
    "/v1/prs/{repo_owner}/{repo_name}/{pr_number}",
    response_model=PrDetail,
    dependencies=[Depends(require_admin_user)],
)
async def pr_detail(
    repo_owner: str = Path(...),
    repo_name: str = Path(...),
    pr_number: int = Path(...),
) -> PrDetail:
    """PR detail view. Pulls the Run state from Cosmos plus the PR
    metadata (title, body, html_url) live from the GH API."""
    cosmos: Cosmos = app.state.cosmos
    minter: GitHubAppTokenMinter | None = app.state.gh_minter
    repo = f"{repo_owner}/{repo_name}"

    lookup = await run_ops.find_run_by_pr(
        cosmos, issue_repo=repo, pr_number=pr_number,
    )
    if lookup is None:
        raise HTTPException(404, f"no run linked to {repo}#{pr_number}")
    run, _ = lookup

    pr_title: str | None = None
    pr_body: str | None = None
    pr_html_url: str | None = None
    if minter is not None:
        try:
            from glimmung.github_app import get_issue as _get_issue
            pr = await _get_issue(minter, repo=repo, issue_number=pr_number)
            pr_title = str(pr.get("title", ""))
            pr_body = str(pr.get("body") or "")
            pr_html_url = str(pr.get("html_url", ""))
        except Exception:
            log.exception("pr_detail: failed to fetch PR metadata for %s#%d", repo, pr_number)

    issue_title: str | None = None
    if minter is not None:
        try:
            from glimmung.github_app import get_issue as _get_issue
            issue = await _get_issue(minter, repo=repo, issue_number=run.issue_number)
            issue_title = str(issue.get("title", ""))
        except Exception:
            pass

    existing_lock = await lock_ops.read_lock(
        cosmos, scope="pr", key=f"{repo}#{pr_number}",
    )
    held = (
        existing_lock is not None
        and existing_lock.state.value == "held"
        and existing_lock.expires_at > datetime.now(UTC)
    )

    history = []
    for a in run.attempts:
        history.append({
            "attempt_index": a.attempt_index,
            "phase": a.phase.value,
            "workflow_filename": a.workflow_filename,
            "workflow_run_id": a.workflow_run_id,
            "dispatched_at": a.dispatched_at.isoformat(),
            "completed_at": a.completed_at.isoformat() if a.completed_at else None,
            "verification_status": a.verification.status.value if a.verification else None,
            "decision": a.decision,
        })

    return PrDetail(
        project=run.project,
        repo=repo,
        pr_number=pr_number,
        pr_branch=run.pr_branch,
        issue_number=run.issue_number,
        issue_title=issue_title,
        pr_title=pr_title,
        pr_body=pr_body,
        pr_html_url=pr_html_url,
        run_id=run.id,
        run_state=run.state.value,
        run_attempts=len(run.attempts),
        run_cumulative_cost_usd=run.cumulative_cost_usd,
        run_attempt_history=history,
        pr_lock_held=held,
    )


@app.post(
    "/v1/signals",
    response_model=Signal,
    dependencies=[Depends(require_admin_user)],
)
async def enqueue_signal_endpoint(req: SignalEnqueueRequest) -> Signal:
    """Enqueue a Signal for the drain loop. Used by the UI reject
    button (POST `{target_type: pr, target_repo, target_id, source:
    glimmung_ui, payload: {kind: "reject", feedback: "..."}}`).

    Future trigger sources (CLI, scheduled re-runs) hit this same
    endpoint."""
    return await signal_ops.enqueue_signal(
        app.state.cosmos,
        target_type=req.target_type,
        target_repo=req.target_repo,
        target_id=req.target_id,
        source=req.source,
        payload=req.payload,
    )


# ─── Static frontend ─────────────────────────────────────────────────────────
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

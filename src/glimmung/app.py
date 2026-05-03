import asyncio
import json
import logging
import os
from contextlib import asynccontextmanager
from datetime import UTC, datetime
from pathlib import Path as FsPath
from typing import Any
from urllib.parse import urlencode

from fastapi import Depends, FastAPI, HTTPException, Path, Request
from fastapi.responses import FileResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field
from sse_starlette.sse import EventSourceResponse
from ulid import ULID

from glimmung import issues as issue_ops
from glimmung import leases as lease_ops
from glimmung import locks as lock_ops
from glimmung import prs as pr_ops
from glimmung import runs as run_ops
from glimmung import signals as signal_ops
from glimmung.dispatch import DispatchResult, ResumeResult, dispatch_resumed_run, dispatch_run
from glimmung.auth import User, require_admin_user
from glimmung.db import Cosmos, query_all
from glimmung.decision import abort_explanation, decide
from glimmung.github_app import (
    GitHubAppTokenMinter,
    PRCreateAlreadyExists,
    PRCreateNoDiff,
    cancel_workflow_run,
    dispatch_workflow,
    open_pull_request,
    post_issue_comment,
    update_pull_request_body,
    verify_webhook_signature,
)
from glimmung.locks import LockBusy
from glimmung.models import (
    PR,
    PRReview,
    PRReviewState,
    BudgetConfig,
    Host,
    Issue,
    IssueComment,
    IssueState,
    Lease,
    LeaseRequest,
    LeaseResponse,
    LeaseState,
    PhaseSpec,
    PRState,
    PrPrimitiveSpec,
    Project,
    ProjectRegister,
    Run,
    RunDecision,
    RunState,
    Signal,
    SignalEnqueueRequest,
    SignalSource,
    SignalState,
    SignalTargetType,
    StateSnapshot,
    TriageDecision,
    VerificationResult,
    Workflow,
    WorkflowRegister,
    substitute_phase_inputs,
    validate_phase_input_refs,
)
from glimmung.replay import ReplayResult, SyntheticCompletion, replay_decision
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


async def _resolve_signal_pr(
    cosmos: Cosmos, signal: Signal,
) -> tuple[str, int, "Run | None", str | None] | None:
    """Resolve a PR-scoped signal's target into `(repo, pr_number, run,
    run_etag)`. Handles both shapes (#50 slice 3 retargeting):

    - Post-#50: `target_repo` is the project name and `target_id` is the
      glimmung PR id (ULID-shaped). We point-read the PR doc to recover
      `(repo, number)` + the linked Run.
    - Pre-#50: `target_repo` is `<owner>/<repo>` and `target_id` is the
      stringified GH PR number. Falls through to the legacy
      find_run_by_pr lookup. Existing in-flight signals enqueued before
      the rewrite drain cleanly via this branch.

    Returns None if neither shape resolves to a usable PR target.
    """
    target_id = signal.target_id
    # ULID is 26 chars Crockford base32; GH PR numbers are pure digits.
    looks_like_id = len(target_id) == 26 and target_id.isalnum() and not target_id.isdigit()

    if looks_like_id:
        pr_lookup = await pr_ops.read_pr(
            cosmos, project=signal.target_repo, pr_id=target_id,
        )
        if pr_lookup is None:
            log.warning(
                "triage: signal %s targets glimmung PR %s/%s which is missing",
                signal.id, signal.target_repo, target_id,
            )
            return None
        pr, _pr_etag = pr_lookup
        run: Run | None = None
        run_etag: str | None = None
        if pr.linked_run_id:
            try:
                doc = await cosmos.runs.read_item(
                    item=pr.linked_run_id, partition_key=pr.project,
                )
                from glimmung.runs import _strip_meta as _strip
                run = Run.model_validate(_strip(doc))
                run_etag = doc["_etag"]
            except Exception:
                log.warning(
                    "triage: linked_run_id=%s on PR %s not readable",
                    pr.linked_run_id, pr.id,
                )
        if run is None:
            # Legacy PRs without explicit linkage (pre-#50 agent flow).
            lookup = await run_ops.find_run_by_pr(
                cosmos, issue_repo=pr.repo, pr_number=pr.number,
            )
            if lookup is not None:
                run, run_etag = lookup
        return pr.repo, pr.number, run, run_etag

    # Legacy shape: target_id is the GH PR number, target_repo is the GH
    # repo. Lookup the Run directly via find_run_by_pr.
    try:
        pr_number = int(target_id)
    except ValueError:
        log.warning(
            "triage: signal %s target_id %r is neither a glimmung PR id nor a GH number",
            signal.id, target_id,
        )
        return None
    lookup = await run_ops.find_run_by_pr(
        cosmos, issue_repo=signal.target_repo, pr_number=pr_number,
    )
    if lookup is not None:
        run, run_etag = lookup
        return signal.target_repo, pr_number, run, run_etag
    return signal.target_repo, pr_number, None, None


async def _triage_decide(app: FastAPI, signal: Signal) -> tuple[str, bool]:
    """Drain decide_fn for triage: look up the Run linked to the PR,
    invoke the pure decision engine, return (decision_value, hold_lock).
    `hold_lock=True` only for DISPATCH_TRIAGE — the triage workflow's
    terminal handler (`_handle_workflow_run`) releases the lock on
    Run terminal transition."""
    cosmos: Cosmos = app.state.cosmos

    run: Run | None = None
    if signal.target_type == SignalTargetType.PR:
        resolved = await _resolve_signal_pr(cosmos, signal)
        if resolved is None:
            return (TriageDecision.ABORT_NO_RUN.value, False)
        _repo, _pr_number, run, _run_etag = resolved
    # Issue/Run scoped signals don't yet have triage decision logic;
    # IGNORE them so they don't sit in PENDING forever.
    elif signal.target_type != SignalTargetType.PR:
        return (TriageDecision.IGNORE.value, False)

    if run is None:
        return (TriageDecision.ABORT_NO_RUN.value, False)
    workflow_doc = await _read_workflow(cosmos, run.project, run.workflow)
    workflow_model = _doc_to_workflow(workflow_doc) if workflow_doc else None
    if workflow_model is None:
        return (TriageDecision.ABORT_NO_RUN.value, False)

    decision = decide_triage(signal=signal, run=run, workflow=workflow_model)
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
    pr_repo: str | None = None
    pr_number: int | None = None
    if signal.target_type == SignalTargetType.PR:
        resolved = await _resolve_signal_pr(cosmos, signal)
        if resolved is None:
            return
        pr_repo, pr_number, run, etag = resolved

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
        workflow_doc = (
            await _read_workflow(cosmos, run.project, run.workflow) if run else None
        )
        workflow_model = _doc_to_workflow(workflow_doc) if workflow_doc else None
        body = triage_abort_explanation(decision_enum, run, signal, workflow_model)
        minter: GitHubAppTokenMinter | None = app.state.gh_minter
        if minter is None:
            log.info("triage_apply: no GH minter; would have posted: %s", body[:80])
            return
        if pr_repo is None or pr_number is None:
            log.warning(
                "triage_apply: cannot post abort comment, no resolved PR coords for signal %s",
                signal.id,
            )
            return
        try:
            await post_issue_comment(
                minter, repo=pr_repo,
                issue_number=pr_number, body=body,
            )
        except Exception:
            log.exception(
                "triage_apply: failed to post abort comment on %s#%s",
                pr_repo, pr_number,
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


# Lease metadata is dual-purpose: glimmung-internal bookkeeping
# (`issue_id`, `issue_lock_holder_id`, `issue_repo`, `phase`) plus
# workflow-facing inputs the consumer workflow declares in
# `on.workflow_dispatch.inputs`. GitHub's workflow_dispatch endpoint
# rejects undeclared inputs with 422, so splatting metadata wholesale
# into `inputs` ships internal-only fields straight to GH and silently
# fails every dispatch — leaving Runs IN_PROGRESS with no
# `workflow_run_id`, which is the orphan shape `_abort_run` exists to
# clean up. Allowlist what we forward; everything else stays internal.
_DISPATCH_INPUT_KEYS = frozenset({
    "issue_number",
    "issue_title",
    "gh_event",
    "gh_action",
    "run_id",
    "attempt_index",
    "prior_verification_artifact_url",
    "feedback",
    "pr_number",
    "recent_comments",
})


async def _maybe_dispatch_workflow(app: FastAPI, lease_doc: dict[str, Any], host: Host) -> bool:
    """Fire workflow_dispatch for the lease's (project, workflow). Returns
    True on a successful dispatch OR a no-op (no GH minter, project not
    registered, workflow doc missing); False only when the dispatch was
    attempted and `dispatch_workflow` raised. The True-on-no-op shape
    keeps the existing test surface (gh_minter=None as a global mute)
    working without back-out side effects, while still letting
    `dispatch_run` distinguish "GH actually said no" from "we never even
    tried" — the former is the orphan-producing case that should roll
    back the lease + lock instead of leaving a Run stranded
    IN_PROGRESS."""
    minter: GitHubAppTokenMinter | None = app.state.gh_minter
    if minter is None:
        return True

    cosmos: Cosmos = app.state.cosmos
    project_doc = await _read_project(cosmos, lease_doc["project"])
    if not project_doc or not project_doc.get("githubRepo"):
        return True

    workflow_name = lease_doc.get("workflow")
    if not workflow_name:
        log.warning("lease %s has no workflow; skipping dispatch", lease_doc["id"])
        return True

    workflow_doc = await _read_workflow(cosmos, lease_doc["project"], workflow_name)
    if not workflow_doc:
        return True
    # #69 schema: read the initial phase off `phases[0]`. The pre-#69
    # workflowFilename top-level field is gone; lease metadata can override
    # the phase pick (see metadata.phase_name, future multi-phase use).
    phases = workflow_doc.get("phases") or []
    metadata = lease_doc.get("metadata") or {}
    target_phase_name = metadata.get("phase_name")
    target_phase = None
    if target_phase_name:
        target_phase = next((p for p in phases if p["name"] == target_phase_name), None)
    if target_phase is None and phases:
        target_phase = phases[0]
    if target_phase is None:
        log.warning(
            "workflow %s/%s has no phases; skipping dispatch",
            lease_doc["project"], workflow_name,
        )
        return True
    workflow_filename = target_phase["workflowFilename"]
    workflow_ref = target_phase.get("workflowRef") or "main"

    inputs = {
        "host": host.name,
        "lease_id": lease_doc["id"],
        **{
            k: str(v) for k, v in metadata.items()
            if k in _DISPATCH_INPUT_KEYS
        },
    }
    # Multi-phase forward dispatch (#101): the substituted phase inputs
    # are stashed on lease metadata so the promote-loop path can pick
    # them up too. Each consumer workflow declares these inputs in its
    # YAML; if not, GH 422s, same orphan shape as the standard inputs.
    phase_inputs = metadata.get("phase_inputs") or {}
    if isinstance(phase_inputs, dict):
        inputs.update({k: str(v) for k, v in phase_inputs.items()})
    try:
        await dispatch_workflow(
            minter,
            repo=project_doc["githubRepo"],
            workflow_filename=workflow_filename,
            ref=workflow_ref,
            inputs=inputs,
        )
        log.info(
            "dispatched %s on %s for lease %s (project=%s workflow=%s phase=%s)",
            workflow_filename, host.name, lease_doc["id"],
            lease_doc["project"], workflow_name, target_phase["name"],
        )
        return True
    except Exception:
        log.exception("workflow_dispatch failed for lease %s", lease_doc["id"])
        return False


async def _release_locks_on_terminal(
    *,
    run: Run,
    repo: str,
    result: dict[str, Any],
) -> None:
    """Issue + PR lock release for a Run that reached a terminal decision
    (ADVANCE / ABORT_*). RETRY intentionally skips this — the lock spans
    the whole verify-loop chain, not per-attempt. Idempotent on the lock
    side via holder_id; safe to call twice if the workflow somehow ends
    up double-completing.
    """
    cosmos: Cosmos = app.state.cosmos

    # Issue lock — keyed off the Run's stored holder_id so retries don't
    # release the lock the initial dispatch claimed.
    if run.issue_lock_holder_id and run.issue_number:
        try:
            released_lock = await lock_ops.release_lock(
                cosmos, scope="issue",
                key=f"{repo}#{run.issue_number}",
                holder_id=run.issue_lock_holder_id,
            )
            result["issue_lock_released"] = released_lock
        except Exception:
            log.exception(
                "issue lock release failed for %s#%s holder=%s",
                repo, run.issue_number, run.issue_lock_holder_id,
            )

    # PR lock — only set on triage cycles. Re-read the run for the
    # freshest pr_lock_holder_id in case a triage re-open landed
    # between record_completion and here.
    try:
        doc = await cosmos.runs.read_item(item=run.id, partition_key=run.project)
        pr_lock_holder = doc.get("pr_lock_holder_id")
        pr_number = doc.get("pr_number")
    except Exception:
        pr_lock_holder = run.pr_lock_holder_id
        pr_number = run.pr_number

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


async def _validate_phase_outputs(
    cosmos: Cosmos,
    *,
    run: Run,
    posted_outputs: dict[str, str] | None,
) -> dict[str, str] | None:
    """Validate `outputs` posted to `/v1/runs/.../completed` against the
    registered phase's declared `PhaseSpec.outputs` (#101).

    Strict equality of key sets — missing keys, extra keys, posting
    outputs against a phase that declares none, or omitting outputs
    against a phase that declares some all 400. Workflow contract
    violations surface to the consumer instead of being recorded as
    malformed completion state.

    Returns the validated dict (or None when both posted and declared
    are empty). Returning a value the caller can pass straight to
    `record_completion` keeps callers from re-deriving "is there
    anything to persist".

    A workflow that vanished mid-run yields `phase_outputs=None`; the
    `_process_run_completion` path then routes through abort_no_workflow
    and records nothing — output capture is a workflow-defined contract,
    so a missing workflow can't be evaluated either way.
    """
    declared: set[str] = set()
    workflow_doc = await _read_workflow(cosmos, run.project, run.workflow)
    workflow_model = _doc_to_workflow(workflow_doc) if workflow_doc else None
    if workflow_model is not None and run.attempts:
        latest_phase_name = run.attempts[-1].phase
        for phase in workflow_model.phases:
            if phase.name == latest_phase_name:
                declared = set(phase.outputs)
                break

    posted = posted_outputs or {}
    if set(posted.keys()) != declared:
        missing = declared - set(posted.keys())
        extra = set(posted.keys()) - declared
        parts = []
        if missing:
            parts.append(f"missing: {sorted(missing)}")
        if extra:
            parts.append(f"unexpected: {sorted(extra)}")
        raise HTTPException(
            400,
            f"phase_outputs contract violation for run {run.project}/{run.id} "
            f"phase {run.attempts[-1].phase if run.attempts else '<none>'!r}: "
            f"{'; '.join(parts)}. Declared outputs: {sorted(declared)}.",
        )

    # Empty dict means "phase declares no outputs and consumer posted
    # nothing" — store None on the attempt rather than {} so the
    # absence is unambiguous when the runtime substitutes inputs later.
    return posted if posted else None


async def _process_run_completion(
    *,
    run: Run,
    etag: str,
    workflow_run_id: int,
    conclusion: str,
    verification_result: VerificationResult | None,
    repo: str,
    screenshots_markdown: str | None = None,
    phase_outputs: dict[str, str] | None = None,
) -> str:
    """Drive a Run through one decision-engine cycle. Returns the
    decision value.

    Inputs come from the workflow's curl-completed callback (verification
    is the parsed `verification.json` content posted directly in the
    body). The retry dispatch path resolves the prior-attempt artifact
    URL lazily via `fetch_verification` only when RETRY actually fires —
    keeps the hot path synchronous-and-cheap and confines GH API calls
    to the one decision branch that needs them.

    `screenshots_markdown` is forwarded to `record_completion` so the
    Run carries the rendered MD block into `_compose_pr_body`.

    `phase_outputs` (#101) is the caller-validated `outputs` payload
    from the completed callback, persisted on the latest attempt.
    """
    cosmos: Cosmos = app.state.cosmos

    run, etag = await run_ops.record_completion(
        cosmos,
        run=run,
        etag=etag,
        workflow_run_id=workflow_run_id,
        conclusion=conclusion,
        verification=verification_result,
        artifact_url=None,
        screenshots_markdown=screenshots_markdown,
        phase_outputs=phase_outputs,
    )

    workflow_doc = await _read_workflow(cosmos, run.project, run.workflow)
    workflow_model = _doc_to_workflow(workflow_doc) if workflow_doc else None
    if workflow_model is None:
        log.warning(
            "run %s: workflow %s/%s vanished mid-flight; aborting",
            run.id, run.project, run.workflow,
        )
        await run_ops.mark_aborted(
            cosmos, run=run, etag=etag,
            reason="workflow registration disappeared mid-run",
        )
        return "abort_no_workflow"

    decision = decide(run, workflow_model)
    run, etag = await run_ops.record_decision(cosmos, run=run, etag=etag, decision=decision)

    if decision == RunDecision.ADVANCE:
        # Multi-phase routing (#101): if there's a next phase in the
        # workflow's ordered list, dispatch it instead of going terminal.
        # Run stays IN_PROGRESS, issue lock stays held, lease released
        # by the just-completed workflow's release-lease job is fine —
        # the next phase acquires its own lease.
        next_phase = _next_phase_after(workflow_model, run.attempts[-1].phase)
        if next_phase is not None:
            await _dispatch_next_phase(
                run=run, etag=etag, repo=repo,
                workflow_model=workflow_model, next_phase=next_phase,
            )
            log.info(
                "run %s advanced from phase %r to %r",
                run.id, run.attempts[-1].phase, next_phase.name,
            )
            return "advance_phase"

        # PR primitive: when the workflow opts in (`pr.enabled=True`),
        # glimmung calls `gh pr create` itself rather than relying on the
        # consumer's YAML. Default-off during the rollout per #69 — flip
        # per-workflow as each consumer migrates.
        if workflow_model.pr.enabled:
            try:
                await _open_pr_primitive(run=run, workflow=workflow_model)
            except PRCreateNoDiff as e:
                log.warning("pr-primitive: no diff for run %s; aborting (%s)", run.id, e)
                await run_ops.mark_aborted(
                    cosmos, run=run, etag=etag,
                    reason=f"PR primitive: no diff between glimmung/{run.id} and base",
                )
                return "abort_no_diff"
            except Exception:
                log.exception("pr-primitive: gh pr create failed for run %s", run.id)
                await run_ops.mark_aborted(
                    cosmos, run=run, etag=etag,
                    reason="PR primitive: gh pr create failed (see glimmung logs)",
                )
                return "abort_pr_create_failed"
        await run_ops.mark_passed(cosmos, run=run, etag=etag)
        log.info("run %s passed verification on attempt %d", run.id, len(run.attempts))
        return decision.value

    if decision == RunDecision.RETRY:
        await _dispatch_retry(
            run=run, etag=etag, repo=repo,
            workflow_model=workflow_model,
        )
        return decision.value

    # Any abort decision.
    reason = abort_explanation(run, workflow_model, decision)
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
    workflow_model: Workflow,
) -> None:
    """Dispatch a recycle (formerly RETRY) for a Run. Reads the failing
    phase's `recycle_policy.lands_at` to pick the destination phase, finds
    that phase's spec, acquires a fresh lease, then fires workflow_dispatch
    with `prior_verification_artifact_url` set so the next attempt can pull
    context from the previous attempt. v1 always lands at "self" (same
    phase), but the lookup is general for the future multi-phase case."""
    cosmos: Cosmos = app.state.cosmos
    settings: Settings = app.state.settings
    minter: GitHubAppTokenMinter = app.state.gh_minter

    if not run.attempts:
        log.warning("retry: run %s has no attempts; cannot dispatch", run.id)
        return
    failing_phase_name = run.attempts[-1].phase
    failing_phase = next(
        (p for p in workflow_model.phases if p.name == failing_phase_name), None,
    )
    if failing_phase is None or failing_phase.recycle_policy is None:
        log.warning(
            "retry: phase %r on run %s has no recycle_policy; cannot dispatch",
            failing_phase_name, run.id,
        )
        return
    target_name = failing_phase.recycle_policy.lands_at
    if target_name == "self":
        target_name = failing_phase_name
    target_phase = next(
        (p for p in workflow_model.phases if p.name == target_name), None,
    )
    if target_phase is None:
        log.warning(
            "retry: lands_at %r doesn't match any phase on workflow %s/%s",
            target_name, run.project, run.workflow,
        )
        return

    # Append the next attempt *before* dispatching so a webhook redelivery
    # of the previous completion can detect and skip duplicate decision cycles.
    run, _ = await run_ops.append_attempt(
        cosmos, run=run, etag=etag,
        phase_name=target_phase.name,
        workflow_filename=target_phase.workflow_filename,
    )

    requirements = target_phase.requirements or workflow_model.default_requirements
    metadata = {
        "issue_number": str(run.issue_number),
        "issue_repo": run.issue_repo,
        "run_id": run.id,
        "phase_name": target_phase.name,
        "attempt_index": str(len(run.attempts) - 1),
    }
    lease, host = await lease_ops.acquire(
        cosmos,
        settings,
        project=run.project,
        workflow=run.workflow,
        requirements=requirements,
        metadata=metadata,
    )

    if host is None:
        log.warning(
            "retry: no host available for run %s; lease %s pending. "
            "Manual re-dispatch required (see #18 followup).",
            run.id, lease.id,
        )
        return

    # Resolve the prior attempt's artifact URL lazily — only the retry
    # path needs it (so the next agent can prepend prior-failure reasons
    # to its prompt). Best-effort: if the lookup fails, the retry still
    # fires without prior context rather than blocking on GH API state.
    prior_artifact_url = ""
    prior_attempt = run.attempts[-2] if len(run.attempts) >= 2 else None
    if prior_attempt is not None and prior_attempt.workflow_run_id:
        try:
            _, archive_url = await fetch_verification(
                minter, repo=repo, run_id=prior_attempt.workflow_run_id,
            )
            prior_artifact_url = archive_url or ""
        except Exception:
            log.exception(
                "retry: failed to resolve prior artifact url for run %s attempt %d",
                run.id, prior_attempt.attempt_index,
            )

    inputs = {
        "host": host.name,
        "lease_id": lease.id,
        "issue_number": str(run.issue_number),
        "run_id": run.id,
        "prior_verification_artifact_url": prior_artifact_url,
        "attempt_index": str(len(run.attempts) - 1),
    }
    try:
        await dispatch_workflow(
            minter,
            repo=repo,
            workflow_filename=target_phase.workflow_filename,
            ref=target_phase.workflow_ref,
            inputs=inputs,
        )
        log.info(
            "dispatched recycle %s on %s for run %s phase=%s (attempt %d)",
            target_phase.workflow_filename, host.name, run.id,
            target_phase.name, len(run.attempts) - 1,
        )
    except Exception:
        log.exception("recycle workflow_dispatch failed for run %s", run.id)


def _next_phase_after(workflow: Workflow, phase_name: str):
    """Return the PhaseSpec immediately after `phase_name` in the
    workflow's declared order, or None if `phase_name` is the last
    phase (or doesn't appear). Pure lookup; multi-phase routing
    depends on this being deterministic."""
    for i, p in enumerate(workflow.phases):
        if p.name == phase_name:
            if i + 1 < len(workflow.phases):
                return workflow.phases[i + 1]
            return None
    return None


def _collect_phase_outputs(run: Run) -> dict[str, dict[str, str]]:
    """Build the prior-outputs map for ref substitution. Walks attempts
    in order so the latest attempt of each phase wins (matters when a
    verify phase recycled — only the passing attempt's outputs should
    flow downstream). Skips attempts without phase_outputs."""
    by_phase: dict[str, dict[str, str]] = {}
    for a in run.attempts:
        if a.phase_outputs:
            by_phase[a.phase] = dict(a.phase_outputs)
    return by_phase


async def _dispatch_next_phase(
    *,
    run: Run,
    etag: str,
    repo: str,
    workflow_model: Workflow,
    next_phase,
) -> None:
    """Forward dispatch (#101): fire `next_phase` for `run` after the
    prior phase ADVANCEd. Substitutes inputs from prior phases'
    captured outputs, acquires a fresh lease (capacity accounting is
    per-runner-job, same shape as recycle dispatch), appends a new
    PhaseAttempt, and fires workflow_dispatch.

    Run stays IN_PROGRESS, issue lock stays held — lock release
    fires only when the run goes terminal (last phase ADVANCEs to
    PR / fails / aborts)."""
    cosmos: Cosmos = app.state.cosmos
    settings: Settings = app.state.settings
    minter: GitHubAppTokenMinter | None = app.state.gh_minter

    prior_outputs = _collect_phase_outputs(run)
    try:
        substituted = substitute_phase_inputs(next_phase, prior_outputs)
    except (KeyError, ValueError):
        log.exception(
            "forward dispatch: input substitution failed for run %s phase %r; aborting",
            run.id, next_phase.name,
        )
        await run_ops.mark_aborted(
            cosmos, run=run, etag=etag,
            reason=(
                f"forward dispatch: input substitution failed for phase "
                f"{next_phase.name!r} — see glimmung logs"
            ),
        )
        return

    # Append the next attempt before dispatching so a webhook redelivery
    # of the previous completion doesn't re-trigger forward dispatch.
    run, etag = await run_ops.append_attempt(
        cosmos, run=run, etag=etag,
        phase_name=next_phase.name,
        workflow_filename=next_phase.workflow_filename,
    )

    requirements = next_phase.requirements or workflow_model.default_requirements
    metadata = {
        "issue_id": run.issue_id,
        "issue_number": str(run.issue_number),
        "issue_repo": run.issue_repo,
        "issue_lock_holder_id": run.issue_lock_holder_id or "",
        "run_id": run.id,
        "phase_name": next_phase.name,
        # Per-phase counter (0 == first dispatch of this phase). The
        # PhaseAttempt.attempt_index field stays run-flat for accounting;
        # this metadata field is the value the consumer workflow reads.
        "attempt_index": "0",
        # Substituted phase inputs land here so the promote-loop path
        # (host comes free later) can splat them into the dispatch the
        # same way the inline path does. Cosmos round-trips dict[str,
        # str] cleanly.
        "phase_inputs": dict(substituted),
    }
    lease, host = await lease_ops.acquire(
        cosmos,
        settings,
        project=run.project,
        workflow=run.workflow,
        requirements=requirements,
        metadata=metadata,
    )

    if host is None:
        # No host capacity — lease is PENDING. The promote loop will
        # fire workflow_dispatch when capacity frees, same as the
        # initial-dispatch pending path.
        log.info(
            "forward dispatch: no host for run %s phase %r; lease %s pending",
            run.id, next_phase.name, lease.id,
        )
        return

    if minter is None:
        # Test path: no GH token minter wired in. The lease + attempt
        # are recorded; downstream tests assert on those without
        # actually firing a workflow_dispatch.
        log.info(
            "forward dispatch (no minter): would dispatch %s on %s for run %s phase %r",
            next_phase.workflow_filename, host.name, run.id, next_phase.name,
        )
        return

    inputs = {
        "host": host.name,
        "lease_id": lease.id,
        "issue_number": str(run.issue_number),
        "run_id": run.id,
        "attempt_index": "0",
        **{k: str(v) for k, v in substituted.items()},
    }
    try:
        await dispatch_workflow(
            minter,
            repo=repo,
            workflow_filename=next_phase.workflow_filename,
            ref=next_phase.workflow_ref,
            inputs=inputs,
        )
        log.info(
            "dispatched next phase %s on %s for run %s phase=%s",
            next_phase.workflow_filename, host.name, run.id, next_phase.name,
        )
    except Exception:
        log.exception("forward workflow_dispatch failed for run %s", run.id)


async def _dispatch_triage(
    app: FastAPI,
    *,
    signal: Signal,
    run: Run,
    etag: str,
    holder_id: str,
) -> None:
    """Re-open a Run via the PR primitive's recycle path and fire the
    workflow for the `lands_at` phase.

    Under #69, "triage" is no longer a separate `triage_workflow_filename`
    — it's the PR primitive's `recycle_policy` firing. Read the workflow's
    `pr.recycle_policy.lands_at`, find that PhaseSpec, and dispatch its
    `workflow_filename`. State machine is unchanged: PASSED → IN_PROGRESS,
    new PhaseAttempt appended at lands_at, both issue + PR locks held
    with `holder_id` for terminal release."""
    cosmos: Cosmos = app.state.cosmos
    settings: Settings = app.state.settings
    minter: GitHubAppTokenMinter | None = app.state.gh_minter

    workflow_doc = await _read_workflow(cosmos, run.project, run.workflow)
    workflow_model = _doc_to_workflow(workflow_doc) if workflow_doc else None
    if workflow_model is None:
        log.warning(
            "pr-recycle: workflow %s/%s vanished; cannot dispatch",
            run.project, run.workflow,
        )
        return

    pr_rp = workflow_model.pr.recycle_policy
    if pr_rp is None:
        log.warning(
            "pr-recycle: workflow %s/%s has no pr.recycle_policy; cannot dispatch",
            run.project, run.workflow,
        )
        return
    target_phase = next(
        (p for p in workflow_model.phases if p.name == pr_rp.lands_at), None,
    )
    if target_phase is None:
        log.warning(
            "pr-recycle: lands_at %r not found on workflow %s/%s",
            pr_rp.lands_at, run.project, run.workflow,
        )
        return

    issue_lock_key = (
        f"{run.issue_repo}#{run.issue_number}"
        if (run.issue_repo and run.issue_number)
        else f"glimmung/{run.issue_id}"
    )
    try:
        await lock_ops.claim_lock(
            cosmos, scope="issue",
            key=issue_lock_key,
            holder_id=holder_id,
            ttl_seconds=settings.lease_default_ttl_seconds,
            metadata={"triage_signal_id": signal.id, "phase_name": target_phase.name},
        )
    except LockBusy as busy:
        log.warning(
            "pr-recycle: issue lock %s is held by %s; deferring signal %s",
            issue_lock_key, busy.lock.held_by, signal.id,
        )
        return

    run, etag = await run_ops.reopen_for_recycle(
        cosmos, run=run, etag=etag,
        phase_name=target_phase.name,
        workflow_filename=target_phase.workflow_filename,
        pr_lock_holder_id=holder_id,
        issue_lock_holder_id=holder_id,
    )

    requirements = target_phase.requirements or workflow_model.default_requirements
    metadata = {
        "issue_number": str(run.issue_number),
        "issue_repo": run.issue_repo,
        "run_id": run.id,
        "phase_name": target_phase.name,
        "attempt_index": str(len(run.attempts) - 1),
        "issue_lock_holder_id": holder_id,
    }
    lease, host = await lease_ops.acquire(
        cosmos, settings,
        project=run.project, workflow=run.workflow,
        requirements=requirements,
        metadata=metadata,
    )

    if host is None:
        log.warning(
            "pr-recycle: no host available for run %s; lease %s pending. "
            "Manual re-dispatch may be required.",
            run.id, lease.id,
        )
        return

    if minter is None:
        log.warning("pr-recycle: no GH minter; cannot dispatch for run %s", run.id)
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
            workflow_filename=target_phase.workflow_filename,
            ref=target_phase.workflow_ref,
            inputs=inputs,
        )
        log.info(
            "dispatched pr-recycle %s on %s for run %s phase=%s (attempt %d, signal %s)",
            target_phase.workflow_filename, host.name, run.id,
            target_phase.name, len(run.attempts) - 1, signal.id,
        )
    except Exception:
        log.exception(
            "pr-recycle workflow_dispatch failed for run %s signal %s",
            run.id, signal.id,
        )


async def _open_pr_primitive(*, run: Run, workflow: Workflow) -> None:
    """Open the PR for a run that just ADVANCEd. Branch is glimmung-
    dictated (`glimmung/<run_id>`); title comes from the linked issue,
    body summarizes the run state in Glimmung; GitHub gets a thin pointer
    back to the canonical Glimmung PR row. On success, stamps `pr_number`
    and `pr_branch` on the Run via `link_pr_to_run`. On `PRCreateNoDiff`, the
    caller turns this into a run-level abort (per #69 v1 — no-diff is a
    terminal error). On `PRCreateAlreadyExists`, the existing PR's number
    is recorded and the run continues — supports rewind/recycle paths
    that re-enter after a PR is already open."""
    cosmos: Cosmos = app.state.cosmos
    minter: GitHubAppTokenMinter | None = app.state.gh_minter
    if minter is None:
        raise RuntimeError("pr-primitive: no GH minter configured")

    head = f"glimmung/{run.id}"
    base = "main"  # v1: hardcoded; future PhaseSpec.pr.base extends this
    title, rich_body = await _compose_pr_body(cosmos, run=run, workflow=workflow)
    initial_github_body = _thin_github_pr_body(
        run=run,
        glimmung_url=None,
    )

    try:
        pr_number, html_url = await open_pull_request(
            minter,
            repo=run.issue_repo,
            head=head,
            base=base,
            title=title,
            body=initial_github_body,
        )
    except PRCreateAlreadyExists as already:
        log.info(
            "pr-primitive: PR already exists for %s head=%s; recording #%d",
            run.issue_repo, head, already.pr_number,
        )
        pr_number = already.pr_number
        html_url = already.html_url

    pr, etag, _created = await pr_ops.ensure_pr_for_github(
        cosmos,
        project=run.project,
        repo=run.issue_repo,
        number=pr_number,
        title=title,
        branch=head,
        body=rich_body,
        base_ref=base,
        html_url=html_url,
    )
    pr, _ = await pr_ops.update_pr(
        cosmos,
        pr=pr,
        etag=etag,
        title=title,
        branch=head,
        body=rich_body,
        base_ref=base,
        html_url=html_url,
        linked_issue_id=run.issue_id or "",
        linked_run_id=run.id,
    )

    glimmung_url = _glimmung_pr_detail_url(
        settings=app.state.settings if getattr(app.state, "settings", None) is not None else get_settings(),
        repo=run.issue_repo,
        number=pr_number,
    )
    try:
        await update_pull_request_body(
            minter,
            repo=run.issue_repo,
            number=pr_number,
            body=_thin_github_pr_body(run=run, glimmung_url=glimmung_url),
        )
    except Exception:
        log.exception(
            "pr-primitive: failed to update thin GitHub body for %s#%d",
            run.issue_repo,
            pr_number,
        )

    # Stamp on the run.
    lookup = await run_ops.find_run_by_workflow_run(
        cosmos, project=run.project,
        workflow_run_id=run.attempts[-1].workflow_run_id or 0,
    )
    if lookup is not None:
        run, etag = lookup
        await run_ops.link_pr_to_run(
            cosmos, run=run, etag=etag,
            pr_number=pr_number, pr_branch=head,
        )


def _glimmung_pr_detail_url(*, settings: Settings, repo: str, number: int) -> str:
    base_url = getattr(settings, "glimmung_base_url", "https://glimmung.romaine.life")
    return f"{base_url.rstrip('/')}/prs/{repo}/{number}"


def _thin_github_pr_body(*, run: Run, glimmung_url: str | None) -> str:
    parts: list[str] = []
    if run.issue_repo and run.issue_number:
        parts.append(f"Closes {run.issue_repo}#{run.issue_number}")
    if glimmung_url:
        parts += ["", f"Canonical context: {glimmung_url}"]
    else:
        parts += ["", "Canonical context is being prepared in Glimmung."]
    return "\n".join(parts).strip()


async def _compose_pr_body(
    cosmos: Cosmos, *, run: Run, workflow: Workflow,
) -> tuple[str, str]:
    """Compose PR title + body from the issue + run state.

    Title is the issue title; body links the issue (`Closes #N`),
    surfaces the live preview env + /_styleguide URLs (#88), inlines
    the screenshot markdown the workflow uploaded (#87 → #88), and
    closes with a short run summary so reviewers see attempts + cost
    without leaving the PR view.

    `validation_url` and `screenshots_markdown` are populated by the
    `started` and `completed` callbacks respectively. Either may be
    None for backend-only workflows; sections drop out cleanly when
    they are."""
    issue_title = ""
    issue_body_link = ""
    if run.issue_id:
        try:
            doc = await cosmos.issues.read_item(item=run.issue_id, partition_key=run.project)
            issue_title = str(doc.get("title") or "")
        except Exception:
            pass
    if run.issue_repo and run.issue_number:
        issue_body_link = f"Closes {run.issue_repo}#{run.issue_number}"
    title = issue_title or f"Run {run.id[:8]}"
    attempts_summary = "\n".join(
        f"- attempt {a.attempt_index} phase={a.phase} "
        f"cost=${(a.cost_usd or 0.0):.4f} decision={a.decision or '—'}"
        for a in run.attempts
    )
    body_parts: list[str] = [
        issue_body_link,
        "",
        "Glimmung-opened PR. Composed from run state — see the dashboard "
        f"for full lineage (run id `{run.id}`).",
    ]

    # Live preview surface (#88). Reviewers get the actual running app
    # plus the styleguide route — that's the contract documented in
    # docs/styleguide-contract.md, and the PR is the moment they need
    # both URLs in one place.
    if run.validation_url:
        env_url = run.validation_url.rstrip("/")
        body_parts += [
            "",
            "## Preview",
            f"- live env: {env_url}",
            f"- styleguide: {env_url}/_styleguide",
        ]

    # Screenshot block from the workflow's upload step (#87 → #88).
    # Already markdown — drop in verbatim. The workflow handles the
    # "_Screenshot upload failed_" case in the same block, so we don't
    # need to repeat that fallback here.
    if run.screenshots_markdown:
        body_parts += ["", run.screenshots_markdown.strip()]

    body_parts += [
        "",
        "## Run summary",
        f"- workflow: `{workflow.name}`",
        f"- attempts: {len(run.attempts)}",
        f"- cumulative cost: ${run.cumulative_cost_usd:.4f}",
        "",
        "## Attempts",
        attempts_summary or "_no attempts recorded_",
    ]
    body = "\n".join(body_parts).strip()
    return title, body


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


def _recycle_policy_to_doc(rp: Any) -> dict[str, Any] | None:
    if rp is None:
        return None
    return {"maxAttempts": rp.max_attempts, "on": list(rp.on), "landsAt": rp.lands_at}


def _phase_to_doc(p: Any) -> dict[str, Any]:
    return {
        "name": p.name,
        "kind": p.kind,
        "workflowFilename": p.workflow_filename,
        "workflowRef": p.workflow_ref,
        "requirements": p.requirements,
        "verify": p.verify,
        "recyclePolicy": _recycle_policy_to_doc(p.recycle_policy),
        "inputs": dict(p.inputs),
        "outputs": list(p.outputs),
    }


def _workflow_to_doc(w: WorkflowRegister) -> dict[str, Any]:
    return {
        "id": w.name,
        "project": w.project,
        "name": w.name,
        "phases": [_phase_to_doc(p) for p in w.phases],
        "pr": {
            "enabled": w.pr.enabled,
            "recyclePolicy": _recycle_policy_to_doc(w.pr.recycle_policy),
        },
        "budget": w.budget.model_dump(),
        "triggerLabel": w.trigger_label,
        "defaultRequirements": w.default_requirements,
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    }


def _recycle_policy_from_doc(d: dict[str, Any] | None):
    """Inverse of `_recycle_policy_to_doc`. Tolerates None and unknown
    fields so legacy / forward-compatible reads don't 500."""
    from glimmung.models import RecyclePolicy
    if not d:
        return None
    return RecyclePolicy(
        max_attempts=int(d.get("maxAttempts", 3)),
        on=list(d.get("on") or []),
        lands_at=str(d.get("landsAt", "self")),
    )


def _phase_from_doc(d: dict[str, Any]):
    from glimmung.models import PhaseSpec
    return PhaseSpec(
        name=d["name"],
        kind=d.get("kind", "gha_dispatch"),
        workflow_filename=d["workflowFilename"],
        workflow_ref=d.get("workflowRef") or "main",
        requirements=d.get("requirements"),
        verify=bool(d.get("verify", False)),
        recycle_policy=_recycle_policy_from_doc(d.get("recyclePolicy")),
        inputs=dict(d.get("inputs") or {}),
        outputs=list(d.get("outputs") or []),
    )


def _doc_to_workflow(doc: dict[str, Any] | None):
    """Cosmos camelCase → Pydantic Workflow. Returns None if `doc` is None
    so callers can null-check the workflow on disappearance mid-flight."""
    if doc is None:
        return None
    from glimmung.models import BudgetConfig, PrPrimitiveSpec
    phases_raw = doc.get("phases") or []
    pr_raw = doc.get("pr") or {}
    budget_raw = doc.get("budget") or {}
    return Workflow(
        id=doc.get("id") or doc["name"],
        project=doc["project"],
        name=doc["name"],
        phases=[_phase_from_doc(p) for p in phases_raw],
        pr=PrPrimitiveSpec(
            enabled=bool(pr_raw.get("enabled", False)),
            recycle_policy=_recycle_policy_from_doc(pr_raw.get("recyclePolicy")),
        ),
        budget=BudgetConfig(total=float(budget_raw.get("total", 25.0))),
        trigger_label=doc.get("triggerLabel", "issue-agent"),
        default_requirements=doc.get("defaultRequirements") or {},
        metadata=doc.get("metadata") or {},
        created_at=datetime.now(UTC),  # not authoritative; used only for the model
    )


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
        "tank_operator_base_url": settings.tank_operator_base_url.rstrip("/"),
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


class AbortRunResult(BaseModel):
    """Outcome of POST /v1/runs/{project}/{run_id}/abort.

    Sibling of `CancelLeaseResult` — same kind of cleanup, started from a
    Run id rather than a lease id. Use this when the Run is orphaned (no
    lease / no workflow_run): `_cancel_lease` needs a lease to start from
    and 404s otherwise.

    `state`:
      - `aborted`: Run was IN_PROGRESS, flipped to ABORTED with the given
        reason. If the latest attempt has a workflow_run_id, a GH cancel
        was POSTed (best-effort; `gh_run_cancelled` records the outcome,
        `None` if no GH dispatch was attempted on this Run).
      - `already_terminal`: Run was already PASSED or ABORTED. No-op.
    """
    state: str
    run_id: str
    gh_run_cancelled: bool | None = None
    issue_lock_released: bool | None = None
    pr_lock_released: bool | None = None


async def _abort_run(
    cosmos: Cosmos,
    minter: GitHubAppTokenMinter | None,
    *,
    run_id: str,
    project: str,
    reason: str,
) -> AbortRunResult:
    """Operator-initiated abort of a Run, keyed by run id.

    Mirrors `_cancel_lease` but starts from a Run rather than a Lease —
    needed when the dispatch failed mid-flight and left a Run IN_PROGRESS
    with no lease + no workflow_run_id (nothing for `_cancel_lease` to
    grip onto). All sub-steps are idempotent: running this twice on the
    same Run returns `already_terminal` the second time.
    """
    # 1. Read the Run.
    try:
        run_doc = await cosmos.runs.read_item(item=run_id, partition_key=project)
    except Exception:
        raise HTTPException(404, f"run {run_id} not found in project {project!r}")

    if run_doc["state"] in (RunState.PASSED.value, RunState.ABORTED.value):
        return AbortRunResult(state="already_terminal", run_id=run_id)

    run = Run.model_validate(run_ops._strip_meta(run_doc))
    etag = run_doc["_etag"]

    # 2. GH cancel — only if the Run was dispatched (workflow_run_id set).
    # Orphans typically don't have one (that's why they're orphans), so
    # this is None in the common case.
    gh_cancelled: bool | None = None
    if run.attempts and minter is not None:
        latest = run.attempts[-1]
        gh_run_id = latest.workflow_run_id
        if gh_run_id is not None:
            try:
                gh_cancelled = await cancel_workflow_run(
                    minter, repo=run.issue_repo, run_id=gh_run_id,
                )
            except Exception:
                log.exception(
                    "abort_run: GH cancel failed for run %s (workflow_run_id=%d); "
                    "proceeding with abort",
                    run.id, gh_run_id,
                )
                gh_cancelled = False

    # 3. Mark the Run aborted.
    try:
        await run_ops.mark_aborted(cosmos, run=run, etag=etag, reason=reason)
    except Exception:
        log.exception("abort_run: mark_aborted failed for run %s", run.id)
        raise HTTPException(500, f"mark_aborted failed for {run.id}")

    # 4. Release the issue lock + PR lock if the Run was holding them.
    # Idempotent — release_lock returns False if we don't hold it.
    issue_lock_released: bool | None = None
    if run.issue_lock_holder_id and run.issue_repo and run.issue_number is not None:
        try:
            issue_lock_released = bool(await lock_ops.release_lock(
                cosmos, scope="issue",
                key=f"{run.issue_repo}#{run.issue_number}",
                holder_id=run.issue_lock_holder_id,
            ))
        except Exception:
            log.exception(
                "abort_run: issue lock release failed for %s#%s holder=%s",
                run.issue_repo, run.issue_number, run.issue_lock_holder_id,
            )

    pr_lock_released: bool | None = None
    if run.pr_lock_holder_id and run.pr_number and run.issue_repo:
        try:
            pr_lock_released = bool(await lock_ops.release_lock(
                cosmos, scope="pr",
                key=f"{run.issue_repo}#{run.pr_number}",
                holder_id=run.pr_lock_holder_id,
            ))
        except Exception:
            log.exception(
                "abort_run: pr lock release failed for %s#%s holder=%s",
                run.issue_repo, run.pr_number, run.pr_lock_holder_id,
            )

    return AbortRunResult(
        state="aborted",
        run_id=run.id,
        gh_run_cancelled=gh_cancelled,
        issue_lock_released=issue_lock_released,
        pr_lock_released=pr_lock_released,
    )


@app.post(
    "/v1/runs/{project}/{run_id}/abort",
    response_model=AbortRunResult,
    dependencies=[Depends(require_admin_user)],
)
async def abort_run(
    project: str = Path(...),
    run_id: str = Path(...),
    reason: str = "aborted_via_admin_api",
) -> AbortRunResult:
    """Admin-only endpoint that flips a Run to ABORTED and releases any
    locks it was holding. Distinct from `cancel_lease`: that one starts
    from a lease and 404s if the lease is gone, leaving orphaned Runs
    (dispatch failed mid-flight) unrecoverable. See `_abort_run` for the
    body of the operation."""
    return await _abort_run(
        app.state.cosmos,
        getattr(app.state, "gh_minter", None),
        run_id=run_id,
        project=project,
        reason=reason,
    )


# ─── Run lifecycle callbacks (workflow → glimmung) ────────────────────────────
#
# The dispatched workflow reports its own lifecycle to glimmung via these
# endpoints rather than relying on GitHub's `workflow_run` webhook —
# `workflow_dispatch` returns 204 with no run id, and the `workflow_run`
# event payload doesn't echo dispatch inputs, so there's no GH-provided
# correlation field to map an inbound webhook to a glimmung Run. The
# workflow already curls glimmung at start (lease verify) and end (lease
# release), so adding two callbacks for run-state is essentially free.
#
# Auth is capability-only: `run_id` is an unguessable ULID, same pattern
# as `/v1/lease/{lease_id}/release`.


class RunStartedRequest(BaseModel):
    """`POST /v1/runs/{project}/{run_id}/started` body. The workflow's
    first step posts its `${{ github.run_id }}` here so subsequent
    dashboard / cancel paths can deep-link the GH workflow run.

    `validation_url` (optional, #88) is the live preview env URL the
    workflow stood up. Stamped on the Run so the PR composer can
    surface env + /_styleguide URLs in the PR body. None for backend-
    only workflows that don't expose a public env.
    """
    workflow_run_id: int
    validation_url: str | None = None


class RunCompletedRequest(BaseModel):
    """`POST /v1/runs/{project}/{run_id}/completed` body.

    `verification` is the parsed `verification.json` content the
    workflow's verify phase produced (still uploaded as a GHA artifact
    for human auditability; this body is the decision-engine input).
    Missing / unparseable verification → decision engine returns
    ABORT_MALFORMED, same as the legacy webhook path.

    `screenshots_markdown` (optional, #88) is the rendered MD block
    from the workflow's upload-to-blob step (#87 captures, blob URLs).
    Stamped on the Run; the PR composer drops it verbatim into the
    body. None for backend workflows or runs where screenshots failed
    upstream (those abort before reaching this callback).

    `outputs` (optional, #101) is the phase's emitted output values.
    Keys MUST match exactly the set declared in the registered phase's
    `PhaseSpec.outputs`. Missing keys, extra keys, or `outputs` posted
    against a phase that declares none → 400. Persisted on the latest
    PhaseAttempt for the multi-phase runtime to substitute into the
    next phase's `workflow_dispatch.inputs` (PR 3 of #101).
    """
    workflow_run_id: int
    conclusion: str   # GH-style: "success" | "failure" | "cancelled"
    verification: dict[str, Any] | None = None
    screenshots_markdown: str | None = None
    outputs: dict[str, str] | None = None


class RunCallbackResult(BaseModel):
    run_id: str
    decision: str | None = None
    issue_lock_released: bool | None = None
    pr_lock_released: bool | None = None


@app.post(
    "/v1/runs/{project}/{run_id}/started",
    response_model=RunCallbackResult,
)
async def run_started(
    req: RunStartedRequest,
    project: str = Path(...),
    run_id: str = Path(...),
) -> RunCallbackResult:
    """Workflow-side callback: the dispatched workflow has started, here
    is its `${{ github.run_id }}`. Stamps `workflow_run_id` on the latest
    PhaseAttempt of the Run. Idempotent on redelivery."""
    cosmos: Cosmos = app.state.cosmos
    found = await run_ops.read_run(cosmos, project=project, run_id=run_id)
    if found is None:
        raise HTTPException(404, f"no run {project}/{run_id}")
    run, etag = found
    await run_ops.record_started(
        cosmos, run=run, etag=etag,
        workflow_run_id=req.workflow_run_id,
        validation_url=req.validation_url,
    )
    return RunCallbackResult(run_id=run.id)


class RunAbortedRequest(BaseModel):
    """`POST /v1/runs/{project}/{run_id}/aborted` body. Lets the dispatched
    workflow flip its own Run to ABORTED with a typed reason — used by
    contract-violation checks (e.g. #86's `frontend_contract_violation`)
    that need to fail the phase *before* it reaches the verify step.

    Capability-only auth: `run_id` is an unguessable ULID, same pattern
    as `started` / `completed`. The workflow already has the run id in
    its inputs, so no new credential plumbing.
    """
    reason: str


@app.post(
    "/v1/runs/{project}/{run_id}/aborted",
    response_model=AbortRunResult,
)
async def run_aborted(
    req: RunAbortedRequest,
    project: str = Path(...),
    run_id: str = Path(...),
) -> AbortRunResult:
    """Workflow-side typed abort. Body identical to `_abort_run` —
    cancels the GH workflow_run if one is recorded, marks the Run
    ABORTED with the given reason, releases any locks the Run held.
    Idempotent: a second call returns `already_terminal`."""
    return await _abort_run(
        app.state.cosmos,
        getattr(app.state, "gh_minter", None),
        run_id=run_id,
        project=project,
        reason=req.reason,
    )


@app.post(
    "/v1/runs/{project}/{run_id}/completed",
    response_model=RunCallbackResult,
)
async def run_completed(
    req: RunCompletedRequest,
    project: str = Path(...),
    run_id: str = Path(...),
) -> RunCallbackResult:
    """Workflow-side callback: the dispatched workflow finished with the
    given conclusion + verification.json content. Records the attempt,
    runs the decision engine, and on terminal decisions releases the
    issue lock (and PR lock for triage cycles).

    Lease release is NOT done here — the workflow's `release-lease` job
    handles that via `/v1/lease/{lease_id}/release` directly so capacity
    frees independent of run-state outcome.
    """
    cosmos: Cosmos = app.state.cosmos
    found = await run_ops.read_run(cosmos, project=project, run_id=run_id)
    if found is None:
        raise HTTPException(404, f"no run {project}/{run_id}")
    run, etag = found

    # Phase outputs (#101): validate posted keys against the registered
    # phase's declared `outputs`. Mismatch is a workflow contract
    # violation — 400 instead of recording a malformed completion. The
    # workflow read sits before _process_run_completion so a bad payload
    # never advances run state.
    phase_outputs = await _validate_phase_outputs(
        cosmos, run=run, posted_outputs=req.outputs,
    )

    verification_result: VerificationResult | None = None
    if req.verification is not None:
        try:
            verification_result = VerificationResult.model_validate(req.verification)
        except Exception:
            log.warning(
                "run %s/%s: posted verification didn't validate; "
                "decision engine will treat as malformed",
                project, run_id,
            )

    decision_value = await _process_run_completion(
        run=run,
        etag=etag,
        workflow_run_id=req.workflow_run_id,
        conclusion=req.conclusion,
        verification_result=verification_result,
        repo=run.issue_repo,
        screenshots_markdown=req.screenshots_markdown,
        phase_outputs=phase_outputs,
    )

    result_dict: dict[str, Any] = {}
    terminal = decision_value in (
        RunDecision.ADVANCE.value,
        RunDecision.ABORT_BUDGET_ATTEMPTS.value,
        RunDecision.ABORT_BUDGET_COST.value,
        RunDecision.ABORT_MALFORMED.value,
    )
    if terminal:
        # Re-read to get the post-decision Run state for lock release.
        post = await run_ops.read_run(cosmos, project=project, run_id=run_id)
        post_run = post[0] if post is not None else run
        await _release_locks_on_terminal(
            run=post_run, repo=run.issue_repo, result=result_dict,
        )

    return RunCallbackResult(
        run_id=run.id,
        decision=decision_value,
        issue_lock_released=result_dict.get("issue_lock_released"),
        pr_lock_released=result_dict.get("pr_lock_released"),
    )


# ─── Decision-engine replay (#111 smoke-test substrate) ────────────────────
#
# Pure-function preview of `decide()` against a Run. The caller posts a
# synthetic `/completed` payload (and optionally an alternative workflow
# shape); glimmung returns the decision the engine *would* make without
# touching Cosmos or firing any GHA dispatch.
#
# Catches the verify=true→false-class registration bugs documented in
# #111 at zero cost: a real agent dispatch was burning ~20 min of agent
# runtime per iteration to surface a bug a static check could find.


class WorkflowReplayOverride(BaseModel):
    """Replay-only workflow shape — same fields decide() reads, minus
    project/name (which are irrelevant for the verdict). Lets a caller
    sketch a `what if my registration looked like this?` scenario
    without re-registering the workflow first.

    Cross-phase input ref validation runs on construction so a typo in
    `${{ phases.X.outputs.Y }}` surfaces in the replay request rather
    than silently producing a meaningless verdict.
    """
    phases: list[PhaseSpec]
    pr: PrPrimitiveSpec = Field(default_factory=PrPrimitiveSpec)
    budget: BudgetConfig = Field(default_factory=BudgetConfig)
    trigger_label: str = "issue-agent"
    default_requirements: dict[str, Any] = Field(default_factory=dict)


class RunReplayRequest(BaseModel):
    """`POST /v1/runs/{project}/{run_id}/replay` body.

    `synthetic_completion` mirrors the live `/completed` callback body —
    copy-paste a real one and tweak fields to ask `what if?`.

    `override_workflow` is optional. When set, the replay runs against
    the provided shape instead of the registered workflow; useful for
    `if I changed my registration to verify=false, would this run have
    advanced?`. When omitted, the live registration drives the verdict.
    """
    synthetic_completion: SyntheticCompletion
    override_workflow: WorkflowReplayOverride | None = None


@app.post(
    "/v1/runs/{project}/{run_id}/replay",
    response_model=ReplayResult,
    dependencies=[Depends(require_admin_user)],
)
async def replay_run_decision(
    req: RunReplayRequest,
    project: str = Path(...),
    run_id: str = Path(...),
) -> ReplayResult:
    """Pure-function replay of the decision engine against a Run.

    Reads the Run + workflow registration, applies the synthetic
    completion to the latest attempt in-memory, returns the verdict the
    engine would produce — plus a next-action hint (which phase would
    be dispatched, which recycle target would fire, what abort
    explanation would be posted). Performs no Cosmos writes and fires
    no GHA dispatches.

    Admin-only: same auth posture as `register_workflow` + `abort_run`,
    since the body can echo back workflow shapes that aren't otherwise
    enumerable through the public API.
    """
    cosmos: Cosmos = app.state.cosmos
    found = await run_ops.read_run(cosmos, project=project, run_id=run_id)
    if found is None:
        raise HTTPException(404, f"no run {project}/{run_id}")
    run, _etag = found

    if req.override_workflow is not None:
        # Validate cross-phase input refs against the override's own
        # phase order — same contract as register_workflow's
        # `_validate_v1`. Surfacing this as a 422 keeps "registration
        # would have been rejected" parity with the live admin API.
        try:
            validate_phase_input_refs(req.override_workflow.phases)
        except ValueError as e:
            raise HTTPException(422, f"override_workflow rejected: {e}")
        workflow_model = Workflow(
            id=run.workflow,
            project=run.project,
            name=run.workflow,
            phases=req.override_workflow.phases,
            pr=req.override_workflow.pr,
            budget=req.override_workflow.budget,
            trigger_label=req.override_workflow.trigger_label,
            default_requirements=req.override_workflow.default_requirements,
            metadata={},
            created_at=datetime.now(UTC),
        )
        workflow_source = "override"
    else:
        workflow_doc = await _read_workflow(cosmos, run.project, run.workflow)
        workflow_model = _doc_to_workflow(workflow_doc) if workflow_doc else None
        if workflow_model is None:
            raise HTTPException(
                404,
                f"no workflow registration {run.project}/{run.workflow!r} "
                "(pass override_workflow if the live registration is missing)",
            )
        workflow_source = "registered"

    # decide() asserts the latest attempt's phase exists on the workflow.
    # Replay is a smoke test, so surface that mismatch as a 422 with a
    # readable error instead of a 500.
    phase_names = [p.name for p in workflow_model.phases]
    if run.attempts and run.attempts[-1].phase not in phase_names:
        raise HTTPException(
            422,
            f"run's latest attempt phase {run.attempts[-1].phase!r} not in "
            f"workflow phases {phase_names}; cannot replay",
        )
    if not run.attempts:
        raise HTTPException(422, f"run {run_id!r} has no attempts to replay against")

    return replay_decision(
        run=run,
        workflow=workflow_model,
        synthetic=req.synthetic_completion,
        workflow_source=workflow_source,
    )


# ─── Resume primitive (#111) ───────────────────────────────────────────────
#
# Spawn a new Run from a prior (terminal) Run with phases preceding a
# named entrypoint skipped — their captured outputs feed forward through
# the multi-phase substitution path (`_collect_phase_outputs`) into the
# entrypoint's dispatch inputs. The motivating case from the prior
# session: agent-execute aborted because of a verify=true→false mismatch
# in the registration; resume from agent-execute reuses env-prep's
# captured validation_url + namespace and re-dispatches agent-execute
# without re-running env-prep.


class RunResumeRequest(BaseModel):
    """`POST /v1/runs/{project}/{run_id}/resume` body.

    `entrypoint_phase` is the phase the resumed Run will start
    executing at. All phases declared earlier in the workflow's order
    are auto-skipped; each gets a synthesized PhaseAttempt with
    `phase_outputs` carried from the prior Run's same-named phase.

    `trigger_source` is recorded on the new Run for observability;
    callers should set `kind` (e.g. `"resume_via_admin_api"`,
    `"resume_via_mcp"`) and any audit-relevant context.
    """
    entrypoint_phase: str
    trigger_source: dict[str, Any] = Field(default_factory=dict)


@app.post(
    "/v1/runs/{project}/{run_id}/resume",
    response_model=ResumeResult,
    dependencies=[Depends(require_admin_user)],
)
async def resume_run(
    req: RunResumeRequest,
    project: str = Path(...),
    run_id: str = Path(...),
) -> ResumeResult:
    """Resume from a prior Run by spawning a new Run that starts at
    `entrypoint_phase` with all earlier phases pre-marked skipped.

    Body of work delegated to `dispatch_resumed_run`. This handler's
    job is HTTP shape: 422 on validation failures, 409 on lock
    collisions (issue already locked by a different in-flight run),
    plain 200 with `state` echoed for the operational outcomes
    (`dispatched`, `pending`, `dispatch_failed`).

    Admin-only auth posture, same as `register_workflow` /
    `recompute_decision`-style admin mutations.
    """
    trigger_source = {**req.trigger_source}
    trigger_source.setdefault("kind", "resume_via_admin_api")
    trigger_source.setdefault("resumed_from_run_id", run_id)

    result = await dispatch_resumed_run(
        app,
        project=project,
        prior_run_id=run_id,
        entrypoint_phase=req.entrypoint_phase,
        trigger_source=trigger_source,
    )

    if result.state == "prior_missing":
        raise HTTPException(404, result.detail)
    if result.state == "workflow_missing":
        raise HTTPException(404, result.detail)
    if result.state == "phase_invalid":
        raise HTTPException(422, result.detail)
    if result.state == "outputs_missing":
        raise HTTPException(422, result.detail)
    if result.state == "prior_in_progress":
        raise HTTPException(409, result.detail)
    if result.state == "already_running":
        raise HTTPException(409, result.detail)
    return result


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
    # Workflows have nested phases with camelCase keys; the shallow
    # `_camel_to_snake` doesn't recurse into them, so `model_validate`
    # 500s on `phases.0.workflowFilename` not matching `workflow_filename`.
    # `_doc_to_workflow` walks the nested shape correctly. Same fix as
    # the list_workflows / register_workflow hot-fix in 4babd13.
    return StateSnapshot(
        hosts=[Host.model_validate(lease_ops._camel_to_snake(h)) for h in host_docs],
        pending_leases=[Lease.model_validate(lease_ops._camel_to_snake(p)) for p in pending_docs],
        active_leases=[Lease.model_validate(lease_ops._camel_to_snake(a)) for a in active_docs],
        projects=[Project.model_validate(lease_ops._camel_to_snake(d)) for d in project_docs],
        workflows=[w for d in workflow_docs if (w := _doc_to_workflow(d)) is not None],
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
    return _doc_to_workflow(doc)


@app.get("/v1/workflows", response_model=list[Workflow], dependencies=[Depends(require_admin_user)])
async def list_workflows() -> list[Workflow]:
    docs = await query_all(app.state.cosmos.workflows, "SELECT * FROM c")
    return [_doc_to_workflow(d) for d in docs]


class WorkflowUpdateRequest(BaseModel):
    """PATCH /v1/workflows/{project}/{name} body. All fields optional —
    None means "don't change". Only carries the rollout-knob fields a
    live workflow row needs to flip without re-registering (`pr.enabled`,
    `budget.total`); structural fields (phases, recycle policy) still go
    through register_workflow's full upsert."""
    pr_enabled: bool | None = None
    budget_total: float | None = None


@app.patch(
    "/v1/workflows/{project}/{name}",
    response_model=Workflow,
    dependencies=[Depends(require_admin_user)],
)
async def patch_workflow_endpoint(
    req: WorkflowUpdateRequest,
    project: str = Path(...),
    name: str = Path(...),
) -> Workflow:
    cosmos: Cosmos = app.state.cosmos
    doc = await _read_workflow(cosmos, project, name)
    if doc is None:
        raise HTTPException(404, f"no workflow {project}/{name}")
    if req.pr_enabled is not None:
        pr = doc.get("pr") or {}
        pr["enabled"] = bool(req.pr_enabled)
        doc["pr"] = pr
    if req.budget_total is not None:
        budget = doc.get("budget") or {}
        budget["total"] = float(req.budget_total)
        doc["budget"] = budget
    await cosmos.workflows.replace_item(item=name, body=doc)
    return _doc_to_workflow(doc)


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

    # `workflow_run` events are intentionally ignored. The workflow
    # itself reports lifecycle to glimmung via curl callbacks
    # (`POST /v1/runs/{project}/{run_id}/started` and `/completed`) —
    # GitHub doesn't echo workflow_dispatch inputs on workflow_run
    # webhook payloads, so there's no way to map an inbound webhook to
    # a glimmung Run without help from the workflow side. The workflow
    # already curls glimmung at start (lease verify) and end (lease
    # release), so adding the run-state callbacks is essentially free.
    if event == "pull_request":
        return await _handle_pull_request(payload)
    if event == "pull_request_review":
        return await _handle_pull_request_review(payload)
    # `issues` events are ignored entirely — glimmung owns the issue
    # substrate; nothing about GH issue activity drives glimmung state.
    return {"ignored": event}


# ─── PR webhook handlers (#50 slice 3 rewrite) ──────────────────────────────────


def _parse_glimmung_meta(body: str) -> dict[str, str]:
    """Parse the agent's `<!-- glimmung-meta ... -->` block out of a PR
    body (#50 slice 4). Returns the key=value pairs as a dict.

    Format (one key per line, no trailing comments):

        <!-- glimmung-meta
        project=glimmung
        issue_number=42
        issue_id=01JABC...
        validation_env=https://...
        notes_md_b64=<base64>
        screenshots_md_b64=<base64>
        -->

    The agent's `Open pull request` step in agent-run.yml emits this
    block. Manual / human-opened PRs don't have it; the function returns
    an empty dict in that case and the webhook treats the PR as a
    standard mirror with no agent linkage.
    """
    import re
    match = re.search(
        r"<!-- glimmung-meta\s*\n(.*?)\n\s*-->",
        body, re.DOTALL,
    )
    if not match:
        return {}
    out: dict[str, str] = {}
    for line in match.group(1).splitlines():
        line = line.strip()
        if not line or "=" not in line:
            continue
        k, _, v = line.partition("=")
        out[k.strip()] = v.strip()
    return out


def _decode_b64(value: str) -> str:
    """Best-effort base64 decode for marker payloads. Falls back to the
    raw string on bad padding so a corrupted marker still surfaces some
    useful content rather than silently swallowing it."""
    import base64
    try:
        return base64.b64decode(value).decode("utf-8", errors="replace")
    except Exception:
        return value


def _render_glimmung_pr_body(meta: dict[str, str]) -> str:
    """Compose the rich glimmung-side PR body from the marker payload.
    Decodes the base64-wrapped notes/screenshots and stitches in the
    validation env link. The output is what the dashboard renders in the
    PR detail view's Body section."""
    parts: list[str] = []
    if validation := meta.get("validation_env"):
        parts.append(f"Validation env (until PR closes): {validation}")
    if notes_b64 := meta.get("notes_md_b64"):
        parts.append(_decode_b64(notes_b64))
    if screenshots_b64 := meta.get("screenshots_md_b64"):
        parts.append(_decode_b64(screenshots_b64))
    if lease_id := meta.get("lease_id"):
        host = meta.get("host", "?")
        parts.append(
            f"Generated by glimmung-leased agent run on host `{host}` "
            f"(lease `{lease_id}`)."
        )
    return "\n\n".join(p for p in parts if p)


async def _handle_pull_request(payload: dict[str, Any]) -> dict[str, Any]:
    """Mirror `pull_request.*` events into the glimmung `prs` container.

    Pre-#50 this parsed `Closes #N` from the PR body to link a Run to a
    GH PR number. Post-#50 the PR is the canonical entity in glimmung's
    own `prs` container and the run/issue linkage is set explicitly by
    the agent's `POST /v1/prs` step (#50 slice 4) — the webhook's job
    is just to keep glimmung's PR document in sync with GH's lifecycle
    (state transitions, head sha refreshes, title/body edits).

    Actions handled:
      - opened / reopened: ensure the glimmung PR exists + reopen if it
        was previously CLOSED (skipped if it was merged — GH wouldn't
        fire reopened for those but the guard is cheap).
      - synchronize / edited: refresh head_sha + title/body so the
        dashboard shows the latest commit.
      - closed: close_pr or merge_pr depending on `pr.merged`.
      - other: ignored.
    """
    action = payload.get("action") or ""
    if action not in ("opened", "reopened", "closed", "synchronize", "edited"):
        return {"ignored": f"pull_request.{action}"}

    pr_payload = payload.get("pull_request") or {}
    repo = (payload.get("repository") or {}).get("full_name", "")
    pr_number = pr_payload.get("number")
    if not repo or not pr_number:
        return {"ignored": "missing fields"}

    cosmos: Cosmos = app.state.cosmos
    matching = await query_all(
        cosmos.projects,
        "SELECT * FROM c WHERE c.githubRepo = @r",
        parameters=[{"name": "@r", "value": repo}],
    )
    if not matching:
        return {"ignored": "no project for repo"}
    project = matching[0]["name"]

    title = pr_payload.get("title") or f"{repo}#{pr_number}"
    raw_body = pr_payload.get("body") or ""
    branch = ((pr_payload.get("head") or {}).get("ref") or "")
    base_ref = ((pr_payload.get("base") or {}).get("ref") or "main")
    head_sha = ((pr_payload.get("head") or {}).get("sha") or "")
    html_url = pr_payload.get("html_url") or ""
    pr_merged = bool(pr_payload.get("merged"))
    merged_by_user = (pr_payload.get("merged_by") or {}).get("login") or ""

    # Slice 4: agent-opened PRs carry a `<!-- glimmung-meta ... -->`
    # block in the body. Parse it to (a) write the rich content into
    # the glimmung PR doc rather than the GH PR body and (b) attach
    # linked_issue_id without round-tripping through admin auth.
    meta = _parse_glimmung_meta(raw_body)
    rich_body = _render_glimmung_pr_body(meta) if meta else ""
    body = rich_body or raw_body

    pr, etag, created = await pr_ops.ensure_pr_for_github(
        cosmos,
        project=project,
        repo=repo,
        number=int(pr_number),
        title=title,
        branch=branch,
        body=body,
        base_ref=base_ref,
        head_sha=head_sha,
        html_url=html_url,
    )
    outcome: dict[str, Any] = {
        "pr_id": pr.id,
        "created": created,
        "action": action,
    }

    # ensure_pr_for_github only honors create-time fields. For an
    # existing PR, patch the user-editable + GH-provided fields so
    # Cosmos stays in sync with GH edits + commits.
    if not created and action in ("opened", "reopened", "edited", "synchronize"):
        pr, etag = await pr_ops.update_pr(
            cosmos, pr=pr, etag=etag,
            title=title or None,
            body=body if body else None,
            branch=branch or None,
            base_ref=base_ref or None,
            head_sha=head_sha or None,
            html_url=html_url or None,
        )
        outcome["patched"] = True

    # Apply linkages from the marker (idempotent — same id wins on
    # webhook redelivery).
    linked_issue_id = meta.get("issue_id") if meta else None
    if linked_issue_id and pr.linked_issue_id != linked_issue_id:
        pr, etag = await pr_ops.update_pr(
            cosmos, pr=pr, etag=etag,
            linked_issue_id=linked_issue_id,
        )
        outcome["linked_issue_id"] = linked_issue_id

    # Best-effort run linkage: derive the active Run for this issue +
    # PR coords if the agent didn't pre-attach it. find_run_by_pr is
    # the canonical post-#33 lookup.
    if pr.linked_run_id is None:
        run_lookup = await run_ops.find_run_by_pr(
            cosmos, issue_repo=repo, pr_number=int(pr_number),
        )
        if run_lookup is not None:
            run_for_link, _ = run_lookup
            pr, etag = await pr_ops.update_pr(
                cosmos, pr=pr, etag=etag,
                linked_run_id=run_for_link.id,
            )
            outcome["linked_run_id"] = run_for_link.id

    if action == "reopened" and pr.state == PRState.CLOSED:
        if pr.merged_at is not None:
            log.warning(
                "pull_request.reopened on already-merged PR %s#%d (glimmung id %s); ignoring",
                repo, pr_number, pr.id,
            )
            outcome["reopen_ignored"] = "merged"
        else:
            pr, etag = await pr_ops.reopen_pr(cosmos, pr=pr, etag=etag)
            outcome["reopened"] = True

    if action == "closed":
        if pr_merged:
            pr, etag = await pr_ops.merge_pr(
                cosmos, pr=pr, etag=etag,
                merged_by=merged_by_user or "unknown",
            )
            outcome["merged"] = True
        else:
            pr, etag = await pr_ops.close_pr(cosmos, pr=pr, etag=etag)
            outcome["closed"] = True

    return outcome


async def _handle_pull_request_review(payload: dict[str, Any]) -> dict[str, Any]:
    """`pull_request_review.submitted` — enqueue a GH_REVIEW signal so
    the drain loop can route it through the triage decision engine.

    Post-#50 the signal targets the glimmung PR id (ULID), not the GH
    PR number. The drain still accepts the legacy `(repo, pr_number)`
    shape so any signals enqueued before the rewrite continue to drain
    cleanly. If no glimmung PR exists for `(repo, pr_number)` (the
    webhook handler above ensures one normally does), the GH coords
    are used as a fallback so the signal isn't lost.

    Other actions (`edited`, `dismissed`) are ignored — only the
    initial submission is decisional."""
    if payload.get("action") != "submitted":
        return {"ignored": f"pull_request_review.{payload.get('action')}"}

    pr_payload = payload.get("pull_request") or {}
    review = payload.get("review") or {}
    repo = (payload.get("repository") or {}).get("full_name", "")
    pr_number = pr_payload.get("number")
    if not repo or not pr_number:
        return {"ignored": "missing fields"}

    cosmos: Cosmos = app.state.cosmos
    target_repo = repo
    target_id = str(pr_number)
    mirrored_review = False
    found = await pr_ops.find_pr_by_repo_number(
        cosmos, repo=repo, number=int(pr_number),
    )
    if found is not None:
        pr, etag = found
        target_repo = pr.project
        target_id = pr.id
        raw_state = str(review.get("state") or PRReviewState.COMMENTED.value).lower()
        try:
            review_state = PRReviewState(raw_state)
        except ValueError:
            log.warning(
                "pull_request_review.submitted on %s#%d has unknown state %r; recording as commented",
                repo, int(pr_number), raw_state,
            )
            review_state = PRReviewState.COMMENTED
        submitted_at_raw = review.get("submitted_at")
        submitted_at = datetime.now(UTC)
        if submitted_at_raw:
            try:
                submitted_at = datetime.fromisoformat(
                    str(submitted_at_raw).replace("Z", "+00:00")
                )
            except ValueError:
                log.warning(
                    "pull_request_review.submitted on %s#%d has invalid submitted_at %r; using now",
                    repo, int(pr_number), submitted_at_raw,
                )
        await pr_ops.append_pr_review(
            cosmos,
            pr=pr,
            etag=etag,
            review=PRReview(
                id=str(ULID()),
                gh_id=review.get("id"),
                author=(review.get("user") or {}).get("login") or "",
                state=review_state,
                body=review.get("body") or "",
                submitted_at=submitted_at,
                html_url=review.get("html_url"),
            ),
        )
        mirrored_review = True

    sig = await signal_ops.enqueue_signal(
        cosmos,
        target_type=SignalTargetType.PR,
        target_repo=target_repo,
        target_id=target_id,
        source=SignalSource.GH_REVIEW,
        payload={
            "state": review.get("state") or "",
            "body": review.get("body") or "",
            "reviewer": (review.get("user") or {}).get("login") or "",
            "review_id": review.get("id"),
        },
    )
    return {"enqueued_signal": sig.id, "mirrored_review": mirrored_review}


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
    comments: list[IssueComment] = Field(default_factory=list)
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


class IssueCommentRequest(BaseModel):
    body: str


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
    """`issue_id` is the canonical handle. `project` is optional — the
    server cross-partition resolves it from the Issue doc when omitted.
    `workflow` is optional; dispatch_run picks the project's only
    workflow if there's exactly one."""
    issue_id: str
    project: str | None = None
    workflow: str | None = None


@app.get(
    "/v1/issues",
    response_model=list[IssueRow],
    dependencies=[Depends(require_admin_user)],
)
async def list_issues() -> list[IssueRow]:
    """All open glimmung Issues, across registered projects. Sourced
    from the Cosmos `issues` container — glimmung is the source of
    truth; nothing about GH issue activity flows back. Issues are
    seeded once via the seed script (or minted via `POST /v1/issues`)
    and lifecycle from there is glimmung-internal.

    Labels are surfaced informationally only — they're a courtesy
    syndication surface, not a dispatch primitive. The dispatch button
    on each row is the trigger."""
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
    a query param. 404 if no glimmung Issue exists for the GH coords —
    glimmung doesn't auto-mint from GH, so any GH issue without a prior
    glimmung-side existence is invisible here."""
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
        comments=list(issue.comments),
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


@app.post(
    "/v1/issues/by-id/{project}/{issue_id}/comments",
    response_model=IssueComment,
)
async def create_issue_comment_endpoint(
    req: IssueCommentRequest,
    project: str = Path(...),
    issue_id: str = Path(...),
    user: User = Depends(require_admin_user),
) -> IssueComment:
    """Append a glimmung-authored Issue comment."""
    if not req.body.strip():
        raise HTTPException(400, "body required")
    cosmos: Cosmos = app.state.cosmos
    found = await issue_ops.read_issue(cosmos, project=project, issue_id=issue_id)
    if found is None:
        raise HTTPException(404, f"no glimmung issue {project}/{issue_id}")
    issue, etag = found
    _, _, comment = await issue_ops.add_comment(
        cosmos,
        issue=issue,
        etag=etag,
        author=user.email,
        body=req.body,
    )
    return comment


@app.patch(
    "/v1/issues/by-id/{project}/{issue_id}/comments/{comment_id}",
    response_model=IssueComment,
)
async def update_issue_comment_endpoint(
    req: IssueCommentRequest,
    project: str = Path(...),
    issue_id: str = Path(...),
    comment_id: str = Path(...),
    user: User = Depends(require_admin_user),
) -> IssueComment:
    """Edit the signed-in admin's own Issue comment."""
    if not req.body.strip():
        raise HTTPException(400, "body required")
    cosmos: Cosmos = app.state.cosmos
    found = await issue_ops.read_issue(cosmos, project=project, issue_id=issue_id)
    if found is None:
        raise HTTPException(404, f"no glimmung issue {project}/{issue_id}")
    issue, etag = found
    existing = next((c for c in issue.comments if c.id == comment_id), None)
    if existing is None:
        raise HTTPException(404, f"no issue comment {comment_id}")
    if existing.author != user.email:
        raise HTTPException(403, "cannot edit another author's comment")
    updated = await issue_ops.update_comment(
        cosmos,
        issue=issue,
        etag=etag,
        comment_id=comment_id,
        body=req.body,
    )
    if updated is None:
        raise HTTPException(404, f"no issue comment {comment_id}")
    _, _, comment = updated
    return comment


@app.delete(
    "/v1/issues/by-id/{project}/{issue_id}/comments/{comment_id}",
    response_model=IssueDetail,
    dependencies=[Depends(require_admin_user)],
)
async def delete_issue_comment_endpoint(
    project: str = Path(...),
    issue_id: str = Path(...),
    comment_id: str = Path(...),
) -> IssueDetail:
    """Delete an Issue comment. Admin-auth gated."""
    cosmos: Cosmos = app.state.cosmos
    found = await issue_ops.read_issue(cosmos, project=project, issue_id=issue_id)
    if found is None:
        raise HTTPException(404, f"no glimmung issue {project}/{issue_id}")
    issue, etag = found
    removed = await issue_ops.remove_comment(
        cosmos,
        issue=issue,
        etag=etag,
        comment_id=comment_id,
    )
    if removed is None:
        raise HTTPException(404, f"no issue comment {comment_id}")
    issue, _ = removed
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


@app.get(
    "/v1/graph",
    response_model=IssueGraph,
    dependencies=[Depends(require_admin_user)],
)
async def system_graph(project: str | None = None) -> IssueGraph:
    """System-wide live graph (#43): every open Issue plus in-flight
    Runs, open PRs, and pending Signals that currently attach to them."""
    return await _build_system_graph(app.state.cosmos, project=project)


async def _build_system_graph(
    cosmos: Cosmos, *, project: str | None = None,
) -> IssueGraph:
    issues = await issue_ops.list_open_issues(cosmos, project=project)
    issue_ids = {i.id for i in issues}
    issue_project_by_id = {i.id: i.project for i in issues}
    nodes: list[GraphNode] = []
    edges: list[GraphEdge] = []

    for issue in issues:
        nodes.append(GraphNode(
            id=f"issue:{issue.id}",
            kind="issue",
            label=issue.title,
            state=issue.state.value,
            timestamp=issue.updated_at.isoformat(),
            metadata={
                "issue_id": issue.id,
                "project": issue.project,
                "repo": issue.metadata.github_issue_repo,
                "number": issue.metadata.github_issue_number,
                "html_url": issue.metadata.github_issue_url,
                "labels": issue.labels,
            },
        ))

    run_docs = await query_all(
        cosmos.runs,
        "SELECT * FROM c WHERE c.state = @s ORDER BY c.created_at ASC",
        parameters=[{"name": "@s", "value": RunState.IN_PROGRESS.value}],
    )
    runs: list[Run] = []
    for doc in run_docs:
        run = Run.model_validate({k: v for k, v in doc.items() if not k.startswith("_")})
        if run.issue_id not in issue_ids:
            continue
        if project is not None and run.project != project:
            continue
        runs.append(run)
        run_node_id = f"run:{run.id}"
        nodes.append(GraphNode(
            id=run_node_id,
            kind="run",
            label=run.workflow,
            state=run.state.value,
            timestamp=run.created_at.isoformat(),
            metadata={
                "run_id": run.id,
                "project": run.project,
                "workflow": run.workflow,
                "issue_id": run.issue_id,
                "validation_url": run.validation_url,
                "cumulative_cost_usd": run.cumulative_cost_usd,
                "cloned_from_run_id": run.cloned_from_run_id,
                "entrypoint_phase": run.entrypoint_phase,
            },
        ))
        edges.append(GraphEdge(
            source=f"issue:{run.issue_id}",
            target=run_node_id,
            kind="spawned",
        ))
        previous_attempt_node: str | None = None
        for attempt in run.attempts:
            attempt_node_id = f"attempt:{run.id}:{attempt.attempt_index}"
            nodes.append(GraphNode(
                id=attempt_node_id,
                kind="attempt",
                label=attempt.phase,
                state=(
                    "skipped" if attempt.skipped_from_run_id
                    else attempt.conclusion or "in_progress"
                ),
                timestamp=attempt.dispatched_at.isoformat(),
                metadata={
                    "run_id": run.id,
                    "attempt_index": attempt.attempt_index,
                    "phase": attempt.phase,
                    "workflow_filename": attempt.workflow_filename,
                    "workflow_run_id": attempt.workflow_run_id,
                    "completed_at": attempt.completed_at.isoformat()
                    if attempt.completed_at else None,
                    "decision": attempt.decision,
                    "skipped_from_run_id": attempt.skipped_from_run_id,
                },
            ))
            edges.append(GraphEdge(
                source=run_node_id if previous_attempt_node is None else previous_attempt_node,
                target=attempt_node_id,
                kind="attempted" if previous_attempt_node is None else "retried",
            ))
            previous_attempt_node = attempt_node_id

    run_ids = {r.id for r in runs}
    pr_docs = await query_all(
        cosmos.prs,
        "SELECT * FROM c WHERE c.state = @s ORDER BY c.created_at ASC",
        parameters=[{"name": "@s", "value": PRState.OPEN.value}],
    )
    for doc in pr_docs:
        pr = PR.model_validate({k: v for k, v in doc.items() if not k.startswith("_")})
        if pr.linked_issue_id not in issue_ids and pr.linked_run_id not in run_ids:
            continue
        if project is not None and pr.project != project:
            continue
        pr_node_id = f"pr:{pr.id}"
        nodes.append(GraphNode(
            id=pr_node_id,
            kind="pr",
            label=f"{pr.repo}#{pr.number}",
            state=pr.state.value,
            timestamp=pr.updated_at.isoformat(),
            metadata={
                "pr_id": pr.id,
                "project": pr.project,
                "repo": pr.repo,
                "number": pr.number,
                "title": pr.title,
                "html_url": pr.html_url,
                "linked_issue_id": pr.linked_issue_id,
                "linked_run_id": pr.linked_run_id,
                "review_count": len(pr.reviews),
                "comment_count": len(pr.comments),
            },
        ))
        if pr.linked_run_id in run_ids:
            edges.append(GraphEdge(source=f"run:{pr.linked_run_id}", target=pr_node_id, kind="opened"))
        elif pr.linked_issue_id in issue_ids:
            edges.append(GraphEdge(source=f"issue:{pr.linked_issue_id}", target=pr_node_id, kind="opened"))

    signal_docs = await query_all(
        cosmos.signals,
        "SELECT * FROM c WHERE c.state = @s ORDER BY c.enqueued_at ASC",
        parameters=[{"name": "@s", "value": SignalState.PENDING.value}],
    )
    for doc in signal_docs:
        sig = Signal.model_validate({k: v for k, v in doc.items() if not k.startswith("_")})
        target_issue_id: str | None = None
        target_node: str | None = None
        if sig.target_type == SignalTargetType.ISSUE and sig.target_id in issue_ids:
            target_issue_id = sig.target_id
            target_node = f"issue:{sig.target_id}"
        elif sig.target_type == SignalTargetType.RUN and sig.target_id in run_ids:
            run = next(r for r in runs if r.id == sig.target_id)
            target_issue_id = run.issue_id
            target_node = f"run:{sig.target_id}"
        elif (
            sig.target_type == SignalTargetType.PR
            and sig.target_repo in issue_project_by_id.values()
        ):
            # Post-#50 PR signals target `(project, pr_id)`. If the PR
            # node is present, attach there; otherwise leave it out of
            # the system view until a linked PR exists.
            candidate = f"pr:{sig.target_id}"
            if any(n.id == candidate for n in nodes):
                target_node = candidate
                target_issue_id = next(
                    (
                        str(n.metadata.get("linked_issue_id"))
                        for n in nodes
                        if n.id == candidate and n.metadata.get("linked_issue_id")
                    ),
                    None,
                )
        if target_node is None:
            continue
        if project is not None and target_issue_id is not None:
            if issue_project_by_id.get(target_issue_id) != project:
                continue
        sig_node_id = f"signal:{sig.id}"
        nodes.append(GraphNode(
            id=sig_node_id,
            kind="signal",
            label=sig.source.value,
            state=sig.state.value,
            timestamp=sig.enqueued_at.isoformat(),
            metadata={
                "signal_id": sig.id,
                "target_type": sig.target_type.value,
                "target_repo": sig.target_repo,
                "target_id": sig.target_id,
                "payload": sig.payload,
            },
        ))
        edges.append(GraphEdge(source=target_node, target=sig_node_id, kind="feedback"))

    return IssueGraph(issue_id="system", nodes=nodes, edges=edges)


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
                # Resume primitive (#111) — surface the lineage pointers
                # so the dashboard can render the Run-lineage tree
                # (parent-child across resume-spawned Runs) and the
                # entrypoint-arrow highlight on resumed Runs.
                "cloned_from_run_id": d.get("cloned_from_run_id"),
                "entrypoint_phase": d.get("entrypoint_phase"),
            },
        ))
        edges.append(GraphEdge(
            source=issue_node_id, target=run_node_id, kind="spawned",
        ))

        # Run-lineage edge: a resumed Run draws back to its prior
        # (cloned-from) Run. Only added if the prior is also in this
        # graph — cross-issue resume isn't a thing today, but the guard
        # keeps the edge set referentially closed so a renderer doesn't
        # have to handle dangling sources.
        cloned_from = d.get("cloned_from_run_id")
        if cloned_from and cloned_from in run_ids:
            edges.append(GraphEdge(
                source=f"run:{cloned_from}",
                target=run_node_id,
                kind="resumed_from",
            ))

        prev_attempt_node: str | None = None
        for a in d.get("attempts") or []:
            ai = a.get("attempt_index")
            attempt_node_id = f"attempt:{run_id}:{ai}"
            verification = a.get("verification") or {}
            skipped_from = a.get("skipped_from_run_id")
            # Synthesized skip-marks (#111) take priority over the
            # generic completed/pending state so the dashboard can
            # grey them out regardless of how the synthesis stamped
            # `completed_at`.
            attempt_state = (
                "skipped" if skipped_from
                else verification.get("status") or (
                    "completed" if a.get("completed_at") else "pending"
                )
            )
            nodes.append(GraphNode(
                id=attempt_node_id,
                kind="attempt",
                label=f"{a.get('phase', 'attempt')} #{ai}",
                state=attempt_state,
                timestamp=a.get("dispatched_at"),
                metadata={
                    "phase": a.get("phase"),
                    "workflow_filename": a.get("workflow_filename"),
                    "workflow_run_id": a.get("workflow_run_id"),
                    "verification": verification or None,
                    "cost_usd": a.get("cost_usd"),
                    "decision": a.get("decision"),
                    "completed_at": a.get("completed_at"),
                    "conclusion": a.get("conclusion"),
                    # Resume primitive (#111) — set on synthesized
                    # skip-marks so the dashboard can render "satisfied
                    # by Run X" tooltips and grey out skipped attempts.
                    "skipped_from_run_id": skipped_from,
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
    """UI-initiated dispatch. The trigger source is recorded on the
    resulting Run for W6 observability."""
    return await dispatch_run(
        app,
        issue_id=req.issue_id,
        project=req.project,
        trigger_source={"kind": "glimmung_ui"},
        workflow_name=req.workflow,
    )



# ─── PR view + reject signal (#19) ───────────────────────────────────────────────


class PrRow(BaseModel):
    """One row in the PR view. Sourced from the `prs` container (#50
    slice 2 cutover). The optional Run join lights up the runtime
    columns (state, attempts, cumulative cost) when a glimmung Run is
    linked; manual PRs mirror in without a Run and surface the same
    way the dashboard already shows non-agent activity."""
    id: str                                # glimmung PR id (ULID)
    project: str
    repo: str
    pr_number: int                         # GH PR number (preserved on seed)
    pr_branch: str | None = None
    title: str = ""
    state: str = "open"                    # PRState value: open | closed
    merged: bool = False                   # CLOSED + merged_at != None
    html_url: str | None = None
    linked_issue_id: str | None = None
    linked_run_id: str | None = None
    issue_number: int | None = None        # legacy convenience for the dashboard
    run_id: str | None = None
    run_state: str | None = None
    validation_url: str | None = None
    session_launch_intent: str = "cold"
    session_launch_url: str | None = None
    run_attempts: int = 0
    run_cumulative_cost_usd: float = 0.0
    pr_lock_held: bool = False             # triage in flight


class PrDetail(BaseModel):
    id: str
    project: str
    repo: str
    pr_number: int
    pr_branch: str | None = None
    title: str = ""
    body: str = ""
    state: str = "open"
    merged: bool = False
    base_ref: str = "main"
    head_sha: str = ""
    html_url: str | None = None
    linked_issue_id: str | None = None
    linked_run_id: str | None = None
    issue_number: int | None = None
    issue_title: str | None = None
    run_id: str | None = None
    run_state: str | None = None
    validation_url: str | None = None
    session_launch_intent: str = "cold"
    session_launch_url: str | None = None
    run_attempts: int = 0
    run_cumulative_cost_usd: float = 0.0
    run_attempt_history: list[dict[str, Any]] = Field(default_factory=list)
    comments: list[dict[str, Any]] = Field(default_factory=list)
    reviews: list[dict[str, Any]] = Field(default_factory=list)
    pr_lock_held: bool = False


class PrCreateRequest(BaseModel):
    """POST /v1/prs body — agent open-PR step calls this immediately
    after the GH PR has been opened (#50 slice 4)."""
    project: str
    repo: str
    number: int
    title: str
    branch: str
    body: str = ""
    base_ref: str = "main"
    head_sha: str = ""
    html_url: str = ""
    linked_issue_id: str | None = None
    linked_run_id: str | None = None


class PrUpdateRequest(BaseModel):
    """PATCH /v1/prs/by-id/{project}/{pr_id} body. Same shape as the
    update_pr substrate signature, plus an optional `state` transition
    (`open` / `closed` / `merged`). Closed-vs-merged uses two distinct
    state values because they hit two different substrate functions
    (`close_pr` vs `merge_pr`)."""
    title: str | None = None
    body: str | None = None
    branch: str | None = None
    base_ref: str | None = None
    head_sha: str | None = None
    html_url: str | None = None
    linked_issue_id: str | None = None
    linked_run_id: str | None = None
    state: str | None = None               # "open" | "closed" | "merged"
    merged_by: str | None = None           # required when state="merged"


@app.get(
    "/v1/prs",
    response_model=list[PrRow],
    dependencies=[Depends(require_admin_user)],
)
async def list_prs() -> list[PrRow]:
    """All open PRs across registered projects. Sourced from the Cosmos
    `prs` container (#50 slice 2 cutover). Bulk-loads `runs` once for
    the linked-Run join + `locks` once for the lock state, mirroring the
    /v1/issues read path's perf shape."""
    return await _list_prs_from_cosmos(app.state.cosmos)


async def _list_prs_from_cosmos(cosmos: Cosmos) -> list[PrRow]:
    """Read-path for `/v1/prs`; lifted out so tests can drive it
    directly without standing up a FastAPI client."""
    prs = await pr_ops.list_open_prs(cosmos)
    if not prs:
        return []

    run_docs = await query_all(cosmos.runs, "SELECT * FROM c")
    runs_by_id: dict[str, dict[str, Any]] = {d["id"]: d for d in run_docs}
    runs_by_repo_pr: dict[tuple[str, int], dict[str, Any]] = {}
    for d in run_docs:
        repo = d.get("issue_repo") or ""
        pr_num = d.get("pr_number")
        if repo and pr_num is not None:
            key = (repo, int(pr_num))
            cur = runs_by_repo_pr.get(key)
            if cur is None or d.get("created_at", "") > cur.get("created_at", ""):
                runs_by_repo_pr[key] = d

    lock_docs = await query_all(
        cosmos.locks,
        "SELECT * FROM c WHERE c.scope = @s",
        parameters=[{"name": "@s", "value": "pr"}],
    )
    locks_by_key = {doc["key"]: doc for doc in lock_docs}

    now = datetime.now(UTC)
    rows: list[PrRow] = []
    for pr in prs:
        run_doc = None
        if pr.linked_run_id:
            run_doc = runs_by_id.get(pr.linked_run_id)
        if run_doc is None:
            run_doc = runs_by_repo_pr.get((pr.repo, pr.number))

        lock_doc = locks_by_key.get(f"{pr.repo}#{pr.number}")
        pr_lock_held = False
        if lock_doc is not None and lock_doc.get("state") == "held":
            expires_at = datetime.fromisoformat(lock_doc["expires_at"])
            pr_lock_held = expires_at > now

        row = PrRow(
            id=pr.id,
            project=pr.project,
            repo=pr.repo,
            pr_number=pr.number,
            pr_branch=pr.branch or None,
            title=pr.title,
            state=pr.state.value,
            merged=pr.merged_at is not None,
            html_url=pr.html_url or None,
            linked_issue_id=pr.linked_issue_id,
            linked_run_id=pr.linked_run_id or (run_doc["id"] if run_doc else None),
            pr_lock_held=pr_lock_held,
        )
        if run_doc is not None:
            row.run_id = run_doc["id"]
            row.run_state = run_doc.get("state")
            row.run_attempts = len(run_doc.get("attempts") or [])
            row.run_cumulative_cost_usd = float(run_doc.get("cumulative_cost_usd") or 0.0)
            issue_number = run_doc.get("issue_number")
            if issue_number is not None and issue_number != 0:
                row.issue_number = int(issue_number)

        rows.append(row)

    rows.sort(key=lambda r: (r.project, -r.pr_number))
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
    """PR detail view. Reads the PR document from Cosmos `prs` (#50
    slice 2). Stitches in the linked Run state if a Run id is on the
    PR; otherwise looks up the most recent Run by `(repo, pr_number)`
    so legacy agent-opened PRs (pre-#50, no `linked_run_id`) still
    show their Run history."""
    repo = f"{repo_owner}/{repo_name}"
    cosmos: Cosmos = app.state.cosmos
    found = await pr_ops.find_pr_by_repo_number(
        cosmos, repo=repo, number=pr_number,
    )
    if found is None:
        raise HTTPException(404, f"no glimmung PR {repo}#{pr_number}")
    pr, _ = found
    return await _build_pr_detail(cosmos, pr=pr)


async def _build_pr_detail(cosmos: Cosmos, *, pr: PR) -> PrDetail:
    """Render a PR + its linked Run state into the `PrDetail` view.
    Shared by both the URL-keyed and id-keyed detail endpoints."""
    detail = PrDetail(
        id=pr.id,
        project=pr.project,
        repo=pr.repo,
        pr_number=pr.number,
        pr_branch=pr.branch or None,
        title=pr.title,
        body=pr.body,
        state=pr.state.value,
        merged=pr.merged_at is not None,
        base_ref=pr.base_ref,
        head_sha=pr.head_sha,
        html_url=pr.html_url or None,
        linked_issue_id=pr.linked_issue_id,
        linked_run_id=pr.linked_run_id,
        comments=[c.model_dump(mode="json") for c in pr.comments],
        reviews=[r.model_dump(mode="json") for r in pr.reviews],
    )

    run = None
    if pr.linked_run_id:
        try:
            doc = await cosmos.runs.read_item(
                item=pr.linked_run_id, partition_key=pr.project,
            )
            run = Run.model_validate({k: v for k, v in doc.items() if not k.startswith("_")})
        except Exception:
            log.warning(
                "pr_detail: linked_run_id=%s on PR %s/%d not readable; falling back",
                pr.linked_run_id, pr.repo, pr.number,
            )
    if run is None:
        lookup = await run_ops.find_run_by_pr(
            cosmos, issue_repo=pr.repo, pr_number=pr.number,
        )
        if lookup is not None:
            run = lookup[0]

    if run is not None:
        detail.run_id = run.id
        detail.run_state = run.state.value
        detail.validation_url = run.validation_url
        detail.session_launch_intent = run.session_launch_intent
        detail.run_attempts = len(run.attempts)
        detail.run_cumulative_cost_usd = run.cumulative_cost_usd
        if run.issue_number:
            detail.issue_number = run.issue_number
        for a in run.attempts:
            detail.run_attempt_history.append({
                "attempt_index": a.attempt_index,
                "phase": a.phase,
                "workflow_filename": a.workflow_filename,
                "workflow_run_id": a.workflow_run_id,
                "dispatched_at": a.dispatched_at.isoformat(),
                "completed_at": a.completed_at.isoformat() if a.completed_at else None,
                "verification_status": a.verification.status.value if a.verification else None,
                "cost_usd": a.cost_usd,
                "decision": a.decision,
            })

    if pr.linked_issue_id:
        try:
            doc = await cosmos.issues.read_item(
                item=pr.linked_issue_id, partition_key=pr.project,
            )
            detail.issue_title = str(doc.get("title") or "")
        except Exception:
            pass
    if run is not None and pr.linked_issue_id:
        detail.session_launch_url = _tank_session_launch_url(
            settings=getattr(app.state, "settings", get_settings()),
            run=run,
            pr=pr,
        )

    existing_lock = await lock_ops.read_lock(
        cosmos, scope="pr", key=f"{pr.repo}#{pr.number}",
    )
    detail.pr_lock_held = (
        existing_lock is not None
        and existing_lock.state.value == "held"
        and existing_lock.expires_at > datetime.now(UTC)
    )
    return detail


def _tank_session_launch_url(*, settings: Settings, run: Run, pr: PR) -> str:
    params: dict[str, str] = {
        "glimmung_run_id": run.id,
        "glimmung_issue_id": run.issue_id,
        "glimmung_pr_id": pr.id,
    }
    if run.validation_url:
        params["validation_url"] = run.validation_url
    return f"{settings.tank_operator_base_url.rstrip('/')}?{urlencode(params)}"


@app.get(
    "/v1/prs/by-id/{project}/{pr_id}",
    response_model=PrDetail,
    dependencies=[Depends(require_admin_user)],
)
async def pr_detail_by_id(
    project: str = Path(...),
    pr_id: str = Path(...),
) -> PrDetail:
    """Detail view keyed by glimmung PR id. Mirrors the issue-by-id
    pattern: useful when the caller already has the canonical id and
    avoids the (repo, number) cross-partition lookup."""
    cosmos: Cosmos = app.state.cosmos
    found = await pr_ops.read_pr(cosmos, project=project, pr_id=pr_id)
    if found is None:
        raise HTTPException(404, f"no glimmung PR {project}/{pr_id}")
    pr, _ = found
    return await _build_pr_detail(cosmos, pr=pr)


@app.post(
    "/v1/prs",
    response_model=PrDetail,
    dependencies=[Depends(require_admin_user)],
)
async def create_pr_endpoint(req: PrCreateRequest) -> PrDetail:
    """Register a glimmung PR. The agent's open-PR step (#50 slice 4)
    is the primary caller: it opens a thin GH PR (one-line link to the
    glimmung PR entity) and immediately registers the rich entity here.
    Idempotent on `(repo, number)` — re-registration of an existing
    PR returns the existing entity rather than minting a duplicate."""
    cosmos: Cosmos = app.state.cosmos
    project_doc = await _read_project(cosmos, req.project)
    if not project_doc:
        raise HTTPException(400, f"project {req.project!r} not registered")
    if not req.title.strip():
        raise HTTPException(400, "title required")
    if not req.branch.strip():
        raise HTTPException(400, "branch required")

    # Idempotent ensure semantics.
    pr, _etag, created = await pr_ops.ensure_pr_for_github(
        cosmos,
        project=req.project,
        repo=req.repo,
        number=req.number,
        title=req.title,
        branch=req.branch,
        body=req.body,
        base_ref=req.base_ref,
        head_sha=req.head_sha,
        html_url=req.html_url,
    )
    if not created and (
        req.linked_issue_id is not None or req.linked_run_id is not None
    ):
        # ensure_pr_for_github only honors create-time fields. Patch the
        # linkages on after the fact so callers don't have to round-trip
        # through PATCH for the common "found existing PR, attach
        # linkage" case.
        found = await pr_ops.read_pr(cosmos, project=req.project, pr_id=pr.id)
        assert found is not None
        pr, etag = found
        pr, _ = await pr_ops.update_pr(
            cosmos, pr=pr, etag=etag,
            linked_issue_id=req.linked_issue_id,
            linked_run_id=req.linked_run_id,
        )
    elif created and (req.linked_issue_id or req.linked_run_id):
        found = await pr_ops.read_pr(cosmos, project=req.project, pr_id=pr.id)
        assert found is not None
        pr, etag = found
        pr, _ = await pr_ops.update_pr(
            cosmos, pr=pr, etag=etag,
            linked_issue_id=req.linked_issue_id,
            linked_run_id=req.linked_run_id,
        )
    return await _build_pr_detail(cosmos, pr=pr)


@app.patch(
    "/v1/prs/by-id/{project}/{pr_id}",
    response_model=PrDetail,
    dependencies=[Depends(require_admin_user)],
)
async def patch_pr_endpoint(
    req: PrUpdateRequest,
    project: str = Path(...),
    pr_id: str = Path(...),
) -> PrDetail:
    """Patch PR fields + state transitions. State transitions go through
    `close_pr` / `merge_pr` / `reopen_pr` so the timestamp invariants
    (closed-vs-merged, merged_by) stay consistent at the call site."""
    cosmos: Cosmos = app.state.cosmos
    found = await pr_ops.read_pr(cosmos, project=project, pr_id=pr_id)
    if found is None:
        raise HTTPException(404, f"no glimmung PR {project}/{pr_id}")
    pr, etag = found

    if any(
        f is not None for f in (
            req.title, req.body, req.branch, req.base_ref,
            req.head_sha, req.html_url, req.linked_issue_id, req.linked_run_id,
        )
    ):
        pr, etag = await pr_ops.update_pr(
            cosmos, pr=pr, etag=etag,
            title=req.title,
            body=req.body,
            branch=req.branch,
            base_ref=req.base_ref,
            head_sha=req.head_sha,
            html_url=req.html_url,
            linked_issue_id=req.linked_issue_id,
            linked_run_id=req.linked_run_id,
        )

    if req.state is not None:
        target = req.state.lower()
        if target == "closed" and pr.state == PRState.OPEN:
            pr, etag = await pr_ops.close_pr(cosmos, pr=pr, etag=etag)
        elif target == "merged":
            if not req.merged_by:
                raise HTTPException(400, "state='merged' requires merged_by")
            pr, etag = await pr_ops.merge_pr(
                cosmos, pr=pr, etag=etag, merged_by=req.merged_by,
            )
        elif target == "open" and pr.state == PRState.CLOSED:
            if pr.merged_at is not None:
                raise HTTPException(409, "merged PR cannot be reopened")
            pr, etag = await pr_ops.reopen_pr(cosmos, pr=pr, etag=etag)
        elif target not in ("open", "closed", "merged"):
            raise HTTPException(400, f"state must be 'open' | 'closed' | 'merged', not {req.state!r}")

    return await _build_pr_detail(cosmos, pr=pr)


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

    @app.get("/{full_path:path}")
    async def serve_spa(full_path: str) -> FileResponse:
        return FileResponse(_static / "index.html")

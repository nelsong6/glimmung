"""Run-state lifecycle for the verify-loop substrate (#18).

Cosmos-backed CRUD for the `runs` container. Stored as snake_case JSON
(unlike the older containers which use camelCase) — new container, new
convention, less translation overhead because Run has deeply nested
models.

Concurrency model: each `workflow_run.completed` webhook for a tracked
run mutates the run document, so we use optimistic concurrency on
`_etag` for the attempt-append + state-transition paths. Conflicts are
rare in practice (one workflow_run per phase per attempt) but possible
under retried webhook delivery; the call-site retries up to 3 times
before giving up.
"""

import logging
from datetime import UTC, datetime
from typing import Any

from azure.core import MatchConditions
from azure.cosmos.exceptions import CosmosAccessConditionFailedError
from ulid import ULID

from glimmung.db import Cosmos, query_all
from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    Run,
    RunDecision,
    RunState,
    VerificationResult,
)

log = logging.getLogger(__name__)


_MAX_CONFLICT_RETRIES = 3


def _now() -> datetime:
    return datetime.now(UTC)


async def create_run(
    cosmos: Cosmos,
    *,
    project: str,
    workflow: str,
    issue_repo: str,
    issue_number: int,
    budget: BudgetConfig,
    initial_phase_name: str,
    initial_workflow_filename: str,
    issue_id: str = "",
    issue_lock_holder_id: str | None = None,
    trigger_source: dict[str, Any] | None = None,
) -> Run:
    """Create a Run record at initial dispatch time. Records the first
    PhaseAttempt with the caller-supplied phase name (matching a
    PhaseSpec.name in the workflow) and `dispatched_at = now`. The
    attempt is completed later via `record_completion` when the
    workflow_run.completed webhook arrives.

    `issue_lock_holder_id` is the holder of the per-issue serialization
    lock claimed by `dispatch_run`. The terminal-state handler in
    `app._handle_workflow_run` reads it and releases the lock when the
    Run reaches PASSED or ABORTED.

    `trigger_source` is a free-form record of why this run started — for
    W6 observability and audit. Not consumed by the decision engine."""
    now = _now()
    run = Run(
        id=str(ULID()),
        project=project,
        workflow=workflow,
        issue_id=issue_id,
        issue_repo=issue_repo,
        issue_number=issue_number,
        state=RunState.IN_PROGRESS,
        budget=budget,
        attempts=[
            PhaseAttempt(
                attempt_index=0,
                phase=initial_phase_name,
                workflow_filename=initial_workflow_filename,
                dispatched_at=now,
            )
        ],
        cumulative_cost_usd=0.0,
        issue_lock_holder_id=issue_lock_holder_id,
        trigger_source=trigger_source,
        created_at=now,
        updated_at=now,
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))
    log.info(
        "created run %s for %s/issues/%d (workflow=%s phase=%s budget=$%.2f trigger=%s)",
        run.id, issue_repo, issue_number, workflow,
        initial_phase_name, budget.total,
        (trigger_source or {}).get("kind", "unspecified"),
    )
    return run


async def get_active_run(
    cosmos: Cosmos,
    *,
    project: str,
    issue_number: int,
) -> tuple[Run, str] | None:
    """Look up the in-progress Run for an issue. Returns (run, etag) or
    None. The etag lets the caller do optimistic concurrency on the
    next replace_item without a re-read."""
    docs = await query_all(
        cosmos.runs,
        "SELECT * FROM c WHERE c.issue_number = @n AND c.state = @s",
        parameters=[
            {"name": "@n", "value": issue_number},
            {"name": "@s", "value": RunState.IN_PROGRESS.value},
        ],
    )
    project_docs = [d for d in docs if d.get("project") == project]
    if not project_docs:
        return None
    if len(project_docs) > 1:
        # Pick the most recently updated. This shouldn't happen — we only
        # create a Run if there isn't one already — but logging it loudly
        # here is better than silent surprise downstream.
        log.warning(
            "multiple in_progress runs for %s/%d: %s",
            project, issue_number, [d["id"] for d in project_docs],
        )
        project_docs.sort(key=lambda d: d.get("updated_at", ""), reverse=True)
    doc = project_docs[0]
    return Run.model_validate(_strip_meta(doc)), doc["_etag"]


async def get_latest_run(
    cosmos: Cosmos,
    *,
    project: str,
    issue_number: int,
) -> Run | None:
    """Most recent Run on an issue regardless of state. Used by the
    Issues view to show last-run status alongside each open issue."""
    docs = await query_all(
        cosmos.runs,
        "SELECT * FROM c WHERE c.project = @p AND c.issue_number = @n",
        parameters=[
            {"name": "@p", "value": project},
            {"name": "@n", "value": issue_number},
        ],
    )
    if not docs:
        return None
    docs.sort(key=lambda d: d.get("created_at", ""), reverse=True)
    return Run.model_validate(_strip_meta(docs[0]))


async def find_run_by_issue_id(
    cosmos: Cosmos,
    *,
    issue_id: str,
) -> Run | None:
    """Most-recent Run for a glimmung issue id, regardless of state.
    Cross-partition because the caller (PR `Closes #N` parser) only
    knows the issue, not the project. Used by `_handle_pull_request`
    after `find_issue_by_github_url` resolves a `Closes #N` to a
    glimmung Issue."""
    docs = await query_all(
        cosmos.runs,
        "SELECT * FROM c WHERE c.issue_id = @i",
        parameters=[{"name": "@i", "value": issue_id}],
    )
    if not docs:
        return None
    docs.sort(key=lambda d: d.get("created_at", ""), reverse=True)
    return Run.model_validate(_strip_meta(docs[0]))


async def find_run_by_pr(
    cosmos: Cosmos,
    *,
    issue_repo: str,
    pr_number: int,
) -> tuple[Run, str] | None:
    """Look up the Run linked to a PR. Cross-partition because the
    `pull_request` webhook handler doesn't know the project name —
    only the repo. Returns `(run, etag)`. Used by the triage signal
    drain to find the Run a PR-feedback signal targets."""
    docs = await query_all(
        cosmos.runs,
        "SELECT * FROM c WHERE c.issue_repo = @r AND c.pr_number = @p",
        parameters=[
            {"name": "@r", "value": issue_repo},
            {"name": "@p", "value": pr_number},
        ],
    )
    if not docs:
        return None
    if len(docs) > 1:
        log.warning(
            "multiple runs linked to PR %s#%d: %s",
            issue_repo, pr_number, [d["id"] for d in docs],
        )
        docs.sort(key=lambda d: d.get("created_at", ""), reverse=True)
    doc = docs[0]
    return Run.model_validate(_strip_meta(doc)), doc["_etag"]


async def link_pr_to_run(
    cosmos: Cosmos,
    *,
    run: Run,
    etag: str,
    pr_number: int,
    pr_branch: str,
) -> tuple[Run, str]:
    """Stamp `pr_number` + `pr_branch` on a Run. Called by the
    `pull_request.opened` webhook handler when the new PR's body
    references the issue (`Closes #N`)."""
    def apply(r: Run) -> Run:
        return r.model_copy(update={
            "pr_number": pr_number,
            "pr_branch": pr_branch,
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, run, etag, apply)


async def reopen_for_recycle(
    cosmos: Cosmos,
    *,
    run: Run,
    etag: str,
    phase_name: str,
    workflow_filename: str,
    pr_lock_holder_id: str,
    issue_lock_holder_id: str,
) -> tuple[Run, str]:
    """Re-open a PASSED Run via the PR primitive's recycle path: state
    PASSED → IN_PROGRESS, append a new PhaseAttempt at `phase_name` (the
    `lands_at` target), stamp both lock holders so the workflow_run.
    completed terminal handler can release both locks. Replaces the
    pre-#69 `reopen_for_triage`; the trigger is still PR feedback but
    the destination phase is now recycle-policy-driven."""
    def apply(r: Run) -> Run:
        next_idx = (r.attempts[-1].attempt_index + 1) if r.attempts else 0
        r.attempts.append(PhaseAttempt(
            attempt_index=next_idx,
            phase=phase_name,
            workflow_filename=workflow_filename,
            dispatched_at=_now(),
        ))
        return r.model_copy(update={
            "state": RunState.IN_PROGRESS,
            "pr_lock_holder_id": pr_lock_holder_id,
            "issue_lock_holder_id": issue_lock_holder_id,
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, run, etag, apply)


async def find_run_by_workflow_run(
    cosmos: Cosmos,
    *,
    project: str,
    workflow_run_id: int,
) -> tuple[Run, str] | None:
    """Look up the Run that owns a given GH Actions workflow_run_id.
    Used in the workflow_run.completed webhook path when the lease
    metadata doesn't pin the issue cleanly (e.g. retry dispatches whose
    inputs are set by glimmung itself, not by the user labeling an
    issue)."""
    docs = await query_all(
        cosmos.runs,
        "SELECT * FROM c WHERE c.project = @p",
        parameters=[{"name": "@p", "value": project}],
    )
    for d in docs:
        for attempt in d.get("attempts", []):
            if attempt.get("workflow_run_id") == workflow_run_id:
                return Run.model_validate(_strip_meta(d)), d["_etag"]
    return None


async def record_completion(
    cosmos: Cosmos,
    *,
    run: Run,
    etag: str,
    workflow_run_id: int,
    conclusion: str,
    verification: VerificationResult | None,
    artifact_url: str | None,
) -> tuple[Run, str]:
    """Record the workflow_run.completed payload on the latest attempt
    of a run. Updates cumulative_cost_usd from the verification
    artifact's cost (or 0 if missing). Optimistic-concurrency
    protected; on _etag mismatch, re-reads and retries up to
    _MAX_CONFLICT_RETRIES. Returns the updated (run, etag) so the
    caller can chain into the next lifecycle op without re-reading."""
    return await _retry_on_conflict(
        cosmos, run, etag,
        lambda r: _apply_completion(r, workflow_run_id, conclusion, verification, artifact_url),
    )


def _apply_completion(
    run: Run,
    workflow_run_id: int,
    conclusion: str,
    verification: VerificationResult | None,
    artifact_url: str | None,
) -> Run:
    if not run.attempts:
        raise RuntimeError(f"run {run.id} has no attempts to complete")
    last = run.attempts[-1]
    if last.completed_at is not None:
        # Already completed (replayed webhook). Skip.
        log.info(
            "run %s attempt %d already completed; treating as no-op",
            run.id, last.attempt_index,
        )
        return run
    last.workflow_run_id = workflow_run_id
    last.completed_at = _now()
    last.conclusion = conclusion
    last.verification = verification
    last.artifact_url = artifact_url
    # Phase-reported cost: prefer an explicit cost_usd on the attempt
    # (set by future native LLM phases that don't emit verification.json),
    # fall back to verification.cost_usd. Surface it on the attempt so the
    # rollup is auditable per attempt without re-parsing verification.
    cost = verification.cost_usd if verification else 0.0
    if last.cost_usd is None:
        last.cost_usd = cost
    return run.model_copy(update={
        "cumulative_cost_usd": run.cumulative_cost_usd + cost,
        "updated_at": _now(),
    })


async def record_decision(
    cosmos: Cosmos,
    *,
    run: Run,
    etag: str,
    decision: RunDecision,
) -> tuple[Run, str]:
    """Persist the decision the engine produced for the latest
    completed attempt. Separate call from record_completion so the
    decision-engine output is auditable in the run document."""
    def apply(r: Run) -> Run:
        if not r.attempts:
            return r
        r.attempts[-1].decision = decision.value
        return r.model_copy(update={"updated_at": _now()})
    return await _retry_on_conflict(cosmos, run, etag, apply)


async def append_attempt(
    cosmos: Cosmos,
    *,
    run: Run,
    etag: str,
    phase_name: str,
    workflow_filename: str,
) -> tuple[Run, str]:
    """Append a new PhaseAttempt to an in-progress run for a freshly-
    dispatched workflow. `phase_name` is the recycle policy's `lands_at`
    target (or 'self' resolved to the current phase's name). Caller must
    call this *before* firing the workflow_dispatch so run state reflects
    intent and duplicate dispatches can be detected on webhook redelivery
    races."""
    def apply(r: Run) -> Run:
        next_idx = (r.attempts[-1].attempt_index + 1) if r.attempts else 0
        r.attempts.append(PhaseAttempt(
            attempt_index=next_idx,
            phase=phase_name,
            workflow_filename=workflow_filename,
            dispatched_at=_now(),
        ))
        return r.model_copy(update={"updated_at": _now()})
    return await _retry_on_conflict(cosmos, run, etag, apply)


async def mark_passed(cosmos: Cosmos, *, run: Run, etag: str) -> tuple[Run, str]:
    def apply(r: Run) -> Run:
        return r.model_copy(update={"state": RunState.PASSED, "updated_at": _now()})
    return await _retry_on_conflict(cosmos, run, etag, apply)


async def mark_aborted(
    cosmos: Cosmos,
    *,
    run: Run,
    etag: str,
    reason: str,
) -> tuple[Run, str]:
    def apply(r: Run) -> Run:
        return r.model_copy(update={
            "state": RunState.ABORTED,
            "abort_reason": reason,
            "updated_at": _now(),
        })
    return await _retry_on_conflict(cosmos, run, etag, apply)


async def _retry_on_conflict(
    cosmos: Cosmos,
    run: Run,
    etag: str,
    apply: Any,
) -> tuple[Run, str]:
    """Apply `apply(run) -> run` with optimistic concurrency. On _etag
    mismatch, re-read the doc and retry. Returns (updated_run, new_etag)
    so callers can chain ops without an extra read."""
    current = run
    current_etag = etag
    for attempt in range(_MAX_CONFLICT_RETRIES):
        updated = apply(current)
        try:
            response = await cosmos.runs.replace_item(
                item=updated.id,
                body=updated.model_dump(mode="json"),
                etag=current_etag,
                match_condition=MatchConditions.IfNotModified,
            )
            return updated, response.get("_etag", current_etag)
        except CosmosAccessConditionFailedError:
            if attempt == _MAX_CONFLICT_RETRIES - 1:
                raise
            log.info("run %s replace_item conflict; re-reading and retrying", current.id)
            doc = await cosmos.runs.read_item(item=current.id, partition_key=current.project)
            current = Run.model_validate(_strip_meta(doc))
            current_etag = doc["_etag"]
    raise RuntimeError("unreachable")


def _strip_meta(doc: dict[str, Any]) -> dict[str, Any]:
    """Drop Cosmos-internal `_*` fields before pydantic validation."""
    return {k: v for k, v in doc.items() if not k.startswith("_")}

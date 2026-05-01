from datetime import datetime
from enum import Enum
from typing import Any

from pydantic import BaseModel, Field


class LeaseState(str, Enum):
    PENDING = "pending"
    ACTIVE = "active"
    RELEASED = "released"
    EXPIRED = "expired"


class Project(BaseModel):
    id: str
    name: str
    github_repo: str = ""
    metadata: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime


class ProjectRegister(BaseModel):
    name: str
    github_repo: str
    metadata: dict[str, Any] = Field(default_factory=dict)


class Workflow(BaseModel):
    id: str                     # workflow name (e.g. "issue-agent")
    project: str                # partition key
    name: str                   # = id; canonical handle is f"{project}.{name}"
    workflow_filename: str
    workflow_ref: str = "main"
    trigger_label: str = "issue-agent"
    default_requirements: dict[str, Any] = Field(default_factory=dict)
    # When set, glimmung treats this workflow as participating in the
    # verify-loop substrate (#18): a Run is created on initial dispatch,
    # workflow_run.completed parses the verification artifact, and the
    # decision engine may dispatch this filename as a retry. When unset,
    # glimmung's pre-#18 fire-and-forget behavior is preserved.
    retry_workflow_filename: str = ""
    # When set, glimmung dispatches this workflow when a PR-feedback
    # signal (#19) drains and the triage decision engine returns
    # DISPATCH_TRIAGE. The triage workflow re-enters impl + verify with
    # the human's feedback as additional context. Same artifact contract
    # as the retry workflow (uploads `verification.json`).
    triage_workflow_filename: str = ""
    default_budget: "BudgetConfig | None" = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime


class WorkflowRegister(BaseModel):
    project: str                # must reference an existing Project
    name: str
    workflow_filename: str
    workflow_ref: str = "main"
    trigger_label: str = "issue-agent"
    default_requirements: dict[str, Any] = Field(default_factory=dict)
    retry_workflow_filename: str = ""
    triage_workflow_filename: str = ""
    default_budget: "BudgetConfig | None" = None


class Host(BaseModel):
    id: str
    name: str
    capabilities: dict[str, Any] = Field(default_factory=dict)
    current_lease_id: str | None = None
    last_heartbeat: datetime | None = None
    last_used_at: datetime | None = None
    drained: bool = False
    created_at: datetime


class Lease(BaseModel):
    id: str
    project: str
    workflow: str | None = None         # workflow name; null on legacy / non-workflow leases
    host: str | None = None
    state: LeaseState = LeaseState.PENDING
    requirements: dict[str, Any] = Field(default_factory=dict)
    metadata: dict[str, Any] = Field(default_factory=dict)
    requested_at: datetime
    assigned_at: datetime | None = None
    released_at: datetime | None = None
    ttl_seconds: int = 900


class LeaseRequest(BaseModel):
    project: str
    workflow: str | None = None
    requirements: dict[str, Any] = Field(default_factory=dict)
    metadata: dict[str, Any] = Field(default_factory=dict)
    ttl_seconds: int | None = None


class LeaseResponse(BaseModel):
    lease: Lease
    host: Host | None = None


class StateSnapshot(BaseModel):
    hosts: list[Host]
    pending_leases: list[Lease]
    active_leases: list[Lease]
    projects: list[Project] = Field(default_factory=list)
    workflows: list[Workflow] = Field(default_factory=list)


# ─── Verify-loop substrate (#18) ─────────────────────────────────────────────
#
# A `Run` is the orchestrator's per-issue record. It is created when the
# issue webhook fires and a workflow with `retry_workflow_filename` set is
# matched, and it accumulates one `PhaseAttempt` per workflow_run.completed
# event for that issue. The decision engine reads the run + the latest
# attempt's verification result and produces a `RunDecision`. Persistence
# lives in the `runs` Cosmos container, partitioned by `/project`.
#
# Schema version is explicit so the eventual move (Cosmos → typed columns,
# or richer phase taxonomy than INITIAL/RETRY) can be a migration rather
# than a rewrite.


class VerificationStatus(str, Enum):
    PASS = "pass"
    FAIL = "fail"
    ERROR = "error"  # producer crashed before reaching a verdict


class VerificationResult(BaseModel):
    """Typed contract for `verification.json`, the artifact every consumer
    workflow uploads at the end of its verify phase. The decision engine
    reads this — never the workflow_run conclusion alone — because the
    producer's typed verdict carries strictly more information than the
    runner exit code.

    Producers are responsible for emitting this shape. A missing or
    schema-invalid artifact is itself a signal (decision engine returns
    ABORT_MALFORMED).
    """
    schema_version: int = 1
    status: VerificationStatus
    reasons: list[str] = Field(default_factory=list)
    evidence_refs: list[str] = Field(default_factory=list)
    cost_usd: float = 0.0
    prompt_version: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


class RunPhase(str, Enum):
    """The phase a particular attempt represents. Long-term this widens
    to a richer taxonomy (test_plan / impl / verify as separate phases);
    #18 ships INITIAL + RETRY, #19 adds TRIAGE (re-entry into impl + verify
    triggered by a PR-feedback signal)."""
    INITIAL = "initial"
    RETRY = "retry"
    TRIAGE = "triage"


class PhaseAttempt(BaseModel):
    attempt_index: int                         # 0-based; 0 == initial dispatch
    phase: RunPhase
    workflow_filename: str
    workflow_run_id: int | None = None         # GH Actions run id; populated on completion
    dispatched_at: datetime
    completed_at: datetime | None = None
    conclusion: str | None = None              # GH workflow_run.conclusion (success/failure/cancelled/...)
    verification: VerificationResult | None = None
    artifact_url: str | None = None            # prior_verification_artifact_url passed into the *next* attempt
    decision: str | None = None                # RunDecision applied after this attempt completed


class RunState(str, Enum):
    IN_PROGRESS = "in_progress"
    PASSED = "passed"
    ABORTED = "aborted"


class BudgetConfig(BaseModel):
    """Hard limits the decision engine enforces. Frozen at run-creation
    time from (issue label → workflow default → glimmung default) so
    relabeling mid-run can't move the goalposts."""
    max_attempts: int = 3
    max_cost_usd: float = 25.0


class RunDecision(str, Enum):
    """The full output universe of the decision engine. Pure: no state
    mutation, no I/O, fully unit-testable."""
    RETRY = "retry"                              # dispatch retry workflow
    ADVANCE = "advance"                          # verification passed; let the consumer's PR step run
    ABORT_BUDGET_ATTEMPTS = "abort_budget_attempts"
    ABORT_BUDGET_COST = "abort_budget_cost"
    ABORT_MALFORMED = "abort_malformed"          # missing or invalid verification artifact


class TriageDecision(str, Enum):
    """The output universe of the PR-triage decision engine. Pure
    function over `(signal, run_for_pr)`. Side effects (workflow
    dispatch, issue/PR comment, lock release) live at the call site."""
    DISPATCH_TRIAGE = "dispatch_triage"          # fire the triage workflow with feedback context
    IGNORE = "ignore"                            # signal not actionable (e.g. an "approved" review)
    ABORT_NO_RUN = "abort_no_run"                # signal targets a PR with no agent-tracked Run
    ABORT_BUDGET_ATTEMPTS = "abort_budget_attempts"
    ABORT_BUDGET_COST = "abort_budget_cost"


class Run(BaseModel):
    schema_version: int = 1
    id: str                                      # ULID
    project: str                                 # partition key
    workflow: str                                # workflow name (e.g. "issue-agent")
    issue_repo: str                              # "<owner>/<repo>" — for GH API calls
    issue_number: int
    state: RunState = RunState.IN_PROGRESS
    budget: BudgetConfig = Field(default_factory=BudgetConfig)
    attempts: list[PhaseAttempt] = Field(default_factory=list)
    cumulative_cost_usd: float = 0.0
    abort_reason: str | None = None
    # Set when dispatch_run claimed an issue-scope Lock for serialization.
    # On terminal transition (PASSED / ABORTED), the workflow_run.completed
    # handler releases the lock with this holder id. Optional because pre-#20
    # runs predate the issue-lock primitive.
    issue_lock_holder_id: str | None = None
    # Where the dispatch came from. Free-form so future trigger sources
    # (scheduled re-runs, CLI, Slack, signal-drain) can plug in without a
    # schema change. Recorded for W6 observability; not consumed by the
    # decision engine.
    trigger_source: dict[str, Any] | None = None
    # PR linkage (#19): set when the agent's PR-opening step lands. Auto-
    # populated by the `pull_request.opened` webhook handler when the new
    # PR's body references the issue (`Closes #N` / `Fixes #N`). The PR
    # triage signal drain queries by `pr_number` to find the right Run.
    pr_number: int | None = None
    pr_branch: str | None = None
    pr_lock_holder_id: str | None = None     # set while a triage workflow is in flight
    created_at: datetime
    updated_at: datetime


# Resolve the `BudgetConfig` forward reference on Workflow + WorkflowRegister
# (BudgetConfig is defined further down in this module so the verify-loop
# substrate can stay grouped under one banner without splitting it across
# the file).
Workflow.model_rebuild()
WorkflowRegister.model_rebuild()


# ─── Lock primitive (W1 substrate) ───────────────────────────────────────────
#
# A generic mutual-exclusion primitive. Used by #19's per-PR triage
# serialization, by #20's per-issue dispatch serialization, by future
# signal-drain locks, and so on. Single primitive, one Cosmos container,
# one sweep loop.
#
# Doc id is deterministic: f"{scope}::{quote(key)}" — Cosmos enforces
# uniqueness on id+partition_key, so concurrent claims race through
# create_item conflicts rather than through application logic. Partition
# key is `/scope` so per-scope diagnostic queries stay single-partition.
#
# State machine:
#   HELD  ── release_lock by holder ──> RELEASED  ── another claim ──> HELD
#   HELD  ── expires_at < now ────────> EXPIRED   ── another claim ──> HELD
#                                       (sweep marks; take-over doesn't wait)


class LockState(str, Enum):
    HELD = "held"
    RELEASED = "released"
    EXPIRED = "expired"


class Lock(BaseModel):
    id: str                              # f"{scope}::{quote(key)}"
    scope: str                           # partition key; "pr" | "issue" | …
    key: str                             # caller-supplied; e.g. "<repo>#<num>"
    state: LockState
    held_by: str                         # caller-supplied holder id (signal_id, run_id, …)
    claimed_at: datetime
    expires_at: datetime
    last_heartbeat_at: datetime | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


# ─── Signal bus (#19) ────────────────────────────────────────────────────────
#
# A `Signal` is a unit of work for the orchestrator's decision engine.
# Webhooks (GH PR review, GH issue/PR comment), the glimmung UI (reject
# button), and future automations (scheduled re-runs, signal-drain
# replays) all enqueue Signals. A background drain loop walks pending
# signals oldest-first, claims the per-target Lock from the lock
# primitive (pr-scope for PR signals, issue-scope for issue signals,
# etc.), invokes the decision engine for that signal type, applies the
# decision (dispatch, abort, comment), marks the signal processed,
# and releases the lock.
#
# Per-PR serialization (the "queue cleanly" property from #19's DoD #6)
# is a free side-effect: while a PR's lock is held by an in-flight
# triage dispatch, the drain skips subsequent signals on that PR; they
# stay PENDING and re-evaluate next drain tick. Strict FIFO within a
# (target_type, target_repo, target_id) keyspace.


class SignalSource(str, Enum):
    GH_REVIEW = "gh_review"                  # pull_request_review event
    GH_REVIEW_COMMENT = "gh_review_comment"  # pull_request_review_comment event
    GH_COMMENT = "gh_comment"                # issue_comment on PR or issue
    GLIMMUNG_UI = "glimmung_ui"              # UI action (reject, force-retry, etc.)
    SCHEDULED = "scheduled"                  # internal timer
    SYSTEM = "system"                        # internal (e.g. health-check fanout)


class SignalState(str, Enum):
    PENDING = "pending"
    PROCESSING = "processing"  # held by drain loop with the per-target lock
    PROCESSED = "processed"
    FAILED = "failed"          # drain raised; manual inspection needed


class SignalTargetType(str, Enum):
    PR = "pr"
    ISSUE = "issue"
    RUN = "run"


class Signal(BaseModel):
    schema_version: int = 1
    id: str                                  # ULID
    target_type: SignalTargetType
    target_repo: str                         # partition key (`<owner>/<repo>`)
    target_id: str                           # str(pr_number) | str(issue_number) | run_id
    source: SignalSource
    payload: dict[str, Any] = Field(default_factory=dict)
    state: SignalState = SignalState.PENDING
    enqueued_at: datetime
    processed_at: datetime | None = None
    processed_decision: str | None = None    # decision engine output, e.g. DISPATCH_TRIAGE
    failure_reason: str | None = None        # set if state=FAILED


class SignalEnqueueRequest(BaseModel):
    """Body of POST /v1/signals — the UI reject button posts this."""
    target_type: SignalTargetType
    target_repo: str
    target_id: str
    source: SignalSource = SignalSource.GLIMMUNG_UI
    payload: dict[str, Any] = Field(default_factory=dict)

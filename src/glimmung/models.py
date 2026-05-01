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
    #18 ships INITIAL + RETRY only since the consumer-side workflow stays
    monolithic for now."""
    INITIAL = "initial"
    RETRY = "retry"


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

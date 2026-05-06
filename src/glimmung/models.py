import re
from datetime import datetime
from enum import Enum
from typing import Any

from pydantic import BaseModel, Field, model_validator


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


# ─── Pipeline schema (#69 — multi-phase, v1 single-phase) ──────────────────
#
# A workflow is a pipeline: an ordered list of `PhaseSpec`s plus a glimmung-
# owned terminal `PrPrimitiveSpec`. v1 enforces exactly one phase at
# registration time; the schema is shaped for N>1 so multi-phase orchestration
# is a runtime addition rather than a schema migration.

# Recognized recycle triggers, kept as plain strings (not enums) so adding
# new ones doesn't churn the registration API. Two namespaces, validated
# per phase kind.
VERIFY_PHASE_TRIGGERS: frozenset[str] = frozenset({"verify_fail", "verify_malformed"})
PR_PRIMITIVE_TRIGGERS: frozenset[str] = frozenset({"pr_review_changes_requested"})
PHASE_KINDS: frozenset[str] = frozenset({"gha_dispatch", "k8s_job"})


# Cross-phase input ref expression: `${{ phases.<phase_name>.outputs.<key> }}`.
# Whitespace is permissive inside the `${{ }}` to match GHA-style ergonomics.
# Phase names and output keys are restricted to a conservative identifier
# alphabet (alnum, underscore, hyphen) so the reference syntax stays
# unambiguous and we don't have to think about quoting.
_PHASE_REF_RE = re.compile(
    r"^\s*\$\{\{\s*phases\.(?P<phase>[A-Za-z0-9_-]+)\.outputs\.(?P<key>[A-Za-z0-9_-]+)\s*\}\}\s*$"
)


def parse_phase_input_ref(ref: str) -> tuple[str, str] | None:
    """Parse a `${{ phases.<name>.outputs.<key> }}` expression.

    Returns `(phase_name, output_key)` or None if the string isn't a
    well-formed phase ref. Free function so tests and the runtime
    substitution path share one parser."""
    m = _PHASE_REF_RE.match(ref)
    if m is None:
        return None
    return m.group("phase"), m.group("key")


def substitute_phase_inputs(
    phase: "PhaseSpec",
    prior_outputs: dict[str, dict[str, str]],
) -> dict[str, str]:
    """Resolve a phase's `inputs` against captured outputs of prior phases.
    `prior_outputs` is keyed by phase name, value is the phase's
    `phase_outputs` dict (validated at /completed callback time).

    Returns a flat dict of `input_name -> value` ready to splat into a
    workflow_dispatch payload. Refs are assumed to have been validated
    at registration time (see `validate_phase_input_refs`); reaching
    this function with an unresolvable ref means runtime state has
    drifted from the registered schema, which is a bug — raise instead
    of silently substituting empty strings.
    """
    resolved: dict[str, str] = {}
    for input_name, ref in phase.inputs.items():
        parsed = parse_phase_input_ref(ref)
        if parsed is None:
            raise ValueError(
                f"phase {phase.name!r} input {input_name!r} ref {ref!r} is "
                "malformed (registration validation should have caught this)"
            )
        ref_phase, ref_key = parsed
        if ref_phase not in prior_outputs:
            raise KeyError(
                f"phase {phase.name!r} input {input_name!r} refs phase "
                f"{ref_phase!r} which has no captured outputs on this run"
            )
        if ref_key not in prior_outputs[ref_phase]:
            raise KeyError(
                f"phase {phase.name!r} input {input_name!r} refs "
                f"{ref_phase}.outputs.{ref_key!r}; phase posted outputs "
                f"{sorted(prior_outputs[ref_phase])}"
            )
        resolved[input_name] = prior_outputs[ref_phase][ref_key]
    return resolved


def validate_phase_input_refs(phases: list["PhaseSpec"]) -> None:
    """Validate every phase's `inputs` map against earlier phases' declared
    `outputs`. Raises ValueError on the first problem.

    Rules:
    - Each input value must be a syntactically valid `${{ phases.NAME.outputs.KEY }}`.
    - The referenced phase must appear *strictly earlier* in the order
      (no self-refs, no forward refs).
    - The referenced output `KEY` must be declared in that earlier phase's
      `outputs` list."""
    declared_outputs: dict[str, frozenset[str]] = {}
    for phase in phases:
        for input_name, ref in phase.inputs.items():
            parsed = parse_phase_input_ref(ref)
            if parsed is None:
                raise ValueError(
                    f"phase {phase.name!r} input {input_name!r}={ref!r} is not a "
                    "valid phase ref (expected `${{ phases.NAME.outputs.KEY }}`)"
                )
            ref_phase, ref_key = parsed
            if ref_phase == phase.name:
                raise ValueError(
                    f"phase {phase.name!r} input {input_name!r} refs itself; "
                    "self-refs are not allowed"
                )
            if ref_phase not in declared_outputs:
                # Either the referenced phase doesn't exist, or it's later
                # in the order. Both are forward refs from this phase's POV.
                raise ValueError(
                    f"phase {phase.name!r} input {input_name!r} refs phase "
                    f"{ref_phase!r} which doesn't appear earlier in the workflow"
                )
            if ref_key not in declared_outputs[ref_phase]:
                raise ValueError(
                    f"phase {phase.name!r} input {input_name!r} refs "
                    f"{ref_phase!r}.outputs.{ref_key!r} but {ref_phase!r} "
                    f"doesn't declare that output (declared: "
                    f"{sorted(declared_outputs[ref_phase])})"
                )
        declared_outputs[phase.name] = frozenset(phase.outputs)


class RecyclePolicy(BaseModel):
    """Where re-dispatch lands when a recycle trigger fires, and how many
    times. `lands_at = "self"` is same-phase retry (today's RETRY); a phase
    name is rewind-and-replay-forward (future). On a verify phase, `on`
    accepts {verify_fail, verify_malformed}; on the PR primitive,
    {pr_review_changes_requested}."""
    max_attempts: int = 3
    on: list[str] = Field(default_factory=list)
    lands_at: str = "self"


class NativeStepSpec(BaseModel):
    """App-owned observational step boundary inside a native Kubernetes Job."""
    slug: str
    title: str | None = None


class NativeJobSpec(BaseModel):
    """One app-owned native runner Job inside a phase.

    v1 executes jobs sequentially inside the phase. The command/image/env
    belong to the app's workflow registration; Glimmung supplies universal
    run context and callback mechanics around it.
    """
    id: str
    name: str | None = None
    image: str
    command: list[str] = Field(default_factory=list)
    args: list[str] = Field(default_factory=list)
    env: dict[str, str] = Field(default_factory=dict)
    steps: list[NativeStepSpec] = Field(default_factory=list)
    timeout_seconds: int | None = None


class PhaseSpec(BaseModel):
    """One phase in a workflow's pipeline.

    `gha_dispatch` phases dispatch a GitHub Actions workflow, preserved for
    legacy/exception flows. `k8s_job` phases run app-owned native Kubernetes
    Jobs and expose first-class job/step observations.

    Verify is opt-in: when true, the phase emits `verification.json` and the
    decision engine routes through `recycle_policy`. When false, any
    non-`success` runner conclusion ends the run (job semantics) and
    `recycle_policy` is invalid.

    `inputs` and `outputs` plumb data between phases (multi-phase runtime,
    glimmung#101). `outputs` is the list of named values this phase emits
    via the `completed` callback's `outputs` payload — string-only in v1.
    `inputs` maps an input name (becomes a `workflow_dispatch.inputs` key)
    to a ref expression of the form `${{ phases.<phase_name>.outputs.<key> }}`
    pointing at an earlier phase's declared output. Cross-phase ref
    validation runs at registration time."""
    name: str
    kind: str = "gha_dispatch"
    workflow_filename: str = ""
    workflow_ref: str = "main"
    inputs: dict[str, str] = Field(default_factory=dict)
    outputs: list[str] = Field(default_factory=list)
    requirements: dict[str, Any] | None = None
    verify: bool = False
    recycle_policy: RecyclePolicy | None = None
    jobs: list[NativeJobSpec] = Field(default_factory=list)


class PrPrimitiveSpec(BaseModel):
    """The glimmung-owned terminal PR-creation step. Always present in the
    run lineage (skipped state reserved for future). Carries its own
    `recycle_policy` for PR-feedback re-entry — `lands_at` points back at a
    user phase, replacing today's `triage_workflow_filename` flow.

    `enabled` is a v1-rollout knob: when True, glimmung calls `gh pr create`
    after the last user phase succeeds. When False (default), the consumer
    workflow is still expected to open the PR itself. Each consumer flips
    this to True as part of its YAML migration. Once all consumers are on
    glimmung-opens-PR, the flag goes away."""
    enabled: bool = False
    recycle_policy: RecyclePolicy | None = None


class Workflow(BaseModel):
    id: str                     # workflow name (e.g. "issue-agent")
    project: str                # partition key
    name: str                   # = id; canonical handle is f"{project}.{name}"
    phases: list[PhaseSpec] = Field(default_factory=list)
    pr: PrPrimitiveSpec = Field(default_factory=PrPrimitiveSpec)
    budget: "BudgetConfig" = Field(default_factory=lambda: BudgetConfig())
    trigger_label: str = "issue-agent"
    default_requirements: dict[str, Any] = Field(default_factory=dict)
    metadata: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime


class WorkflowRegister(BaseModel):
    project: str                # must reference an existing Project
    name: str
    phases: list[PhaseSpec]
    pr: PrPrimitiveSpec = Field(default_factory=PrPrimitiveSpec)
    budget: "BudgetConfig" = Field(default_factory=lambda: BudgetConfig())
    trigger_label: str = "issue-agent"
    default_requirements: dict[str, Any] = Field(default_factory=dict)

    @model_validator(mode="after")
    def _validate_v1(self) -> "WorkflowRegister":
        # Ordering matters: per-phase validation (kind, recycle), name
        # uniqueness, and ref validation all run BEFORE the single-phase
        # enforcement so 2-phase fixtures exercise those validators in
        # tests. The single-phase check stays last as a v1 runtime gate
        # (relaxed when the multi-phase runtime lands; see glimmung#101).
        if not self.phases:
            raise ValueError("workflow must declare at least one phase")
        names = [p.name for p in self.phases]
        if len(set(names)) != len(names):
            raise ValueError(f"phase names must be unique within a workflow; got {names}")
        for p in self.phases:
            if p.kind not in PHASE_KINDS:
                raise ValueError(
                    f"phase {p.name!r} kind={p.kind!r} not supported in v1 "
                    f"(valid: {sorted(PHASE_KINDS)})"
                )
            if p.kind == "gha_dispatch":
                if not p.workflow_filename:
                    raise ValueError(
                        f"phase {p.name!r} kind='gha_dispatch' requires workflow_filename"
                    )
                if p.jobs:
                    raise ValueError(
                        f"phase {p.name!r} kind='gha_dispatch' cannot declare native jobs"
                    )
            if p.kind == "k8s_job":
                if not p.jobs:
                    raise ValueError(
                        f"phase {p.name!r} kind='k8s_job' must declare at least one job"
                    )
                job_ids = [j.id for j in p.jobs]
                if len(set(job_ids)) != len(job_ids):
                    raise ValueError(
                        f"phase {p.name!r} job ids must be unique; got {job_ids}"
                    )
                for job in p.jobs:
                    step_slugs = [s.slug for s in job.steps]
                    if not step_slugs:
                        raise ValueError(
                            f"phase {p.name!r} job {job.id!r} must declare at least one step"
                        )
                    if len(set(step_slugs)) != len(step_slugs):
                        raise ValueError(
                            f"phase {p.name!r} job {job.id!r} step slugs must be unique; "
                            f"got {step_slugs}"
                        )
            if p.recycle_policy is not None:
                if not p.verify:
                    raise ValueError(
                        f"phase {p.name!r} has recycle_policy but verify=False; "
                        "recycle is only valid on verify phases"
                    )
                if p.recycle_policy.lands_at != "self" and p.recycle_policy.lands_at not in names:
                    raise ValueError(
                        f"phase {p.name!r} recycle_policy.lands_at="
                        f"{p.recycle_policy.lands_at!r} doesn't match any phase name"
                    )
                bad = [t for t in p.recycle_policy.on if t not in VERIFY_PHASE_TRIGGERS]
                if bad:
                    raise ValueError(
                        f"phase {p.name!r} recycle_policy.on contains unknown triggers: {bad}; "
                        f"valid: {sorted(VERIFY_PHASE_TRIGGERS)}"
                    )
        validate_phase_input_refs(self.phases)
        if self.pr.recycle_policy is not None:
            la = self.pr.recycle_policy.lands_at
            if la == "self":
                raise ValueError("PR primitive recycle_policy.lands_at='self' is meaningless")
            if la not in names:
                raise ValueError(
                    f"PR primitive recycle_policy.lands_at={la!r} doesn't match any phase name"
                )
            bad = [t for t in self.pr.recycle_policy.on if t not in PR_PRIMITIVE_TRIGGERS]
            if bad:
                raise ValueError(
                    f"PR primitive recycle_policy.on contains unknown triggers: {bad}; "
                    f"valid: {sorted(PR_PRIMITIVE_TRIGGERS)}"
                )
        return self


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


class PlaybookEntryState(str, Enum):
    PENDING = "pending"
    CREATED = "created"
    RUNNING = "running"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    SKIPPED = "skipped"


class PlaybookState(str, Enum):
    DRAFT = "draft"
    READY = "ready"
    RUNNING = "running"
    PAUSED = "paused"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    CANCELLED = "cancelled"


class PlaybookIssueSpec(BaseModel):
    title: str
    body: str = ""
    labels: list[str] = Field(default_factory=list)
    workflow: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


class PlaybookEntry(BaseModel):
    id: str
    title: str | None = None
    issue: PlaybookIssueSpec
    depends_on: list[str] = Field(default_factory=list)
    manual_gate: bool = False
    state: PlaybookEntryState = PlaybookEntryState.PENDING
    created_issue_id: str | None = None
    run_id: str | None = None
    completed_at: datetime | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


class Playbook(BaseModel):
    schema_version: int = 1
    id: str
    project: str
    title: str
    description: str = ""
    entries: list[PlaybookEntry] = Field(default_factory=list)
    concurrency_limit: int | None = None
    state: PlaybookState = PlaybookState.DRAFT
    metadata: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime
    updated_at: datetime


class PlaybookCreate(BaseModel):
    project: str
    title: str
    description: str = ""
    entries: list[PlaybookEntry] = Field(default_factory=list)
    concurrency_limit: int | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


# ─── Verify-loop substrate (#18) ───────────────────────────────────────
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


class NativeStepState(str, Enum):
    PENDING = "pending"
    ACTIVE = "active"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    SKIPPED = "skipped"


class NativeStepAttempt(BaseModel):
    slug: str
    title: str | None = None
    state: NativeStepState = NativeStepState.PENDING
    started_at: datetime | None = None
    completed_at: datetime | None = None
    exit_code: int | None = None
    message: str | None = None


class NativeJobAttempt(BaseModel):
    job_id: str
    name: str | None = None
    state: NativeStepState = NativeStepState.PENDING
    steps: list[NativeStepAttempt] = Field(default_factory=list)
    started_at: datetime | None = None
    completed_at: datetime | None = None
    last_seq: int = 0


class NativeRunEventType(str, Enum):
    STEP_STARTED = "step_started"
    LOG = "log"
    STEP_COMPLETED = "step_completed"
    STEP_SKIPPED = "step_skipped"
    STEP_FAILED = "step_failed"


def native_job_attempts_from_specs(jobs: list[NativeJobSpec]) -> list[NativeJobAttempt]:
    return [
        NativeJobAttempt(
            job_id=job.id,
            name=job.name,
            steps=[
                NativeStepAttempt(slug=step.slug, title=step.title)
                for step in job.steps
            ],
        )
        for job in jobs
    ]


class PhaseAttempt(BaseModel):
    """One dispatch of a phase (or the PR primitive). `phase` is the phase
    name from the workflow's `phases` list, or a glimmung-reserved name for
    the PR primitive. `cost_usd` is decoupled from `verification` so non-
    verify LLM phases can report cost too; verify phases can leave it null
    and the rollup falls back to `verification.cost_usd`."""
    attempt_index: int                         # 0-based; 0 == initial dispatch
    phase: str                                 # phase name from PhaseSpec.name
    phase_kind: str = "gha_dispatch"
    workflow_filename: str
    workflow_run_id: int | None = None         # GH Actions run id; populated on completion
    dispatched_at: datetime
    completed_at: datetime | None = None
    conclusion: str | None = None              # GH workflow_run.conclusion (success/failure/cancelled/...)
    verification: VerificationResult | None = None
    cost_usd: float | None = None              # phase-reported; fallback to verification.cost_usd if null
    artifact_url: str | None = None            # prior_verification_artifact_url passed into the *next* attempt
    decision: str | None = None                # RunDecision applied after this attempt completed
    # Phase outputs (#101) — values this phase emitted via the `completed`
    # callback's `outputs` payload. Keys match the phase's declared
    # `PhaseSpec.outputs`; mismatches are rejected at callback time. The
    # multi-phase runtime (PR 3 of #101) substitutes these into the next
    # phase's `workflow_dispatch.inputs` per its declared `inputs` refs.
    phase_outputs: dict[str, str] | None = None
    # Native k8s_job observations. Jobs/steps are initialized from the
    # workflow registration and then updated by native runner callbacks.
    jobs: list[NativeJobAttempt] = Field(default_factory=list)
    log_archive_url: str | None = None
    capability_token_sha256: str | None = None
    cancel_requested_at: datetime | None = None
    cancel_reason: str | None = None
    # Resume primitive (#111) — set when this attempt is a skip-mark
    # synthesized during run resumption, not a real dispatch. The phase
    # didn't actually execute; `phase_outputs` were carried forward from
    # the named prior Run's same-named phase attempt. `workflow_run_id`
    # stays None and `conclusion` is "success" so downstream multi-phase
    # substitution (`_collect_phase_outputs`) sees a completed-looking
    # attempt and feeds the prior outputs into the next phase's dispatch.
    skipped_from_run_id: str | None = None


class RunState(str, Enum):
    IN_PROGRESS = "in_progress"
    PASSED = "passed"
    REVIEW_REQUIRED = "review_required"
    ABORTED = "aborted"


class BudgetConfig(BaseModel):
    """Run-cumulative cost cap, frozen at run-creation time so relabeling
    mid-run can't move the goalposts. Per-phase attempt counts live on the
    phase's `recycle_policy.max_attempts` (formerly part of this config)."""
    total: float = 25.0


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
    # Canonical glimmung-issue handle (#28 consumer-PR-1). Optional
    # during transition: pre-#28-consumer Runs predate the issues
    # container and have an empty string here. New Runs always set it.
    # The eventual cleanup PR drops `issue_repo` + `issue_number` and
    # forces this to be required; callers reach for GH coords through
    # the linked Issue's metadata.
    issue_id: str = ""
    issue_repo: str                              # "<owner>/<repo>" — for GH API calls
    issue_number: int
    state: RunState = RunState.IN_PROGRESS
    budget: BudgetConfig = Field(default_factory=BudgetConfig)
    attempts: list[PhaseAttempt] = Field(default_factory=list)
    cumulative_cost_usd: float = 0.0
    abort_reason: str | None = None
    # Set when dispatch_run claimed an issue-scope Lock for serialization.
    # On terminal transition (PASSED / REVIEW_REQUIRED / ABORTED), the
    # workflow_run.completed handler releases the lock with this holder id.
    # Optional because pre-#20 runs predate the issue-lock primitive.
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
    # Live preview env URL the workflow stood up (#88). Stamped by the
    # `started` callback so the PR composer can surface the env + the
    # /_styleguide URL alongside the diff. None for non-frontend
    # workflows that don't expose a public env.
    validation_url: str | None = None
    # Attended-pickup intent parsed from Issue labels at dispatch time.
    # "warm" means the dashboard should surface a ready session-launch link
    # once PR/run context exists; "cold" means launch remains explicit.
    session_launch_intent: str = "cold"
    # Markdown block of inline screenshot embeds rendered by the
    # workflow's upload-to-blob step (#87 + #88). Stamped by the
    # `completed` callback. The PR composer drops this verbatim into
    # the PR body — failures are surfaced in the markdown itself by
    # the workflow, so we don't need a separate failure list here.
    screenshots_markdown: str | None = None
    # Resume primitive (#111) — set when this Run was created via
    # `dispatch_resumed_run`. Points at the prior Run whose captured
    # phase outputs got carried forward into this Run's skipped
    # attempts. Visualization uses this to render the Run-lineage tree
    # (parent-child across resume-spawned Runs); the decision engine
    # ignores it.
    cloned_from_run_id: str | None = None
    # Resume primitive (#111) — the phase this Run started executing
    # at, set when the Run is a resumed clone. None for fresh dispatches
    # (which always start at phases[0]). The visualization layer uses
    # this to highlight which entrypoint arrow lit up on this Run.
    entrypoint_phase: str | None = None
    created_at: datetime
    updated_at: datetime


# Resolve the `BudgetConfig` forward reference on Workflow + WorkflowRegister
# (BudgetConfig is defined further down in this module so the verify-loop
# substrate can stay grouped under one banner without splitting it across
# the file).
Workflow.model_rebuild()
WorkflowRegister.model_rebuild()


# ─── Lock primitive (W1 substrate) ─────────────────────────────────────────
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


# ─── Signal bus (#19) ───────────────────────────────────────────────────────
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


# ─── Glimmung-native issues (#28) ─────────────────────────────────────────
#
# A glimmung Issue is a first-class control-plane object: title, body,
# labels, lifecycle. Stored in the `issues` Cosmos container, partitioned
# by `/project`. GitHub is one of N possible syndication targets; an Issue
# may carry `metadata.github_issue_url` to link out, but it exists and is
# dispatchable whether or not a GH counterpart exists.
#
# Future trigger sources (Slack message → glimmung issue, scheduled
# re-run, glimmung-internal CLI) drop in cleanly under this model — they
# create a glimmung Issue with a different `metadata.source` and the
# downstream dispatch path doesn't change.


class IssueState(str, Enum):
    OPEN = "open"
    CLOSED = "closed"


class IssueSource(str, Enum):
    """Where the Issue came from. Surfaced for observability + future
    routing (e.g. a Slack-sourced issue might get a different default
    workflow). Not consumed by the dispatch path itself."""
    MANUAL = "manual"
    GITHUB_WEBHOOK_IMPORT = "github_webhook_import"
    SLACK = "slack"
    SCHEDULED = "scheduled"


class IssueMetadata(BaseModel):
    source: IssueSource = IssueSource.MANUAL
    workflow: str | None = None
    ui_validation_requested: bool = False
    # GH-issue link-out. `github_issue_url` is the canonical handle for
    # `find_issue_by_github_url`; `github_issue_repo` and
    # `github_issue_number` are denormalized so dispatch / completion /
    # comment-posting paths can read GH coords without parsing a URL on
    # every call. All three move together: an Issue minted from a GH
    # webhook or dispatch shim has all three set; one minted from
    # Slack/CLI/scheduled has none of them.
    github_issue_url: str | None = None
    github_issue_repo: str | None = None
    github_issue_number: int | None = None


class IssueComment(BaseModel):
    """One glimmung-authored comment on an Issue. There is no GitHub id:
    post-#50 the glimmung Issue is canonical and GitHub is not the source
    for this thread."""
    id: str
    author: str
    body: str
    created_at: datetime
    updated_at: datetime


class Issue(BaseModel):
    schema_version: int = 1
    id: str                                  # ULID; canonical glimmung-issue-id
    number: int                              # Glimmung-native issue number, scoped to project
    project: str                             # partition key
    title: str
    body: str = ""
    labels: list[str] = Field(default_factory=list)
    state: IssueState = IssueState.OPEN
    metadata: IssueMetadata = Field(default_factory=IssueMetadata)
    comments: list[IssueComment] = Field(default_factory=list)
    created_at: datetime
    updated_at: datetime
    closed_at: datetime | None = None


# ─── Design portfolio review elements (#225) ────────────────────────────────


class PortfolioReviewState(str, Enum):
    UNREVIEWED = "unreviewed"
    NEEDS_REVIEW = "needs_review"
    APPROVED = "approved"
    NEEDS_WORK = "needs_work"


class PortfolioElement(BaseModel):
    id: str
    project: str
    route: str
    element_id: str
    title: str = ""
    screenshot_url: str | None = None
    preview_url: str | None = None
    status: PortfolioReviewState = PortfolioReviewState.UNREVIEWED
    notes: str = ""
    last_touched_run_id: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime
    updated_at: datetime


# ─── Glimmung Reports ──────────────────────────────────────────────────────
#
# Report is the canonical Glimmung review object. GitHub pull requests are
# one syndication target, so their repo/number/branch metadata stays on the
# record when present, but Report is the object the UI/API/MCP surfaces own.


class ReportState(str, Enum):
    READY = "ready"
    NEEDS_REVIEW = "needs_review"
    FAILED = "failed"
    CLOSED = "closed"
    MERGED = "merged"


class ReportReviewState(str, Enum):
    """GH review verdicts. `DISMISSED` is the post-state when an author or
    maintainer dismisses an earlier review; we record it as a separate review
    entry rather than mutating the original so the audit trail stays append-
    only."""
    APPROVED = "approved"
    CHANGES_REQUESTED = "changes_requested"
    COMMENTED = "commented"
    DISMISSED = "dismissed"


class ReportComment(BaseModel):
    """One mirrored GitHub PR comment or Glimmung-native report comment."""
    id: str                                  # ULID; glimmung-side id
    gh_id: int | None = None                 # GH comment id; mirror dedupe key
    author: str                              # GH login
    body: str
    created_at: datetime
    updated_at: datetime | None = None
    html_url: str | None = None


class ReportReview(BaseModel):
    """One mirrored GitHub PR review submission."""
    id: str                                  # ULID
    gh_id: int | None = None                 # GH review id; mirror dedupe key
    author: str
    state: ReportReviewState
    body: str = ""
    submitted_at: datetime
    html_url: str | None = None


class Report(BaseModel):
    schema_version: int = 1
    id: str                                  # canonical glimmung Report id
    project: str                             # partition key
    repo: str                                # GitHub "<owner>/<repo>" when syndicated
    number: int                              # GitHub PR number when syndicated
    title: str
    body: str = ""
    state: ReportState = ReportState.READY
    branch: str                              # GitHub head ref when syndicated
    base_ref: str = "main"                   # base ref
    head_sha: str = ""                       # latest head commit sha; updated on `pull_request.synchronize`
    html_url: str = ""
    comments: list[ReportComment] = Field(default_factory=list)
    reviews: list[ReportReview] = Field(default_factory=list)
    linked_issue_id: str | None = None       # glimmung Issue.id (ULID)
    linked_run_id: str | None = None         # glimmung Run.id (ULID)
    created_at: datetime
    updated_at: datetime
    merged_at: datetime | None = None
    merged_by: str | None = None


class ReportVersion(BaseModel):
    schema_version: int = 1
    id: str                                  # "<report_id>.<version>"
    project: str                             # partition key
    report_id: str
    version: int = 0
    state: ReportState = ReportState.READY
    title: str
    body: str = ""
    linked_run_id: str | None = None
    github_repo: str | None = None
    github_pr_number: int | None = None
    github_html_url: str | None = None
    created_at: datetime

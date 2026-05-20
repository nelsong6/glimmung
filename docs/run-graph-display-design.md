# Run Graph Display Design

## Context

Glimmung owns the run display. Other workflow systems are useful references,
but the graph shown to the user must follow Glimmung's product model:

```text
Issue
  -> Run[]                  # user/reviewer intent history
    -> Cycle[]              # flat issue-scoped execution ledger
      -> Phase
        -> Job
          -> Step
    -> Evidence
  -> Touchpoint
```

The executor behind a phase is a native Kubernetes job. The user-facing graph
is Glimmung's explanation of agent work, review evidence, and next decisions.

## Terminology

Use **phase** consistently. A phase is the user-facing pipeline boundary
and the backend workflow boundary. Do not introduce a second UI term for
the same concept.

Suggested mapping:

```text
Issue          -> Issue / origin
Run            -> User/reviewer intent
Cycle          -> One durable execution record / ledger row
Workflow phase -> Phase
Native job -> Job
Native step -> Step
Attempt        -> Display counter or executor-level retry attribute only
Report         -> Touchpoint / evidence, depending on context
```

The API can expose either raw names or display labels, but the UI should
prefer phase/job/step language unless it is showing raw backend metadata.

The key distinction is **Phase vs Attempt**:

- A **Run** is the user/reviewer intent. A user pressing Run starts a new
  run. Reviewer feedback or a touchpoint requesting more work starts a new
  run.
- A **Cycle** is the durable unit of execution, scheduling, logs, cost,
  lifecycle, abort, and reporting. The issue history's leftmost number is a
  flat cycle ledger (`1`, `2`, `3`, ...). Each cycle also has a run number
  and a run-local cycle ordinal, displayed compactly as `1.1`, `1.2`, `2.1`.
  Recycle policy creates a new cycle under the same run.
- A **Phase** is a workflow column. Phases flow left-to-right according
  to the workflow's `depends_on` graph.
- A **Job** is a runnable box inside a phase. Multiple jobs in one phase
  stack vertically and run in parallel.
- An **Attempt** is a lower-level executor fact. It can explain executor retries
  and historical execution, but it should not replace phase or
  job as the main UI container.

This lets Glimmung show a flat issue cycle history while preserving the
pipeline shape inside each run: issue -> run -> cycle -> phase -> job -> step.

**Touchpoint** replaces the old user-facing `Report` concept. A
Touchpoint is the issue-level live summary and navigation page: what the
human needs to inspect, approve, reject, or discuss now. Touchpoints are
strictly one-to-one with Issues; repeated Runs, cycles, and PR
updates revise the same Touchpoint rather than creating additional
Touchpoints. A GitHub PR, validation URL, screenshots, generated design
portfolio rows, logs, or artifacts can be linked from the Touchpoint as
current evidence, but anything that must be retained per Run belongs in the
Run and RunReport UI.

The Touchpoint must not feel like an instance detail page. It is the live
frontend for interacting with the Issue: inspect the current evidence, make
the current decision, request changes, rerun, or enter attended work. It may
have an audit/debug history later, but that history describes updates to the
same Touchpoint; it is not a collection of Touchpoint instances.

The fuller object-boundary and Playbook integration vocabulary lives in
[Touchpoints, RunReports, And Playbook Integration](touchpoints-runreports-playbooks.md).

## Why Not Argo First

Starting with Argo Workflows would make the implementation lighter in
some areas, but it would push Glimmung toward Argo's model for DAGs,
retry behavior, artifacts, and run identity.

Glimmung needs to explain things Argo does not own:

- Issues and issue locks.
- Leases and scarce agent capacity.
- Phase attempts and recycle paths.
- Run cycle lineage.
- Validation environments.
- Screenshots and UI review evidence.
- PR/evidence/touchpoint state.
- Budgets and cost.
- Human approval, request-changes, and attended-mode decisions.
- Playbook integration strategy such as shared feature branches.

Argo can still become an executor later. If that happens, it should be a
phase kind beneath the Glimmung graph, not the source of truth for the
graph itself.

## Screen Structure

The current issue detail tab layout does not give the run enough space.
The run graph and step logs need to become a primary work surface, not a
secondary tab panel.

Preferred direction:

- Move away from the current multi-tab issue detail layout.
- Use a breadcrumb trail for navigation back to the issue, project, or
run list.
- Let the run view take most of the screen.
- Keep issue prose/context available, but do not force the run graph into
the same narrow tab frame as prose.

Example breadcrumb:

```text
Issues / glimmung / Add design portfolio / Run 01KQ...
```

The run page should support deep links to:

- the current run overview,
- a phase,
- a job,
- a specific step log,
- the related Touchpoint.

## Run Overview

The overview should show the high-level execution shape:

```text
env-prep -> agent-execute -> verify -> touchpoint
Cycle 1 (run 1.1): env-prep -> agent-execute -> touchpoint
Cycle 2 (run 1.2): env-prep -> agent-execute -> verify -> touchpoint
```

For more complex runs, the overview should show phase ordering and
branching/recycle relationships. The graph should answer:

- What ran?
- What is running now?
- What was skipped?
- What failed?
- What was retried or recycled?
- What produced evidence?
- What can the user inspect or decide next?

The overview should read as a graph, not as a strip of unrelated cards.
Nodes can keep Glimmung's chamfered block vocabulary, but the boxes
themselves should stay sparse. The graph should use hierarchy and
position to explain structure:

- phases own columns or lanes,
- phase names sit at the column/header tier,
- jobs are boxes inside the phase column,
- parallel jobs stack inside the same phase,
- directional connectors show flow between phases/jobs,
- recycle/request-changes paths use secondary/dashed connectors.

Parallel work must be represented as multiple jobs inside one phase column,
not as multiple sibling phase columns. The workflow uses `phase.jobs[]` for
this: jobs in the same phase run in parallel, and the phase completes only
after all sibling jobs have completed.

Clicking a node should pin an inspector panel that explains what is in
the box. Hover previews can be added later, but click selection is the
more durable interaction for debugging, sharing, and review. The graph
should not depend on hover-only state for important information.

## Phase Detail

A phase is the first meaningful drill-down boundary.

Phase detail should show:

- phase name,
- executor kind,
- state,
- retry count when relevant,
- duration,
- cost when known,
- input/output values,
- validation URL or artifacts produced by the phase,
- child jobs.

The normal product workflow starts cycles at the workflow entry phase.
Skipped phase markers are not part of the default run model.

In the overview, phase information should usually appear as a lane or
column heading rather than as a full graph box. The full phase detail can
appear in the inspector when the user selects the phase heading or a
phase-level summary affordance.

## Job and Step Detail

Step visibility should follow the proven pattern from CI systems and
Azure DevOps:

```text
+----------------------+-------------------------------------------+
| Step list            | Terminal/log output                       |
|                      |                                           |
| ✓ checkout           | $ npm run build                           |
| ✓ install            | ...                                       |
| ▶ build              | error TS...                               |
| · screenshot         |                                           |
+----------------------+-------------------------------------------+
```

The left side is a compact list of steps for the selected job. The right
side is a terminal-style log surface for the selected step.

This solves the step visibility problem without forcing every step into
the global graph. Phases and jobs are graph nodes; steps are usually
shown in the selected job's detail pane. A future view may promote steps
into graph nodes for very small jobs or debugging, but that should not be
the default run overview.

Step list requirements:

- stable ordering,
- current step highlighted,
- status icon or status pill,
- duration when known,
- retry/skipped markers,
- keyboard-friendly selection later.

Log surface requirements:

- terminal-like monospace output,
- preserve line breaks,
- support large output through truncation or lazy loading,
- show missing logs as an explicit empty state,
- link to raw external logs when the executor owns them.

## Layout Proportion

The run page should dedicate most of the viewport to execution and logs.
A reasonable default layout:

```text
Top:    breadcrumb + run title + state/action strip
Middle: phase graph / timeline
Bottom: selected phase/job/step inspector
```

For native job execution, the inspector should be large enough that the
terminal log is useful without requiring immediate full-screen expansion.
On desktop, the step list plus log viewer should be the dominant lower
surface. On mobile, the step list can stack above the log.

## Touchpoint vs Run Graph

The run graph is explanatory. It tells the user what happened and why.

The Touchpoint is decision-oriented. It tells the user what to inspect
or decide now, and acts as navigation to the relevant Run surfaces.

Do not overload the run graph with every approval workflow. The graph
should link to evidence and decision surfaces, while the Touchpoint
should aggregate the things that need human attention:

- validation URL,
- screenshots,
- changed files or PR,
- generated artifacts,
- design portfolio rows marked `Needs review`,
- run summary and recommendation,
- approve/request changes/rerun actions.

Touchpoints are one-to-one with issues. They should not need their own
top-level tab or an issue-local collection route. The issue workspace should
surface the current Touchpoint alongside issue context and the run/phase
graph. Historical or per-Run evidence should live in Run and RunReport views,
with the Touchpoint linking to the relevant current Run instead of carrying
its own history. The Touchpoint UI should not enumerate Runs, retries, or
past PRs as its primary content; those belong in the Runs tab and RunReport
surfaces.

## Design Portfolio Implications

The design portfolio should include run-display specimens before the
large refactor lands. This gives the UI vocabulary a place to stabilize
without requiring live data.

Add portfolio rows for at least:

- simple two-phase run,
- native job with step list and selected terminal log,
- failed step with log output,
- phase recycle/retry,
- cycle run created by recycle/retry,
- waiting/no-capacity state,
- aborted/cancelled state,
- Touchpoint evidence checklist.

These rows should use fixture data and passive review state. Marking a
row `Needs review` does not trigger agent work. It only creates a queue
for a later explicit instruction such as "look at the elements that need
review."

## Projection Contract

The canonical issue graph endpoint embeds `RunGraphProjection` under
`projection` on `GET /v1/issues/by-number/{project}/{issue_number}/graph`.
The execution work surface uses the run-scoped projection endpoint:

`GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/cycles/{cycle_number}/graph`

The projection is separate from storage and separate from any executor's
native model. The execution UI renders from this projection, not from generic
graph `nodes` / `edges` metadata.

Each projected Run carries a `topology` object derived from the workflow
schema referenced by that run or cycle. Execution fields such as phase, job,
step state, logs, evidence, and cost are then painted onto that topology.
Recycle/request-changes arrows belong to topology, not to execution status,
so the run execution surface does not need a separate metadata fallback to
discover them.

It is fixture-friendly and can be rendered without standing up a real run.

Projection concepts:

```text
RunGraphProjection
  issue_ref
  runs[]
  edges[]
  current_run_ref
  default_focus
  next_action
  touchpoints[]
  signals[]

Run
  run_ref
  workflow
  workflow_schema_ref
  state
  current_phase
  validation_url
  cost_usd
  attempts_count
  topology
  phases[]
  evidence[]

Topology
  phases[]
  default_entry
  recycle_arrows[]
  terminal

TopologyPhase
  name
  kind
  verify
  always
  evidence_verification_gate
  depends_on[]
  jobs[]

PhaseNode
  name
  state
  reason
  kind
  verify
  always
  depends_on[]
  jobs[]
  attempts[]

JobNode
  id
  name
  state
  reason
  conclusion
  completed_at
  steps[]

StepNode
  slug
  title
  state
  reason
  exit_code

RecycleArrow
  source
  target
  trigger
  max_attempts
  active
  kind

Signal
  target_type
  target_repo
  target_id
  source
  state
  kind
  feedback
```

The graph state vocabulary is closed: `not_started`, `skipped`, `dispatching`,
`active`, `succeeded`, and `failed`. `pending` and vague terminal `completed`
are not graph states. `reason` is required only for `failed`; otherwise it is
null/absent. External native values are translated at ingestion/projection
boundaries into this vocabulary, while raw Kubernetes or runner details belong
in inspector/debug fields.

Run cycles persist this vocabulary as phase/job/step execution records.
Projection reads those records first so an unreported native job can still be
clicked, inspected, and shown as `dispatching` or `failed` without relying on a
best-effort log scrape. `dispatch_timeout` is a normal failed reason: it turns
the affected phase/job red and skips later unstarted phases.

Native job completions are job-scoped in the run attempt record so the
projection can show one sibling job as complete while the phase waits for the
remaining jobs.

GitHub Check Runs are intentionally not part of this contract. Glimmung
keeps run state canonical in the issue workspace and syndicates PR URLs as
Touchpoint evidence; adding GitHub Check Runs should be a separate product
issue if GitHub-native status boxes become necessary.

## Refactor Sequence

1. Define the projection contract and fixture cases.
2. Add portfolio specimens for the run graph and job/step inspector.
3. Update the issue/run frontend toward breadcrumb-based navigation.
4. Build the live run graph against the projection.
5. Move current tab-only issue detail behavior behind retired-route tombstones
   or redirects.
6. Add Touchpoint surfaces once the graph and evidence model are stable.

This order keeps the refactor visible and reviewable while avoiding a
large backend/UI rewrite with no stable design target.

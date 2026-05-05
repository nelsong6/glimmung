# Run Graph Display Design

## Context

Glimmung owns the run display. Argo Workflows, GitHub Actions, Azure
DevOps, and other workflow systems are useful references, but the graph
shown to the user must follow Glimmung's product model:

```text
Issue
  -> Run
    -> Cycle
      -> Stage
        -> Job
          -> Step
      -> Evidence
  -> Touchpoint
```

The executor behind a stage may be GitHub Actions, a native Kubernetes
job, a future Argo Workflow, or something else. That executor is an
implementation detail. The user-facing graph is Glimmung's explanation
of agent work, review evidence, and next decisions.

## Terminology

Use **stage** in the UI. The backend may continue to use `phase` where
that is already established, but stage is the more common user-facing
term and maps better to how people read pipelines.

Suggested mapping:

```text
Issue          -> Issue / origin
Run            -> Continuous lifecycle from one trigger
Cycle          -> One pass through the graph inside a Run
Workflow phase -> Stage
Native/GHA job -> Job
Native/GHA step -> Step
Attempt        -> Executor-level attempt, not the main UI container
Report         -> Touchpoint / evidence, depending on context
```

The API can expose either raw names or display labels, but the UI should
prefer stage/job/step language unless it is showing raw backend metadata.

The key distinction is **Run vs Cycle**:

- A **Run** starts from one continuous trigger or origin event and ends
  when the issue is accepted, abandoned, or otherwise closed out.
- A **Cycle** is one traversal through the run graph. Requesting changes,
  recycling a stage, or resuming from a later point may create another
  cycle under the same run.
- A **Stage attempt** remains a lower-level executor fact. Do not use
  attempt as the user-facing name for the whole graph pass.

This lets Glimmung show "the run" as the complete story while still
showing each pass through the graph as a distinct cycle.

**Touchpoint** replaces the old user-facing `Report` concept. A
Touchpoint is the issue-level decision surface: what the human needs to
inspect, approve, reject, or discuss. A GitHub PR, validation URL,
screenshots, generated design portfolio rows, logs, or artifacts are
evidence inside the Touchpoint; they are not separate primary navigation
surfaces.

## Why Not Argo First

Starting with Argo Workflows would make the implementation lighter in
some areas, but it would push Glimmung toward Argo's model for DAGs,
retry behavior, artifacts, and run identity.

Glimmung needs to explain things Argo does not own:

- Issues and issue locks.
- Leases and scarce agent capacity.
- Stage attempts and resume/recycle paths.
- Validation environments.
- Screenshots and UI review evidence.
- PR/evidence/touchpoint state.
- Budgets and cost.
- Human approval, request-changes, and attended-mode decisions.
- Playbook integration strategy such as shared feature branches.

Argo can still become an executor later. If that happens, it should be a
stage kind beneath the Glimmung graph, not the source of truth for the
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
- a cycle,
- a stage,
- a job,
- a specific step log,
- the related Touchpoint.

## Run Overview

The overview should show the high-level execution shape:

```text
Cycle 1: env-prep -> agent-execute -> touchpoint
Cycle 2: agent-execute -> verify -> touchpoint
```

For more complex runs, the overview should show stage ordering and
branching/recycle/resume relationships. The graph should answer:

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

- stages own columns or lanes,
- stage names sit at the column/header tier,
- jobs are boxes inside the stage column,
- parallel jobs stack inside the same stage,
- directional connectors show flow between stages/jobs,
- recycle/request-changes paths use secondary/dashed connectors.

Clicking a node should pin an inspector panel that explains what is in
the box. Hover previews can be added later, but click selection is the
more durable interaction for debugging, sharing, and review. The graph
should not depend on hover-only state for important information.

## Stage Detail

A stage is the first meaningful drill-down boundary.

Stage detail should show:

- stage name,
- executor kind,
- state,
- attempt count,
- duration,
- cost when known,
- input/output values,
- validation URL or artifacts produced by the stage,
- child jobs.

Stages may be skipped during resume. Skipped stages should remain
visible because they explain why a resumed run started later in the
workflow.

In the overview, stage information should usually appear as a lane or
column heading rather than as a full graph box. The full stage detail can
appear in the inspector when the user selects the stage heading or a
stage-level summary affordance.

## Job and Step Detail

Step visibility should follow the proven pattern from GitHub Actions and
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
the global graph. Stages and jobs are graph nodes; steps are usually
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
Middle: stage graph / timeline
Bottom: selected stage/job/step inspector
```

For native job execution, the inspector should be large enough that the
terminal log is useful without requiring immediate full-screen expansion.
On desktop, the step list plus log viewer should be the dominant lower
surface. On mobile, the step list can stack above the log.

## Touchpoint vs Run Graph

The run graph is explanatory. It tells the user what happened and why.

The Touchpoint is decision-oriented. It tells the user what to inspect
or decide now.

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

Touchpoints are one-to-one with issues in the primary UI. They should
not need their own top-level tab. The issue workspace should surface the
current Touchpoint alongside issue context and the run/cycle graph. A
historical list of touchpoint evidence may exist as part of issue
history, but it should not become a separate "Reports" area.

## Design Portfolio Implications

The design portfolio should include run-display specimens before the
large refactor lands. This gives the UI vocabulary a place to stabilize
without requiring live data.

Add portfolio rows for at least:

- simple two-stage run,
- native job with step list and selected terminal log,
- failed step with log output,
- resumed run with skipped earlier stages,
- stage recycle/retry,
- pending/no-host state,
- aborted/cancelled state,
- Touchpoint evidence checklist.

These rows should use fixture data and passive review state. Marking a
row `Needs review` does not trigger agent work. It only creates a queue
for a later explicit instruction such as "look at the elements that need
review."

## Projection Contract

The backend should expose or derive a UI-friendly graph projection. The
projection is separate from storage and separate from any executor's
native model.

It should be possible to build fixtures for the projection without
standing up a real run.

Minimum projection concepts:

```text
RunGraphProjection
  run
  stages[]
  edges[]
  selected/default focus hints
  evidence links
  available actions

StageNode
  id
  label
  state
  kind
  attempts[]
  jobs[]
  inputs
  outputs

JobNode
  id
  label
  state
  steps[]

StepNode
  id
  label
  state
  duration
  log_ref
  raw_external_url
```

The projection should normalize GitHub Actions phases, native Kubernetes
jobs, PR evidence, touchpoint state, and future executor kinds into one
display vocabulary.

## Refactor Sequence

1. Define the projection contract and fixture cases.
2. Add portfolio specimens for the run graph and job/step inspector.
3. Update the issue/run frontend toward breadcrumb-based navigation.
4. Build the live run graph against the projection.
5. Move current tab-only issue detail behavior behind compatibility
   routes or redirects.
6. Add Touchpoint surfaces once the graph and evidence model are stable.

This order keeps the refactor visible and reviewable while avoiding a
large backend/UI rewrite with no stable design target.

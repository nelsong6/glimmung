# Run Graph Display Design

## Context

Glimmung owns the run display. Argo Workflows, GitHub Actions, Azure
DevOps, and other workflow systems are useful references, but the graph
shown to the user must follow Glimmung's product model:

```text
Issue
  -> Run[]                  # flat issue-scoped history
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
Run            -> One durable execution record
Cycle          -> A run whose origin is recycle/resume/request-changes
Workflow phase -> Stage
Native/GHA job -> Job
Native/GHA step -> Step
Attempt        -> Display counter or executor-level retry attribute only
Report         -> Touchpoint / evidence, depending on context
```

The API can expose either raw names or display labels, but the UI should
prefer stage/job/step language unless it is showing raw backend metadata.

The key distinction is **Run vs Cycle**:

- A **Run** is the durable unit of execution, scheduling, logs, cost,
  lifecycle, abort, and reporting.
- A **Cycle** is not a nested object. It is a run whose origin links it to
  an earlier run, usually because a recycle policy, resume action, or
  request-changes signal re-entered the workflow.
- An **attempt** is not a Glimmung entity. If useful, it is a field on a
  run or phase execution, such as an issue-scoped display number (`1.2`)
  or an executor retry counter.

This lets Glimmung show a flat issue run history while still grouping
related runs into a visible cycle lineage.

**Touchpoint** replaces the old user-facing `Report` concept. A
Touchpoint is the issue-level live summary and navigation page: what the
human needs to inspect, approve, reject, or discuss now. Touchpoints are
strictly one-to-one with Issues; repeated Runs, retries, resumes, and PR
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
- Run cycle lineage and resume/recycle paths.
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
- a stage,
- a job,
- a specific step log,
- the related Touchpoint.

## Run Overview

The overview should show the high-level execution shape:

```text
Run 1:   env-prep -> agent-execute -> touchpoint
Run 1.1: agent-execute -> verify -> touchpoint
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
- retry count when relevant,
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
surface the current Touchpoint alongside issue context and the run lineage
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

- simple two-stage run,
- native job with step list and selected terminal log,
- failed step with log output,
- resumed run with skipped earlier stages,
- cycle run created by recycle/retry,
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
  retry_count
  cycle_number
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

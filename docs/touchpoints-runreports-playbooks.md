# Touchpoints, RunReports, And Playbook Integration

This document fixes the object boundaries for review surfaces and execution
telemetry. The current code still has compatibility names such as `Report`,
but new design and API work should use these boundaries.

## RunReport

A RunReport is the factual audit lens for exactly one Run.

It answers: what happened in this execution?

V1 cardinality:

```text
Run -> RunReport
```

Each RunReport is strictly scoped to one Run. Do not introduce a RunReport
grouping object for v1. Cross-run totals can be derived by Touchpoint or
Playbook views when needed.

A RunReport may eventually include:

- wall time and phase durations
- total cost and per-phase or per-step cost
- verification result
- screenshots, artifacts, and validation URL
- logs or native step summaries
- decision outcome
- abort or failure reason

RunReport can be persisted or materialized from Run attempts, native events,
and artifacts later. The invariant is the one-Run scope.

## Touchpoint

A Touchpoint is the user-facing decision surface for one Issue.

It answers: what should the user inspect or decide now?

V1 cardinality:

```text
Issue -> Touchpoint
```

The Touchpoint is not the historical log. It updates as new Runs happen.
History lives under Runs and RunReports.

Potential generic actions:

- approve, merge, or submit
- request changes
- enter attended mode
- rerun or request a second unattended pass

V1 actions should stay generic rather than workflow-defined. Request changes
should requeue work, not merely annotate the Touchpoint.

## UI Responsibility

The Touchpoint should behave like a compact checklist/dashboard for the current
Issue decision. It may show:

- validation URL
- screenshots
- changed files or PR-equivalent link
- generated artifacts
- portfolio elements or UI checks when available
- run summary and recommendation

The Issue tab remains the primary prose/context surface. Epic and Playbook
context belongs mostly there. The Touchpoint should focus on exact evidence and
the decision in front of the user.

## Compatibility Rename

Conceptual rename:

```text
Current Report -> Touchpoint
Current ReportVersion -> TouchpointVersion, if versions remain useful
New RunReport -> per-Run factual execution report
```

Existing API, frontend, storage, and MCP callers still use some `Report` names.
Keep compatibility aliases during staged migration and avoid changing storage
names only for terminology cleanup.

## Playbooks And Touchpoints

A Playbook is an execution sequencer, not primarily a human approval workflow.

Two useful execution modes:

```text
automatic sequence
  run entry 1
  if successful, run entry 2
  if successful, run entry 3
  stop on failure, explicit gate, budget, or conflict

bulk queue
  enqueue all eligible entries
  dependency and concurrency rules control execution
```

Automatic Playbook entries should not create full normal Touchpoints by
default. Telemetry still exists through Runs and RunReports, but review
surfaces should not pile up when no human is expected to review each entry.

Automatic entries may have a minimal connective page when useful:

- "This entry is part of an automatic Playbook execution."
- link to the overall Playbook
- link to the current Run or next execution

Create a full Touchpoint at human decision boundaries, such as final review of
a shared feature branch, failure triage, or attended intervention.

## Issue Dependencies

Dependency/readiness is separate from Playbook membership.

A future issue dependency primitive can let bulk queues and Playbooks share the
same readiness rule:

```text
IssueDependency
  issue_id
  depends_on_issue_id
  condition: succeeded | touchpoint_approved | merged | closed
```

Dependencies answer:

```text
Can this issue start yet?
```

They should not decide branch or environment behavior.

## Integration Strategy

Execution sequencing is separate from integration policy.

Playbook answers:

```text
What work runs, in what order, with what dependencies?
```

Integration policy answers:

```text
Where does each entry land?
```

V1 vocabulary:

- `isolated_prs`
- `shared_feature_branch`
- `rolling_main`

`isolated_prs`: each issue gets its own branch, Touchpoint, and merge.
Dependencies only control order. Use for unrelated or loosely related work.

`shared_feature_branch`: all automatic Playbook entries build on one branch or
work context. A final Touchpoint reviews the whole feature. Use for one large
feature split into smaller agent tasks.

`rolling_main`: each successful entry merges to `main` before the next entry
starts. Use for bootstrap, app, and infra flows where later work depends on
real integrated resources, Argo health, Tofu apply, or cloud-provider state.
This strategy must be explicit and planner-selected.

## Work Context

Branch and environment handoff should come from the Playbook integration
strategy, not from individual issue dependencies by default.

Potential future object:

```text
WorkContext
  branch
  base_ref
  owner_issue_id or playbook_id
  current_run_id
  state: available | in_use | finalized
```

For automatic Playbooks, leaving a branch up is not necessarily an error. It
may be the artifact passed to the next entry. The existing Lock primitive likely
applies to marking a branch or work context as in use by a running issue.

## Design Principles

- Do not make one agent session do a whole feature.
- Break large work into discoverable, queueable segments.
- Preserve branch and main guardrails while supporting fast iteration.
- Keep RunReport one-to-one with Run for v1.
- Keep Touchpoint one-to-one with Issue for v1.
- Put cross-run summaries in the Touchpoint or dashboard as derived data.
- Make dangerous integration behavior like rolling merges explicit.

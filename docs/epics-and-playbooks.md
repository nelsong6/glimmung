# Epics and Playbooks

Glimmung's execution queue is already concrete: an Issue dispatches into a
Run, a Run produces a Report, and feedback can re-enter the loop. Epics and
Playbooks sit one level earlier. They are the pre-queue planning layer that
keeps a large body of work from becoming one oversized agent task.

## Mental Model

```text
Epic -> Playbook -> ordered Entries -> Issue -> Run -> Report/evidence -> next Entry
```

An Epic is durable context. A Playbook is an executable plan.

Use them together when a feature needs sequencing, dependency tracking, or
handoff context before individual issues are dispatched.

## Epic

An Epic is the feature/story object. It answers why the work exists and what
shape success has.

An Epic should carry:

- goal and product context
- constraints and non-goals
- success criteria
- decisions that should survive across entries
- agent-facing background that every entry needs

An Epic is not the execution queue. It should not own dispatch state, lease
state, run attempts, or report transitions. Those belong below it.

For now, Epic is a documented boundary rather than a persisted model in the
codebase. Until the model exists, keep Epic context in the Playbook
description or in linked issue documentation.

## Playbook

A Playbook is the executable ordered plan for an Epic. It decides what can run,
when it can run, and which small issue should be minted or dispatched next.

The current Playbook substrate already owns:

- `title` and `description`
- `entries`
- `depends_on`
- `manual_gate`
- `concurrency_limit`
- `state`
- `run_playbook` advancement, which creates issues and dispatches ready entries

Each entry should be sized so one agent can complete it independently. If an
entry needs the whole Epic body to make progress, it is probably too large or
too vague.

When a Playbook entry starts, the agent should receive:

- the entry-specific issue title and body
- the relevant Epic context
- explicit acceptance criteria
- selected prior entry outputs, reports, or evidence when needed

## Relationship

Use a 1:1 Epic-to-Playbook relationship initially.

The hierarchy is intentionally shallow:

```text
Epic
  Playbook
    Entry
    Entry
    Entry
```

Avoid `Epic -> many Playbooks` until there is a real need. Many playbooks under
one Epic introduce cross-playbook ordering, rollup, cancellation, pause, and UI
semantics before the usage pattern has proved itself. If that need appears
later, multiple Playbooks can reference the same `epic_id`.

For the first implementation, prefer either:

- `Epic.playbook_id`, if Epic owns the relationship, or
- `Playbook.epic_id`, if Playbook lands first and links back to a future Epic

Do not duplicate Epic narrative into every Playbook entry. The context assembly
path should attach the relevant Epic sections when an entry is dispatched.

## State Boundaries

Epic state should be sparse and human-readable. It can summarize feature-level
completion, but it should not reimplement lower-level state machines.

Playbook state is operational:

- `draft`: stored plan, not ready to advance
- `ready`: no entry is running, at least one entry may become runnable
- `running`: one or more entries are in progress
- `paused`: a manual gate blocks otherwise-ready work
- `succeeded`: all entries succeeded or were skipped
- `failed`: an entry failed and blocks the plan
- `cancelled`: operator stopped the plan

Entry state follows execution:

- `pending`: not yet started
- `created`: issue exists, dispatch not yet running
- `running`: entry has an active or pending Run
- `succeeded`: linked Run passed
- `failed`: linked Run aborted or dispatch failed
- `skipped`: intentionally bypassed

Issue, Run, Report, Signal, lock, and lease state remain the source of truth
for execution details. Playbook state is a rollup, not a replacement.

Touchpoint, RunReport, issue dependency, and integration-strategy boundaries
are defined in
[Touchpoints, RunReports, And Playbook Integration](touchpoints-runreports-playbooks.md).
Use that vocabulary when deciding whether a Playbook entry should create its
own review surface, share a feature branch, or roll through `main`.

## Ordering And Dependencies

Dependency edges decide readiness. Stable ordering decides presentation and
tie-breaking.

Playbooks should keep entry order independent of `depends_on` edges. That lets
operators express "do these in this order when otherwise unconstrained" without
inventing artificial dependencies.

Recommended readiness rule:

```text
entry is runnable when:
  state is pending or created
  manual_gate is false
  every depends_on entry has succeeded or was skipped
  concurrency_limit has capacity
```

## Follow-Up Surface

Likely implementation follow-ups:

- persist an `Epic` model or add an `epic_id`/`playbook_id` link
- support Playbook entries that link existing issues, not only issue specs
- add entry-level acceptance criteria and evidence expectations
- define context assembly rules for Epic + entry + prior outputs
- roll Report/evidence outcomes back into Playbook and Epic summaries
- expose Epic/Playbook detail in the dashboard and MCP tools

The key constraint is that the value add is pre-queue grouping and sequencing.
Epics and Playbooks should make large work handoffable by splitting it into
small dispatchable entries; they should not replace the execution queue that
already exists below Issues and Runs.

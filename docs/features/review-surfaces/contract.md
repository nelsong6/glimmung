# Review Surfaces Contract

This contract applies to Touchpoints, RunReports, PR syndication, signals,
Playbooks, evidence links, screenshots, and reviewer decision state.

## Product Model

Glimmung exists to make agent work reviewable. Review surfaces should answer
two different questions without mixing them: what should the human decide now,
and what happened in a specific run. Touchpoints own the current decision;
RunReports own factual per-run audit state.

## Sources Of Truth

- Cosmos `issues` owns the work item state.
- Cosmos `runs` and native events own per-run facts.
- Cosmos `reports` physically stores Touchpoint state.
- Cosmos `playbooks` owns ordered multi-issue planning and execution state.
- Cosmos `signals` owns reviewer feedback and re-entry requests.
- GitHub PRs are syndication/review targets, not the canonical Glimmung issue
  or run record.
- `docs/touchpoints-runreports-playbooks.md` owns object boundaries and
  integration strategy vocabulary.

## Migration Rules

- Do not add multiple Touchpoints per Issue.
- Do not store per-run historical facts primarily on the Touchpoint.
- Do not reintroduce Report-named routes, UI controls, aliases, or tests for
  migrated Touchpoint concepts.
- Do not model GitHub PR merge/reject events as hidden side effects when the
  signal bus is the canonical feedback input.
- Do not create full human review Touchpoints for automatic Playbook entries
  unless that entry is a human decision boundary.

## Live Behavior

- A Touchpoint summarizes the current decision surface for exactly one Issue.
- A RunReport reports facts for exactly one Run: attempts, cost, validation
  URL, screenshots, abort reason, terminal status, and evidence.
- Reviewer feedback enters as signals and re-enters the run loop through
  durable issue/run state.
- Merged Touchpoints close their Issue in the normal isolated-PR case.
- Playbook integration strategy controls where entries land and when a final
  review surface is required.

## Failure And Recovery

- Missing or delayed PR syndication must not erase the canonical Glimmung run
  and issue state.
- A failed signal drain leaves durable queued signal state or clear failure
  evidence.
- RunReport derivation failure should surface as missing/invalid report state,
  not as a misleading successful review.
- Playbook advance failures should preserve prior entry state and gates.

## Observability

- Touchpoint, RunReport, signal, and Playbook APIs should expose enough state
  for an operator to distinguish missing evidence, failed syndication, queued
  feedback, and failed rerun.
- PR body generation should name issue/run refs and the Glimmung Touchpoint.
  Review evidence, including screenshots, is stored and rendered by Glimmung
  rather than copied into the GitHub PR body.
- Signal drain logs should identify target repo/ref, source, kind, and outcome.

## Acceptance Checks

- Touchpoint changes preserve one-to-one Issue cardinality.
- RunReport changes preserve one-Run scope and include factual evidence fields.
- Signal changes include tests or evidence for durable enqueue and drain
  behavior.
- Playbook changes prove dependency/gate/integration behavior for the changed
  path.
- PR syndication changes show that GitHub remains a projection of Glimmung
  state rather than the canonical source.

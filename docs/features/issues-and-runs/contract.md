# Issues And Runs Contract

This contract applies to Glimmung issues, run dispatch, run/cycle numbering,
locks, verify-loop retry, native callbacks, abort/failure state, and run
history projection.

## Product Model

An Issue is the canonical work target. A Run is one invocation of a workflow for
that Issue. A Cycle is the durable execution ledger created by retry/recycle
inside a Run. The product must never make a user infer whether work is queued,
running, retried, failed, or complete from GitHub Actions side effects or
browser memory.

## Sources Of Truth

- Cosmos `issues` owns the canonical issue row and project-scoped issue number.
- Cosmos `runs` owns run state, cycle numbering, phase/job/step ledgers,
  callback metadata, cost, validation URL, abort reason, and terminal state.
- Cosmos `locks` owns issue and PR mutual exclusion.
- Native job callbacks to `/v1/run-callbacks/{callback_token}/native/completed`
  own job completion input.
- `docs/workflow-shape.md` owns run/cycle identity, workflow schema snapshots,
  and verify/recycle model.
- GitHub Issues are not the live run-loop source of truth.

## Migration Rules

- Do not reintroduce GitHub Issues as the canonical run trigger.
- Do not add GitHub Actions workflow-run state as a canonical run-state source.
- Do not add manual mid-run restart as a product route; user-driven rerun
  creates a new Run, and recycle policy creates a new Cycle.
- Do not store path strings as canonical IDs when they can be computed from
  typed entity identity.
- Do not keep retired callback routes, route aliases, or tests for native
  failure endpoints.

## Live Behavior

- Dispatch resolves project, workflow, and issue from durable records before
  creating run state.
- Dispatch serializes active work per issue with the issue lock.
- No native work starts without a claimed lease or the configured admission
  state for queued runs.
- Job completion callbacks include `job_id`; phase completion waits for every
  registered job in the phase.
- Recycle policy creates a new Cycle under the same Run. Manual rerun after a
  terminal state creates a new Run.
- Run display numbering remains stable across reloads and schema changes.

## Failure And Recovery

- Concurrent dispatch returns a clear already-running/admission state rather
  than creating duplicate active runs.
- Callback replay is idempotent at the job/cycle boundary.
- Missing capacity does not launch executor work under an unclaimed lease.
- A service restart must project existing run state from Cosmos rather than
  losing queued, running, terminal, or callback-waiting state.
- Terminal paths release issue/PR locks through durable store operations.

## Observability

- Run state, current phase, attempts/cycles, abort reason, cost, validation
  URL, and callback status must be inspectable through API/UI surfaces.
- Native event inspection should let an operator map hot job events back to
  run, cycle, phase, job, and step.
- Lock contention and duplicate dispatch attempts should be logged or surfaced
  clearly enough to distinguish user contention from a stuck run.

## Acceptance Checks

- Dispatch and callback changes include unit/integration tests for lock,
  run-state, and callback idempotency paths.
- Workflow schema or numbering changes preserve historical run projection.
- Verify-loop changes prove retry, terminal, and cleanup behavior.
- Any issue/run UI change reloads from durable state and does not depend on
  browser-local ordering.
- Retired GitHub Issue or GitHub Actions run-loop paths are deleted end to end.

# Touchpoint Gate + PR Merge Plan

Status: in-flight (stage 1).
Owners: review-surfaces, workflow-execution, test-slots, issues-and-runs.

This plan extends the live review surface so a reviewer can approve a
Touchpoint from the UI and have the system idempotently merge the PR, close
the Issue, and tear down the validation environment. It also makes the
reviewer's choice to keep the validation environment alive during review a
first-class workflow concept rather than a per-call flag.

The plan is staged. Each stage leaves the system in a coherent state.

## Long-term endpoint

Every Glimmung workflow ends in a human-reviewed PR. The required shape
becomes:

```
prepare → work → testing → cleanup_early → touchpoint → touchpoint_gate → cleanup_final
```

- `cleanup_early` is always scheduled. It executes by default; it returns
  `skipped` when the originating Issue has `preserve_test_env=true`. When it
  executes, it tears down lease-scoped runtime so the validation environment
  is gone before the reviewer sees `review_required`.
- `touchpoint` hosts the `pr_touchpoint` primitive (PR creation + Touchpoint
  linking). Today this primitive lives inside the single `cleanup` phase
  alongside env teardown; this plan extracts it into its own always-run phase
  so PR creation does not race the early teardown.
- `touchpoint_gate` is a new phase kind. It dispatches no jobs on its own and
  parks the Run with the slot lease intact when `cleanup_early` was skipped.
  An `approve` signal advances the gate by dispatching the managed `pr_merge`
  primitive job. A `reject` signal recycles through the existing
  PR-feedback path.
- `cleanup_final` is always-run and always-executes. It is idempotent: if
  `cleanup_early` already tore down the validation environment, `cleanup_final`
  is a no-op success that still records the cleanup decision in the
  run history.

The `pr.enabled` workflow toggle is removed. There is no documented product
scenario for `pr.enabled=false` and per migration-policy unused toggles are
deletion targets, not design options. After stage 1, every registered workflow
either matches the required shape or is rejected at registration time.

## Sources of truth (additions)

- `workflows`: new phase kind `touchpoint_gate`. Validation requires the seven
  named phases above in the listed order, with the listed `always` and `verify`
  flags and a single `pr_touchpoint` job inside `touchpoint`.
- `issues`: new column `preserve_test_env` (bool, default false). Mutable on
  any open Issue. Read at dispatch time and snapshotted onto the run record.
- `runs`: new column `preserve_test_env` (bool, captured at dispatch). The
  `cleanup_early` phase consults this snapshot to decide execute vs skip.
- `attempts`: `skipped` becomes a first-class attempt conclusion. It advances
  the workflow exactly like `success` for routing purposes; it renders
  distinctly in projection and UI.
- `signals`: new `payload.kind: "approve"` for `source: glimmung_ui` (parallel
  to today's `"reject"`).

## Run-state semantics

- `review_required` becomes the steady state of a run sitting at the
  `touchpoint_gate`. It is not terminal: the gate is open, the slot may or may
  not be alive depending on `preserve_test_env`, and a reviewer signal is the
  only thing that advances it.
- `approve` signal → dispatch `pr_merge` primitive in the gate phase →
  advance to `cleanup_final` → mark run terminal `closed`. Issue closes
  derived from terminal-closed run, honoring the previously aspirational
  "Merged Touchpoints close their Issue" line in
  `docs/features/review-surfaces/contract.md`.
- `reject` signal → recycle to the configured `lands_at`, unchanged from
  today.
- Gate timeout / abort → `cleanup_final` runs, slot released, run terminal
  `aborted` with reason.

## Stages

Each stage is independently coherent. The system continues to dispatch and
verify runs at every stage.

### Stage 1 — shape, outcome, schema

In flight in this branch.

1. Register `touchpoint_gate` as a valid phase kind.
2. Add `skipped` as a first-class attempt conclusion plumbed through
   completion routing, projection, and UI projection types.
3. Add `issues.preserve_test_env` (mutable bool) and `runs.preserve_test_env`
   (immutable snapshot captured at dispatch).
4. Tighten registered workflow validation to the required shape and reject
   anything else.
5. Delete `pr.enabled` field, its conditional validations, and its tests.
6. No runtime gate behavior yet. The `touchpoint_gate` phase, when dispatched,
   currently behaves as an always-run no-op; gate semantics arrive in stage 3.

Projects must re-register their workflows against the new shape before their
next dispatch. There is no auto-migration.

### Stage 2 — `pr_merge` primitive

1. New managed native job primitive `pr_merge` that idempotently merges the
   target PR (check `pull.merged` before attempting; treat already-merged as
   success).
2. GitHub App installation-token minting wired into the managed runner.
3. Durable record on `runs.pr_merged_at` and the touchpoint history.
4. Admin endpoint `POST /v1/projects/{p}/issues/{n}/runs/{r}/touchpoint/merge`
   mirrors the existing `/touchpoint/finalize` shape.
5. Observability: merge attempt logs name project, issue, run, repo, pr,
   sha, outcome.

### Stage 3 — signal-drain triage for `approve`

1. `decideTriageSignal` learns `payload.kind: "approve"` for
   `source: glimmung_ui`.
2. Approve dispatches the `pr_merge` primitive inside the `touchpoint_gate`
   phase. Reject path untouched.
3. Drain logs identify kind (reject vs approve vs ignored).
4. Tests cover approve, reject, timeout, double-approve idempotency, and
   restart-mid-gate.

### Stage 4 — UI affordance

1. `Approve` button on `TouchpointTab`, parallel to today's Request Changes
   button, posts `{kind: "approve"}` to `/v1/signals`.
2. Honors the existing one-pending-signal rule (`pendingSignal`).
3. Renders the gate state: which phase the run sits at, whether the slot is
   still alive, and what `preserve_test_env` was at dispatch.
4. Issue creation and Issue detail surface a `preserve_test_env` toggle on
   open Issues.

### Stage 5 — Issue close + observability

1. Issue state closes derived from terminal-closed run.
2. Metrics: gate-time histogram, approve / reject / timeout counters,
   merge attempt outcome counters.
3. Migration-guard tests: registering a workflow that doesn't match the
   required shape fails with a named-phase error.

## What is explicitly out of scope

- A per-run `preserve_test_slot` boolean that bypasses the workflow shape.
  Preserve lives on the Issue and is snapshotted to the Run.
- A side-effect approve handler that merges, closes, and releases outside
  the phase graph. Per workflow-inspiration the phase graph stays the
  durable orchestration surface.
- Preserving the `pr.enabled` toggle for unknown callers. Unknown callers
  are unsupported per migration-policy.
- Replacing the existing PR feedback recycle path. Reject continues to use
  the signal-drain dispatch path that exists today.

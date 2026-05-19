# Project Run Queue And Test Slot Leases

This document is the design contract for admitting Glimmung issue runs onto a
project's scarce test-slot capacity. It extends the native slot terms in
[`test-slot-lifecycle.md`](test-slot-lifecycle.md) and the workflow shape in
[`workflow-shape.md`](workflow-shape.md).

If implementation behavior disagrees with this document, the implementation is
wrong and should be migrated. This is not a compatibility plan for the older
native dispatch path.

## Goals

- Make the project run queue a durable Glimmung primitive.
- Keep issue and PR work serialized by lock ownership from run creation.
- Reserve project test-slot capacity before any native workflow job starts.
- Keep environment provisioning visible as the workflow's `env-prep` work.
- Drive admission and native launch from durable state, not HTTP request
  lifetimes.
- Preserve the phase -> job graph contract for queued, running, skipped,
  failed, and cleanup-only paths.

## Terms

- **Project run queue**: the durable per-project sequence of run intents
  waiting for admission. This is distinct from test-slot **queue size** in
  `test-slot-lifecycle.md`, which means configured slot count.
- **Queued run**: a durable run record that owns the issue/PR lock and has a
  workflow snapshot, but has not been admitted onto a test slot.
- **Admitted run**: a run that passed startability checks, reserved one
  project test slot, and has begun workflow execution.
- **Test slot lease**: the project-scoped lease over one warm test slot. For
  Glimmung-managed runs, the lease may be reserved before the hot runtime is
  provisioned.
- **Reserved/unprovisioned lease**: a claimed test-slot lease that consumes
  project capacity but whose hot runtime is not ready yet.
- **Project reconciler**: an event-triggered, idempotent server worker that
  rereads durable project state and advances queued runs, workflow jobs, and
  cleanup.

## Run Creation

Clicking Run records durable intent. It does not launch native work.

Run creation must:

- acquire the issue/PR lock;
- create the run record in `queued` state;
- attach the selected workflow snapshot/version to the run;
- materialize phase and job rows from the snapshot with state `not_started`;
- append the run to the project run queue;
- wake the project reconciler.

Run creation must not:

- reserve a test slot;
- create Kubernetes Jobs;
- infer a workflow graph from issue state, attempts, or current phase names;
- depend on a long-running request to finish orchestration.

Queued runs are visible in run history. Their workflow shape is visible because
the snapshot and phase/job rows already exist, but workflow execution has not
started.

## Admission

Admission is project-scoped. The project reconciler owns it.

For each project wake, the reconciler reads durable state and admits queued
runs in queue order while project capacity and policy allow. The wake reason is
only diagnostic; correctness comes from rereading durable state.

Admission order:

1. Select the next queued run for the project.
2. Validate startability from the run's stored workflow snapshot and current
   project/issue state.
3. If validation fails, mark the run terminal `failed_to_start`, release the
   issue/PR lock, leave all phase/job rows `not_started`, and do not reserve a
   test slot.
4. If validation passes, reserve one available project test slot as
   reserved/unprovisioned.
5. Attach the `test_slot_lease_id`, slot name, and validation environment
   identity to durable run state.
6. Transition the run to `running`.
7. Mark the entry phase's runnable job state and launch it through the
   reconciler's native launch path.

A queued run owns the issue/PR lock even before it owns a test slot. The lock
means the issue has one active run intent. If the run is canceled or fails
before admission, the lock is released.

## Lease Lifecycle For Glimmung Runs

Glimmung-managed runs use the same project test-slot pool as
`POST /v1/test-slots/checkout`, but with a different activation boundary.

The run path reserves the slot before hot runtime exists:

```text
available slot
  -> reserved/unprovisioned lease
  -> env-prep provisioning
  -> ready/active lease
  -> cleanup requested
  -> released
```

The invariant is that every state before `released` consumes the slot. A
reserved/unprovisioned lease blocks other queued runs and checkout callers from
using the same slot.

`env-prep` does not request a lease. It receives the already-reserved lease
identity and provisions hot runtime for that lease. Later workflow jobs consume
the durable environment identity from the run/lease state. `env-destroy`
tears down the hot runtime and releases the lease.

Internal retries and recycle decisions within one active run keep the same
test slot lease and environment. A user-triggered retry after a terminal run is
a new queued run with a new queue position and a new future lease.

## Workflow Execution

The run's workflow snapshot is fixed at run creation. A queued run must not
dynamically adopt a newer workflow definition when it is admitted.

The project reconciler owns native launch from durable job state:

```text
attempt native_launch_state says pending/launching/launch_failed
  -> deterministic native job identity
  -> Kubernetes Job created idempotently
  -> launch state recorded durably
```

HTTP handlers may write requested state transitions and wake the reconciler.
They must not be the only owner of side effects such as slot reservation or
Kubernetes Job creation.

Native completion updates durable job state and runs the workflow decision path
against the run's stored workflow snapshot. If the decision advances, retries,
or enters cleanup, the callback handler records the next durable run attempt
with `native_launch_state=pending` and wakes the project reconciler. The
reconciler is still the only owner of Kubernetes Job creation.

A restart after an attempt is recorded but before Kubernetes Job creation is
recoverable: startup reconciliation reads launch-pending attempts and relaunches
them through deterministic native job names. A restart after Kubernetes accepts
some jobs but before Glimmung records `native_launch_state=launched` is also
safe because native job creation is idempotent.

## Failure And Cancellation

Failure before slot reservation:

- run becomes terminal `failed_to_start`;
- issue/PR lock is released;
- no test-slot lease exists;
- no cleanup job runs;
- all phase/job rows remain `not_started`.

Cancellation before slot reservation:

- run becomes terminal `canceled`;
- issue/PR lock is released;
- the run leaves the project queue;
- no workflow cleanup runs.

Failure after slot reservation:

- the cleanup rail runs through the workflow, even if the environment never
  reached ready;
- phases that did not execute are marked `not_run`;
- `env-destroy` receives the lease identity and performs best-effort cleanup;
- the test slot is released only after cleanup confirms the slot is safe to
  allocate again;
- the run becomes terminal only after cleanup outcome is recorded.

Optional environment persistence for debugging must be explicit operator or
user intent. The default for failed provisioning is cleanup and release.

## Event-Driven Reconciliation

The project reconciler is event-driven in normal operation. State transitions
wake it; the wake tells it that a project changed, not which exact transition
to perform.

Examples of wake sources:

- run created;
- queued run canceled;
- slot released;
- native job completed;
- run terminal state changed;
- service process started.

The reconciler must be idempotent. Duplicate, stale, or coalesced wakes are
safe because the reconciler rereads durable state before acting.

Startup reconciliation is required to recover in-flight project queues,
reserved/unprovisioned leases, cleanup work, and runnable jobs after deploys or
process crashes. A periodic scan is not the primary engine; if one is added, it
is a low-frequency recovery guard with a measured Cosmos cost story.

## UI And Projection

The UI must render queued and running runs from canonical durable data:

- workflow snapshot;
- materialized phase/job rows;
- run state;
- lease/environment state when present.

It must not synthesize workflow shape from current phase names, attempt
history, or fallback metadata. Missing snapshot or phase/job rows are data
integrity errors, not an invitation to infer the graph.

Queued run display:

```text
run: queued
phases/jobs: not_started
lease: none
native jobs: none
```

Admitted run display:

```text
run: running
env-prep: running
lease: reserved/unprovisioned or provisioning
native job: launched
```

Failure after env prep starts may show cleanup out of the usual left-to-right
path. Intermediate phases that never ran stay visible as `not_run`.

## Migration Boundary

This migration retires the old assumption that dispatch owns immediate native
launch and that a native lease acquired during phase execution is the source of
test environment identity for the run.

Delete live paths that:

- launch workflow native jobs directly from run creation request handling;
- let `env-prep` or another workflow job request its own test slot lease;
- pass caller-selected slot identity through phase inputs or lease metadata;
- infer run graph shape from non-canonical fallback fields;
- treat missing workflow snapshot/phase rows as renderable by synthesis;
- release test-slot leases without the cleanup path confirming runtime
  teardown.

Existing `POST /v1/test-slots/checkout` behavior remains the session/MCP
checkout surface described in `test-slot-lifecycle.md`. The Glimmung-run path
is not a compatibility wrapper around checkout; it is a first-class run
admission path over the same project slot pool.

## Implementation Stages

1. Define durable run queue, workflow snapshot, phase/job row, and
   reserved/unprovisioned lease contracts.
2. Introduce the event-triggered project reconciler and wake path.
3. Move run creation to queued intent only.
4. Move project admission and test-slot reservation into the reconciler.
5. Move `env-prep` native launch into the reconciler's idempotent job launch
   path.
6. Move forward, retry, resume, and triage launch requests to durable
   launch-pending attempts woken through the reconciler.
7. Make `env-prep` provision the already-reserved lease.
8. Make cleanup release the run-owned lease on every post-reservation terminal
   path.
9. Remove fallback graph synthesis and old native dispatch launch branches.
10. Add contract tests and live-style failure tests.

Each stage must leave the system in a coherent state. Do not preserve the old
launch path as a compatibility branch.

## Contract Tests

The migration is not complete until tests cover:

- queued run creation stores workflow snapshot and `not_started` phase/job
  rows without reserving a slot;
- queued run cancellation releases the issue/PR lock without cleanup;
- admission failure before slot reservation marks `failed_to_start`, releases
  the lock, and leaves no lease;
- admitted run reserves exactly one project test slot before launching
  `env-prep`;
- `env-prep` receives an existing lease identity and cannot request or choose
  a slot;
- reserved/unprovisioned leases count against project capacity;
- request cancellation or service restart after queued run creation does not
  strand execution;
- service restart after slot reservation but before Kubernetes Job creation is
  reconciled;
- completion, retry, resume, and request-changes paths never acquire a new
  per-phase native lease and never launch Kubernetes Jobs directly;
- env-prep failure runs cleanup, marks skipped phases `not_run`, and releases
  the slot;
- internal evidence-gate retry keeps the same lease/environment;
- user retry after terminal creates a new queued run and does not reuse the
  previous lease;
- UI/projection refuses to synthesize graph shape without canonical snapshot
  and phase/job state.

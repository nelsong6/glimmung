# Workflow shape

The opinionated structure every glimmung-managed workflow follows,
the data model that enforces it, and the conventions for naming /
identifying entities.

## The shape

Every workflow is a left-to-right pipeline of phases:

```
prepare  →  work        →  testing  →  cleanup
            ┌─────┐
            │ plan│
            ├─────┤
            │ impl│
            └─────┘
```

- **Phases** flow horizontally. Each phase is a stage of the
  pipeline. The first phase is the only entry phase and declares
  `depends_on: []`. Every later phase declares exactly one
  `depends_on` entry, and that entry must be the immediately
  previous phase.
- **Jobs** stack vertically inside a phase. Multiple jobs in one
  phase always run in parallel — there is no job-level
  `depends_on` and the system does not support gitlab-style
  inter-job-within-a-phase dependencies. Pipeline composition
  happens at the phase boundary.
- **A phase is one job wide and any number of jobs deep.** "Wide"
  meaning horizontal — phase boundaries are the only place a
  pipeline advances as one left-to-right chain. "Deep" meaning vertical
  parallel jobs within a single phase.

This rule keeps pipeline design legible: anyone reading a
workflow definition can see the order of work by scanning
phases left-to-right, and see what runs in parallel by reading
jobs top-to-bottom in each column.

## Required phases

Glimmung-managed workflows must declare:

1. **prepare** — exactly one phase with `depends_on=[]` (the entry
   phase). Project owns what goes here; common shape is "build a
   container image and deploy it to a per-run validation
   namespace."
2. **testing** — at least one phase with `verify=True`. The phase
   emits `verification.json` and exits non-zero on bad verdict
   (self-enforcing). Even `npm build` or `go test` is enough; what
   matters is that the workflow produces a verdict.
3. **cleanup** — at least one phase with `always=True`. Runs on
   every terminal outcome (success / abort / fail). Tears down
   the validation environment.

Any number of `work` phases between prepare and testing — that's
where the actual implementation happens.

The mandatory-phase and linear-topology enforcement is active in the Go workflow
writer, sync path, and Postgres upsert path. Registrations that miss the entry
phase, a `verify: true` testing phase, or an always-run cleanup phase are
rejected before they can become the project runtime contract. Registrations with
multiple entry phases, fan-in/fan-out phase dependencies, invalid cross-phase
input refs, duplicate phase names, or duplicate job IDs are rejected too.

Blank phase `kind` values default to `k8s_job`. Registered workflow phases must
use `k8s_job`; any other executor kind is rejected before dispatch.

## Job-level concurrency within a phase

In a phase with N jobs, all N dispatch simultaneously. No
dependencies between them. Each job is its own k8s Job; each
emits its own completion callback; the phase is "complete"
when all jobs have completed.

The native completion contract is enforced at
`POST /v1/run-callbacks/{callback_token}/native/completed`: the payload must
include `job_id`. Managed runner payloads include positive `cost_usd` when the
runner observed agent result lines with top-level `total_cost_usd`; that value
is the durable job-completion cost. Glimmung records each job completion
independently, returns a `wait_jobs` response while sibling jobs are still
pending, and runs the phase decision path only on the transition where the
final registered job completes. This is the only native terminal callback.
Failed jobs report through the same endpoint with a non-`success`
`conclusion`; the retired `/native/failed` callback must not be reintroduced or
required by runner images.

Because jobs in a phase are strictly parallel, **a job can never
depend on the output of another job in the same phase**. If
verifier needs implementation's output, verifier goes in a
*later* phase, not as a sibling job in the work phase.

This rules out gitlab-style `needs:` graphs at the job level, by
design — pipeline shape is determined by phases, not by job DAGs.

## The verify/gate boundary

Two valid shapes for emitting a verdict at the testing boundary:

**Self-enforcing verify** (recommended default):

```yaml
- name: testing
  kind: k8s_job
  verify: true
  jobs:
    - id: testing
      managed: true
      checkout:
        ref: main
      working_directory: /workspace/ambience
      steps:
        - slug: tests
          type: run
          run: |
            npm test
            printf 'verification=pass\n' >> "$GLIMMUNG_OUTPUT_FILE"
      # step writes phase outputs AND exits non-zero
      # if status != "pass". The phase itself renders red.
```

**Verify + glimmung-owned gate**:

```yaml
- name: testing
  kind: k8s_job
  verify: true
  outputs: [verification]
  jobs: [...]   # writes verification.json, exits 0 always

- name: gate
  kind: k8s_job
  evidence_verification_gate: true
  inputs:
    verification: ${{ phases.testing.outputs.verification }}
  recycle_policy:
    max_attempts: 2
    on: [verify_fail]
    lands_at: testing
```

The gate primitive is Glimmung-supplied: no project jobs, no consumer
repository runner script. Glimmung owns the native gate image and command that
reads the substituted verification input and exits by status. Workflow
registration canonicalizes an evidence gate into the managed Glimmung runner
job, so a project cannot accidentally make the gate an uninstrumented arbitrary
container. Use the gate when you want enforcement to be its own visible box, its
own recycle policy, or its own budget separately from the verifier.

## PR touchpoint primitive

Workflows with `pr.enabled: true` must declare exactly one native job with
`primitive: pr_touchpoint`, and that job must live in an `always` phase. The
usual shape is to place it beside environment teardown so PR creation and
touchpoint linking are visible in native job logs while cleanup runs:

```yaml
pr:
  enabled: true

phases:
  - name: cleanup
    kind: k8s_job
    always: true
    depends_on: [testing]
    jobs:
      - id: env-destroy
        # normal teardown job
      - id: pr-touchpoint
        primitive: pr_touchpoint
```

The job is Glimmung-supplied. Registration canonicalizes the declared job into
the managed native runner step that calls Glimmung's PR/touchpoint finalizer.
The workflow owns the placement and job id; Glimmung owns the implementation. If
`pr.enabled` is false, declaring this primitive is invalid.

The same finalizer is also exposed as an admin repair/control endpoint:
`POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/touchpoint/finalize`.
For recycled runs, use the cycle-addressable form that matches the UI URL:
`POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/cycles/{cycle_number}/touchpoint/finalize`.
It is idempotent and uses the durable Run state as source of truth: it creates
or reuses the GitHub PR, records `run.pr_number`, and ensures the Touchpoint
linked to the Issue and Run. During that same call, Glimmung promotes review
facts such as `validation_url` into canonical Run fields, normalizes run
artifact evidence into Touchpoint evidence, and validates required screenshot
artifacts before the Touchpoint is ready. GitHub PR bodies stay a syndicated
pointer into Glimmung; screenshots and other review evidence belong on the
Glimmung Touchpoint. Operators should use this endpoint when a Run already
passed verification but an older or interrupted workflow did not materialize
the review surface.

## Naming convention

The reference names for the four mandatory phases are:

- **prepare** — entry phase, environment setup
- **work** — implementation labor (1+ phases between prepare and
  testing)
- **testing** — the verdict-rendering phase
- **cleanup** — always-run teardown

Projects may use other names; these are the canonical defaults.
The MCP `scaffold_workflow` tool (TODO) emits a starter template
with these names pre-filled.

## Runtime source of truth

Postgres workflow registrations are the runtime source of truth. The
`.glimmung/workflows/<name>.yaml` upstream endpoints remain an import/sync
convenience for older desired-state flows, but dispatch reads the registered
workflow document, not a consumer repository file.
The native runner direction is documented in
[`project-native-runner-architecture.md`](project-native-runner-architecture.md):
Glimmung owns the runner contract and project workflows use inline step
commands rather than repo-owned callback plumbing.

Workflow registrations are logical pointers. Updating a registration creates a
new immutable workflow schema and moves the logical pointer forward. Existing
runs and cycles keep referencing the schema they were created with. Historical
schemas are retained; this rollout does not garbage-collect them. Deleting or
deactivating a logical workflow must not delete schemas referenced by run
history.

Each cycle stores a durable execution ledger for the schema snapshot it was
created with: phase records contain job records, and job records contain step
records. The graph UI projects from this ledger first, then uses raw native
events as live detail. This keeps state names and colors stable even when a
native job has not emitted logs yet.

## Path-typed identity

Entities are addressed by URL-shaped paths that match the HTTP
API surface:

```
projects/<project>
projects/<project>/workflows/<workflow>
projects/<project>/workflow-schemas/<schema_ref>
projects/<project>/workflows/<workflow>/phases/<phase>
projects/<project>/issues/<issue_number>/runs/<run_number>
projects/<project>/issues/<issue_number>/runs/<run_number>/cycles/<cycle_number>
projects/<project>/issues/<issue_number>/runs/<run_number>/cycles/<cycle_number>/phases/<phase>
projects/<project>/issues/<issue_number>/runs/<run_number>/cycles/<cycle_number>/phases/<phase>/jobs/<job_id>
projects/<project>/issues/<issue_number>/runs/<run_number>/cycles/<cycle_number>/phases/<phase>/jobs/<job_id>/steps/<slug>
```

Logs, MCP tool outputs, error messages, and notification surfaces
all emit these. Inside a known scope (e.g. inside one run's logs),
the trailing path can be elided: `attempts/0/jobs/agent` is enough when the
run is implicit.

Runs are user/reviewer intent records. Cycles are the durable execution
ledger records. The issue history keeps a flat, monotonically increasing
cycle number (`1`, `2`, `3`, ...), but each cycle also belongs to a run and
has a run-local cycle ordinal. The compact display form is
`<run>.<run_cycle>` such as `1.1`, `1.2`, `2.1`.

Recycle policy creates a new cycle under the same run. Reviewer feedback,
touchpoint changes, and a user pressing Run after terminal state create a
new run with its first cycle. Manual mid-run restart is not part of the
product HTTP surface; emergency surgery belongs outside the normal run
workflow model.

Within one run, only one cycle can be active at a time. Within one issue, only
one run can be active at a time. A cycle stores the workflow schema ref it was
created with; phase/job/step projection and retry/cleanup decisions use that
schema ref, not whatever logical workflow registration is current later.

Use **attempt** as an execution-scoped display counter for a concrete phase
launch. It is not a first-class product entity. Recycle policy is represented
by a new cycle, not by appending another product-level attempt to the prior
cycle.

Manual run dispatch is admission-gated: if no test slot is available, the API
returns `no_capacity` and does not create a run or cycle. Queueing remains an
issue-level product workflow; queued cycles that already exist are admitted by
the run-queue reconciler when capacity appears.

Never store paths as canonical identifiers — compute at render
time from the entity's slug + parent context. This avoids
renumbering churn when phases are added/removed and naturally
handles DAGs (parent path encodes type structurally; depth
doesn't matter for naming).

Helpers live in `internal/domain/paths`: `RunPath`, `PhasePath`, `JobPath`,
and `StepPath`.

## Why this shape

The constraints are deliberate:

- **Strict left-to-right** removes the gitlab-style "wonky
  semantics" where `needs:` DAGs at the job level make pipelines
  hard to read. Jobs can fan out inside a phase; phases themselves
  stay a single chain.
- **Mandatory testing** means glimmung-managed workflows are
  self-validating; an agent's PR doesn't ship without a verdict
  step, even if the verdict is just `npm build`.
- **Mandatory cleanup** means orphaned environments don't
  accumulate. Glimmung enforces what every project would have
  built awkwardly on its own.
- **Path-typed identity** makes references uniform across logs,
  UI URLs, MCP, slack — one canonical form, parent-encoded by
  structure, no decoration.

These are the four levers from `CLAUDE.md`: precise lanes,
heavy automation around the agent, guard rails, and token-spend
protection. Workflow shape is a guard rail.

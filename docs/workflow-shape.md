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
  pipeline. Phases run sequentially with respect to their
  `depends_on` graph (a phase dispatches when all its predecessors
  have completed-with-ADVANCE).
- **Jobs** stack vertically inside a phase. Multiple jobs in one
  phase always run in parallel — there is no job-level
  `depends_on` and the system does not support gitlab-style
  inter-job-within-a-phase dependencies. Pipeline composition
  happens at the phase boundary.
- **A phase is one job wide and any number of jobs deep.** "Wide"
  meaning horizontal — phase boundaries are the only place a
  pipeline can fan out left-to-right. "Deep" meaning vertical
  parallel jobs within a single phase.

This rule keeps pipeline design legible: anyone reading a
workflow definition can see the order of work by scanning
phases left-to-right, and see what runs in parallel by reading
jobs top-to-bottom in each column.

## Required phases

Glimmung-managed workflows must declare:

1. **prepare** — at least one phase with `depends_on=[]` (an entry
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

The mandatory-phase enforcement is opinionated; future glimmung
versions will reject registrations that don't match. Until the
enforcement lands (depends on a test-fixture migration), the
pattern is the recommended convention rather than a hard
requirement.

## Job-level concurrency within a phase

In a phase with N jobs, all N dispatch simultaneously. No
dependencies between them. Each job is its own k8s Job; each
emits its own completion callback; the phase is "complete"
when all jobs have completed.

Because jobs in a phase are strictly parallel, **a job can never
depend on the output of another job in the same phase**. If
verifier needs implementation's output, verifier goes in a
*later* phase, not as a sibling job in the work phase.

This rules out gitlab-style `needs:` graphs at the job level, by
design — pipeline shape is determined by phases, not by job DAGs.

## The verify/gate seam

Two valid shapes for emitting a verdict at the testing boundary:

**Self-enforcing verify** (recommended default):

```yaml
- name: testing
  kind: k8s_job
  verify: true
  jobs:
    - id: testing
      command: ["/bin/bash", "/opt/.../run-testing.sh"]
      # script writes verification.json AND exits non-zero
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

The gate primitive is glimmung-supplied — no project jobs,
glimmung fills in the runner image + script (a small Python
that reads `$VERIFICATION` and exits 0/1 by status). Use the
gate when you want enforcement to be its own visible box,
its own recycle policy, or its own budget separately from the
verifier.

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

## Path-typed identity

Entities are addressed by URL-shaped paths that match the HTTP
API surface:

```
projects/<project>
projects/<project>/workflows/<workflow>
projects/<project>/workflows/<workflow>/phases/<phase>
projects/<project>/runs/<run_id>
projects/<project>/runs/<run_id>/phases/<phase>
projects/<project>/runs/<run_id>/phases/<phase>/jobs/<job_id>
projects/<project>/runs/<run_id>/phases/<phase>/jobs/<job_id>/steps/<slug>
```

Logs, MCP tool outputs, error messages, and notification surfaces
all emit these. Inside a known scope (e.g. inside one run's logs),
the trailing path can be elided — `phases/agent-execute/jobs/agent`
is enough when the run is implicit.

Runs are the durable execution records. Recycles, resumes, and
request-changes loops create additional issue-scoped runs, not nested
attempt entities. A run may carry lineage fields such as
`parent_run_id`, `root_run_id`, `origin`, `entrypoint_phase`, and
cycle display numbers (for example `1.1`, `1.2`) so the UI can
group related runs without inventing an attempt layer. Within one run,
the workflow is represented by phase/job/step execution records.

Use **attempt** only as an attribute or display counter when an executor
or recycle policy needs to say "this is the second try of this run or
phase." It is not a first-class Glimmung entity and should not appear as
a public hierarchy between run and job.

Never store paths as canonical identifiers — compute at render
time from the entity's slug + parent context. This avoids
renumbering churn when phases are added/removed and naturally
handles DAGs (parent path encodes type structurally; depth
doesn't matter for naming).

Helpers in `glimmung/paths.py`: `path_for_run(run)`,
`path_for_phase(workflow, phase)`, `step_path(p, r, i, j, s)`,
etc.

## Why this shape

The constraints are deliberate:

- **Strict left-to-right** removes the gitlab-style "wonky
  semantics" where `needs:` DAGs at the job level make pipelines
  hard to read. Phases are the only fan-out boundary.
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

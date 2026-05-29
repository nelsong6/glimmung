# Workflow Execution Contract

This contract applies to workflow registration, schema snapshots, phase/job
shape, native Kubernetes job launch, managed evidence gates, callback tokens,
and workflow sync helpers.

## Product Model

A Workflow is the registered automation shape for a project. It gives agents a
precise lane: prepare capacity, do work, verify the result, and clean up. The
workflow graph should be legible before dispatch and should remain reconstructable
after registration changes.

## Sources Of Truth

- Postgres `workflows` owns logical workflow registrations and current schema
  pointers.
- Historical workflow schemas referenced by runs own projection for past
  cycles.
- `docs/workflow-shape.md` owns required phases, linear topology, job
  concurrency, evidence gate semantics, and path-typed identity.
- Native Kubernetes Jobs own execution process state while running.
- Native job event rows own hot execution telemetry.
- `.glimmung/workflows/<name>.yaml` is an import/sync convenience only, not the
  runtime source of truth.

## Migration Rules

- Do not make repo workflow files the dispatch source of truth.
- Do not register executor kinds other than `k8s_job` or `touchpoint_gate`.
  `touchpoint_gate` phases must declare exactly one `pr_merge` primitive job
  and no other jobs; the `pr_merge` primitive must live inside a
  `touchpoint_gate` phase and nowhere else.
- Do not add phase fan-in, fan-out, job-level dependencies, or non-linear DAG
  behavior without replacing this contract.
- Do not allow project-owned arbitrary gate jobs to stand in for the managed
  evidence gate.
- Do not delete historical schemas still referenced by run history.
- Do not start a workflow-execution background reconciler (run queue,
  dispatch timeout, completion sweep, native Job inspection, etc.) outside
  the `settings.ControlPlaneLoopsEnabled` gate in `cmd/glimmung-go/main.go`.
  The control-plane isolation boundary belongs to the
  [Test Slots contract](../test-slots/contract.md); a workflow-execution
  reconciler that ignores it lets a hot-swapped slot binary race the prod
  glimmung Deployment on the same runs, Postgres rows, and `glimmung-runs`
  Kubernetes Jobs.

## Live Behavior

- Registration rejects missing entry, verify, or always-run cleanup phases.
- Registration rejects invalid dependencies, duplicate phases, duplicate job
  IDs, invalid inputs, and unsupported executor kinds before they become a
  runtime contract.
- Jobs inside one phase launch in parallel and complete independently.
- Phase advancement happens only after all registered jobs in the phase reach
  terminal callback state.
- Evidence verification gates are canonicalized into managed Glimmung runner
  jobs.
- Runs use the workflow schema snapshot captured at run/cycle creation, not a
  later logical workflow update.

## Failure And Recovery

- A failed native Job produces durable job/phase failure state through the
  completion callback path, not a retired failure route.
- Callback-token validation failure must not mutate unrelated runs or phases.
- Workflow update failure should leave the previous logical workflow pointer
  intact.
- Service restart must preserve the ability to project active and historical
  cycles from schema refs and run ledgers.

## Observability

- Native event streams should identify project, issue, run, cycle, phase, job,
  step, conclusion, and relevant log tail or archive link.
- Registration failures should name the exact invalid phase, dependency, input,
  job, or unsupported kind.
- Run graph projection should make schema mismatch or missing schema failures
  explicit.

## Acceptance Checks

- Workflow shape changes include registration validation tests.
- Native launcher/callback changes include multi-job phase behavior when the
  change can affect phase completion.
- Gate changes prove managed gate canonicalization and terminal behavior.
- Sync-helper changes prove they do not bypass registration validation.
- Historical run projection still works when the logical workflow changes.

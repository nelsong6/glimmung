# Workflow Inspiration

This note names the external systems worth reading while the Glimmung workflow
model is settling. It is not an adoption plan. The local contracts in
[`workflow-shape.md`](workflow-shape.md) and
[`run-graph-display-design.md`](run-graph-display-design.md) stay primary:
Glimmung owns issues, runs, phases, jobs, evidence, touchpoints, queueing, and
human decisions.

The useful pattern is to borrow proven primitives without inheriting another
product's boundary. Argo, Tekton, Temporal, Kueue, Nomad, Prow, Buildkite, and
Concourse each solve one part of the problem. None of them describes the whole
Glimmung shape.

## Design Stance

- **Runtime source of truth:** Cosmos workflow registrations remain the runtime
  contract. Repository workflow files are import/sync inputs, not what dispatch
  reads.
- **Execution layer:** Glimmung-managed phases run as Kubernetes Jobs. Another
  workflow engine can be useful under a phase later, but it should not become
  the user-facing run graph.
- **Graph level:** Phases are the horizontal graph. There is exactly one entry
  phase, and every later phase depends on the immediately previous phase. Jobs
  inside one phase run in parallel and do not depend on each other.
- **Required safety rails:** Every workflow has an entry phase, a verifying
  phase, and an always-run cleanup phase.
- **Human surface:** Touchpoints and run reports explain what a reviewer needs
  to inspect or decide. CI-style logs are inputs to that surface, not the
  surface itself.

## Closest References

| System | Read For | Borrow | Do Not Inherit |
| --- | --- | --- | --- |
| [Argo Workflows](https://github.com/argoproj/argo-workflows) | Kubernetes-native workflow execution, node status, artifacts, DAG/steps vocabulary. | Executor ideas, status projection, artifact references, pod/job lifecycle handling. | Argo as the source of truth for issue runs, phase identity, retries, evidence, or UI graph semantics. |
| [Tekton Pipelines](https://tekton.dev/docs/pipelines/pipelineruns/) | `PipelineRun` / `TaskRun` execution records, task results, Kubernetes-native CI building blocks. | Clear task-result contracts, per-task execution status, reusable task boundaries. | A CI pipeline as the product model. Glimmung phases/jobs remain the visible model. |
| [Temporal](https://github.com/temporalio/temporal) | Durable orchestration, cancellation, signals, retries, activity idempotency. | State-machine rigor for long-running work, signal handling, replay-safe decisions, cancellation semantics. | Hiding workflow shape inside SDK code. The phase graph must stay inspectable and data-defined. |
| [Kueue](https://github.com/kubernetes-sigs/kueue) | Kubernetes admission control for scarce batch capacity. | Queue/admission concepts, cluster queues, resource flavors, fair sharing. | Treating capacity admission as the whole product. Glimmung still owns leases, issue priority, and run state. |
| [Nomad scheduling](https://developer.hashicorp.com/nomad/docs/concepts/scheduling/how-scheduling-works) | Evaluations, allocations, placement, rescheduling. | The distinction between desired work, scheduler decisions, and concrete allocations on scarce hosts. | A generic cluster scheduler UI or full Nomad job model. |
| [Prow/Tide](https://docs.prow.k8s.io/docs/components/core/tide/) | PR gating, retest loops, merge pools, status dashboards. | Review-gate discipline, visible merge readiness, explicit retest signals. | GitHub PRs as the canonical Glimmung run or issue loop. PRs are syndicated review targets. |
| [Zuul](https://zuul-ci.org/docs/zuul/latest/concepts.html) | Cross-project gating, dependent changes, speculative merge queues. | Thinking about queues of reviewable changes and test-before-merge policy. | Full speculative multi-repo gating unless Glimmung has a concrete product need for it. |
| [Buildkite hooks](https://buildkite.com/docs/agent/hooks) and [annotations](https://buildkite.com/docs/pipelines/configure/annotations) | Agent hooks, per-build annotations, concise evidence attached to CI work. | Hook points around execution and human-readable evidence snippets. | Buildkite-style pipeline ownership or treating annotations as the primary review object. |
| [Concourse tasks](https://concourse-ci.org/docs/tasks/) | Explicit task inputs, outputs, images, and isolated execution. | Strong input/output discipline for phase jobs and artifacts. | Concourse's resource/check model as Glimmung's issue/run model. |
| [SWE-agent](https://github.com/princeton-nlp/SWE-agent/blob/main/docs/index.md) | Issue-to-patch agent loop, trajectories, sandboxed execution. | Agent execution traces, issue-oriented repair loop, evidence from the working session. | A single-agent benchmark harness as the platform boundary. |
| [OpenHands](https://github.com/OpenHands/OpenHands) | General software-development agent runtime and workspace interaction. | Agent workspace ergonomics, task execution loop, human-in-the-loop affordances. | Replacing Glimmung's queue, workflow registration, evidence, or touchpoint model. |

## Negative Reference

[GitLab CI `needs`](https://docs.gitlab.com/ci/yaml/needs/) is useful mostly as
a warning. It proves that job-level DAGs are powerful, but they also make a
pipeline harder to read once every job can point at every other job. Glimmung's
constraint is deliberate: phases express left-to-right ordering; jobs inside a
phase are parallel siblings.

If a job needs another job's output, put it in a later phase. Do not add
job-to-job dependencies inside a phase.

## What This Means For Glimmung

The nearest established path is a composite:

1. Use **Argo/Tekton** as references for Kubernetes execution records.
2. Use **Temporal** as the reference for durable orchestration semantics.
3. Use **Kueue/Nomad** as references for capacity and allocation decisions.
4. Use **Prow/Zuul** as references for review gates and merge readiness.
5. Use **Buildkite/Concourse** as references for job boundaries, hooks,
   artifacts, and annotations.
6. Use **SWE-agent/OpenHands** as references for agent work loops and evidence.
7. Use **GitLab `needs`** as the line Glimmung is intentionally not crossing.

That composite is why Glimmung feels unusual. It is combining known primitives
around a narrower product model: issue-driven agentic development with a
visible verify loop, scarce agent capacity, durable run history, and human
review as a first-class decision.

## Review Checklist

When a workflow change claims to follow the established path, check it against
these questions:

- Does the registered workflow remain the runtime source of truth?
- Does the visible graph still use phase -> job -> step vocabulary?
- Does the phase chain still have exactly one entry and one previous phase per
  later phase?
- Are jobs within a phase still strictly parallel?
- Is verification explicit and self-enforcing or gated by a Glimmung-owned
  gate?
- Does cleanup run on every terminal outcome?
- Are capacity decisions modeled as Glimmung queue/lease decisions?
- Does evidence flow into run reports and touchpoints instead of disappearing
  into executor-native logs?
- Does the change avoid making GitHub Actions, Argo, Tekton, or another system
  the canonical owner of run identity?

Research snapshot: 2026-05-14.

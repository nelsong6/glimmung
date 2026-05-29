# Inner-Job Observation Contract

Native phase scripts sometimes spawn *child* Kubernetes Jobs in a different
namespace from the runner. Today Glimmung sees only the outer Job. When the
child hangs, fails silently, or has its logs buried, the outer phase looks
healthy until its `activeDeadlineSeconds` finally trips — and there is no
durable record of what the child was, where it ran, or how to find its logs.

`ambience#170/runs/1.1` is the canonical incident. The `verify` phase script
created an inner verification-agent Job in the slot namespace
(`ambience-slot-3/agent-4d950d23-…-ve-2`). The inner agent finished in
~4 minutes with a correct `abort` verdict. The wrapping
`ambience_preview.cli wait-agent-job` misclassified the Completed Job as a
failure 30 minutes later. The run hung. Glimmung had no record that the inner
Job existed.

This document is the staged plan for closing that visibility gap.

## Principles

- **Children are first-class.** A phase that spawns inner k8s Jobs registers
  them with Glimmung. They show up in the run report alongside the outer Job,
  not as a footnote.
- **Registration is event-driven.** The native runner already streams the
  child script's stdout. We piggy-back on that channel rather than introducing
  a parallel control plane.
- **Detection is k8s-Watch-based, not Glimmung-process-aware.** Once
  registered, the child Job is watched the same way the outer Job is —
  conditions are the authoritative completion signal. (See
  PR glimmung#621 for the outer-side reconciler that this contract mirrors.)
- **Closed-enum surfaces.** New metric labels and new event types use bounded
  enums per `docs/observability.md`'s cardinality policy.
- **No retroactive evidence fabrication.** When a child registration is lost
  (TTL'd, never observed), the run report marks it as such; we do not infer
  outcomes the runner did not stamp.

## Registration channel

The runner already calls `r.observeLogCost(line)` on every child stdout line
that looks like JSON, extracting cost from the standard claude-cli format.
The same dispatcher gets a second callback for `glimmung_inner_job` records.

### Marker line

A phase script emits one line containing a known prefix followed by a single
JSON object:

```text
===GLIMMUNG-INNER-JOB=== {"namespace":"ambience-slot-3","job_name":"agent-4d950d23-…-ve-2","intent":"verification_agent","label":"verify-agent"}
```

Single-line so the streaming log scanner does not need multi-line state. The
prefix is distinctive enough that no real log payload will match it
accidentally.

The runner parses the embedded JSON, validates the shape (required:
`namespace`, `job_name`; optional: `intent`, `label`, `selector`), and emits a
`native event` with `event_type="inner_job_registered"` and the payload as
metadata. Existing `EventsURL` callback path is reused — no new endpoint.

`intent` is a bounded enum: `verification_agent`, `helper`, `tooling`,
`unknown`. Used as a metric label.

The marker line is also retained verbatim in the streamed log so post-hoc Loki
queries still find it.

## Storage and surface

### Stage 1: events-only (this PR)

- Runner emits `inner_job_registered` events.
- Server accepts the new event type, stores it on the existing native_run_events
  table, no schema migration.
- Run-report API surfaces inner Jobs as `inner_jobs[]` on `RunPhaseExecution`,
  populated by scanning event records at read time.
- Frontend renders a sub-row under the parent Job in `ProjectionInspector`,
  with the same Grafana Explore deep-link the parent has (the helper from PR
  glimmung#621 is namespace-aware and works for any pod).
- `glimmung_run_inner_jobs_registered_total{intent}` counter — bounded labels.

This leaves the system in a coherent state: every child Job that has ever
been registered is queryable through the existing native event stream, and the
dashboard shows what was launched even when the outer pod was killed before
the child terminated.

### Stage 2: durable child status (follow-up PR)

- New table `native_run_inner_jobs` keyed on (run_id, attempt_index,
  parent_job_id, namespace, child_job_name).
- Watcher loop (same shape as `ExpireFailedActiveJobs`) reads each child Job's
  `.status.conditions[]`, stamps Complete/Failed back onto the row.
- Run report shows child terminal state, condition reason, and completion
  time.
- Metric `glimmung_run_inner_jobs_terminal_total{intent, conclusion, reason}`.

### Stage 3: evidence linkage

When the reconciler observes an inner Job reaching a terminal
condition, it now captures the pod's stdout into the artifact store
under
`runs/{project}/{runID}/inner_jobs/{namespace}/{jobName}.log` (bounded
to 8 MiB; server-side `tailLines=20000` + a client-side
`LimitReader`). The `log_archive_url` on the inner-Job row points at
the resulting `/v1/artifacts/...` blob; the dashboard's existing
artifact-download surface dereferences it.

When the artifact writer is unconfigured or the launcher cannot fetch
logs (RBAC, pod GC race, transport error), the watcher falls back to
the Grafana Explore deep-link as the `log_archive_url`. The Grafana
link works while Loki has the data; the artifact link works forever.

There is no remaining outstanding piece in the inner-Job contract.

## What does *not* live in this contract

- Inner pods that are not k8s Jobs (bare Pods, DaemonSet-scheduled work) are
  out of scope. The contract is Job-shaped.
- Cross-cluster children are out of scope.
- Children spawned by other children (grandchildren) are out of scope. A
  child registering its own children is recorded; the runner does not chase
  the tree.

## Schema additions (Stage 1)

### Native event

New `event` enum value: `inner_job_registered`.

`metadata` payload (validated server-side; reject any other shape with 400):

```jsonc
{
  "namespace": "ambience-slot-3",
  "job_name": "agent-4d950d23-fef0-4069-b7c1-1d6c372a3656-ve-2",
  "intent": "verification_agent", // bounded enum
  "label": "verify-agent",        // human-readable; optional
  "selector": "ambience.io/issue=170" // optional, free text
}
```

### Run report API

`RunPhaseExecution` gains `inner_jobs: []InnerJobRef`:

```go
type InnerJobRef struct {
    ParentJobID string `json:"parent_job_id"`
    Namespace   string `json:"namespace"`
    JobName     string `json:"job_name"`
    Intent      string `json:"intent"`
    Label       string `json:"label,omitempty"`
    Selector    string `json:"selector,omitempty"`
    RegisteredAt time.Time `json:"registered_at"`
}
```

Populated by scanning the run's native events for `inner_job_registered`
records.

### Metric

```
glimmung_run_inner_jobs_registered_total{intent="verification_agent|helper|tooling|unknown"}
```

Closed enum, four label values total; cardinality bound by construction.

## Migration policy

Per `docs/migration-policy.md`, there is no pre-existing inner-Job support to
preserve. The marker channel is additive; phase scripts that do not emit
markers continue to work and simply have no inner-Job rows.

There is no compatibility shim and no fallback. A future change that wants to
move registration to a different channel must remove this one in the same
work.

## Done criteria for this stage

- Phase script can emit a marker line and see the resulting child Job in the
  run report within one event-stream round trip.
- `glimmung_run_inner_jobs_registered_total{intent}` increments on each
  registration.
- Frontend shows the registered child Jobs as sub-rows under the parent Job,
  each with a Grafana deep-link.
- Tests cover: marker parser (happy + invalid JSON + missing required fields),
  event handler accepts the new event type, run report surfaces the row.
- `docs/observability.md` references this contract in the V1-deferred section
  the original "k8s Job terminal" line came from.

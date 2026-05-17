# Observability

Glimmung exposes a Prometheus `/metrics` endpoint on the existing app port,
alongside `/healthz`. The metric surface is domain-shaped — it describes
runs, decisions, leases, hot-swap outcomes, and the HTTP layer — not just
generic Go runtime data. The contract here is the names, labels, and
cardinality budget; treat changes to them like API changes.

## Endpoint

- Path: `GET /metrics`
- Port: same as the rest of the app (`8000`)
- Format: Prometheus text exposition + OpenMetrics
- Auth: none (same surface as `/healthz`; relies on in-cluster network
  scope, not on per-request auth)

The endpoint is served from
[`internal/metrics`](../internal/metrics/metrics.go) by a package-private
registry — the default Prometheus global registry is intentionally
unused so the metric set is explicit and testable in isolation.

## Metric families

All glimmung metrics are namespaced `glimmung_*`. Default Go runtime and
process collectors are also exposed (`go_*`, `process_*`).

### HTTP layer

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glimmung_http_requests_total` | counter | `pattern`, `method`, `status` | `pattern` is the Go 1.22+ `ServeMux` registered route (e.g. `GET /v1/runs/dispatch`), never raw URL. Unmatched requests get `pattern="unmatched"`. |
| `glimmung_http_request_duration_seconds` | histogram | `pattern`, `method` | Default Prometheus buckets. |

### Verify loop

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glimmung_decisions_total` | counter | `decision` | One increment per well-formed `decision.Decide()` return. Values: `retry`, `advance`, `abort_budget_attempts`, `abort_budget_cost`, `abort_malformed`. |
| `glimmung_budget_breaches_total` | counter | `reason` | Increments when `Decide()` returns an abort caused by budget. Values: `cost`, `attempts`. |

### Runs

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glimmung_runs_created_total` | counter | `workflow` | One per successful `CreateRun` at dispatch. |

Terminal-state run histograms (duration, attempt count, cumulative cost)
are intentionally out of scope for V1 — they need the run's
creation timestamp and cost data plumbed into `SetRunTerminalState`
callers, which is a separate plumbing change. Terminal *decisions* remain
fully observable via `glimmung_decisions_total`.

### Leases

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glimmung_leases_acquired_total` | counter | `purpose`, `outcome` | `purpose` is one of: `dispatch`, `advance`, `retry`, `resume`, `signal_drain`, `test_slot_checkout`. `outcome` is `granted`, `conflict`, or `error`. |
| `glimmung_leases_released_total` | counter | `purpose`, `outcome` | `outcome` is `cancelled` (admin), `expired` (TTL fired), or `completed` (consumer release). |
| `glimmung_leases_held` | gauge | `purpose` | Approximate. In-process delta of acquire/release; authoritative state lives in Cosmos. Per-purpose breakdown can drift because release sites do not always know the original acquire purpose; the total across purposes is correct. |
| `glimmung_lease_acquire_wait_seconds` | histogram | `purpose`, `outcome` | Exponential buckets from 10ms to ~40s. |

### Hot-swap

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glimmung_hot_swap_outcomes_total` | counter | `outcome` | One per `ApplyHotSwap` invocation. Values: `persisted`, `build_failed`, `swap_failed`, `timeout`. |
| `glimmung_hot_swap_duration_seconds` | histogram | `outcome` | Exponential buckets from 1s to ~17 min. |

## Cardinality

Every label is either a closed enum (`decision`, `outcome`, `verification`,
`purpose`, `reason`) or a registered identifier (`workflow`, `pattern`).
Raw user input — issue numbers, project slugs, repo URLs — never lands in
a label. To add a new label value:

- For HTTP `pattern`: register the route in
  [`internal/server/server.go`](../internal/server/server.go); the
  middleware reads `r.Pattern` directly so new routes are picked up
  automatically.
- For `decision`: add a constant in
  [`internal/domain/decision/decision.go`](../internal/domain/decision/decision.go);
  the deferred recorder picks it up.
- For lease `purpose`: add a constant to the `LeasePurpose*` set in
  [`internal/server/lease_api.go`](../internal/server/lease_api.go) and use
  it at the new acquire site.
- For hot-swap `outcome`: only the four named modes; adding one means
  updating both the Go path and the migration-check pattern in
  [`scripts/check-apply-test-slot-hot-swap-migration.mjs`](../scripts/check-apply-test-slot-hot-swap-migration.mjs).

Empty label values are coerced to `unknown` to prevent operators from
staring at blank label rows in Grafana.

## Deployment

Glimmung's Helm chart ships an opt-in ServiceMonitor and pod scrape
annotations. Both default off so installs work cleanly in clusters that
do not run the Prometheus Operator.

```yaml
# k8s/values.yaml — override as needed
observability:
  serviceMonitor:
    enabled: true                # require monitoring.coreos.com/v1 CRD
    interval: 30s
    scrapeTimeout: 10s
    namespace: ""                # default: chart namespace
    labels:
      release: kube-prometheus   # any selector your Prometheus CR uses
  scrapeAnnotations:
    enabled: true                # pod-level prometheus.io/* annotations
```

The per-issue chart at [`k8s/issue/`](../../k8s/issue/) ships the
`scrapeAnnotations` toggle but no ServiceMonitor — per-issue releases are
ephemeral and not normally scraped by central Prometheus. The `/metrics`
endpoint is always live on the pod for ad-hoc `kubectl port-forward`
debugging.

## Operating heuristics

These are the metrics that answer the operator-facing questions glimmung's
shape implies. They are not (yet) wired to alerting rules — alert
configuration belongs with the Prometheus deployment, not with glimmung's
chart.

| Question | Query |
|---|---|
| How many runs are dispatched per hour, by workflow? | `sum by (workflow) (rate(glimmung_runs_created_total[1h])) * 3600` |
| What fraction of attempts are passing on first try? | `1 - (sum(rate(glimmung_decisions_total{decision="retry"}[1h])) / sum(rate(glimmung_decisions_total[1h])))` |
| Is the cost ceiling firing? | `sum by (workflow) (rate(glimmung_budget_breaches_total{reason="cost"}[1h]))` |
| Hot-swap success rate (last 6h)? | `sum(rate(glimmung_hot_swap_outcomes_total{outcome="persisted"}[6h])) / sum(rate(glimmung_hot_swap_outcomes_total[6h]))` |
| Are leases stacking up? | `sum by (purpose) (glimmung_leases_held)` |
| AcquireLease p99 wait time? | `histogram_quantile(0.99, sum by (le, purpose) (rate(glimmung_lease_acquire_wait_seconds_bucket[5m])))` |
| HTTP error rate by route? | `sum by (pattern) (rate(glimmung_http_requests_total{status=~"5.."}[5m]))` |

## Out of scope (V1)

The V1 cut deliberately stops short of the following — each is a follow-up
PR with its own data-plumbing requirement:

- **Run histograms** (`run_duration_seconds`, `run_attempts`,
  `run_cost_usd`): require the run's created-at timestamp and cumulative
  cost at every `SetRunTerminalState` caller. Plumbing them through is a
  refactor, not a metric addition.
- **k8s Job apply/terminal metrics**: dispatch fires a Job and forgets;
  terminal state propagates back through callbacks with no single
  in-process site that owns "the Job is now terminal." The lease metric
  covers the queue side until the run-callback surface grows a
  job-terminal signal.
- **OpenTelemetry traces**: separate decision; metrics alone cover the
  operator questions above.
- **Per-metric alert rules**: belong with the Prometheus deployment, not
  with the chart that exposes the metrics.

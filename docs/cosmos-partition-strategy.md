# Cosmos partition strategy

Glimmung uses an Azure Cosmos DB account in serverless mode. Every container
in the `glimmung` database has a partition key chosen for the dominant
access pattern of the data it stores. Every query in
`internal/store/cosmos/` must be explicit about which partition(s) it
touches, because the Azure Go SDK (`azcosmos`) does not implement the
client-side query plan required for cross-partition `ORDER BY` / `DISTINCT`
/ `GROUP BY` / `OFFSET` / `TOP`. The Cosmos gateway returns
`400 BadRequest "The provided cross partition query can not be directly
served by the gateway"` for those shapes; in glimmung that 400 bubbled
through `writeInternalError` as a 5xx every minute on `GET /v1/touchpoints`
until the migration documented here.

## Container inventory

| Container | Partition key | Notes |
|---|---|---|
| `projects` | `/id` (project name) | Document id is the project name. Cross-partition list is small (~projects in org). |
| `workflows` | `/project` | One project owns many workflows. |
| `leases` | `/project` | One project owns many leases. Global concurrency caps require cross-partition reads of the small "claimed" subset. |
| `runs` | `/project` | One project owns many runs. Reverse lookups by `id` / `callback_token` / `(issue_repo, pr_number)` need explicit cross-partition or fan-out handling. |
| `runEvents` | `/project` | Co-located with runs. Always scope by project. |
| `issues` | `/project` | One project owns many issues. Reverse `id` lookups always have the project in scope from the calling context. |
| `locks` | `/scope` (e.g. `"issue"`, `"pr"`) | Cross-project lock namespace; partition is the lock kind, not the project. |
| `reports` | `/project` | Touchpoints; cross-project index requires fan-out. The original bug source. |
| `playbooks` | `/project` | One project owns many playbooks; cross-project list requires fan-out. |
| `signals` | `/target_repo` | Routed by target repo, not project. Cross-repo dispatch queue uses cross-partition scans without `ORDER BY`. |
| `slots` | `/project` | Test-slot allocator state. |
| `slot_history` | `/project` | Test-slot audit trail. |

If a new container is added or a partition key changes, update this table
in the same PR. The change-summary script does not enforce the inventory,
but reviewers will reject store changes where the partition strategy is
implicit.

## Query primitives

Three helpers live in `internal/store/cosmos/query.go`. Every Cosmos
query in this package must use one of them — the migration guard at
`scripts/check-cosmos-queries.mjs` fails CI if a direct
`NewQueryItemsPager` call or the retired `queryAll` / `queryAllWhere`
helpers reappear.

### `singlePartitionQuery`

Use when the query's predicates scope to one partition-key value. Pass
the partition key explicitly via `azcosmos.NewPartitionKeyString` (or
`Bool` / `Number`). `ORDER BY`, `OFFSET`, `LIMIT` all work locally
inside one partition.

```go
err := singlePartitionQuery(ctx, s.runs,
    azcosmos.NewPartitionKeyString(project),
    "SELECT * FROM c WHERE c.project = @project ORDER BY c.updated_at DESC",
    []azcosmos.QueryParameter{{Name: "@project", Value: project}},
    &docs,
)
```

Almost every callsite in glimmung is of this shape because the read
APIs are project-scoped.

### `crossPartitionQuery`

Use only when the query genuinely spans partitions *and* has no
`ORDER BY` / `DISTINCT` / `GROUP BY` / `OFFSET` / `TOP`. These are
primarily secondary-index lookups by id / callback token / etc. The
helper passes the empty partition key, and the Cosmos gateway scatters
and gathers the simple `WHERE` query in a single round trip.

```go
err := crossPartitionQuery(ctx, s.runs,
    "SELECT * FROM c WHERE c.callback_token = @token",
    []azcosmos.QueryParameter{{Name: "@token", Value: token}},
    &docs,
)
```

The helper has a runtime guard that rejects queries containing the
disallowed clauses; the migration guard then refuses to allow the empty
partition key anywhere else, so the choice between "fan out" and
"scatter-gather" is forced.

### `fanOutByProject`

Use when the query needs `ORDER BY` / `LIMIT` semantics across multiple
project partitions. The helper iterates the supplied project list,
binds `@project` per iteration, scopes the partition key, and appends
results to a single slice. The caller owns the final merge ordering
(sort the merged slice in Go) and any caller-side `Limit` enforcement.

```go
projects, err := s.listProjectNames(ctx)
if err != nil { return nil, err }

err = fanOutByProject(ctx, s.reports, projects,
    "SELECT * FROM c WHERE c.project = @project",
    nil,
    &touchpointDocs,
)
sort.SliceStable(touchpointDocs, func(i, j int) bool {
    return touchpointDocs[i].UpdatedAt > touchpointDocs[j].UpdatedAt
})
```

Per-partition queries that would benefit from `TOP @limit` (to bound
fan-out work when N projects grows) should add it explicitly — the
helper does not inject one.

## Decision matrix

When you write a new query, decide which primitive to use:

1. Does the caller know the partition key value (project, scope, etc.)?
   → `singlePartitionQuery`.
2. Does the query need `ORDER BY` / `DISTINCT` / `GROUP BY` / `OFFSET` /
   `TOP` across multiple partitions?
   → `fanOutByProject` (or a future `fanOutByScope` / `fanOutByRepo`
   sibling if a non-project container needs one).
3. Otherwise (genuinely cross-partition, simple `WHERE` only):
   → `crossPartitionQuery`.

If none of those fit, the right answer is usually a data-model change
(secondary-index container, denormalized lookup doc), not a new
primitive. Bring it to design review.

## Why the SDK can't do this for us

The Azure Cosmos query gateway implements two paths:

- **Direct path**: simple WHERE filters across one or many partitions
  (the latter via internal scatter-gather). Returns rows directly.
- **Query-plan path**: ORDER BY / DISTINCT / GROUP BY / OFFSET / TOP
  across multiple partitions. The gateway returns a query plan
  describing per-partition rewrites; the SDK then issues per-partition
  queries with the rewritten SQL, merges results client-side, and
  honors the ordering. C#, Java, and JS SDKs implement this. The Go
  SDK (`azcosmos`) does not — it surfaces the gateway's 400 as a hard
  error. The Cosmos response body even calls this out:

  > "This exception is traced, but unless you see it bubble up as an
  > exception (which only happens on older SDK clients), then you can
  > safely ignore this message."

The Go SDK is one of those "older SDK clients" by capability. Until it
implements query-plan handling (no public timeline at the time of
writing), the only options are the three primitives above.

## Observability

Every query through the three primitives is instrumented (Stage 2 of
the contract rollout). Five Prometheus families plus a structured
`slog.Error` on failure cover the surface:

| Metric | Labels | What it tells you |
|---|---|---|
| `glimmung_cosmos_queries_total` | `container`, `mode`, `outcome` | Rate of Cosmos queries by container, partition strategy (`single` / `cross` / `fanout`), and outcome (`success` / `error`). |
| `glimmung_cosmos_query_duration_seconds` | `container`, `mode` | Wall-clock duration histogram. Fan-out duration is the total across all per-partition iterations. |
| `glimmung_cosmos_query_ru_charge_total` | `container`, `mode` | Cumulative RU charge, summed across pages. Divide by `queries_total` for average RU per query. |
| `glimmung_cosmos_fanout_partitions_total` | `container` | Per-partition iterations executed by `fanOutByProject`. Divide by the fanout-mode `queries_total` for observed fan-out factor. |
| `glimmung_cosmos_query_plan_error_total` | `container` | The 400 BadRequest shape this contract migration retired. Direct alerting target: any nonzero rate is a regression of the original bug. |

`slog.Error` lines emitted at query failure include `container`,
`mode`, `duration_ms`, `ru_charge`, `query_plan_error` (bool),
`query` (whitespace-collapsed, 240-char-capped shape), and `err`.
The slog line uses the same field names as the metric labels so a
dashboard pivot and a log search land on the same dimensions.

## Migration backlog

This document is the source of truth for follow-on work that the
contract migration did not absorb. As of Stage 2:

- All read paths in `cosmos.go` go through the three primitives.
- The guard in `scripts/check-cosmos-queries.mjs` enforces the contract.
- Per-query observability ships via the metrics above (Stage 2 — this
  PR). Operators should add a Grafana row for the five families and an
  alert on `rate(glimmung_cosmos_query_plan_error_total[5m]) > 0`.
- `slot.go` and `slot_history.go` use `NewQueryItemsPager` directly
  with explicit partition keys; they predate the contract and the
  script allows them by name. They are **not** instrumented by the
  observability layer because they bypass the helpers. Next time
  they're touched, fold them into `singlePartitionQuery` so both the
  allowlist and the metrics gap shrink.
- Stage 3 (shipped): the SPA's 20-second `/v1/issues` + `/v1/touchpoints`
  poll was deleted. The "needs attention" pulse now derives from
  `StateSnapshot.InflightLocks` (two single-partition `locks` queries
  per snapshot tick, scoped to `/scope` = `"issue"` and `"pr"`). The
  cross-project fan-out the poll used to force against `reports`
  every 20s is gone; the only remaining `/v1/touchpoints` traffic is
  user-driven (one fetch when the user opens the touchpoints view).
- The once-a-minute external probe pointing at `/v1/touchpoints` is
  out of scope for this PR — it is not configured inside this repo
  and needs an owner conversation. `/healthz` is the right target
  for an availability probe; it is what the k8s readiness/liveness
  probes already use. Operators: retarget the external monitor at
  `/healthz` and delete the `/v1/touchpoints` probe.
- Stage 4 — `writeUnavailable` helper for deliberate 503 responses
  (test-slot saturation, etc.) so they leave a structured log line and
  a counter rather than being silent.

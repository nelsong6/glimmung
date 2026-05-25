# Postgres Migration: Glimmung Cosmos → Glimmung Postgres Flexible Server

Plan for retiring glimmung's `glimmung` database on the shared
`infra-cosmos-serverless` account and moving the same data to a dedicated
Azure Postgres Flexible Server (`glimmung-pg`) in the glimmung resource
group. Written per the bar in [docs/quality-timeframes.md](quality-timeframes.md)
and the cutover contract in [docs/migration-policy.md](migration-policy.md).

This migration is **glimmung-scoped only**. The shared
`infra-cosmos-serverless` account stays — it still hosts ambience, kill-me,
my-homepage, and investing, each of which migrates (or doesn't) under its own
separate plan in its own repo. Cross-app migration is explicitly not the
shape of this work.

## Why migrate

The same access-pattern argument tank-operator made in #466. Glimmung's
workload is single-region, relational, modest write volume, and benefits
materially from real indexes, PITR backups, and deterministic TTL. Cosmos's
per-RU write cost was the dominant line item in tank-operator's pre-migration
bill (~$73/mo for one app); glimmung's container count is larger (9+) and
its write volume is comparable or higher. Postgres at B1ms is ~$15/mo flat.

Beyond cost, the migration unblocks two existing pain points that the Cosmos
shape forces:

- **Locks are leaning on id-uniqueness as a primitive.** `internal/store/cosmos/cosmos.go`'s `acquire` path relies on deterministic `lockDocID(scope, key)` and Cosmos's 409 on duplicate create; `release` uses ETag IfMatch optimistic concurrency. Postgres expresses this directly as a row with `PRIMARY KEY (scope, key)` and an atomic `INSERT ... ON CONFLICT DO UPDATE WHERE ...` — fewer moving parts, no ETag round-trips.
- **`run_events` TTL is stochastic.** Cosmos `default_ttl = 604800` deletes documents in background sweeps with no precise guarantee on timing. `pg_cron` on Flexible Server gives a deterministic daily delete; the cleanup is a real scheduled job with a real metric.

## Why a dedicated server, not the shared cosmos account's replacement

Tank-operator's pattern is one app, one server: its Postgres lives in
`tank-operator/infra/postgres.tf` and serves only tank-operator. Glimmung
matches that pattern. Reasons not to share with the other 4 small apps:

- Other apps haven't been scoped for migration. Provisioning a shared
  platform server now would be a speculative move with no migrating
  consumers.
- Per-server cost is flat; one dedicated B1ms costs the same as a shared
  B1ms hosting one app — no savings from sharing until at least 2 apps
  ride it.
- Per-app servers preserve blast-radius independence by construction.
  No "sized for headroom" tier math, no shared maintenance window.

If other apps later migrate, each gets its own dedicated server under the
same pattern, owned by its own repo. A shared platform server is not the
target shape.

## Why managed Flexible Server, not CloudNativePG

CNPG is installed in the cluster but has zero production users in the org.
The AKS cluster's pool is sized for mixed platform + app workloads; adding
1-3 Postgres pods and PVCs taxes the RAM/disk slots the cluster is already
careful with. Flexible Server runs outside the cluster — no PVC pressure,
no operator pods, no PG CPU/RAM on the workload pool.

The proven runtime in this org is Flexible Server (tank-operator #466).
A glimmung migration off Cosmos onto an unproven runtime would put two
unproven moving parts on the critical path. Not the shape of this work.

## Consumer inventory (glimmung's current Cosmos surface)

Read directly from [tofu/db.tf](../tofu/db.tf) at investigation time. Stage 0
re-verifies against main.

| Container | Partition key | Notes |
|---|---|---|
| projects | `/name` | low cardinality reference data |
| workflows | `/project` | per-project workflow registry |
| leases | `/project` | run leases |
| runs | `/project` | one doc per (project, issue_number) accumulating attempts |
| run_events | `/project` | ordered native-runner event/log stream, **TTL=7d**. RU usage actively painful — `f62cd03 Reduce Cosmos RU usage for native logs` is in the history. |
| locks | `/scope` | id-uniqueness used as mutex primitive (issue, pr, signal-drain, …) |
| signals | `/target_repo` | webhook signal queue |
| issues | `/project` | first-class glimmung issue model |
| playbooks | `/project` | operator-authored issue-spec batches |
| reports | `/project` | run reports |
| slots | `/project` | per-(project, slot_index) test-slot state. Added #518. |
| slot_history | `/project` | append-only test-slot return history. Added #518. uuid id assigned at write, typically queried by (project, slot_index). |

Verified against `git show origin/main:tofu/db.tf` — 12 containers as of Stage 0. If main moves before Stage 2 ships, re-run the grep.

Go code: `internal/store/cosmos/cosmos.go`, `cosmos_test.go`,
`live_smoke_test.go`. Anything else importing the cosmos package gets
deleted with it.

## Target architecture

```
glimmung/tofu/postgres.tf          # New: provisions glimmung-pg (B1ms)
glimmung/tofu/db.tf                # Deleted at Stage 2
glimmung-pg (Flexible Server, glimmung resource group)
├── database: glimmung
│   ├── tables: projects, workflows, leases, runs, run_events, locks,
│   │   signals, issues, playbooks, reports (+ any post-#518)
│   ├── extension: pg_cron (for run_events TTL job)
│   └── role grant: glimmung-identity (Entra admin)
└── AAD admin: glimmung-identity UAMI (existing, repurposed)
```

- **Sizing**: B1ms (1 vCore burstable, 2 GiB RAM). Matches tank-operator.
- **Storage**: 32 GiB Premium SSD. Grows online.
- **Connections**: glimmung's pgxpool configured at `MaxConns = 6` per replica × ~3 replicas = ~18 concurrent worst case. Sits comfortably under B1ms's ~50 default `max_connections`. Alert trigger at sustained >35 conns/10min (covers room for replica growth).
- **Auth**: AAD via the existing `glimmung-identity` UAMI, no password in steady state. Break-glass password in KV. Same shape as `tank-operator/infra/postgres.tf`.
- **Backups**: PITR enabled, 7-day retention (default).
- **Network**: public_network_access_enabled=true with `0.0.0.0` Azure-services firewall rule. AAD at the data plane is the gate. VNet integration is a later tightening if/when needed.
- **Extensions**: `pg_cron` enabled via `azure.extensions = 'PG_CRON'` server parameter, then `CREATE EXTENSION pg_cron` in the glimmung database.
- **Maintenance window**: configured to a low-traffic hour. Single-AZ, no HA tier — burstable B-series doesn't support HA. Acceptable: tank-operator runs the same shape.

## Stage plan

Stage 0 is a checklist (no PR). Stages 1-3 are PRs in dependency order.

### Stage 0: Surface verification

Before Stage 1 ships:

- [ ] `grep -rn 'azurerm_cosmosdb_sql_container' tofu/` against current main to confirm the container list above matches reality (catches anything added after #518).
- [ ] `grep -rn '"glimmung"' ../*/tofu/ ../*/infra/` org-wide to confirm no other repo references the `glimmung` Cosmos database name.
- [ ] `grep -rn 'glimmung-identity\|glimmung_dedicated' ../*/tofu/ ../*/infra/` to confirm the UAMI is glimmung-owned and no other repo grants Cosmos access using it.
- [ ] Confirm mcp-azure-personal's `cosmos_query_items` against the glimmung database has no automated downstream consumers (one-off operator queries are fine — they just stop working post-cutover, and the post-migration `pg_query` path replaces them).

### Stage 1: Provision `glimmung-pg` (1 PR)

- New: `glimmung/tofu/postgres.tf`. Mirrors `tank-operator/infra/postgres.tf` (server resource, AAD admin via UAMI, firewall rule, public access, PITR, `azure_extensions = 'PG_CRON'`).
- New: `azurerm_postgresql_flexible_server_active_directory_administrator` for `glimmung-identity` (the existing UAMI from `tofu/identity.tf`).
- New: `azurerm_postgresql_flexible_server_database "glimmung"` for the per-database resource.
- New: KV secret `glimmung-pg-admin-password` for break-glass admin.
- **No app changes yet.** Glimmung still reads/writes Cosmos. Server sits idle.
- Acceptance: `psql` connects from a workstation via `az login`-issued AAD token. `CREATE EXTENSION pg_cron;` succeeds inside the database. PITR backups appear in the Azure portal.

This PR is independently safe to land — nothing breaks if it stays applied indefinitely with no follow-up.

### Stage 2: Glimmung migration (1 PR — the substantive one)

This is the cutover. Per migration-policy.md, atomic: Cosmos out, Postgres in, no dual-write, no fallback flag, no compatibility read path.

**Tofu changes:**
- Deleted: `tofu/db.tf` (every `azurerm_cosmosdb_sql_database` + `azurerm_cosmosdb_sql_container` resource).
- Deleted from `tofu/identity.tf`: the `azurerm_cosmosdb_sql_role_assignment.glimmung_dedicated_cosmos` resource and the `data.azurerm_cosmosdb_account.infra` data source it depended on (if not used elsewhere in glimmung's tofu — verify).
- The `glimmung-identity` UAMI itself stays. The federated credential, ACR roles, subscription Contributor/RBAC Admin grants stay. Only the Cosmos data-plane assignment moves to Postgres-AAD-admin.

**Go code changes:**
- New: `internal/store/pg/` package. Mirrors the shape of `tank-operator/backend-go/internal/pgstore/`:
  - `connect.go` — pgxpool config with AAD-token `BeforeConnect`, `MaxConnLifetime = 50 * time.Minute`, `MaxConns = 6`, `MinConns = 1`. Token scope `https://ossrdbms-aad.database.windows.net/.default`.
  - `migrations.go` — embedded SQL applied at startup. Idempotent.
  - One file per logical container (`projects.go`, `workflows.go`, `runs.go`, `run_events.go`, `locks.go`, `signals.go`, `issues.go`, `playbooks.go`, `reports.go`, `leases.go`, `slots.go`, `slot_history.go`). Slot tables shape follows `docs/test-slot-storage-rework.md`.
- Deleted: entire `internal/store/cosmos/` directory.
- Deleted from `go.mod`: `github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos`. `azcore` stays (still needed for AAD token).
- All callers of the cosmos store package updated to the pg package. The interface shape may or may not be identical — design choice during PR.

**Schema design (notable bits):**

- **`locks`** — replaces Cosmos id-uniqueness primitive:
  ```sql
  CREATE TABLE locks (
    scope text NOT NULL,
    key text NOT NULL,
    holder_id text,
    state text NOT NULL CHECK (state IN ('held', 'released')),
    expires_at timestamptz,
    acquired_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, key)
  );
  ```
  - Acquire: `INSERT INTO locks (scope, key, holder_id, state, expires_at) VALUES ($1, $2, $3, 'held', $4) ON CONFLICT (scope, key) DO UPDATE SET holder_id = EXCLUDED.holder_id, state = 'held', expires_at = EXCLUDED.expires_at, acquired_at = now() WHERE locks.state = 'released' OR locks.expires_at < now() RETURNING ...` — single atomic statement, no read-modify-write race.
  - Release: `UPDATE locks SET state = 'released' WHERE scope = $1 AND key = $2 AND holder_id = $3 AND state = 'held'`. The ETag IfMatch semantics fall out of the row-level lock + holder_id filter.
  - Index for diagnostic queries by scope is implicit in the PK leading column.

- **`run_events`** — replaces TTL=7d container:
  ```sql
  CREATE TABLE run_events (
    run_id text NOT NULL,
    attempt_index int NOT NULL,
    job_id text NOT NULL,
    seq int NOT NULL,
    project text NOT NULL,
    event text NOT NULL,
    payload jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, attempt_index, job_id, seq)
  );
  CREATE INDEX run_events_by_project_created_at ON run_events (project, created_at DESC);
  CREATE INDEX run_events_ordered ON run_events (run_id, attempt_index, seq);
  ```
  - Idempotent insert: `INSERT ... ON CONFLICT (run_id, attempt_index, job_id, seq) DO NOTHING`. Replaces the current 409-and-accept-replay path.
  - TTL via `pg_cron`:
    ```sql
    SELECT cron.schedule(
      'run_events_ttl',
      '0 4 * * *',
      $$DELETE FROM run_events WHERE created_at < now() - interval '7 days'$$
    );
    ```
  - Deterministic daily delete vs. Cosmos's stochastic sweep. Operationally an improvement.

- **Other 8 containers** — direct table-per-container translation. Cosmos partition key becomes the leading column of the table's primary index (often combined with the doc id field). Documents map to rows where Cosmos used JSON shapes; columns that get filtered or sorted get real Postgres columns, the rest live in a `payload jsonb`. Specific column extraction is owned by the Stage 2 PR.

**Chart changes** (in `k8s/`):
- `values.yaml`: new `postgres.host`, `postgres.database`, `postgres.user` keys.
- Deployment env vars: `POSTGRES_HOST`, `POSTGRES_DATABASE`, `POSTGRES_USER` wired from values.
- No password Secret (workload identity issues the token; no static password reaches the pod). The KV `glimmung-pg-admin-password` exists for break-glass only.

**Migration guard** (cross-cutting):
- New: `scripts/check-removed-cosmos.mjs` (or `.sh`). Fails CI on any of:
  - `azcosmos` import
  - `azurerm_cosmosdb_` resource
  - `data.azurerm_cosmosdb_account` reference
  - The string `infra-cosmos-serverless` (except in this doc and in a banner comment somewhere explaining the migration)
  - `cosmosSessionEventStore`-style symbols
- Wired into the existing CI lint job. Pattern mirrors `tank-operator/scripts/check-removed-chat-runtime.mjs`.

**Tests:**
- Deleted: `internal/store/cosmos/cosmos_test.go`, `live_smoke_test.go`.
- New: `internal/store/pg/*_test.go` — contract assertions, not implementation detail. Live-DB integration tests gated on `POSTGRES_HOST` env var, run in CI against a transient container.
- Convert any "matches Cosmos impl" assertions to "matches the contract."

**Observability** (mandatory per quality-timeframes.md):
- Counter: `glimmung_pg_queries_total{operation,result}` — operation keyed by logical name (`acquire_lock`, `list_issues`, `record_run_event`, …). Labels low-cardinality.
- Histogram: `glimmung_pg_query_duration_seconds{operation}`.
- Counter: `glimmung_run_events_ttl_deleted_total` — incremented by the pg_cron job's wrapper. Confirms TTL cleanup is happening.
- Alert: `glimmung_pg_queries_total{result="error"}` rate > 1% over 5min → Grafana alert.
- Alert: connection count (from `pg_stat_activity` exporter) > 35 sustained 10min → tier upgrade signal.

**Acceptance evidence in the PR description:**
- A logged-in glimmung dashboard session showing live data after cutover (e.g., issue list renders with rows that exist in `issues` table).
- A locks acquire/release cycle observed in metrics (acquire_lock counter increments, release succeeds).
- A run_events insert + 7-day-old test row deleted by the cron job's first run.
- `grep -rn azcosmos .` returns zero matches in the glimmung codebase.

### Stage 3: mcp-azure-personal gets pg_query access to glimmung-pg (1 PR)

- New in `mcp-azure-personal/infra/main.tf`: `azurerm_postgresql_flexible_server_active_directory_administrator` for the mcp-azure-personal UAMI on `glimmung-pg`. Mirrors the existing block for tank-operator's Postgres.
- The `cosmos_query_items` tool **stays**. The shared `infra-cosmos-serverless` account still hosts ambience, kill-me, my-homepage, investing — the tool remains valid for those.
- The data-plane role assignment on `infra-cosmos-serverless` for mcp-azure-personal stays (covers the 4 remaining apps).
- Acceptance: from an MCP session, `pg_query` against the glimmung database returns rows.

This PR depends on Stage 2 landing (the database needs to have real data before the query path is useful).

## Per-PR migration-policy guards

Each stage's PR must include in its description:

- The grep evidence that no live Cosmos reference exists in the affected scope. (Stage 1 has none yet by design; Stage 2 has the substantive evidence; Stage 3 confirms mcp-azure-personal kept Cosmos access intentionally for non-glimmung apps.)
- A named contract this PR upholds (e.g., "glimmung's locks primitive is now atomic Postgres rows; no read-modify-write race").
- Acceptance evidence per the bullets above, concrete and reproducible.

Per migration-policy.md: keywords `legacy`, `fallback`, `temporary`, `compat`, `exception` in the Stage 2 diff are review-blocks by default. Name what they preserve and why, or delete them.

## Failure handling

migration-policy.md forbids fallbacks. Concretely:

- If Stage 1 apply fails (Azure-side provisioning error), the server simply doesn't exist yet — nothing else is broken. Re-run apply.
- If Stage 2 ships and a query is wrong, **fix forward**: a new PR that fixes the schema, query, or index. Not a revert to Cosmos.
- If Stage 2 cutover reveals a missed code path (some caller of the cosmos store we didn't grep), that's a blocker. The PR doesn't merge until the caller is converted; no half-migrated state lands.
- If Stage 3 reveals an automated consumer of `cosmos_query_items` against the glimmung database (Stage 0 grep is supposed to catch this), surface it before Stage 2 merges. After Stage 2 merges, the data is gone from Cosmos and the consumer is broken — fix-forward via Postgres.

Acceptable rollback shape: **the migration didn't ship yet**. Never **the migration shipped and we fell back**.

## What this plan does *not* cover

- Migration of ambience, kill-me, my-homepage, investing. Each is a separate decision under its own plan; the shared `infra-cosmos-serverless` account stays for them.
- VNet integration / private endpoint for `glimmung-pg`. Matches tank-operator's current public + AAD-gated shape. Revisit when a concrete data-residency requirement appears.
- HA tier. B-series doesn't support HA; this is a deliberate cost/availability trade. If glimmung's availability requirements ever exceed PITR + ~1 min restart, fold to GP-tier.
- Specific index tuning beyond the skeletons above. The Stage 2 PR owns this and may add or change indexes after live-traffic measurement.
- Folding glimmung-pg into a shared platform server later. Not on the roadmap; if it ever happens, that's a separate migration with its own plan.

## Living document

This is the single source of truth for the glimmung migration. Each PR in
the sequence links to it. If reality diverges from the plan during
execution, update the plan in the same PR that diverges — not in a
follow-up.

# Postgres Migration Record

Glimmung's durable runtime state now lives in Azure Database for PostgreSQL
Flexible Server (`glimmung-pg`). The previous Glimmung database on the shared
`infra-cosmos-serverless` account is retired for this repo.

This document is the historical record and operational contract for that
migration. It is the only repo file, besides the retired-store guard itself,
that may name the retired system. Live code, active docs, Terraform, workflow
configuration, metrics, and filenames must not carry Cosmos references.

## Current Runtime Shape

- Runtime store wrapper: `internal/store/store`.
- Postgres connection, migrations, table stores, and query tracer:
  `internal/store/pg`.
- Production wiring: `cmd/glimmung-go/main.go` constructs the pgx pool, applies
  schema migrations, wires every per-table store into the wrapper, and starts
  HTTP/reconciler work only with the Postgres-backed runtime store.
- Infrastructure: `tofu/postgres.tf` provisions `glimmung-pg`, the `glimmung`
  database, pg_cron preload, AAD admin binding for `glimmung-identity`, and the
  break-glass admin password.
- Kubernetes auth: the API/dashboard pod uses workload identity. The app uses
  AAD tokens for pgx connections; no static Postgres password is mounted into
  the pod.

## Durable Tables

The active schema is applied idempotently at startup from
`internal/store/pg/migrations.go`.

Core tables:

- `projects`
- `workflows`
- `workflow_schemas`
- `leases`
- `runs`
- `run_events`
- `locks`
- `signals`
- `issues`
- `issue_comments`
- `issue_counters`
- `lease_counters`
- `playbooks`
- `reports`
- `portfolios`
- `slots`
- `slot_history`
- `touchpoints`
- `test_lease_defaults`

The JSON payload columns preserve Glimmung's domain shapes where decomposing
every nested field would add churn without improving query behavior. Fields
used for identity, filtering, sorting, uniqueness, or TTL live in typed columns
or indexes.

## Required Guarantees

Per `docs/migration-policy.md`, this migration is complete only while these
remain true:

- no `azcosmos` dependency or import;
- no live store file, package, route, allocator, executor, UI, metric, or
  script that uses Cosmos;
- no Terraform resources or data sources for Glimmung Cosmos databases,
  containers, or role assignments;
- no docs outside this migration record that describe Cosmos as supported or
  canonical for Glimmung;
- no compatibility, fallback, or read-only runtime path to old Glimmung data;
- no tests asserting behavior against the retired store.

The CI guard `scripts/check-removed-retired-store.mjs` enforces that contract
repo-wide. It deliberately allows this document so operators can understand the
cutover history without reopening a live path.

## Operational Notes

- Query observability is Postgres-native:
  `glimmung_pg_queries_total{operation,outcome}` and
  `glimmung_pg_query_duration_seconds{operation}` are emitted through the pgx
  query tracer.
- `run_events` retention is deterministic: the pg_cron `run_events_ttl` job
  deletes rows older than seven days.
- Locks use a real relational primitive: `(scope, key)` is the primary key, and
  acquire is a single `INSERT ... ON CONFLICT DO UPDATE` statement gated on
  released or expired rows.
- Run, issue, workflow, touchpoint, signal, playbook, and slot APIs are all
  backed by Postgres stores. If one of those paths is wrong, fix forward in the
  Postgres path.

## What Stays Out Of Scope

The shared `infra-cosmos-serverless` account may still serve other repos. This
repo must not depend on it for Glimmung runtime behavior.

The `mcp-azure-personal` `cosmos_query_items` tool can continue to exist for
other apps. Glimmung operator data-plane inspection should use the Postgres
query path instead.

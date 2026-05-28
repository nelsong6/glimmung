package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaMigrations are run idempotently at backend startup under a Postgres
// advisory lock so concurrent replicas don't race on CREATE statements.
//
// All schema definitions use `IF NOT EXISTS` so a re-run is a no-op. Schema
// changes go in as new entries appended to this slice with their own
// `IF NOT EXISTS` semantics — there is no version table. This matches the
// pattern tank-operator's pgstore established.
//
// Schema design notes per table are in docs/postgres-migration.md.
var schemaMigrations = []string{
	// ------------------------------------------------------------------
	// pg_cron extension. Server params (azure.extensions=PG_CRON,
	// shared_preload_libraries=pg_cron, cron.database_name=glimmung) were
	// set in tofu/postgres.tf at Stage 1 so the extension can be created
	// here. The extension installs its schema in the `cron` namespace.
	// ------------------------------------------------------------------
	`CREATE EXTENSION IF NOT EXISTS pg_cron`,

	// ------------------------------------------------------------------
	// projects — low-cardinality reference data. Name is the primary key and
	// the rest is stored as jsonb
	// for forward compatibility with whatever shape future fields take.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS projects (
		name              text PRIMARY KEY,
		kind              text NOT NULL DEFAULT 'project',
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		updated_at        timestamptz NOT NULL DEFAULT now()
	)`,

	// ------------------------------------------------------------------
	// workflows — one row per (project, name). Workflow-schema documents get
	// their own table
	// rather than sharing a discriminator column, because the read paths
	// are distinct (workflow CRUD vs. schema lookup).
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS workflows (
		project           text NOT NULL,
		name              text NOT NULL,
		schema_ref        text NOT NULL DEFAULT '',
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		updated_at        timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project, name)
	)`,

	`CREATE TABLE IF NOT EXISTS workflow_schemas (
		project           text NOT NULL,
		schema_ref        text NOT NULL,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project, schema_ref)
	)`,

	// ------------------------------------------------------------------
	// leases — the callback_token is a uuid the native runner presents; the
	// existing code path looks leases up by token, so it gets a real index.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS leases (
		id                text PRIMARY KEY,
		project           text NOT NULL,
		callback_token    text NOT NULL,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		updated_at        timestamptz NOT NULL DEFAULT now(),
		expires_at        timestamptz
	)`,
	`CREATE INDEX IF NOT EXISTS leases_by_project
		ON leases (project)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS leases_by_callback_token
		ON leases (callback_token)
		WHERE callback_token <> ''`,

	// ------------------------------------------------------------------
	// runs — one row per run, with project as the leading key for per-project
	// query locality.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS runs (
		id                text NOT NULL,
		project           text NOT NULL,
		issue_number      int,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		updated_at        timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project, id)
	)`,
	`CREATE INDEX IF NOT EXISTS runs_by_project_issue
		ON runs (project, issue_number)
		WHERE issue_number IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS runs_by_project_updated
		ON runs (project, updated_at DESC)`,

	// ------------------------------------------------------------------
	// run_events — the primary key matches the natural (run_id,
	// attempt_index, job_id, seq) event identity. TTL is handled by pg_cron
	// scheduled in this same migration block.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS run_events (
		run_id            text NOT NULL,
		attempt_index     int NOT NULL,
		job_id            text NOT NULL,
		seq               int NOT NULL,
		project           text NOT NULL,
		event             text NOT NULL,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (run_id, attempt_index, job_id, seq)
	)`,
	`CREATE INDEX IF NOT EXISTS run_events_by_project_created_at
		ON run_events (project, created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS run_events_ordered
		ON run_events (run_id, attempt_index, seq)`,
	// Rich nativeEventDoc fields use typed columns so `event`, `phase`, and
	// per-job `step_slug` queries can use real indexes if they grow.
	`ALTER TABLE run_events ADD COLUMN IF NOT EXISTS phase text NOT NULL DEFAULT ''`,
	`ALTER TABLE run_events ADD COLUMN IF NOT EXISTS step_slug text NOT NULL DEFAULT ''`,
	`ALTER TABLE run_events ADD COLUMN IF NOT EXISTS message text NOT NULL DEFAULT ''`,
	`ALTER TABLE run_events ADD COLUMN IF NOT EXISTS exit_code int`,
	`ALTER TABLE run_events ADD COLUMN IF NOT EXISTS metadata jsonb NOT NULL DEFAULT '{}'::jsonb`,

	// github_repo and created_at columns are surfaced from project payload
	// jsonb for indexed lookups; test_lease_defaults owns global TTL settings.
	`ALTER TABLE projects ADD COLUMN IF NOT EXISTS github_repo text NOT NULL DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS test_lease_defaults (
		id                          text PRIMARY KEY,
		global_ttl_seconds          int NOT NULL DEFAULT 0,
		hot_swap_min_ttl_seconds    int NOT NULL DEFAULT 0,
		created_at                  timestamptz NOT NULL DEFAULT now(),
		updated_at                  timestamptz NOT NULL DEFAULT now()
	)`,

	// portfolios — each (project, route, element_id) is unique.
	`CREATE TABLE IF NOT EXISTS portfolios (
		project    text NOT NULL,
		route      text NOT NULL,
		element_id text NOT NULL,
		payload    jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project, route, element_id)
	)`,
	`CREATE INDEX IF NOT EXISTS portfolios_by_project_updated
		ON portfolios (project, updated_at DESC)`,

	// Clean up mis-typed rows that landed in `reports` before touchpoints had
	// their own table. Filter by `repo` field presence: touchpoint payloads
	// carry a repo field; report payloads do not.
	`DELETE FROM reports WHERE payload ? 'repo'`,

	// ------------------------------------------------------------------
	// locks — acquire uses the atomic "INSERT ... ON CONFLICT DO UPDATE WHERE
	// state='released' OR expires_at < now()" pattern documented in
	// docs/postgres-migration.md. Release filters on holder_id.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS locks (
		scope             text NOT NULL,
		key               text NOT NULL,
		holder_id         text,
		state             text NOT NULL CHECK (state IN ('held', 'released')),
		expires_at        timestamptz,
		acquired_at       timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (scope, key)
	)`,
	`CREATE INDEX IF NOT EXISTS locks_active_by_scope
		ON locks (scope, expires_at)
		WHERE state = 'held'`,

	// ------------------------------------------------------------------
	// signals — webhook signal queue. Per-repo drain is a single index scan
	// with target_repo as the leading column.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS signals (
		id                text PRIMARY KEY,
		target_repo       text NOT NULL,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		processed_at      timestamptz
	)`,
	`CREATE INDEX IF NOT EXISTS signals_unprocessed_by_repo
		ON signals (target_repo, created_at)
		WHERE processed_at IS NULL`,

	// ------------------------------------------------------------------
	// issues — first-class glimmung issue model. issue_number is per-project
	// unique. Comments live in a child table so they can be patched without
	// rewriting the entire issue payload.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS issues (
		project           text NOT NULL,
		number            int NOT NULL,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		archived_at       timestamptz,
		created_at        timestamptz NOT NULL DEFAULT now(),
		updated_at        timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project, number)
	)`,
	`CREATE INDEX IF NOT EXISTS issues_active_by_project
		ON issues (project, updated_at DESC)
		WHERE archived_at IS NULL`,

	`CREATE TABLE IF NOT EXISTS issue_comments (
		id                text PRIMARY KEY,
		project           text NOT NULL,
		issue_number      int NOT NULL,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		updated_at        timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS issue_comments_by_issue
		ON issue_comments (project, issue_number, created_at)`,

	// ------------------------------------------------------------------
	// issue_counters — per-project next-issue-number allocator. next_number
	// stores the value to allocate on the next CreateIssue call; on each
	// allocation the row is incremented and the prior value is returned.
	// First write seeds from MAX(number) + 1 of existing rows.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS issue_counters (
		project           text PRIMARY KEY,
		next_number       int NOT NULL
	)`,

	// ------------------------------------------------------------------
	// lease_counters — per-project next-lease-number allocator. Same
	// shape as issue_counters. Seeded from MAX(payload->>'leaseNumber'::int) + 1 across
	// existing leases on first call per-project.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS lease_counters (
		project           text PRIMARY KEY,
		next_number       int NOT NULL
	)`,

	// ------------------------------------------------------------------
	// playbooks — operator-authored batches of issue specs.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS playbooks (
		project           text NOT NULL,
		name              text NOT NULL,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		updated_at        timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project, name)
	)`,

	// ------------------------------------------------------------------
	// reports — run reports keyed by id; project leads the index for
	// per-project list queries.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS reports (
		id                text PRIMARY KEY,
		project           text NOT NULL,
		issue_number      int,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		updated_at        timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS reports_by_project_issue_updated
		ON reports (project, issue_number, updated_at DESC)`,

	// ------------------------------------------------------------------
	// slots — project and slot_index form the composite primary key. Per-slot
	// writes don't contend because each slot is its own row.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS slots (
		project           text NOT NULL,
		slot_index        int NOT NULL,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		updated_at        timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project, slot_index)
	)`,

	// ------------------------------------------------------------------
	// slot_history — append-only test-slot return history. uuid id assigned
	// at write time; queries are project-scoped and typically filter by
	// slot_index.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS slot_history (
		id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		project           text NOT NULL,
		slot_index        int NOT NULL,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS slot_history_by_project_slot
		ON slot_history (project, slot_index, created_at DESC)`,

	// ------------------------------------------------------------------
	// touchpoints — operator-visible per-issue activity. Per-project lookups
	// and per-issue single reads are the dominant access pattern.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS touchpoints (
		project           text NOT NULL,
		issue_number      int NOT NULL,
		payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at        timestamptz NOT NULL DEFAULT now(),
		updated_at        timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project, issue_number)
	)`,
	`CREATE INDEX IF NOT EXISTS touchpoints_by_project_updated
		ON touchpoints (project, updated_at DESC)`,

	// ------------------------------------------------------------------
	// slot_inspections — durable ledger for free (lease-scoped) inspections
	// uploaded through POST /v1/inspections. Run-bound inspections (caller
	// in a Run context) are tracked by existing Run evidence machinery and
	// are not indexed here. Sweep runs at lease-cleanup time, keyed by
	// lease_id. Idempotent inserts are deduplicated by (lease_id, request_id)
	// when X-Inspection-Request-Id is supplied.
	// ------------------------------------------------------------------
	`CREATE TABLE IF NOT EXISTS slot_inspections (
		id                       text PRIMARY KEY,
		project                  text NOT NULL,
		slot                     text NOT NULL DEFAULT '',
		lease_id                 text NOT NULL,
		session_id               text NOT NULL DEFAULT '',
		request_id               text NOT NULL DEFAULT '',
		blob_prefix              text NOT NULL,
		report_blob_path         text NOT NULL,
		screenshot_blob_path     text NOT NULL,
		screenshot_content_type  text NOT NULL DEFAULT 'image/png',
		byte_size_screenshot     bigint NOT NULL DEFAULT 0,
		byte_size_report         bigint NOT NULL DEFAULT 0,
		created_at               timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS slot_inspections_by_lease
		ON slot_inspections (lease_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS slot_inspections_by_request
		ON slot_inspections (lease_id, request_id)
		WHERE request_id <> ''`,
}

// cronJobs are scheduled after the table migrations succeed. Each
// `cron.schedule` is idempotent at the schedule-name level: re-scheduling
// the same name is a no-op (pg_cron upserts on jobname). Server
// configuration `cron.database_name = glimmung` (set in tofu/postgres.tf)
// routes the job into this database so the DELETE runs against the
// right table.
var cronJobs = []string{
	`SELECT cron.schedule(
		'run_events_ttl',
		'0 4 * * *',
		$$DELETE FROM run_events WHERE created_at < now() - interval '7 days'$$
	)`,
}

// migrationsAdvisoryLockKey is an arbitrary stable 64-bit value used to
// serialize schema-migration runs across replicas via pg_advisory_lock.
// Any constant works as long as it doesn't collide with another caller's
// lock. Different from tank-operator's (7164301728471038113) because
// these are different servers anyway.
const migrationsAdvisoryLockKey int64 = 6219384721650183974

// RunMigrations applies every entry in schemaMigrations under a session-
// scoped advisory lock, then ensures cronJobs are registered. Safe to
// invoke at backend startup; idempotent on re-run.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("pg: acquire migration conn: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationsAdvisoryLockKey); err != nil {
		return fmt.Errorf("pg: take migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrationsAdvisoryLockKey)
	}()

	for i, stmt := range schemaMigrations {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: migration %d failed: %w", i, err)
		}
	}
	for i, stmt := range cronJobs {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: cron job %d failed: %w", i, err)
		}
	}
	return nil
}

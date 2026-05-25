package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunEventsStore is the Postgres-backed event log for glimmung's native
// runner. Replaces cosmos.Store.runEvents (the `run_events` container)
// per Stage 2c of docs/postgres-migration.md.
//
// The schema lives in pg/migrations.go (Stage 2a) and gets extended
// here via additional ALTER TABLE entries that are idempotent.
//
// Idempotent insert uses `INSERT ... ON CONFLICT DO NOTHING` against the
// natural primary key (run_id, attempt_index, job_id, seq). Stage 2c
// preserves the cosmos contract: a duplicate write with the same PK and
// the same payload is accepted as a no-op; a duplicate write with the
// same PK and a different payload returns ErrConflict.
//
// run_events ages out via the pg_cron `run_events_ttl` job scheduled in
// pg/migrations.go (daily DELETE of rows older than 7 days), which
// replaces the Cosmos `default_ttl = 604800` stochastic background sweep.
// Stage 2c is the first sub-stage that puts rows into the table, so the
// cron's first non-trivial DELETE happens 7 days after this lands.
type RunEventsStore struct {
	pool *pgxpool.Pool
}

// RunEventRow is the row shape used to insert/return events. Mirrors the
// fields cosmos.Store used in nativeEventDoc; the runtime conversion
// happens in cosmos.Store's RecordNativeEventByID / ListNativeEventsByID
// helpers (which decompose / recompose between the cosmos-shaped doc and
// this row).
type RunEventRow struct {
	RunID        string
	AttemptIndex int
	JobID        string
	Seq          int
	Project      string
	Event        string
	Phase        string
	StepSlug     string
	Message      string
	ExitCode     *int
	Metadata     map[string]any
	CreatedAt    time.Time
}

// RunEventMigrationSource is the narrow interface cosmos.Store satisfies
// for the one-shot Migrate call. Implemented by
// cosmos.Store.ListAllRunEventDocsForMigration (which exists only for
// this transition; Stage 2i deletes it).
type RunEventMigrationSource interface {
	ListAllRunEventDocsForMigration(ctx context.Context) ([]RunEventRow, error)
}

// ErrRunEventConflict signals that an event with the same primary key
// already exists in the table but with a different payload. Mirrors the
// cosmos.Store behavior (server.ErrConflict) which the public API
// (RecordNativeEventByID) propagates to its caller.
var ErrRunEventConflict = errors.New("run event conflict: same primary key, different payload")

// NewRunEventsStore returns a RunEventsStore backed by pool. The pool's
// migrations must have applied successfully before the first call.
func NewRunEventsStore(pool *pgxpool.Pool) *RunEventsStore {
	return &RunEventsStore{pool: pool}
}

// Insert tries to record an event idempotently. Return values:
//   - created=true, err=nil → newly inserted
//   - created=false, err=nil → identical event already existed (no-op)
//   - created=false, err=ErrRunEventConflict → same PK, different payload
//   - created=false, err=<other> → pool / serialization error
//
// Callers that need to mutate other state (e.g. cosmos.Store.applyNative
// EventExecutionState) should gate that work on `created == true`.
func (s *RunEventsStore) Insert(ctx context.Context, row RunEventRow) (bool, error) {
	if s == nil || s.pool == nil {
		return false, fmt.Errorf("run events store not configured")
	}
	metadata, err := marshalMetadata(row.Metadata)
	if err != nil {
		return false, fmt.Errorf("run events: marshal metadata: %w", err)
	}
	createdAt := row.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	// ON CONFLICT DO NOTHING + RETURNING xmin gives us a non-empty result
	// iff the INSERT actually happened. An empty result means the row
	// already existed.
	const insertSQL = `
		INSERT INTO run_events (
		  run_id, attempt_index, job_id, seq,
		  project, event, phase, step_slug, message, exit_code, metadata,
		  created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (run_id, attempt_index, job_id, seq) DO NOTHING
		RETURNING run_id
	`
	var stub string
	err = s.pool.QueryRow(ctx, insertSQL,
		row.RunID, row.AttemptIndex, row.JobID, row.Seq,
		row.Project, row.Event, row.Phase, row.StepSlug, row.Message, row.ExitCode, metadata,
		createdAt,
	).Scan(&stub)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("run events: insert: %w", err)
	}

	// Conflict on the primary key. Read the existing row back so we can
	// compare and either swallow as idempotent or return ErrRunEventConflict.
	existing, err := s.getByPK(ctx, row.RunID, row.AttemptIndex, row.JobID, row.Seq)
	if err != nil {
		return false, fmt.Errorf("run events: read after conflict: %w", err)
	}
	if !sameEvent(existing, row) {
		return false, ErrRunEventConflict
	}
	return false, nil
}

// List returns events for runID, optionally filtered by attemptIndex and
// jobID, ordered by (attempt_index, job_id, seq, created_at). If limit
// is non-nil and positive, the slice is truncated to that many rows.
//
// The result is the canonical sort order; callers don't need to re-sort.
func (s *RunEventsStore) List(ctx context.Context, runID string, attemptIndex *int, jobID *string, limit *int) ([]RunEventRow, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("run events store not configured")
	}
	// Build a parameterized query that filters by optional attempt and
	// job_id. We avoid the project column on the WHERE clause because
	// run_id is globally unique (project leads the table's primary key
	// only via the row itself, not the query path).
	sql := `
		SELECT run_id, attempt_index, job_id, seq,
		       project, event, phase, step_slug, message, exit_code, metadata,
		       created_at
		FROM run_events
		WHERE run_id = $1
	`
	args := []any{runID}
	idx := 2
	if attemptIndex != nil {
		sql += fmt.Sprintf(" AND attempt_index = $%d", idx)
		args = append(args, *attemptIndex)
		idx++
	}
	if jobID != nil {
		sql += fmt.Sprintf(" AND job_id = $%d", idx)
		args = append(args, *jobID)
		idx++
	}
	sql += ` ORDER BY attempt_index, job_id, seq, created_at`
	if limit != nil && *limit > 0 {
		sql += fmt.Sprintf(" LIMIT $%d", idx)
		args = append(args, *limit)
	}

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("run events: list: %w", err)
	}
	defer rows.Close()

	out := []RunEventRow{}
	for rows.Next() {
		row, err := scanRunEventRow(rows)
		if err != nil {
			return nil, fmt.Errorf("run events: scan list row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run events: iterate list: %w", err)
	}
	return out, nil
}

// Migrate copies every run event document from the cosmos source into
// the Postgres run_events table, idempotently. ON CONFLICT DO NOTHING
// means re-running on every pod startup is safe — already-migrated rows
// stay untouched.
//
// Failures on individual rows abort the migration with the row's
// identifying info so the operator can diagnose and re-run. The choice
// of "abort vs. skip" matches LocksStore.Migrate's semantics; per the
// plan doc, the user opted for "full copy of everything" so silently
// skipping bad rows would mask data loss.
func (s *RunEventsStore) Migrate(ctx context.Context, source RunEventMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("run events store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	rows, err := source.ListAllRunEventDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("run events: migrate: read source: %w", err)
	}
	for _, row := range rows {
		created, insErr := s.Insert(ctx, row)
		if errors.Is(insErr, ErrRunEventConflict) {
			// Migration row collides with an existing pg row whose
			// payload differs — treat as skip-with-warning rather
			// than fatal. Indicates either a partial earlier
			// migration or a mid-flight write between cosmos and
			// pg. Either way, the pg row wins (it was written by
			// the migrated app); don't overwrite it from cosmos.
			skipped++
			continue
		}
		if insErr != nil {
			return copied, skipped, fmt.Errorf("run events: migrate row %s/%d/%s/%d: %w",
				row.RunID, row.AttemptIndex, row.JobID, row.Seq, insErr)
		}
		if created {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

// getByPK is the internal helper that reads back a row after a conflict
// in Insert. Returns the existing row so the caller can compare against
// the proposed one.
func (s *RunEventsStore) getByPK(ctx context.Context, runID string, attemptIndex int, jobID string, seq int) (RunEventRow, error) {
	const sql = `
		SELECT run_id, attempt_index, job_id, seq,
		       project, event, phase, step_slug, message, exit_code, metadata,
		       created_at
		FROM run_events
		WHERE run_id = $1 AND attempt_index = $2 AND job_id = $3 AND seq = $4
	`
	rows, err := s.pool.Query(ctx, sql, runID, attemptIndex, jobID, seq)
	if err != nil {
		return RunEventRow{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		// Row vanished between INSERT-conflict and SELECT — race with TTL
		// or operator deletion. Surface as "no conflict to compare," which
		// upstream treats as a transient anomaly worth aborting on.
		return RunEventRow{}, pgx.ErrNoRows
	}
	return scanRunEventRow(rows)
}

func scanRunEventRow(rows pgx.Rows) (RunEventRow, error) {
	var row RunEventRow
	var rawMetadata []byte
	if err := rows.Scan(
		&row.RunID, &row.AttemptIndex, &row.JobID, &row.Seq,
		&row.Project, &row.Event, &row.Phase, &row.StepSlug, &row.Message, &row.ExitCode, &rawMetadata,
		&row.CreatedAt,
	); err != nil {
		return RunEventRow{}, err
	}
	row.Metadata = unmarshalMetadata(rawMetadata)
	return row, nil
}

// sameEvent matches the equality contract the cosmos event store used: identical
// content along every business-relevant field (PK is implied because
// the conflict-comparison path only fires for matching PKs).
func sameEvent(a, b RunEventRow) bool {
	if a.Project != b.Project ||
		a.Event != b.Event ||
		a.Phase != b.Phase ||
		a.StepSlug != b.StepSlug ||
		a.Message != b.Message {
		return false
	}
	if (a.ExitCode == nil) != (b.ExitCode == nil) {
		return false
	}
	if a.ExitCode != nil && *a.ExitCode != *b.ExitCode {
		return false
	}
	return equalMaps(a.Metadata, b.Metadata)
}

func equalMaps(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	left, err := json.Marshal(canonicalizeMap(a))
	if err != nil {
		return false
	}
	right, err := json.Marshal(canonicalizeMap(b))
	if err != nil {
		return false
	}
	return string(left) == string(right)
}

func canonicalizeMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func marshalMetadata(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

func unmarshalMetadata(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	if out == nil {
		return map[string]any{}
	}
	return out
}

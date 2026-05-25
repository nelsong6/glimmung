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

// RunsStore is the Postgres-backed runs store.
type RunsStore struct {
	pool *pgxpool.Pool
}

type RunRow struct {
	ID          string
	Project     string
	IssueNumber *int
	Payload     []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type RunMigrationSource interface {
	ListAllRunDocsForMigration(ctx context.Context) ([]RunRow, error)
}

var ErrRunNotFound = errors.New("run not found")

func NewRunsStore(pool *pgxpool.Pool) *RunsStore {
	return &RunsStore{pool: pool}
}

// Get returns the run row for (project, id).
func (s *RunsStore) Get(ctx context.Context, project, id string) (RunRow, error) {
	if s == nil || s.pool == nil {
		return RunRow{}, fmt.Errorf("runs store not configured")
	}
	const sql = `SELECT id, project, issue_number, payload, created_at, updated_at FROM runs WHERE project = $1 AND id = $2`
	var out RunRow
	if err := s.pool.QueryRow(ctx, sql, project, id).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RunRow{}, ErrRunNotFound
		}
		return RunRow{}, fmt.Errorf("runs: get: %w", err)
	}
	return out, nil
}

// List returns runs for project, ordered by updated_at DESC. limit
// applies if > 0.
func (s *RunsStore) List(ctx context.Context, project string, limit int) ([]RunRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	sqlText := `SELECT id, project, issue_number, payload, created_at, updated_at FROM runs WHERE project = $1 ORDER BY updated_at DESC`
	args := []any{project}
	if limit > 0 {
		sqlText += " LIMIT $2"
		args = append(args, limit)
	}
	return s.queryRows(ctx, sqlText, args)
}

// ListAll returns every run row across all projects, ordered by
// updated_at DESC. Used by callers that need a global view (cosmos
// did a cross-partition scan).
func (s *RunsStore) ListAll(ctx context.Context) ([]RunRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `SELECT id, project, issue_number, payload, created_at, updated_at FROM runs ORDER BY updated_at DESC`
	return s.queryRows(ctx, sql, nil)
}

// ListByIssue returns every run for (project, issue_number) ordered
// by created_at ASC.
func (s *RunsStore) ListByIssue(ctx context.Context, project string, issueNumber int) ([]RunRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `
		SELECT id, project, issue_number, payload, created_at, updated_at
		FROM runs
		WHERE project = $1 AND issue_number = $2
		ORDER BY created_at ASC
	`
	return s.queryRows(ctx, sql, []any{project, issueNumber})
}

// FindByPR finds the most-recently-updated run whose payload
// references repo + pr_number. Both fields live inside the jsonb
// payload (issue_repo + pr_number).
func (s *RunsStore) FindByPR(ctx context.Context, repo string, prNumber int) (RunRow, error) {
	if s == nil || s.pool == nil {
		return RunRow{}, fmt.Errorf("runs store not configured")
	}
	const sql = `
		SELECT id, project, issue_number, payload, created_at, updated_at
		FROM runs
		WHERE payload->>'issue_repo' = $1
		  AND payload->>'pr_number' = $2
		ORDER BY updated_at DESC
		LIMIT 1
	`
	var out RunRow
	if err := s.pool.QueryRow(ctx, sql, repo, fmt.Sprintf("%d", prNumber)).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RunRow{}, ErrRunNotFound
		}
		return RunRow{}, fmt.Errorf("runs: find by pr: %w", err)
	}
	return out, nil
}

// Create inserts a new run row.
func (s *RunsStore) Create(ctx context.Context, row RunRow) (RunRow, error) {
	if s == nil || s.pool == nil {
		return RunRow{}, fmt.Errorf("runs store not configured")
	}
	const insertSQL = `
		INSERT INTO runs (id, project, issue_number, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (project, id) DO NOTHING
		RETURNING id, project, issue_number, payload, created_at, updated_at
	`
	var out RunRow
	err := s.pool.QueryRow(ctx, insertSQL, row.ID, row.Project, row.IssueNumber, row.Payload).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRow{}, fmt.Errorf("run %s/%s already exists", row.Project, row.ID)
	}
	if err != nil {
		return RunRow{}, fmt.Errorf("runs: create: %w", err)
	}
	return out, nil
}

// PatchPayload mutates the jsonb payload inside a SELECT FOR UPDATE
// tx. Replaces cosmos's read-modify-write ETag retry loop. The
// mutator may also reset top-level keys; issue_number column is
// derived from payload.issue_number to keep the partial index
// runs_by_project_issue accurate.
func (s *RunsStore) PatchPayload(ctx context.Context, project, id string, mutate func(payload map[string]any) error) (RunRow, error) {
	if s == nil || s.pool == nil {
		return RunRow{}, fmt.Errorf("runs store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RunRow{}, fmt.Errorf("runs: begin patch: %w", err)
	}
	defer tx.Rollback(ctx)

	const selectSQL = `SELECT payload FROM runs WHERE project = $1 AND id = $2 FOR UPDATE`
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, project, id).Scan(&payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RunRow{}, ErrRunNotFound
		}
		return RunRow{}, fmt.Errorf("runs: select for patch: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return RunRow{}, fmt.Errorf("runs: unmarshal payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return RunRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return RunRow{}, fmt.Errorf("runs: marshal patched payload: %w", err)
	}
	// issue_number column tracks payload.issue_number for the partial index.
	var issueNumArg any
	if v, ok := payload["issue_number"].(float64); ok && int(v) > 0 {
		issueNumArg = int(v)
	}
	const updateSQL = `
		UPDATE runs SET payload = $3, issue_number = $4, updated_at = now()
		WHERE project = $1 AND id = $2
		RETURNING id, project, issue_number, payload, created_at, updated_at
	`
	var out RunRow
	if err := tx.QueryRow(ctx, updateSQL, project, id, newPayload, issueNumArg).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return RunRow{}, fmt.Errorf("runs: update patched: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RunRow{}, fmt.Errorf("runs: commit patch: %w", err)
	}
	return out, nil
}

func (s *RunsStore) queryRows(ctx context.Context, sqlText string, args []any) ([]RunRow, error) {
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("runs: query: %w", err)
	}
	defer rows.Close()
	out := []RunRow{}
	for rows.Next() {
		var row RunRow
		if err := rows.Scan(&row.ID, &row.Project, &row.IssueNumber, &row.Payload, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("runs: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runs: iterate: %w", err)
	}
	return out, nil
}

func (s *RunsStore) Migrate(ctx context.Context, source RunMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("runs store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	rows, err := source.ListAllRunDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("runs: migrate: read source: %w", err)
	}
	const insertSQL = `
		INSERT INTO runs (id, project, issue_number, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, COALESCE($5, now()), COALESCE($6, $5, now()))
		ON CONFLICT (project, id) DO NOTHING
	`
	for _, row := range rows {
		tag, execErr := s.pool.Exec(ctx, insertSQL, row.ID, row.Project, row.IssueNumber, row.Payload, nullableTime(row.CreatedAt), nullableTime(row.UpdatedAt))
		if execErr != nil {
			return copied, skipped, fmt.Errorf("runs: migrate %s: %w", row.ID, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

// LeasesStore foundation (cutover is the next stage).
type LeasesStore struct {
	pool *pgxpool.Pool
}

type LeaseRow struct {
	ID            string
	Project       string
	CallbackToken string
	Payload       []byte
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     *time.Time
}

type LeaseMigrationSource interface {
	ListAllLeaseDocsForMigration(ctx context.Context) ([]LeaseRow, error)
}

var ErrLeaseNotFound = errors.New("lease not found")

func NewLeasesStore(pool *pgxpool.Pool) *LeasesStore {
	return &LeasesStore{pool: pool}
}

func (s *LeasesStore) Migrate(ctx context.Context, source LeaseMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("leases store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	rows, err := source.ListAllLeaseDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("leases: migrate: read source: %w", err)
	}
	const insertSQL = `
		INSERT INTO leases (id, project, callback_token, payload, created_at, updated_at, expires_at)
		VALUES ($1, $2, $3, $4, COALESCE($5, now()), COALESCE($6, $5, now()), $7)
		ON CONFLICT (id) DO NOTHING
	`
	for _, row := range rows {
		tag, execErr := s.pool.Exec(ctx, insertSQL, row.ID, row.Project, row.CallbackToken, row.Payload, nullableTime(row.CreatedAt), nullableTime(row.UpdatedAt), row.ExpiresAt)
		if execErr != nil {
			return copied, skipped, fmt.Errorf("leases: migrate %s: %w", row.ID, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

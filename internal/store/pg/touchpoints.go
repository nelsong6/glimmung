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

type TouchpointsStore struct {
	pool *pgxpool.Pool
}

type TouchpointRow struct {
	Project     string
	IssueNumber int
	Payload     []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

var ErrTouchpointNotFound = errors.New("touchpoint not found")

func NewTouchpointsStore(pool *pgxpool.Pool) *TouchpointsStore {
	return &TouchpointsStore{pool: pool}
}

// List returns touchpoint rows, optionally filtered by project, repo
// (matches payload->>'repo'), state (matches payload->>'state').
// Ordered by updated_at DESC.
func (s *TouchpointsStore) List(ctx context.Context, project, repo, state string, limit int) ([]TouchpointRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	sqlText := `SELECT project, issue_number, payload, created_at, updated_at FROM touchpoints`
	args := []any{}
	var where []string
	if project != "" {
		where = append(where, fmt.Sprintf("project = $%d", len(args)+1))
		args = append(args, project)
	}
	if repo != "" {
		where = append(where, fmt.Sprintf("payload->>'repo' = $%d", len(args)+1))
		args = append(args, repo)
	}
	if state != "" {
		where = append(where, fmt.Sprintf("payload->>'state' = $%d", len(args)+1))
		args = append(args, state)
	}
	if len(where) > 0 {
		sqlText += " WHERE " + where[0]
		for i := 1; i < len(where); i++ {
			sqlText += " AND " + where[i]
		}
	}
	sqlText += " ORDER BY updated_at DESC"
	if limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("touchpoints: list: %w", err)
	}
	defer rows.Close()
	out := []TouchpointRow{}
	for rows.Next() {
		var row TouchpointRow
		if err := rows.Scan(&row.Project, &row.IssueNumber, &row.Payload, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("touchpoints: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("touchpoints: iterate: %w", err)
	}
	return out, nil
}

// FindByLinkedIssueID returns the most-recently-updated touchpoint
// for a (project, payload->>'linked_issue_id') pair.
func (s *TouchpointsStore) FindByLinkedIssueID(ctx context.Context, project, linkedIssueID string) (TouchpointRow, error) {
	if s == nil || s.pool == nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints store not configured")
	}
	const sql = `
		SELECT project, issue_number, payload, created_at, updated_at
		FROM touchpoints
		WHERE project = $1 AND payload->>'linked_issue_id' = $2
		ORDER BY updated_at DESC
		LIMIT 1
	`
	var out TouchpointRow
	if err := s.pool.QueryRow(ctx, sql, project, linkedIssueID).Scan(
		&out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TouchpointRow{}, ErrTouchpointNotFound
		}
		return TouchpointRow{}, fmt.Errorf("touchpoints: find by linked_issue_id: %w", err)
	}
	return out, nil
}

// FindByRepoNumber returns the touchpoint with the given (repo, number)
// regardless of project.
func (s *TouchpointsStore) FindByRepoNumber(ctx context.Context, repo string, number int) (TouchpointRow, error) {
	if s == nil || s.pool == nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints store not configured")
	}
	const sql = `
		SELECT project, issue_number, payload, created_at, updated_at
		FROM touchpoints
		WHERE payload->>'repo' = $1 AND issue_number = $2
		ORDER BY updated_at DESC
		LIMIT 1
	`
	var out TouchpointRow
	if err := s.pool.QueryRow(ctx, sql, repo, number).Scan(
		&out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TouchpointRow{}, ErrTouchpointNotFound
		}
		return TouchpointRow{}, fmt.Errorf("touchpoints: find by repo+number: %w", err)
	}
	return out, nil
}

// GetByProjectAndPR looks up a touchpoint by (project, issue_number).
// issue_number is the PR number on the touchpoint cluster.
func (s *TouchpointsStore) GetByProjectAndPR(ctx context.Context, project string, prNumber int) (TouchpointRow, error) {
	if s == nil || s.pool == nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints store not configured")
	}
	const sql = `SELECT project, issue_number, payload, created_at, updated_at FROM touchpoints WHERE project = $1 AND issue_number = $2`
	var out TouchpointRow
	if err := s.pool.QueryRow(ctx, sql, project, prNumber).Scan(
		&out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TouchpointRow{}, ErrTouchpointNotFound
		}
		return TouchpointRow{}, fmt.Errorf("touchpoints: get by project+pr: %w", err)
	}
	return out, nil
}

// Create inserts a new touchpoint row. Returns the inserted row, or the
// existing row on conflict. Create is reserved for the explicit "create new"
// branch of EnsureTouchpoint.
func (s *TouchpointsStore) Create(ctx context.Context, row TouchpointRow) (TouchpointRow, error) {
	if s == nil || s.pool == nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints store not configured")
	}
	const insertSQL = `
		INSERT INTO touchpoints (project, issue_number, payload, created_at, updated_at)
		VALUES ($1, $2, $3, now(), now())
		ON CONFLICT (project, issue_number) DO NOTHING
		RETURNING project, issue_number, payload, created_at, updated_at
	`
	var out TouchpointRow
	err := s.pool.QueryRow(ctx, insertSQL, row.Project, row.IssueNumber, row.Payload).Scan(
		&out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TouchpointRow{}, fmt.Errorf("touchpoint %s/%d already exists", row.Project, row.IssueNumber)
	}
	if err != nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints: create: %w", err)
	}
	return out, nil
}

// PatchPayload mutates the jsonb payload inside a SELECT FOR UPDATE
// transaction. Used by EnsureTouchpoint when linkages are patched on
// an existing row.
func (s *TouchpointsStore) PatchPayload(ctx context.Context, project string, prNumber int, mutate func(payload map[string]any) error) (TouchpointRow, error) {
	if s == nil || s.pool == nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints: begin patch: %w", err)
	}
	defer tx.Rollback(ctx)
	const selectSQL = `SELECT payload FROM touchpoints WHERE project = $1 AND issue_number = $2 FOR UPDATE`
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, project, prNumber).Scan(&payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TouchpointRow{}, ErrTouchpointNotFound
		}
		return TouchpointRow{}, fmt.Errorf("touchpoints: select for patch: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints: unmarshal payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return TouchpointRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints: marshal patched payload: %w", err)
	}
	const updateSQL = `
		UPDATE touchpoints SET payload = $3, updated_at = now()
		WHERE project = $1 AND issue_number = $2
		RETURNING project, issue_number, payload, created_at, updated_at
	`
	var out TouchpointRow
	if err := tx.QueryRow(ctx, updateSQL, project, prNumber, newPayload).Scan(
		&out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints: update patched: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TouchpointRow{}, fmt.Errorf("touchpoints: commit patch: %w", err)
	}
	return out, nil
}

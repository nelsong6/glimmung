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

// IssuesStore is the Postgres-backed issues + issue_comments store.
type IssuesStore struct {
	pool *pgxpool.Pool
}

// IssueRow is the per-(project, number) issue. The full cosmos
// issueDoc shape (minus comments) is stored as jsonb payload.
type IssueRow struct {
	Project    string
	Number     int
	Payload    []byte
	ArchivedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// IssueCommentRow is one comment in the issue_comments table, keyed
// by its own id with (project, issue_number) as FK columns.
type IssueCommentRow struct {
	ID          string
	Project     string
	IssueNumber int
	Payload     []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// IssueMigrationSource is the narrow interface cosmos.Store satisfies
// for the one-shot Migrate.
type IssueMigrationSource interface {
	ListAllIssueDocsForMigration(ctx context.Context) ([]IssueRow, []IssueCommentRow, error)
}

var ErrIssueNotFound = errors.New("issue not found")

func NewIssuesStore(pool *pgxpool.Pool) *IssuesStore {
	return &IssuesStore{pool: pool}
}

// List returns issue rows, optionally scoped by project. The caller
// filters further (state, workflow, etc.) in Go because the payload
// is jsonb and the cosmos call site already does in-memory sorting +
// run-cross-join. Issues are returned in no particular order; the
// caller is expected to apply its own ordering.
func (s *IssuesStore) List(ctx context.Context, project string) ([]IssueRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	sqlText := `SELECT project, number, payload, archived_at, created_at, updated_at FROM issues`
	args := []any{}
	if project != "" {
		sqlText += " WHERE project = $1"
		args = append(args, project)
	}
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("issues: list: %w", err)
	}
	defer rows.Close()
	out := []IssueRow{}
	for rows.Next() {
		var row IssueRow
		if err := rows.Scan(&row.Project, &row.Number, &row.Payload, &row.ArchivedAt, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("issues: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("issues: iterate: %w", err)
	}
	return out, nil
}

// GetByPayloadID looks up a single issue by the legacy cosmos doc id
// (payload->>'id'). Migration preserved that field on every row, so
// touchpoint / playbook entries that still reference issues by their
// cosmos UUID can resolve them. The (project, number) primary key is
// the preferred lookup; this exists only for legacy ref paths.
func (s *IssuesStore) GetByPayloadID(ctx context.Context, project, id string) (IssueRow, error) {
	if s == nil || s.pool == nil {
		return IssueRow{}, fmt.Errorf("issues store not configured")
	}
	const sql = `
		SELECT project, number, payload, archived_at, created_at, updated_at
		FROM issues
		WHERE project = $1 AND payload->>'id' = $2
	`
	var out IssueRow
	if err := s.pool.QueryRow(ctx, sql, project, id).Scan(
		&out.Project, &out.Number, &out.Payload, &out.ArchivedAt, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssueRow{}, ErrIssueNotFound
		}
		return IssueRow{}, fmt.Errorf("issues: get by payload id: %w", err)
	}
	return out, nil
}

// GetByNumber looks up a single issue by (project, number).
func (s *IssuesStore) GetByNumber(ctx context.Context, project string, number int) (IssueRow, error) {
	if s == nil || s.pool == nil {
		return IssueRow{}, fmt.Errorf("issues store not configured")
	}
	const sql = `SELECT project, number, payload, archived_at, created_at, updated_at FROM issues WHERE project = $1 AND number = $2`
	var out IssueRow
	if err := s.pool.QueryRow(ctx, sql, project, number).Scan(
		&out.Project, &out.Number, &out.Payload, &out.ArchivedAt, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssueRow{}, ErrIssueNotFound
		}
		return IssueRow{}, fmt.Errorf("issues: get by number: %w", err)
	}
	return out, nil
}

// AllocateNextNumber atomically allocates the next issue number for
// project. First call for a project seeds the counter from
// MAX(issues.number) + 1; subsequent calls increment-and-return.
// Wrapped in a transaction so the read/update is a single critical
// section.
func (s *IssuesStore) AllocateNextNumber(ctx context.Context, project string) (int, error) {
	if s == nil || s.pool == nil {
		return 0, fmt.Errorf("issues store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("issues: begin allocate: %w", err)
	}
	defer tx.Rollback(ctx)

	// Seed if missing — only writes when the row doesn't exist.
	const seedSQL = `
		INSERT INTO issue_counters (project, next_number)
		SELECT $1, COALESCE(MAX(number), 0) + 1 FROM issues WHERE project = $1
		ON CONFLICT (project) DO NOTHING
	`
	if _, err := tx.Exec(ctx, seedSQL, project); err != nil {
		return 0, fmt.Errorf("issues: seed counter: %w", err)
	}
	// Atomic increment-and-return prior value.
	const allocSQL = `
		UPDATE issue_counters
		SET next_number = next_number + 1
		WHERE project = $1
		RETURNING next_number - 1
	`
	var allocated int
	if err := tx.QueryRow(ctx, allocSQL, project).Scan(&allocated); err != nil {
		return 0, fmt.Errorf("issues: allocate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("issues: commit allocate: %w", err)
	}
	return allocated, nil
}

// Create inserts a new issue row. Caller is expected to allocate the
// number via AllocateNextNumber first and pass it on the row.
func (s *IssuesStore) Create(ctx context.Context, row IssueRow) (IssueRow, error) {
	if s == nil || s.pool == nil {
		return IssueRow{}, fmt.Errorf("issues store not configured")
	}
	const insertSQL = `
		INSERT INTO issues (project, number, payload, archived_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (project, number) DO NOTHING
		RETURNING project, number, payload, archived_at, created_at, updated_at
	`
	var out IssueRow
	err := s.pool.QueryRow(ctx, insertSQL, row.Project, row.Number, row.Payload, row.ArchivedAt).Scan(
		&out.Project, &out.Number, &out.Payload, &out.ArchivedAt, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return IssueRow{}, fmt.Errorf("issue %s#%d already exists", row.Project, row.Number)
	}
	if err != nil {
		return IssueRow{}, fmt.Errorf("issues: create: %w", err)
	}
	return out, nil
}

// PatchPayload mutates the jsonb payload inside a SELECT FOR UPDATE
// tx. The mutator may also set top-level keys like state / closed_at;
// archived_at column is derived from payload.closed_at to keep the
// partial index issues_active_by_project (WHERE archived_at IS NULL)
// accurate for archive transitions.
func (s *IssuesStore) PatchPayload(ctx context.Context, project string, number int, mutate func(payload map[string]any) error) (IssueRow, error) {
	if s == nil || s.pool == nil {
		return IssueRow{}, fmt.Errorf("issues store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IssueRow{}, fmt.Errorf("issues: begin patch: %w", err)
	}
	defer tx.Rollback(ctx)

	const selectSQL = `SELECT payload FROM issues WHERE project = $1 AND number = $2 FOR UPDATE`
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, project, number).Scan(&payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssueRow{}, ErrIssueNotFound
		}
		return IssueRow{}, fmt.Errorf("issues: select for patch: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return IssueRow{}, fmt.Errorf("issues: unmarshal payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return IssueRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return IssueRow{}, fmt.Errorf("issues: marshal patched payload: %w", err)
	}
	// archived_at column tracks state=closed for the partial index.
	var archivedArg any
	state, _ := payload["state"].(string)
	if state == "closed" {
		if v, ok := payload["closed_at"].(string); ok && v != "" {
			if t, perr := time.Parse(time.RFC3339Nano, v); perr == nil {
				archivedArg = t
			}
		}
	}
	const updateSQL = `
		UPDATE issues SET payload = $3, archived_at = $4, updated_at = now()
		WHERE project = $1 AND number = $2
		RETURNING project, number, payload, archived_at, created_at, updated_at
	`
	var out IssueRow
	if err := tx.QueryRow(ctx, updateSQL, project, number, newPayload, archivedArg).Scan(
		&out.Project, &out.Number, &out.Payload, &out.ArchivedAt, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return IssueRow{}, fmt.Errorf("issues: update patched: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IssueRow{}, fmt.Errorf("issues: commit patch: %w", err)
	}
	return out, nil
}

// TouchUpdatedAt bumps the parent issue's updated_at column without
// changing its payload. Called by comment mutations so listings of
// issues ordered by updated_at reflect comment activity.
func (s *IssuesStore) TouchUpdatedAt(ctx context.Context, project string, number int) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("issues store not configured")
	}
	const sql = `UPDATE issues SET updated_at = now() WHERE project = $1 AND number = $2`
	_, err := s.pool.Exec(ctx, sql, project, number)
	if err != nil {
		return fmt.Errorf("issues: touch updated_at: %w", err)
	}
	return nil
}

// ListComments returns all comments for (project, number), ordered by
// created_at ASC (oldest first).
func (s *IssuesStore) ListComments(ctx context.Context, project string, number int) ([]IssueCommentRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `
		SELECT id, project, issue_number, payload, created_at, updated_at
		FROM issue_comments
		WHERE project = $1 AND issue_number = $2
		ORDER BY created_at ASC
	`
	rows, err := s.pool.Query(ctx, sql, project, number)
	if err != nil {
		return nil, fmt.Errorf("issues: list comments: %w", err)
	}
	defer rows.Close()
	out := []IssueCommentRow{}
	for rows.Next() {
		var row IssueCommentRow
		if err := rows.Scan(&row.ID, &row.Project, &row.IssueNumber, &row.Payload, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("issues: scan comment: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("issues: iterate comments: %w", err)
	}
	return out, nil
}

// CreateComment inserts a new comment row and bumps issues.updated_at
// in the same transaction so the parent's last-touched timestamp
// reflects the comment activity.
func (s *IssuesStore) CreateComment(ctx context.Context, row IssueCommentRow) (IssueCommentRow, error) {
	if s == nil || s.pool == nil {
		return IssueCommentRow{}, fmt.Errorf("issues store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IssueCommentRow{}, fmt.Errorf("issues: begin create comment: %w", err)
	}
	defer tx.Rollback(ctx)
	const insertSQL = `
		INSERT INTO issue_comments (id, project, issue_number, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		RETURNING id, project, issue_number, payload, created_at, updated_at
	`
	var out IssueCommentRow
	if err := tx.QueryRow(ctx, insertSQL, row.ID, row.Project, row.IssueNumber, row.Payload).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return IssueCommentRow{}, fmt.Errorf("issues: insert comment: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE project = $1 AND number = $2`, row.Project, row.IssueNumber); err != nil {
		return IssueCommentRow{}, fmt.Errorf("issues: touch parent on comment create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IssueCommentRow{}, fmt.Errorf("issues: commit create comment: %w", err)
	}
	return out, nil
}

// GetComment looks up a single comment by id.
func (s *IssuesStore) GetComment(ctx context.Context, id string) (IssueCommentRow, error) {
	if s == nil || s.pool == nil {
		return IssueCommentRow{}, fmt.Errorf("issues store not configured")
	}
	const sql = `SELECT id, project, issue_number, payload, created_at, updated_at FROM issue_comments WHERE id = $1`
	var out IssueCommentRow
	if err := s.pool.QueryRow(ctx, sql, id).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssueCommentRow{}, ErrIssueNotFound
		}
		return IssueCommentRow{}, fmt.Errorf("issues: get comment: %w", err)
	}
	return out, nil
}

// PatchComment mutates a comment's jsonb payload inside a SELECT FOR
// UPDATE tx and bumps both comment.updated_at and parent issue.
// updated_at in the same transaction.
func (s *IssuesStore) PatchComment(ctx context.Context, id string, mutate func(payload map[string]any) error) (IssueCommentRow, error) {
	if s == nil || s.pool == nil {
		return IssueCommentRow{}, fmt.Errorf("issues store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IssueCommentRow{}, fmt.Errorf("issues: begin patch comment: %w", err)
	}
	defer tx.Rollback(ctx)
	const selectSQL = `SELECT project, issue_number, payload FROM issue_comments WHERE id = $1 FOR UPDATE`
	var project string
	var issueNumber int
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, id).Scan(&project, &issueNumber, &payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssueCommentRow{}, ErrIssueNotFound
		}
		return IssueCommentRow{}, fmt.Errorf("issues: select comment for patch: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return IssueCommentRow{}, fmt.Errorf("issues: unmarshal comment payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return IssueCommentRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return IssueCommentRow{}, fmt.Errorf("issues: marshal comment payload: %w", err)
	}
	const updateSQL = `
		UPDATE issue_comments SET payload = $2, updated_at = now()
		WHERE id = $1
		RETURNING id, project, issue_number, payload, created_at, updated_at
	`
	var out IssueCommentRow
	if err := tx.QueryRow(ctx, updateSQL, id, newPayload).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return IssueCommentRow{}, fmt.Errorf("issues: update comment: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE project = $1 AND number = $2`, project, issueNumber); err != nil {
		return IssueCommentRow{}, fmt.Errorf("issues: touch parent on comment patch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IssueCommentRow{}, fmt.Errorf("issues: commit patch comment: %w", err)
	}
	return out, nil
}

// DeleteComment removes a comment and bumps the parent issue's
// updated_at column. Returns ErrIssueNotFound if no comment matched.
func (s *IssuesStore) DeleteComment(ctx context.Context, id string) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("issues store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("issues: begin delete comment: %w", err)
	}
	defer tx.Rollback(ctx)
	const sql = `DELETE FROM issue_comments WHERE id = $1 RETURNING project, issue_number`
	var project string
	var issueNumber int
	if err := tx.QueryRow(ctx, sql, id).Scan(&project, &issueNumber); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrIssueNotFound
		}
		return fmt.Errorf("issues: delete comment: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE project = $1 AND number = $2`, project, issueNumber); err != nil {
		return fmt.Errorf("issues: touch parent on comment delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("issues: commit delete comment: %w", err)
	}
	return nil
}

// Migrate copies every cosmos issue doc into the pg issues table and
// every embedded comment into the issue_comments table. Idempotent.
func (s *IssuesStore) Migrate(ctx context.Context, source IssueMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("issues store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	issues, comments, err := source.ListAllIssueDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("issues: migrate: read source: %w", err)
	}
	const insertIssueSQL = `
		INSERT INTO issues (project, number, payload, archived_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, COALESCE($5, now()), COALESCE($6, $5, now()))
		ON CONFLICT (project, number) DO NOTHING
	`
	for _, row := range issues {
		createdAt := nullableTime(row.CreatedAt)
		updatedAt := nullableTime(row.UpdatedAt)
		tag, execErr := s.pool.Exec(ctx, insertIssueSQL,
			row.Project, row.Number, row.Payload, row.ArchivedAt, createdAt, updatedAt,
		)
		if execErr != nil {
			return copied, skipped, fmt.Errorf("issues: migrate %s/%d: %w", row.Project, row.Number, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	const insertCommentSQL = `
		INSERT INTO issue_comments (id, project, issue_number, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, COALESCE($5, now()), COALESCE($6, $5, now()))
		ON CONFLICT (id) DO NOTHING
	`
	for _, row := range comments {
		createdAt := nullableTime(row.CreatedAt)
		updatedAt := nullableTime(row.UpdatedAt)
		tag, execErr := s.pool.Exec(ctx, insertCommentSQL,
			row.ID, row.Project, row.IssueNumber, row.Payload, createdAt, updatedAt,
		)
		if execErr != nil {
			return copied, skipped, fmt.Errorf("issues: migrate comment %s: %w", row.ID, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

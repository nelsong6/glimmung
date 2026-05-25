package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IssuesStore is the Postgres-backed issues + issue_comments store
// (Stage 2h). Foundation-only this PR — the public read/write
// methods land in Stage 2i (the issues cutover stage). cosmos.Store
// still serves all issue R/W until then.
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
	ID           string
	Project      string
	IssueNumber  int
	Payload      []byte
	CreatedAt    time.Time
	UpdatedAt    time.Time
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

var _ = pgx.ErrNoRows // silence linter; used by future cutover-stage methods

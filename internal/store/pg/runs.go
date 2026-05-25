package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RunsStore + LeasesStore foundation (stage 2k). Cutover for both is
// follow-up; the run cluster is the largest in cosmos and the most
// intricate to delegate, so this PR ships only the migration plumbing.
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

// LeasesStore foundation.
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

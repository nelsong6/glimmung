package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ReportsStore struct {
	pool *pgxpool.Pool
}

type ReportRow struct {
	ID          string
	Project     string
	IssueNumber *int
	Payload     []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type ReportMigrationSource interface {
	ListAllReportDocsForMigration(ctx context.Context) ([]ReportRow, error)
}

var ErrReportNotFound = errors.New("report not found")

func NewReportsStore(pool *pgxpool.Pool) *ReportsStore {
	return &ReportsStore{pool: pool}
}

func (s *ReportsStore) Migrate(ctx context.Context, source ReportMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("reports store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	rows, err := source.ListAllReportDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("reports: migrate: read source: %w", err)
	}
	const insertSQL = `
		INSERT INTO reports (id, project, issue_number, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, COALESCE($5, now()), COALESCE($6, $5, now()))
		ON CONFLICT (id) DO NOTHING
	`
	for _, row := range rows {
		tag, execErr := s.pool.Exec(ctx, insertSQL, row.ID, row.Project, row.IssueNumber, row.Payload, nullableTime(row.CreatedAt), nullableTime(row.UpdatedAt))
		if execErr != nil {
			return copied, skipped, fmt.Errorf("reports: migrate %s: %w", row.ID, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

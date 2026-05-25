package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

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

type TouchpointMigrationSource interface {
	ListAllTouchpointDocsForMigration(ctx context.Context) ([]TouchpointRow, error)
}

var ErrTouchpointNotFound = errors.New("touchpoint not found")

func NewTouchpointsStore(pool *pgxpool.Pool) *TouchpointsStore {
	return &TouchpointsStore{pool: pool}
}

func (s *TouchpointsStore) Migrate(ctx context.Context, source TouchpointMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("touchpoints store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	rows, err := source.ListAllTouchpointDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("touchpoints: migrate: read source: %w", err)
	}
	const insertSQL = `
		INSERT INTO touchpoints (project, issue_number, payload, created_at, updated_at)
		VALUES ($1, $2, $3, COALESCE($4, now()), COALESCE($5, $4, now()))
		ON CONFLICT (project, issue_number) DO NOTHING
	`
	for _, row := range rows {
		tag, execErr := s.pool.Exec(ctx, insertSQL, row.Project, row.IssueNumber, row.Payload, nullableTime(row.CreatedAt), nullableTime(row.UpdatedAt))
		if execErr != nil {
			return copied, skipped, fmt.Errorf("touchpoints: migrate %s/%d: %w", row.Project, row.IssueNumber, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

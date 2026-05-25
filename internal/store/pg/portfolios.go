package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type PortfoliosStore struct {
	pool *pgxpool.Pool
}

type PortfolioRow struct {
	Project   string
	Route     string
	ElementID string
	Payload   []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

type PortfolioMigrationSource interface {
	ListAllPortfolioDocsForMigration(ctx context.Context) ([]PortfolioRow, error)
}

var ErrPortfolioNotFound = errors.New("portfolio not found")

func NewPortfoliosStore(pool *pgxpool.Pool) *PortfoliosStore {
	return &PortfoliosStore{pool: pool}
}

func (s *PortfoliosStore) Migrate(ctx context.Context, source PortfolioMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("portfolios store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	rows, err := source.ListAllPortfolioDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("portfolios: migrate: read source: %w", err)
	}
	const insertSQL = `
		INSERT INTO portfolios (project, route, element_id, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, COALESCE($5, now()), COALESCE($6, $5, now()))
		ON CONFLICT (project, route, element_id) DO NOTHING
	`
	for _, row := range rows {
		tag, execErr := s.pool.Exec(ctx, insertSQL, row.Project, row.Route, row.ElementID, row.Payload, nullableTime(row.CreatedAt), nullableTime(row.UpdatedAt))
		if execErr != nil {
			return copied, skipped, fmt.Errorf("portfolios: migrate %s/%s/%s: %w", row.Project, row.Route, row.ElementID, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

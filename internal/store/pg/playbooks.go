package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type PlaybooksStore struct {
	pool *pgxpool.Pool
}

type PlaybookRow struct {
	Project   string
	Name      string
	Payload   []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

type PlaybookMigrationSource interface {
	ListAllPlaybookDocsForMigration(ctx context.Context) ([]PlaybookRow, error)
}

var ErrPlaybookNotFound = errors.New("playbook not found")

func NewPlaybooksStore(pool *pgxpool.Pool) *PlaybooksStore {
	return &PlaybooksStore{pool: pool}
}

func (s *PlaybooksStore) Migrate(ctx context.Context, source PlaybookMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("playbooks store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	rows, err := source.ListAllPlaybookDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("playbooks: migrate: read source: %w", err)
	}
	const insertSQL = `
		INSERT INTO playbooks (project, name, payload, created_at, updated_at)
		VALUES ($1, $2, $3, COALESCE($4, now()), COALESCE($5, $4, now()))
		ON CONFLICT (project, name) DO NOTHING
	`
	for _, row := range rows {
		tag, execErr := s.pool.Exec(ctx, insertSQL, row.Project, row.Name, row.Payload, nullableTime(row.CreatedAt), nullableTime(row.UpdatedAt))
		if execErr != nil {
			return copied, skipped, fmt.Errorf("playbooks: migrate %s/%s: %w", row.Project, row.Name, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

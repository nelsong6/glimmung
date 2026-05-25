package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SignalsStore is the Postgres-backed signals store (Stage 2i
// foundation; cutover is a follow-up).
type SignalsStore struct {
	pool *pgxpool.Pool
}

type SignalRow struct {
	ID          string
	TargetRepo  string
	Payload     []byte
	CreatedAt   time.Time
	ProcessedAt *time.Time
}

type SignalMigrationSource interface {
	ListAllSignalDocsForMigration(ctx context.Context) ([]SignalRow, error)
}

var ErrSignalNotFound = errors.New("signal not found")

func NewSignalsStore(pool *pgxpool.Pool) *SignalsStore {
	return &SignalsStore{pool: pool}
}

// Migrate copies cosmos signal docs into pg.
func (s *SignalsStore) Migrate(ctx context.Context, source SignalMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("signals store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	rows, err := source.ListAllSignalDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("signals: migrate: read source: %w", err)
	}
	const insertSQL = `
		INSERT INTO signals (id, target_repo, payload, created_at, processed_at)
		VALUES ($1, $2, $3, COALESCE($4, now()), $5)
		ON CONFLICT (id) DO NOTHING
	`
	for _, row := range rows {
		tag, execErr := s.pool.Exec(ctx, insertSQL, row.ID, row.TargetRepo, row.Payload, nullableTime(row.CreatedAt), row.ProcessedAt)
		if execErr != nil {
			return copied, skipped, fmt.Errorf("signals: migrate %s: %w", row.ID, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

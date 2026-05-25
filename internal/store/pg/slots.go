package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SlotsStore is the Postgres-backed slots + slot_history store
// (Stage 2i foundation; cutover is a follow-up).
type SlotsStore struct {
	pool *pgxpool.Pool
}

type SlotRow struct {
	Project   string
	SlotIndex int
	Payload   []byte
	UpdatedAt time.Time
}

type SlotHistoryRow struct {
	ID         string
	Project    string
	SlotIndex  int
	Payload    []byte
	CreatedAt  time.Time
}

type SlotMigrationSource interface {
	ListAllSlotDocsForMigration(ctx context.Context) ([]SlotRow, []SlotHistoryRow, error)
}

var ErrSlotNotFound = errors.New("slot not found")

func NewSlotsStore(pool *pgxpool.Pool) *SlotsStore {
	return &SlotsStore{pool: pool}
}

// Migrate copies cosmos slot + slot_history docs into pg.
func (s *SlotsStore) Migrate(ctx context.Context, source SlotMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("slots store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	slots, history, err := source.ListAllSlotDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("slots: migrate: read source: %w", err)
	}
	const insertSlotSQL = `
		INSERT INTO slots (project, slot_index, payload, updated_at)
		VALUES ($1, $2, $3, COALESCE($4, now()))
		ON CONFLICT (project, slot_index) DO NOTHING
	`
	for _, row := range slots {
		tag, execErr := s.pool.Exec(ctx, insertSlotSQL, row.Project, row.SlotIndex, row.Payload, nullableTime(row.UpdatedAt))
		if execErr != nil {
			return copied, skipped, fmt.Errorf("slots: migrate %s/%d: %w", row.Project, row.SlotIndex, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	const insertHistorySQL = `
		INSERT INTO slot_history (id, project, slot_index, payload, created_at)
		VALUES ($1::uuid, $2, $3, $4, COALESCE($5, now()))
		ON CONFLICT (id) DO NOTHING
	`
	for _, row := range history {
		tag, execErr := s.pool.Exec(ctx, insertHistorySQL, row.ID, row.Project, row.SlotIndex, row.Payload, nullableTime(row.CreatedAt))
		if execErr != nil {
			return copied, skipped, fmt.Errorf("slots: migrate history %s: %w", row.ID, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

var _ = pgx.ErrNoRows

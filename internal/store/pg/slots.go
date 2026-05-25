package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SlotsStore is the Postgres-backed slots + slot_history store.
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
	ID        string
	Project   string
	SlotIndex int
	Payload   []byte
	CreatedAt time.Time
}

type SlotMigrationSource interface {
	ListAllSlotDocsForMigration(ctx context.Context) ([]SlotRow, []SlotHistoryRow, error)
}

var (
	ErrSlotNotFound            = errors.New("slot not found")
	ErrSlotPreconditionFailed  = errors.New("slot precondition failed")
	ErrSlotAlreadyExists       = errors.New("slot already exists")
)

func NewSlotsStore(pool *pgxpool.Pool) *SlotsStore {
	return &SlotsStore{pool: pool}
}

// Get returns the slot row for (project, slot_index). The UpdatedAt
// field is the CAS version — pass it back to UpdateWithCAS to
// optimistically replace the payload.
func (s *SlotsStore) Get(ctx context.Context, project string, slotIndex int) (SlotRow, error) {
	if s == nil || s.pool == nil {
		return SlotRow{}, fmt.Errorf("slots store not configured")
	}
	const sql = `SELECT project, slot_index, payload, updated_at FROM slots WHERE project = $1 AND slot_index = $2`
	var out SlotRow
	if err := s.pool.QueryRow(ctx, sql, project, slotIndex).Scan(
		&out.Project, &out.SlotIndex, &out.Payload, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SlotRow{}, ErrSlotNotFound
		}
		return SlotRow{}, fmt.Errorf("slots: get: %w", err)
	}
	return out, nil
}

// ListByProject returns every slot for project, ordered by slot_index ASC.
func (s *SlotsStore) ListByProject(ctx context.Context, project string) ([]SlotRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `
		SELECT project, slot_index, payload, updated_at
		FROM slots
		WHERE project = $1
		ORDER BY slot_index ASC
	`
	rows, err := s.pool.Query(ctx, sql, project)
	if err != nil {
		return nil, fmt.Errorf("slots: list: %w", err)
	}
	defer rows.Close()
	out := []SlotRow{}
	for rows.Next() {
		var row SlotRow
		if err := rows.Scan(&row.Project, &row.SlotIndex, &row.Payload, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("slots: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("slots: iterate: %w", err)
	}
	return out, nil
}

// Create inserts a new slot row. If (project, slot_index) already
// exists, returns ErrSlotAlreadyExists; the caller is expected to
// fall back to Get to fetch the existing row (CreateSlot is
// idempotent at the cosmos.Store layer).
func (s *SlotsStore) Create(ctx context.Context, row SlotRow) (SlotRow, error) {
	if s == nil || s.pool == nil {
		return SlotRow{}, fmt.Errorf("slots store not configured")
	}
	const insertSQL = `
		INSERT INTO slots (project, slot_index, payload, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (project, slot_index) DO NOTHING
		RETURNING project, slot_index, payload, updated_at
	`
	var out SlotRow
	err := s.pool.QueryRow(ctx, insertSQL, row.Project, row.SlotIndex, row.Payload).Scan(
		&out.Project, &out.SlotIndex, &out.Payload, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SlotRow{}, ErrSlotAlreadyExists
	}
	if err != nil {
		return SlotRow{}, fmt.Errorf("slots: create: %w", err)
	}
	return out, nil
}

// UpdateWithCAS optimistically replaces the slot payload. expected is
// the UpdatedAt the caller read from Get; if the row's current
// updated_at differs (someone else wrote), returns
// ErrSlotPreconditionFailed. If the row doesn't exist, returns
// ErrSlotNotFound.
func (s *SlotsStore) UpdateWithCAS(ctx context.Context, project string, slotIndex int, payload []byte, expected time.Time) (SlotRow, error) {
	if s == nil || s.pool == nil {
		return SlotRow{}, fmt.Errorf("slots store not configured")
	}
	const updateSQL = `
		UPDATE slots
		SET payload = $3, updated_at = now()
		WHERE project = $1 AND slot_index = $2 AND updated_at = $4
		RETURNING project, slot_index, payload, updated_at
	`
	var out SlotRow
	err := s.pool.QueryRow(ctx, updateSQL, project, slotIndex, payload, expected).Scan(
		&out.Project, &out.SlotIndex, &out.Payload, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "doesn't exist" from "version mismatch" so the
		// caller can return ErrNotFound vs ErrPreconditionFailed.
		const existsSQL = `SELECT 1 FROM slots WHERE project = $1 AND slot_index = $2`
		var dummy int
		existsErr := s.pool.QueryRow(ctx, existsSQL, project, slotIndex).Scan(&dummy)
		if errors.Is(existsErr, pgx.ErrNoRows) {
			return SlotRow{}, ErrSlotNotFound
		}
		if existsErr != nil {
			return SlotRow{}, fmt.Errorf("slots: cas existence check: %w", existsErr)
		}
		return SlotRow{}, ErrSlotPreconditionFailed
	}
	if err != nil {
		return SlotRow{}, fmt.Errorf("slots: update: %w", err)
	}
	return out, nil
}

// Delete removes a slot row. Idempotent: returns nil if no row matched.
func (s *SlotsStore) Delete(ctx context.Context, project string, slotIndex int) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("slots store not configured")
	}
	const sql = `DELETE FROM slots WHERE project = $1 AND slot_index = $2`
	if _, err := s.pool.Exec(ctx, sql, project, slotIndex); err != nil {
		return fmt.Errorf("slots: delete: %w", err)
	}
	return nil
}

// AppendHistory writes a new slot_history row. id is the caller-
// supplied uuid (already assigned by the slot-history layer).
func (s *SlotsStore) AppendHistory(ctx context.Context, row SlotHistoryRow) (SlotHistoryRow, error) {
	if s == nil || s.pool == nil {
		return SlotHistoryRow{}, fmt.Errorf("slots store not configured")
	}
	const insertSQL = `
		INSERT INTO slot_history (id, project, slot_index, payload, created_at)
		VALUES ($1::uuid, $2, $3, $4, now())
		RETURNING id, project, slot_index, payload, created_at
	`
	var out SlotHistoryRow
	if err := s.pool.QueryRow(ctx, insertSQL, row.ID, row.Project, row.SlotIndex, row.Payload).Scan(
		&out.ID, &out.Project, &out.SlotIndex, &out.Payload, &out.CreatedAt,
	); err != nil {
		return SlotHistoryRow{}, fmt.Errorf("slots: append history: %w", err)
	}
	return out, nil
}

// ListHistory returns slot_history rows for project, optionally
// scoped to a single slot_index, ordered by created_at ASC.
func (s *SlotsStore) ListHistory(ctx context.Context, project string, slotIndex *int) ([]SlotHistoryRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	sqlText := `SELECT id, project, slot_index, payload, created_at FROM slot_history WHERE project = $1`
	args := []any{project}
	if slotIndex != nil {
		sqlText += " AND slot_index = $2"
		args = append(args, *slotIndex)
	}
	sqlText += " ORDER BY created_at ASC"
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("slots: list history: %w", err)
	}
	defer rows.Close()
	out := []SlotHistoryRow{}
	for rows.Next() {
		var row SlotHistoryRow
		if err := rows.Scan(&row.ID, &row.Project, &row.SlotIndex, &row.Payload, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("slots: scan history: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("slots: iterate history: %w", err)
	}
	return out, nil
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

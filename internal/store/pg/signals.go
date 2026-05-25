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

// SignalsStore is the Postgres-backed signals store.
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

// List returns signals, optionally filtered by state (payload->>'state'),
// optionally filtered by target_repo, ordered by created_at ASC.
// limit applies if > 0.
func (s *SignalsStore) List(ctx context.Context, state, targetRepo string, limit int) ([]SignalRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	sqlText := `SELECT id, target_repo, payload, created_at, processed_at FROM signals`
	args := []any{}
	var where []string
	if state != "" {
		where = append(where, fmt.Sprintf("payload->>'state' = $%d", len(args)+1))
		args = append(args, state)
	}
	if targetRepo != "" {
		where = append(where, fmt.Sprintf("target_repo = $%d", len(args)+1))
		args = append(args, targetRepo)
	}
	if len(where) > 0 {
		sqlText += " WHERE " + where[0]
		for i := 1; i < len(where); i++ {
			sqlText += " AND " + where[i]
		}
	}
	sqlText += " ORDER BY created_at ASC"
	if limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("signals: list: %w", err)
	}
	defer rows.Close()
	out := []SignalRow{}
	for rows.Next() {
		var row SignalRow
		if err := rows.Scan(&row.ID, &row.TargetRepo, &row.Payload, &row.CreatedAt, &row.ProcessedAt); err != nil {
			return nil, fmt.Errorf("signals: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("signals: iterate: %w", err)
	}
	return out, nil
}

// Create inserts a new signal row. ID is the doc's id (uuid by caller).
func (s *SignalsStore) Create(ctx context.Context, row SignalRow) (SignalRow, error) {
	if s == nil || s.pool == nil {
		return SignalRow{}, fmt.Errorf("signals store not configured")
	}
	const insertSQL = `
		INSERT INTO signals (id, target_repo, payload, created_at, processed_at)
		VALUES ($1, $2, $3, now(), NULL)
		ON CONFLICT (id) DO NOTHING
		RETURNING id, target_repo, payload, created_at, processed_at
	`
	var out SignalRow
	err := s.pool.QueryRow(ctx, insertSQL, row.ID, row.TargetRepo, row.Payload).Scan(
		&out.ID, &out.TargetRepo, &out.Payload, &out.CreatedAt, &out.ProcessedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SignalRow{}, fmt.Errorf("signal %s already exists", row.ID)
	}
	if err != nil {
		return SignalRow{}, fmt.Errorf("signals: create: %w", err)
	}
	return out, nil
}

// GetByID looks up a signal by its id.
func (s *SignalsStore) GetByID(ctx context.Context, id string) (SignalRow, error) {
	if s == nil || s.pool == nil {
		return SignalRow{}, fmt.Errorf("signals store not configured")
	}
	const sql = `SELECT id, target_repo, payload, created_at, processed_at FROM signals WHERE id = $1`
	var out SignalRow
	if err := s.pool.QueryRow(ctx, sql, id).Scan(
		&out.ID, &out.TargetRepo, &out.Payload, &out.CreatedAt, &out.ProcessedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SignalRow{}, ErrSignalNotFound
		}
		return SignalRow{}, fmt.Errorf("signals: get by id: %w", err)
	}
	return out, nil
}

// PatchPayload mutates the jsonb payload inside a SELECT FOR UPDATE tx.
// The mutator may set top-level keys like state / processed_at /
// processed_decision / failure_reason. processed_at column is also
// derived from payload.processed_at if present.
func (s *SignalsStore) PatchPayload(ctx context.Context, id string, mutate func(payload map[string]any) error) (SignalRow, error) {
	if s == nil || s.pool == nil {
		return SignalRow{}, fmt.Errorf("signals store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SignalRow{}, fmt.Errorf("signals: begin patch: %w", err)
	}
	defer tx.Rollback(ctx)

	const selectSQL = `SELECT payload FROM signals WHERE id = $1 FOR UPDATE`
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, id).Scan(&payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SignalRow{}, ErrSignalNotFound
		}
		return SignalRow{}, fmt.Errorf("signals: select for patch: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return SignalRow{}, fmt.Errorf("signals: unmarshal payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return SignalRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return SignalRow{}, fmt.Errorf("signals: marshal patched payload: %w", err)
	}
	// processed_at column is derived from the payload so the partial
	// index (signals_unprocessed_by_repo WHERE processed_at IS NULL)
	// stays accurate as the state transitions.
	var processedAtArg any
	if v, ok := payload["processed_at"].(string); ok && v != "" {
		if t, perr := time.Parse(time.RFC3339Nano, v); perr == nil {
			processedAtArg = t
		}
	}
	const updateSQL = `
		UPDATE signals SET payload = $2, processed_at = $3
		WHERE id = $1
		RETURNING id, target_repo, payload, created_at, processed_at
	`
	var out SignalRow
	if err := tx.QueryRow(ctx, updateSQL, id, newPayload, processedAtArg).Scan(
		&out.ID, &out.TargetRepo, &out.Payload, &out.CreatedAt, &out.ProcessedAt,
	); err != nil {
		return SignalRow{}, fmt.Errorf("signals: update patched: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SignalRow{}, fmt.Errorf("signals: commit patch: %w", err)
	}
	return out, nil
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

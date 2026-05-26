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

var ErrPortfolioNotFound = errors.New("portfolio not found")

func NewPortfoliosStore(pool *pgxpool.Pool) *PortfoliosStore {
	return &PortfoliosStore{pool: pool}
}

// List returns portfolio rows, optionally scoped to a project and/or
// filtered by status, optionally limited.
func (s *PortfoliosStore) List(ctx context.Context, project, status string, limit *int) ([]PortfolioRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	sqlText := `SELECT project, route, element_id, payload, created_at, updated_at FROM portfolios`
	args := []any{}
	var where []string
	if project != "" {
		where = append(where, fmt.Sprintf("project = $%d", len(args)+1))
		args = append(args, project)
	}
	if status != "" {
		where = append(where, fmt.Sprintf("payload->>'status' = $%d", len(args)+1))
		args = append(args, status)
	}
	if len(where) > 0 {
		sqlText += " WHERE " + where[0]
		for i := 1; i < len(where); i++ {
			sqlText += " AND " + where[i]
		}
	}
	sqlText += " ORDER BY updated_at DESC"
	if limit != nil && *limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, *limit)
	}
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("portfolios: list: %w", err)
	}
	defer rows.Close()
	out := []PortfolioRow{}
	for rows.Next() {
		var row PortfolioRow
		if err := rows.Scan(&row.Project, &row.Route, &row.ElementID, &row.Payload, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("portfolios: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("portfolios: iterate: %w", err)
	}
	return out, nil
}

// Upsert creates or updates a portfolio row. On update, CreatedAt is preserved.
func (s *PortfoliosStore) Upsert(ctx context.Context, row PortfolioRow) (PortfolioRow, error) {
	if s == nil || s.pool == nil {
		return PortfolioRow{}, fmt.Errorf("portfolios store not configured")
	}
	const upsertSQL = `
		INSERT INTO portfolios (project, route, element_id, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (project, route, element_id) DO UPDATE
		  SET payload    = EXCLUDED.payload,
		      updated_at = now()
		RETURNING project, route, element_id, payload, created_at, updated_at
	`
	var out PortfolioRow
	if err := s.pool.QueryRow(ctx, upsertSQL, row.Project, row.Route, row.ElementID, row.Payload).Scan(
		&out.Project, &out.Route, &out.ElementID, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return PortfolioRow{}, fmt.Errorf("portfolios: upsert: %w", err)
	}
	return out, nil
}

// GetByRef looks up a portfolio row by (project, route, element_id).
func (s *PortfoliosStore) GetByRef(ctx context.Context, project, route, elementID string) (PortfolioRow, error) {
	if s == nil || s.pool == nil {
		return PortfolioRow{}, fmt.Errorf("portfolios store not configured")
	}
	const sql = `SELECT project, route, element_id, payload, created_at, updated_at FROM portfolios WHERE project = $1 AND route = $2 AND element_id = $3`
	var out PortfolioRow
	if err := s.pool.QueryRow(ctx, sql, project, route, elementID).Scan(
		&out.Project, &out.Route, &out.ElementID, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PortfolioRow{}, ErrPortfolioNotFound
		}
		return PortfolioRow{}, fmt.Errorf("portfolios: get by ref: %w", err)
	}
	return out, nil
}

// PatchPayload mutates the jsonb payload inside a SELECT FOR UPDATE tx.
func (s *PortfoliosStore) PatchPayload(ctx context.Context, project, route, elementID string, mutate func(payload map[string]any) error) (PortfolioRow, error) {
	if s == nil || s.pool == nil {
		return PortfolioRow{}, fmt.Errorf("portfolios store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PortfolioRow{}, fmt.Errorf("portfolios: begin patch: %w", err)
	}
	defer tx.Rollback(ctx)

	const selectSQL = `SELECT payload FROM portfolios WHERE project = $1 AND route = $2 AND element_id = $3 FOR UPDATE`
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, project, route, elementID).Scan(&payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PortfolioRow{}, ErrPortfolioNotFound
		}
		return PortfolioRow{}, fmt.Errorf("portfolios: select for patch: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return PortfolioRow{}, fmt.Errorf("portfolios: unmarshal payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return PortfolioRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return PortfolioRow{}, fmt.Errorf("portfolios: marshal patched payload: %w", err)
	}
	const updateSQL = `
		UPDATE portfolios SET payload = $4, updated_at = now()
		WHERE project = $1 AND route = $2 AND element_id = $3
		RETURNING project, route, element_id, payload, created_at, updated_at
	`
	var out PortfolioRow
	if err := tx.QueryRow(ctx, updateSQL, project, route, elementID, newPayload).Scan(
		&out.Project, &out.Route, &out.ElementID, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return PortfolioRow{}, fmt.Errorf("portfolios: update patched: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PortfolioRow{}, fmt.Errorf("portfolios: commit patch: %w", err)
	}
	return out, nil
}


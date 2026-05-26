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

var ErrPlaybookNotFound = errors.New("playbook not found")

func NewPlaybooksStore(pool *pgxpool.Pool) *PlaybooksStore {
	return &PlaybooksStore{pool: pool}
}

// List returns playbook rows, optionally scoped to a project and/or
// filtered by state.
func (s *PlaybooksStore) List(ctx context.Context, project, state string, limit *int) ([]PlaybookRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	sqlText := `SELECT project, name, payload, created_at, updated_at FROM playbooks`
	args := []any{}
	var where []string
	if project != "" {
		where = append(where, fmt.Sprintf("project = $%d", len(args)+1))
		args = append(args, project)
	}
	if state != "" {
		where = append(where, fmt.Sprintf("payload->>'state' = $%d", len(args)+1))
		args = append(args, state)
	}
	if len(where) > 0 {
		sqlText += " WHERE " + where[0]
		for i := 1; i < len(where); i++ {
			sqlText += " AND " + where[i]
		}
	}
	sqlText += " ORDER BY created_at DESC"
	if limit != nil && *limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, *limit)
	}
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("playbooks: list: %w", err)
	}
	defer rows.Close()
	out := []PlaybookRow{}
	for rows.Next() {
		var row PlaybookRow
		if err := rows.Scan(&row.Project, &row.Name, &row.Payload, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("playbooks: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("playbooks: iterate: %w", err)
	}
	return out, nil
}

// Create inserts a new playbook row.
func (s *PlaybooksStore) Create(ctx context.Context, row PlaybookRow) (PlaybookRow, error) {
	if s == nil || s.pool == nil {
		return PlaybookRow{}, fmt.Errorf("playbooks store not configured")
	}
	const sql = `
		INSERT INTO playbooks (project, name, payload, created_at, updated_at)
		VALUES ($1, $2, $3, now(), now())
		ON CONFLICT (project, name) DO NOTHING
		RETURNING project, name, payload, created_at, updated_at
	`
	var out PlaybookRow
	err := s.pool.QueryRow(ctx, sql, row.Project, row.Name, row.Payload).Scan(
		&out.Project, &out.Name, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already exists. Read it back so the caller can either return
		// the existing playbook or treat as conflict at the call site.
		return PlaybookRow{}, fmt.Errorf("playbook %s/%s already exists", row.Project, row.Name)
	}
	if err != nil {
		return PlaybookRow{}, fmt.Errorf("playbooks: create: %w", err)
	}
	return out, nil
}

// PatchPayload mutates the jsonb payload inside a SELECT FOR UPDATE
// transaction. Used by PatchPlaybookEntryGate.
func (s *PlaybooksStore) PatchPayload(ctx context.Context, project, name string, mutate func(payload map[string]any) error) (PlaybookRow, error) {
	if s == nil || s.pool == nil {
		return PlaybookRow{}, fmt.Errorf("playbooks store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PlaybookRow{}, fmt.Errorf("playbooks: begin patch: %w", err)
	}
	defer tx.Rollback(ctx)

	const selectSQL = `SELECT payload FROM playbooks WHERE project = $1 AND name = $2 FOR UPDATE`
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, project, name).Scan(&payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PlaybookRow{}, ErrPlaybookNotFound
		}
		return PlaybookRow{}, fmt.Errorf("playbooks: select for patch: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return PlaybookRow{}, fmt.Errorf("playbooks: unmarshal payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return PlaybookRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return PlaybookRow{}, fmt.Errorf("playbooks: marshal patched payload: %w", err)
	}
	const updateSQL = `
		UPDATE playbooks SET payload = $3, updated_at = now()
		WHERE project = $1 AND name = $2
		RETURNING project, name, payload, created_at, updated_at
	`
	var out PlaybookRow
	if err := tx.QueryRow(ctx, updateSQL, project, name, newPayload).Scan(
		&out.Project, &out.Name, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return PlaybookRow{}, fmt.Errorf("playbooks: update patched: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PlaybookRow{}, fmt.Errorf("playbooks: commit patch: %w", err)
	}
	return out, nil
}


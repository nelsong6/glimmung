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

// RunsStore is the Postgres-backed runs store.
type RunsStore struct {
	pool *pgxpool.Pool
}

type RunRow struct {
	ID          string
	Project     string
	IssueNumber *int
	Payload     []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

var ErrRunNotFound = errors.New("run not found")

func NewRunsStore(pool *pgxpool.Pool) *RunsStore {
	return &RunsStore{pool: pool}
}

// Get returns the run row for (project, id).
func (s *RunsStore) Get(ctx context.Context, project, id string) (RunRow, error) {
	if s == nil || s.pool == nil {
		return RunRow{}, fmt.Errorf("runs store not configured")
	}
	const sql = `SELECT id, project, issue_number, payload, created_at, updated_at FROM runs WHERE project = $1 AND id = $2`
	var out RunRow
	if err := s.pool.QueryRow(ctx, sql, project, id).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RunRow{}, ErrRunNotFound
		}
		return RunRow{}, fmt.Errorf("runs: get: %w", err)
	}
	return out, nil
}

// List returns runs for project, ordered by updated_at DESC. limit
// applies if > 0.
func (s *RunsStore) List(ctx context.Context, project string, limit int) ([]RunRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	sqlText := `SELECT id, project, issue_number, payload, created_at, updated_at FROM runs WHERE project = $1 ORDER BY updated_at DESC`
	args := []any{project}
	if limit > 0 {
		sqlText += " LIMIT $2"
		args = append(args, limit)
	}
	return s.queryRows(ctx, sqlText, args)
}

// ListAll returns every run row across all projects, ordered by
// updated_at DESC. Used by callers that need a global view.
func (s *RunsStore) ListAll(ctx context.Context) ([]RunRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `SELECT id, project, issue_number, payload, created_at, updated_at FROM runs ORDER BY updated_at DESC`
	return s.queryRows(ctx, sql, nil)
}

// ListByIssue returns every run for (project, issue_number) ordered
// by created_at ASC.
func (s *RunsStore) ListByIssue(ctx context.Context, project string, issueNumber int) ([]RunRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `
		SELECT id, project, issue_number, payload, created_at, updated_at
		FROM runs
		WHERE project = $1 AND issue_number = $2
		ORDER BY created_at ASC
	`
	return s.queryRows(ctx, sql, []any{project, issueNumber})
}

// FindByPR finds the most-recently-updated run whose payload
// references repo + pr_number. Both fields live inside the jsonb
// payload (issue_repo + pr_number).
func (s *RunsStore) FindByPR(ctx context.Context, repo string, prNumber int) (RunRow, error) {
	if s == nil || s.pool == nil {
		return RunRow{}, fmt.Errorf("runs store not configured")
	}
	const sql = `
		SELECT id, project, issue_number, payload, created_at, updated_at
		FROM runs
		WHERE payload->>'issue_repo' = $1
		  AND payload->>'pr_number' = $2
		ORDER BY updated_at DESC
		LIMIT 1
	`
	var out RunRow
	if err := s.pool.QueryRow(ctx, sql, repo, fmt.Sprintf("%d", prNumber)).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RunRow{}, ErrRunNotFound
		}
		return RunRow{}, fmt.Errorf("runs: find by pr: %w", err)
	}
	return out, nil
}

// Create inserts a new run row.
func (s *RunsStore) Create(ctx context.Context, row RunRow) (RunRow, error) {
	if s == nil || s.pool == nil {
		return RunRow{}, fmt.Errorf("runs store not configured")
	}
	const insertSQL = `
		INSERT INTO runs (id, project, issue_number, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (project, id) DO NOTHING
		RETURNING id, project, issue_number, payload, created_at, updated_at
	`
	var out RunRow
	err := s.pool.QueryRow(ctx, insertSQL, row.ID, row.Project, row.IssueNumber, row.Payload).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRow{}, fmt.Errorf("run %s/%s already exists", row.Project, row.ID)
	}
	if err != nil {
		return RunRow{}, fmt.Errorf("runs: create: %w", err)
	}
	return out, nil
}

// PatchPayload mutates the jsonb payload inside a SELECT FOR UPDATE
// tx. The mutator may also reset top-level keys; issue_number column is
// derived from payload.issue_number to keep the partial index
// runs_by_project_issue accurate.
func (s *RunsStore) PatchPayload(ctx context.Context, project, id string, mutate func(payload map[string]any) error) (RunRow, error) {
	if s == nil || s.pool == nil {
		return RunRow{}, fmt.Errorf("runs store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RunRow{}, fmt.Errorf("runs: begin patch: %w", err)
	}
	defer tx.Rollback(ctx)

	const selectSQL = `SELECT payload FROM runs WHERE project = $1 AND id = $2 FOR UPDATE`
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, project, id).Scan(&payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RunRow{}, ErrRunNotFound
		}
		return RunRow{}, fmt.Errorf("runs: select for patch: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return RunRow{}, fmt.Errorf("runs: unmarshal payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return RunRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return RunRow{}, fmt.Errorf("runs: marshal patched payload: %w", err)
	}
	// issue_number column tracks payload.issue_number for the partial index.
	var issueNumArg any
	if v, ok := payload["issue_number"].(float64); ok && int(v) > 0 {
		issueNumArg = int(v)
	}
	const updateSQL = `
		UPDATE runs SET payload = $3, issue_number = $4, updated_at = now()
		WHERE project = $1 AND id = $2
		RETURNING id, project, issue_number, payload, created_at, updated_at
	`
	var out RunRow
	if err := tx.QueryRow(ctx, updateSQL, project, id, newPayload, issueNumArg).Scan(
		&out.ID, &out.Project, &out.IssueNumber, &out.Payload, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return RunRow{}, fmt.Errorf("runs: update patched: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RunRow{}, fmt.Errorf("runs: commit patch: %w", err)
	}
	return out, nil
}

func (s *RunsStore) queryRows(ctx context.Context, sqlText string, args []any) ([]RunRow, error) {
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("runs: query: %w", err)
	}
	defer rows.Close()
	out := []RunRow{}
	for rows.Next() {
		var row RunRow
		if err := rows.Scan(&row.ID, &row.Project, &row.IssueNumber, &row.Payload, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("runs: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runs: iterate: %w", err)
	}
	return out, nil
}

// LeasesStore is the Postgres-backed leases store.
type LeasesStore struct {
	pool *pgxpool.Pool
}

type LeaseRow struct {
	ID            string
	Project       string
	CallbackToken string
	Payload       []byte
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     *time.Time
}

var ErrLeaseNotFound = errors.New("lease not found")

func NewLeasesStore(pool *pgxpool.Pool) *LeasesStore {
	return &LeasesStore{pool: pool}
}

// Get returns the lease row for (project, id).
func (s *LeasesStore) Get(ctx context.Context, project, id string) (LeaseRow, error) {
	if s == nil || s.pool == nil {
		return LeaseRow{}, fmt.Errorf("leases store not configured")
	}
	const sql = `SELECT id, project, callback_token, payload, created_at, updated_at, expires_at FROM leases WHERE id = $1 AND project = $2`
	var out LeaseRow
	if err := s.pool.QueryRow(ctx, sql, id, project).Scan(
		&out.ID, &out.Project, &out.CallbackToken, &out.Payload, &out.CreatedAt, &out.UpdatedAt, &out.ExpiresAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LeaseRow{}, ErrLeaseNotFound
		}
		return LeaseRow{}, fmt.Errorf("leases: get: %w", err)
	}
	return out, nil
}

// GetByCallbackToken looks up a lease by its callback_token (the
// indexed column extracted from payload.metadata.lease_callback_token
// at create-time).
func (s *LeasesStore) GetByCallbackToken(ctx context.Context, token string) (LeaseRow, error) {
	if s == nil || s.pool == nil {
		return LeaseRow{}, fmt.Errorf("leases store not configured")
	}
	const sql = `SELECT id, project, callback_token, payload, created_at, updated_at, expires_at FROM leases WHERE callback_token = $1`
	var out LeaseRow
	if err := s.pool.QueryRow(ctx, sql, token).Scan(
		&out.ID, &out.Project, &out.CallbackToken, &out.Payload, &out.CreatedAt, &out.UpdatedAt, &out.ExpiresAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LeaseRow{}, ErrLeaseNotFound
		}
		return LeaseRow{}, fmt.Errorf("leases: get by callback_token: %w", err)
	}
	return out, nil
}

// List returns leases for project, ordered by created_at DESC.
func (s *LeasesStore) List(ctx context.Context, project string) ([]LeaseRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `SELECT id, project, callback_token, payload, created_at, updated_at, expires_at FROM leases WHERE project = $1 ORDER BY created_at DESC`
	return s.queryLeaseRows(ctx, sql, []any{project})
}

// ListAll returns every lease across all projects.
func (s *LeasesStore) ListAll(ctx context.Context) ([]LeaseRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `SELECT id, project, callback_token, payload, created_at, updated_at, expires_at FROM leases ORDER BY created_at DESC`
	return s.queryLeaseRows(ctx, sql, nil)
}

// ListClaimedNative returns claimed native-k8s leases for project
// (used by availableNativeSlot to compute "slot used by another
// active lease").
func (s *LeasesStore) ListClaimedNative(ctx context.Context, project string) ([]LeaseRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `
		SELECT id, project, callback_token, payload, created_at, updated_at, expires_at
		FROM leases
		WHERE project = $1
		  AND payload->>'state' = 'claimed'
		  AND (payload->'metadata'->>'native_k8s')::bool = true
	`
	return s.queryLeaseRows(ctx, sql, []any{project})
}

// Create inserts a new lease row. callback_token is extracted from
// payload.metadata.lease_callback_token.
func (s *LeasesStore) Create(ctx context.Context, row LeaseRow) (LeaseRow, error) {
	if s == nil || s.pool == nil {
		return LeaseRow{}, fmt.Errorf("leases store not configured")
	}
	const insertSQL = `
		INSERT INTO leases (id, project, callback_token, payload, created_at, updated_at, expires_at)
		VALUES ($1, $2, $3, $4, now(), now(), $5)
		ON CONFLICT (id) DO NOTHING
		RETURNING id, project, callback_token, payload, created_at, updated_at, expires_at
	`
	var out LeaseRow
	err := s.pool.QueryRow(ctx, insertSQL, row.ID, row.Project, row.CallbackToken, row.Payload, row.ExpiresAt).Scan(
		&out.ID, &out.Project, &out.CallbackToken, &out.Payload, &out.CreatedAt, &out.UpdatedAt, &out.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeaseRow{}, fmt.Errorf("lease %s already exists", row.ID)
	}
	if err != nil {
		return LeaseRow{}, fmt.Errorf("leases: create: %w", err)
	}
	return out, nil
}

// PatchPayload mutates the jsonb payload inside a SELECT FOR UPDATE
// transaction. callback_token + expires_at columns are derived from
// the payload so the indexes stay accurate after state transitions.
func (s *LeasesStore) PatchPayload(ctx context.Context, project, id string, mutate func(payload map[string]any) error) (LeaseRow, error) {
	if s == nil || s.pool == nil {
		return LeaseRow{}, fmt.Errorf("leases store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LeaseRow{}, fmt.Errorf("leases: begin patch: %w", err)
	}
	defer tx.Rollback(ctx)
	const selectSQL = `SELECT payload FROM leases WHERE project = $1 AND id = $2 FOR UPDATE`
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, project, id).Scan(&payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LeaseRow{}, ErrLeaseNotFound
		}
		return LeaseRow{}, fmt.Errorf("leases: select for patch: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return LeaseRow{}, fmt.Errorf("leases: unmarshal payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return LeaseRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return LeaseRow{}, fmt.Errorf("leases: marshal patched payload: %w", err)
	}
	// Derive callback_token from payload metadata.lease_callback_token.
	var tokenArg any
	if meta, ok := payload["metadata"].(map[string]any); ok {
		if v, ok := meta["lease_callback_token"].(string); ok {
			tokenArg = v
		}
	}
	if tokenArg == nil {
		tokenArg = ""
	}
	// Derive expires_at from assigned_at + ttl_seconds when state==claimed.
	var expiresArg any
	if state, _ := payload["state"].(string); state == "claimed" {
		if assigned, ok := payload["assigned_at"].(string); ok && assigned != "" {
			if t, perr := time.Parse(time.RFC3339Nano, assigned); perr == nil {
				if ttlF, ok := payload["ttl_seconds"].(float64); ok && ttlF > 0 {
					exp := t.Add(time.Duration(ttlF) * time.Second)
					expiresArg = exp
				}
			}
		}
	}
	const updateSQL = `
		UPDATE leases SET payload = $3, callback_token = $4, expires_at = $5, updated_at = now()
		WHERE project = $1 AND id = $2
		RETURNING id, project, callback_token, payload, created_at, updated_at, expires_at
	`
	var out LeaseRow
	if err := tx.QueryRow(ctx, updateSQL, project, id, newPayload, tokenArg, expiresArg).Scan(
		&out.ID, &out.Project, &out.CallbackToken, &out.Payload, &out.CreatedAt, &out.UpdatedAt, &out.ExpiresAt,
	); err != nil {
		return LeaseRow{}, fmt.Errorf("leases: update patched: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return LeaseRow{}, fmt.Errorf("leases: commit patch: %w", err)
	}
	return out, nil
}

// AllocateNextNumber atomically allocates the next lease number for
// project. First call seeds the counter from MAX(leaseNumber) + 1
// over existing leases (read from payload->>'leaseNumber'); subsequent
// calls increment-and-return inside a tx.
func (s *LeasesStore) AllocateNextNumber(ctx context.Context, project string) (int, error) {
	if s == nil || s.pool == nil {
		return 0, fmt.Errorf("leases store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("leases: begin allocate: %w", err)
	}
	defer tx.Rollback(ctx)

	// Seed if missing.
	const seedSQL = `
		INSERT INTO lease_counters (project, next_number)
		SELECT $1, COALESCE(MAX((payload->>'leaseNumber')::int), 0) + 1
		FROM leases WHERE project = $1 AND payload->>'leaseNumber' IS NOT NULL
		ON CONFLICT (project) DO NOTHING
	`
	if _, err := tx.Exec(ctx, seedSQL, project); err != nil {
		return 0, fmt.Errorf("leases: seed counter: %w", err)
	}
	// Ensure a row exists (the seed SELECT returns no rows if there
	// are no existing leases for the project; INSERT ... SELECT with
	// an empty subquery does nothing). Add a fallback seed.
	const fallbackSeedSQL = `
		INSERT INTO lease_counters (project, next_number)
		VALUES ($1, 1)
		ON CONFLICT (project) DO NOTHING
	`
	if _, err := tx.Exec(ctx, fallbackSeedSQL, project); err != nil {
		return 0, fmt.Errorf("leases: fallback seed counter: %w", err)
	}
	// Atomic increment-and-return prior value.
	const allocSQL = `
		UPDATE lease_counters
		SET next_number = next_number + 1
		WHERE project = $1
		RETURNING next_number - 1
	`
	var allocated int
	if err := tx.QueryRow(ctx, allocSQL, project).Scan(&allocated); err != nil {
		return 0, fmt.Errorf("leases: allocate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("leases: commit allocate: %w", err)
	}
	return allocated, nil
}

func (s *LeasesStore) queryLeaseRows(ctx context.Context, sqlText string, args []any) ([]LeaseRow, error) {
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("leases: query: %w", err)
	}
	defer rows.Close()
	out := []LeaseRow{}
	for rows.Next() {
		var row LeaseRow
		if err := rows.Scan(&row.ID, &row.Project, &row.CallbackToken, &row.Payload, &row.CreatedAt, &row.UpdatedAt, &row.ExpiresAt); err != nil {
			return nil, fmt.Errorf("leases: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("leases: iterate: %w", err)
	}
	return out, nil
}

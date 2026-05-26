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

// ProjectsStore is the Postgres-backed projects + test-lease-defaults
// store. Replaces cosmos.Store.projects (the `projects` container, which
// held both per-project rows kind='project' and the singleton settings
// doc kind='test-lease-defaults') per Stage 2d of docs/postgres-migration.md.
//
// Two tables back this store: `projects` (per-project rows, name PK,
// payload jsonb for the metadata that cosmos kept inline) and
// `test_lease_defaults` (single-row settings; the cosmos sentinel doc
// moves into its own table to keep the `projects` table free of
// discriminator-column tricks).
//
// Read-modify-write helpers for the SetProject* methods take a
// SERIALIZABLE-equivalent shape via `SELECT ... FOR UPDATE` inside a
// transaction. That replaces the cosmos read+ReplaceItem pattern which
// had no CAS protection (the cosmos code didn't use ETag IfMatch on
// these methods — concurrent writes would race in cosmos too). Postgres
// row locking gives us strict serialization "for free."
type ProjectsStore struct {
	pool *pgxpool.Pool
}

// TestLeaseDefaultsSingletonID is the row id under which the global
// test-lease defaults live. Matches the cosmos sentinel doc id so any
// operator query that referenced that id by string continues to work.
const TestLeaseDefaultsSingletonID = "test-lease-defaults"

// ProjectRow is the row shape the cosmos-side migration source emits
// and that ProjectsStore.Migrate consumes. Closely mirrors the cosmos
// projectDoc — the `payload` map captures the metadata jsonb (which is
// what the cosmos doc stored under `metadata`).
type ProjectRow struct {
	Name       string
	GitHubRepo string
	ArgoCDApp  string
	Metadata   map[string]any
	CreatedAt  time.Time
}

// TestLeaseDefaultsRow is the singleton row shape Migrate consumes for
// the global settings.
type TestLeaseDefaultsRow struct {
	GlobalTTLSeconds     int
	HotSwapMinTTLSeconds int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// ProjectRegister is the narrow payload UpsertProject accepts. Matches
// the field set the server's ProjectRegister type carries (Name +
// GitHubRepo + Metadata); kept separate so this package doesn't import
// internal/server.
type ProjectRegister struct {
	Name       string
	GitHubRepo string
	Metadata   map[string]any
}

// ProjectRecord is the canonical return shape. cosmos.Store's
// SetProject* wrappers convert this back to server.Project at the
// call site.
type ProjectRecord struct {
	Name       string
	GitHubRepo string
	ArgoCDApp  string
	Metadata   map[string]any
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

var ErrProjectNotFound = errors.New("project not found")

func NewProjectsStore(pool *pgxpool.Pool) *ProjectsStore {
	return &ProjectsStore{pool: pool}
}

// List returns every per-project row, excluding the singleton settings
// (which now lives in test_lease_defaults). Ordering is unspecified —
// matches cosmos.ListProjects.
func (s *ProjectsStore) List(ctx context.Context) ([]ProjectRecord, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("projects store not configured")
	}
	const sql = `SELECT name, github_repo, payload, created_at, updated_at FROM projects WHERE kind = 'project'`
	rows, err := s.pool.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("projects: list: %w", err)
	}
	defer rows.Close()

	out := []ProjectRecord{}
	for rows.Next() {
		rec, err := scanProjectRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projects: iterate list: %w", err)
	}
	return out, nil
}

// ListNames returns project names in unspecified order. Equivalent to
// cosmos.Store.listProjectNames; used by fanOutByProject.
func (s *ProjectsStore) ListNames(ctx context.Context) ([]string, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("projects store not configured")
	}
	const sql = `SELECT name FROM projects WHERE kind = 'project'`
	rows, err := s.pool.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("projects: list names: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("projects: scan name: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projects: iterate names: %w", err)
	}
	return out, nil
}

// Read returns one project by name. Returns ErrProjectNotFound if no
// row exists.
func (s *ProjectsStore) Read(ctx context.Context, name string) (ProjectRecord, error) {
	if s == nil || s.pool == nil {
		return ProjectRecord{}, fmt.Errorf("projects store not configured")
	}
	const sql = `SELECT name, github_repo, payload, created_at, updated_at FROM projects WHERE kind = 'project' AND name = $1`
	rows, err := s.pool.Query(ctx, sql, name)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: read: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return ProjectRecord{}, ErrProjectNotFound
	}
	return scanProjectRow(rows)
}

// ReadGitHubRepo is the narrow lookup used by dispatch/clone paths.
// Returns "" + ErrProjectNotFound when the project doesn't exist.
func (s *ProjectsStore) ReadGitHubRepo(ctx context.Context, name string) (string, error) {
	if s == nil || s.pool == nil {
		return "", fmt.Errorf("projects store not configured")
	}
	const sql = `SELECT github_repo FROM projects WHERE kind = 'project' AND name = $1`
	var repo string
	if err := s.pool.QueryRow(ctx, sql, name).Scan(&repo); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrProjectNotFound
		}
		return "", fmt.Errorf("projects: read github repo: %w", err)
	}
	return repo, nil
}

// Upsert creates or updates a project row. On update, the original
// CreatedAt is preserved; updated_at is set to now(). The github_repo
// column gets mirrored from req.GitHubRepo for indexed lookup; the
// canonical value also lives in payload.
func (s *ProjectsStore) Upsert(ctx context.Context, req ProjectRegister) (ProjectRecord, error) {
	if s == nil || s.pool == nil {
		return ProjectRecord{}, fmt.Errorf("projects store not configured")
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{
		"name":       req.Name,
		"githubRepo": req.GitHubRepo,
		"metadata":   req.Metadata,
	})
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: marshal payload: %w", err)
	}
	const upsertSQL = `
		INSERT INTO projects (name, kind, payload, github_repo, created_at, updated_at)
		VALUES ($1, 'project', $2, $3, now(), now())
		ON CONFLICT (name) DO UPDATE
		  SET payload     = EXCLUDED.payload,
		      github_repo = EXCLUDED.github_repo,
		      updated_at  = now()
		RETURNING name, github_repo, payload, created_at, updated_at
	`
	rows, err := s.pool.Query(ctx, upsertSQL, req.Name, payload, req.GitHubRepo)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: upsert: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return ProjectRecord{}, fmt.Errorf("projects: upsert returned no row")
	}
	return scanProjectRow(rows)
}

// mutateProject is the shared read-modify-write helper. Wraps the
// operation in a transaction with `SELECT ... FOR UPDATE` so concurrent
// writers serialize at the row level. The mutator receives the current
// metadata map and returns the new metadata; it does not need to worry
// about the surrounding payload structure.
func (s *ProjectsStore) mutateProject(ctx context.Context, name string, mutate func(metadata map[string]any) error) (ProjectRecord, error) {
	if s == nil || s.pool == nil {
		return ProjectRecord{}, fmt.Errorf("projects store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: begin mutate: %w", err)
	}
	defer tx.Rollback(ctx)

	const selectSQL = `SELECT payload, github_repo FROM projects WHERE kind = 'project' AND name = $1 FOR UPDATE`
	var payloadBytes []byte
	var githubRepo string
	if err := tx.QueryRow(ctx, selectSQL, name).Scan(&payloadBytes, &githubRepo); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProjectRecord{}, ErrProjectNotFound
		}
		return ProjectRecord{}, fmt.Errorf("projects: select for update: %w", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(payloadBytes, &doc); err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: unmarshal payload: %w", err)
	}
	metadata, _ := doc["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	if err := mutate(metadata); err != nil {
		return ProjectRecord{}, err
	}
	doc["metadata"] = metadata

	newPayload, err := json.Marshal(doc)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: marshal mutated payload: %w", err)
	}
	const updateSQL = `
		UPDATE projects
		SET payload = $2, updated_at = now()
		WHERE kind = 'project' AND name = $1
		RETURNING name, github_repo, payload, created_at, updated_at
	`
	rows, err := tx.Query(ctx, updateSQL, name, newPayload)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: update mutated: %w", err)
	}
	if !rows.Next() {
		rows.Close()
		return ProjectRecord{}, fmt.Errorf("projects: update returned no row")
	}
	rec, err := scanProjectRow(rows)
	if err != nil {
		rows.Close()
		return ProjectRecord{}, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: update mutated rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: commit mutate: %w", err)
	}
	return rec, nil
}

// SetTestEnvironmentCount updates metadata.native_standby_dns.count and
// strips the legacy `slots` array (per the #518 slot-storage rework).
// The count under metadata.native_standby_workload_identity is mirrored
// when that nested map exists, matching the cosmos behavior.
func (s *ProjectsStore) SetTestEnvironmentCount(ctx context.Context, name string, count int) (ProjectRecord, error) {
	return s.mutateProject(ctx, name, func(metadata map[string]any) error {
		standbyDNS, _ := metadata["native_standby_dns"].(map[string]any)
		if standbyDNS == nil {
			standbyDNS = map[string]any{}
		}
		standbyDNS["count"] = count
		delete(standbyDNS, "slots")
		metadata["native_standby_dns"] = standbyDNS
		if wi, ok := metadata["native_standby_workload_identity"].(map[string]any); ok {
			wi["count"] = count
			metadata["native_standby_workload_identity"] = wi
		}
		return nil
	})
}

// SetNativeWorkloadIdentityStatus sets the
// metadata.native_standby_workload_identity_status field.
func (s *ProjectsStore) SetNativeWorkloadIdentityStatus(ctx context.Context, name string, status any) (ProjectRecord, error) {
	return s.mutateProject(ctx, name, func(metadata map[string]any) error {
		metadata["native_standby_workload_identity_status"] = status
		return nil
	})
}

// SetManagedAuthOriginStatus sets the
// metadata.managed_auth_origin_status field.
func (s *ProjectsStore) SetManagedAuthOriginStatus(ctx context.Context, name string, status any) (ProjectRecord, error) {
	return s.mutateProject(ctx, name, func(metadata map[string]any) error {
		metadata["managed_auth_origin_status"] = status
		return nil
	})
}

// SetTestLeaseDefaultTTL updates the per-project TTL. ttlSeconds=nil
// clears the field (caller-controlled "use global default").
func (s *ProjectsStore) SetTestLeaseDefaultTTL(ctx context.Context, name string, ttlSeconds *int) (ProjectRecord, error) {
	return s.mutateProject(ctx, name, func(metadata map[string]any) error {
		// Drop the legacy camelCase key cosmos historically used; the
		// snake_case key is the canonical field.
		delete(metadata, "testLeaseDefaultTTLSeconds")
		if ttlSeconds == nil {
			delete(metadata, "test_lease_default_ttl_seconds")
		} else {
			metadata["test_lease_default_ttl_seconds"] = *ttlSeconds
		}
		return nil
	})
}

// SetTestLeaseHotSwapMinTTL updates the per-project hot-swap min TTL.
func (s *ProjectsStore) SetTestLeaseHotSwapMinTTL(ctx context.Context, name string, ttlSeconds *int) (ProjectRecord, error) {
	return s.mutateProject(ctx, name, func(metadata map[string]any) error {
		delete(metadata, "testLeaseHotSwapMinTTLSeconds")
		if ttlSeconds == nil {
			delete(metadata, "test_lease_hot_swap_min_ttl_seconds")
		} else {
			metadata["test_lease_hot_swap_min_ttl_seconds"] = *ttlSeconds
		}
		return nil
	})
}

// StripLegacySlotsArray removes metadata.native_standby_dns.slots[].
// Called by the one-shot slot-storage-rework migration in
// internal/server/. Idempotent: re-running is harmless.
// Stage 2i deletes this method along with the slot-storage migration.
func (s *ProjectsStore) StripLegacySlotsArray(ctx context.Context, name string) error {
	_, err := s.mutateProject(ctx, name, func(metadata map[string]any) error {
		standbyDNS, _ := metadata["native_standby_dns"].(map[string]any)
		if standbyDNS == nil {
			return nil
		}
		delete(standbyDNS, "slots")
		metadata["native_standby_dns"] = standbyDNS
		return nil
	})
	return err
}

// ReadTestLeaseDefaults returns the global settings row. Returns
// ErrProjectNotFound when no row exists (the upsert paths create one
// on first write).
func (s *ProjectsStore) ReadTestLeaseDefaults(ctx context.Context) (TestLeaseDefaultsRow, error) {
	if s == nil || s.pool == nil {
		return TestLeaseDefaultsRow{}, fmt.Errorf("projects store not configured")
	}
	const sql = `SELECT global_ttl_seconds, hot_swap_min_ttl_seconds, created_at, updated_at FROM test_lease_defaults WHERE id = $1`
	var row TestLeaseDefaultsRow
	if err := s.pool.QueryRow(ctx, sql, TestLeaseDefaultsSingletonID).Scan(
		&row.GlobalTTLSeconds, &row.HotSwapMinTTLSeconds, &row.CreatedAt, &row.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TestLeaseDefaultsRow{}, ErrProjectNotFound
		}
		return TestLeaseDefaultsRow{}, fmt.Errorf("projects: read defaults: %w", err)
	}
	return row, nil
}

// SetGlobalTestLeaseDefaultTTL upserts the singleton row's
// global_ttl_seconds. nil clears (sets to 0, matching the cosmos
// "field unset → 0" behavior).
func (s *ProjectsStore) SetGlobalTestLeaseDefaultTTL(ctx context.Context, ttlSeconds *int) (TestLeaseDefaultsRow, error) {
	value := 0
	if ttlSeconds != nil {
		value = *ttlSeconds
	}
	return s.upsertGlobalDefault(ctx, "global_ttl_seconds", value)
}

// SetGlobalTestLeaseHotSwapMinTTL upserts the singleton row's
// hot_swap_min_ttl_seconds.
func (s *ProjectsStore) SetGlobalTestLeaseHotSwapMinTTL(ctx context.Context, ttlSeconds *int) (TestLeaseDefaultsRow, error) {
	value := 0
	if ttlSeconds != nil {
		value = *ttlSeconds
	}
	return s.upsertGlobalDefault(ctx, "hot_swap_min_ttl_seconds", value)
}

func (s *ProjectsStore) upsertGlobalDefault(ctx context.Context, column string, value int) (TestLeaseDefaultsRow, error) {
	if s == nil || s.pool == nil {
		return TestLeaseDefaultsRow{}, fmt.Errorf("projects store not configured")
	}
	// The column is a fixed allowlist controlled by callers in this
	// file; no SQL injection risk. The CONFLICT path overrides only
	// the specified column.
	var sql string
	switch column {
	case "global_ttl_seconds":
		sql = `
			INSERT INTO test_lease_defaults (id, global_ttl_seconds, created_at, updated_at)
			VALUES ($1, $2, now(), now())
			ON CONFLICT (id) DO UPDATE
			  SET global_ttl_seconds = EXCLUDED.global_ttl_seconds, updated_at = now()
			RETURNING global_ttl_seconds, hot_swap_min_ttl_seconds, created_at, updated_at
		`
	case "hot_swap_min_ttl_seconds":
		sql = `
			INSERT INTO test_lease_defaults (id, hot_swap_min_ttl_seconds, created_at, updated_at)
			VALUES ($1, $2, now(), now())
			ON CONFLICT (id) DO UPDATE
			  SET hot_swap_min_ttl_seconds = EXCLUDED.hot_swap_min_ttl_seconds, updated_at = now()
			RETURNING global_ttl_seconds, hot_swap_min_ttl_seconds, created_at, updated_at
		`
	default:
		return TestLeaseDefaultsRow{}, fmt.Errorf("projects: unknown defaults column %q", column)
	}
	var row TestLeaseDefaultsRow
	if err := s.pool.QueryRow(ctx, sql, TestLeaseDefaultsSingletonID, value).Scan(
		&row.GlobalTTLSeconds, &row.HotSwapMinTTLSeconds, &row.CreatedAt, &row.UpdatedAt,
	); err != nil {
		return TestLeaseDefaultsRow{}, fmt.Errorf("projects: upsert defaults: %w", err)
	}
	return row, nil
}

func scanProjectRow(rows pgx.Rows) (ProjectRecord, error) {
	var rec ProjectRecord
	var payloadBytes []byte
	if err := rows.Scan(&rec.Name, &rec.GitHubRepo, &payloadBytes, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: scan row: %w", err)
	}
	doc := map[string]any{}
	if len(payloadBytes) > 0 {
		if err := json.Unmarshal(payloadBytes, &doc); err != nil {
			return ProjectRecord{}, fmt.Errorf("projects: unmarshal payload: %w", err)
		}
	}
	rec.GitHubRepo = stringFromMap(doc, "githubRepo", rec.GitHubRepo)
	rec.ArgoCDApp = stringFromMap(doc, "argocdApp", "")
	rec.Metadata = mapFromMap(doc, "metadata")
	return rec, nil
}

func ensureMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func stringFromMap(m map[string]any, key, fallback string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

func mapFromMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok && v != nil {
		return v
	}
	return map[string]any{}
}

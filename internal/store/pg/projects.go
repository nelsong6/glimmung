package pg

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProjectsStore is the Postgres-backed projects + test-lease-defaults store.
// Three tables back it: `projects` for per-project rows,
// `project_config_schemas` for the immutable authored-config version history,
// and `test_lease_defaults` for the singleton global settings row.
//
// Authored config and reconciler-owned status are kept in separate columns:
// `payload` holds the authored config a register/sync replaces wholesale (with
// an immutable version minted in `project_config_schemas` on every change),
// while `status` holds the reconciler outputs (managed_auth_origin_status,
// native_standby_workload_identity_status) that the status setters own. Reads
// merge the two so the returned shape is unchanged. See
// docs/durable-project-config.md.
//
// Read-modify-write helpers (mutateProject for authored config, mutateStatus
// for reconciler status) take a SERIALIZABLE-equivalent shape via
// `SELECT ... FOR UPDATE` inside a transaction. Postgres row locking gives
// these updates strict serialization.
type ProjectsStore struct {
	pool *pgxpool.Pool
}

// TestLeaseDefaultsSingletonID is the row id under which the global
// test-lease defaults live.
const TestLeaseDefaultsSingletonID = "test-lease-defaults"

// projectSelectColumns is the canonical column list every project read returns,
// kept in one place so List/Read/Upsert/mutate* stay in lockstep with
// scanProjectRow.
const projectSelectColumns = "name, github_repo, payload, status, config_schema_ref, created_at, updated_at"

// insertProjectConfigSchemaSQL mints an immutable authored-config version. The
// (name, schema_ref) conflict is a no-op because schema_ref is the content
// hash: re-registering identical config does not duplicate history.
const insertProjectConfigSchemaSQL = `
	INSERT INTO project_config_schemas (name, schema_ref, payload, created_at)
	VALUES ($1, $2, $3, now())
	ON CONFLICT (name, schema_ref) DO NOTHING
`

// serverManagedProjectStatusKeys are reconciler-owned outputs that live in the
// projects.status column, never in authored config. They are stripped from any
// inbound register/sync metadata (so a round-tripped read cannot pollute
// authored config or its content hash) and merged back into the returned
// metadata on read so the API shape is unchanged. See
// docs/durable-project-config.md.
var serverManagedProjectStatusKeys = []string{
	"managed_auth_origin_status",
	"native_standby_workload_identity_status",
}

func isServerManagedProjectStatusKey(key string) bool {
	for _, k := range serverManagedProjectStatusKeys {
		if k == key {
			return true
		}
	}
	return false
}

// stripServerManagedStatus returns a copy of metadata with the reconciler-owned
// status keys removed. Authored config never carries these.
func stripServerManagedStatus(metadata map[string]any) map[string]any {
	out := make(map[string]any, len(metadata))
	for k, v := range metadata {
		if isServerManagedProjectStatusKey(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// mergeStatusIntoMetadata overlays the reconciler-owned status column onto a
// copy of the authored metadata so callers see the unified historical shape.
// Status values win — they are the live reconciled outputs.
func mergeStatusIntoMetadata(metadata, status map[string]any) map[string]any {
	out := make(map[string]any, len(metadata)+len(status))
	for k, v := range metadata {
		out[k] = v
	}
	for k, v := range status {
		out[k] = v
	}
	return out
}

// projectConfigSchemaRef is the content hash of the authored-config document.
// Identical to workflowSchemaRef in spirit: a stable JSON canonicalization
// hashed with sha256. encoding/json sorts map keys, so the byte sequence is
// deterministic for a given authored config.
func projectConfigSchemaRef(name, githubRepo string, authoredMetadata map[string]any) string {
	canonical := struct {
		Name       string         `json:"name"`
		GitHubRepo string         `json:"githubRepo"`
		Metadata   map[string]any `json:"metadata"`
	}{
		Name:       name,
		GitHubRepo: githubRepo,
		Metadata:   ensureMap(authoredMetadata),
	}
	payload, _ := json.Marshal(canonical)
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("pcs_%x", sum[:8])
}

// ProjectRow is the row shape ProjectsStore persists and returns.
type ProjectRow struct {
	Name       string
	GitHubRepo string
	ArgoCDApp  string
	Metadata   map[string]any
	CreatedAt  time.Time
}

// TestLeaseDefaultsRow is the singleton row shape for global settings.
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

// ProjectRecord is the canonical return shape. Store's
// SetProject* wrappers convert this back to server.Project at the
// call site. Metadata is the unified read shape (authored config + reconciler
// status merged); Status is the reconciler-owned subset on its own for callers
// that need it.
type ProjectRecord struct {
	Name            string
	GitHubRepo      string
	ArgoCDApp       string
	Metadata        map[string]any
	Status          map[string]any
	ConfigSchemaRef string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ProjectWriteOutcome describes what an Upsert did, for observability. Created
// is true when the row did not exist; Versioned is true when the authored
// config content changed (the schema_ref pointer moved).
type ProjectWriteOutcome struct {
	Created       bool
	Versioned     bool
	SchemaRef     string
	PrevSchemaRef string
}

var ErrProjectNotFound = errors.New("project not found")

func NewProjectsStore(pool *pgxpool.Pool) *ProjectsStore {
	return &ProjectsStore{pool: pool}
}

// List returns every per-project row. Ordering is unspecified.
func (s *ProjectsStore) List(ctx context.Context) ([]ProjectRecord, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("projects store not configured")
	}
	sql := `SELECT ` + projectSelectColumns + ` FROM projects WHERE kind = 'project'`
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

// ListNames returns project names in unspecified order.
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
	sql := `SELECT ` + projectSelectColumns + ` FROM projects WHERE kind = 'project' AND name = $1`
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

// Upsert creates or updates a project's authored config. The write is
// transactional: it mints an immutable project_config_schemas version, moves
// the config_schema_ref pointer, and replaces `payload` — while leaving the
// reconciler-owned `status` column untouched. Server-managed status keys in the
// inbound metadata are stripped so a round-tripped read cannot pollute authored
// config. On update the original CreatedAt is preserved; updated_at is set to
// now(). The github_repo column is mirrored for indexed lookup; the canonical
// value also lives in payload.
func (s *ProjectsStore) Upsert(ctx context.Context, req ProjectRegister) (ProjectRecord, ProjectWriteOutcome, error) {
	if s == nil || s.pool == nil {
		return ProjectRecord{}, ProjectWriteOutcome{}, fmt.Errorf("projects store not configured")
	}
	authored := stripServerManagedStatus(ensureMap(req.Metadata))
	payload, err := json.Marshal(map[string]any{
		"name":       req.Name,
		"githubRepo": req.GitHubRepo,
		"metadata":   authored,
	})
	if err != nil {
		return ProjectRecord{}, ProjectWriteOutcome{}, fmt.Errorf("projects: marshal payload: %w", err)
	}
	schemaRef := projectConfigSchemaRef(req.Name, req.GitHubRepo, authored)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectRecord{}, ProjectWriteOutcome{}, fmt.Errorf("projects: begin upsert: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the row (if any) and capture prior pointer for the outcome.
	var prevRef string
	existed := true
	if err := tx.QueryRow(ctx, `SELECT config_schema_ref FROM projects WHERE kind = 'project' AND name = $1 FOR UPDATE`, req.Name).Scan(&prevRef); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			existed = false
		} else {
			return ProjectRecord{}, ProjectWriteOutcome{}, fmt.Errorf("projects: lock for upsert: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, insertProjectConfigSchemaSQL, req.Name, schemaRef, payload); err != nil {
		return ProjectRecord{}, ProjectWriteOutcome{}, fmt.Errorf("projects: mint config schema: %w", err)
	}

	const upsertSQL = `
		INSERT INTO projects (name, kind, payload, status, github_repo, config_schema_ref, created_at, updated_at)
		VALUES ($1, 'project', $2, '{}'::jsonb, $3, $4, now(), now())
		ON CONFLICT (name) DO UPDATE
		  SET payload           = EXCLUDED.payload,
		      github_repo       = EXCLUDED.github_repo,
		      config_schema_ref = EXCLUDED.config_schema_ref,
		      updated_at        = now()
		RETURNING ` + projectSelectColumns + `
	`
	rows, err := tx.Query(ctx, upsertSQL, req.Name, payload, req.GitHubRepo, schemaRef)
	if err != nil {
		return ProjectRecord{}, ProjectWriteOutcome{}, fmt.Errorf("projects: upsert: %w", err)
	}
	rec, scanErr := scanProjectFirstRow(rows)
	rows.Close()
	if scanErr != nil {
		return ProjectRecord{}, ProjectWriteOutcome{}, scanErr
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectRecord{}, ProjectWriteOutcome{}, fmt.Errorf("projects: commit upsert: %w", err)
	}
	outcome := ProjectWriteOutcome{
		Created:       !existed,
		Versioned:     schemaRef != prevRef,
		SchemaRef:     schemaRef,
		PrevSchemaRef: prevRef,
	}
	return rec, outcome, nil
}

// mutateProject is the shared read-modify-write helper for authored config.
// Wraps the operation in a transaction with `SELECT ... FOR UPDATE` so
// concurrent writers serialize at the row level. The mutator receives the
// current authored metadata map and returns the new metadata; it does not need
// to worry about the surrounding payload structure or the status column. Each
// mutation re-versions: a new project_config_schemas row is minted and the
// pointer moved, keeping config_schema_ref consistent with payload.
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
	authored := stripServerManagedStatus(metadata)
	doc["metadata"] = authored

	newPayload, err := json.Marshal(doc)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: marshal mutated payload: %w", err)
	}
	canonicalRepo := stringFromMap(doc, "githubRepo", githubRepo)
	schemaRef := projectConfigSchemaRef(name, canonicalRepo, authored)
	if _, err := tx.Exec(ctx, insertProjectConfigSchemaSQL, name, schemaRef, newPayload); err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: mint config schema: %w", err)
	}

	const updateSQL = `
		UPDATE projects
		SET payload = $2, config_schema_ref = $3, updated_at = now()
		WHERE kind = 'project' AND name = $1
		RETURNING ` + projectSelectColumns + `
	`
	rows, err := tx.Query(ctx, updateSQL, name, newPayload, schemaRef)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: update mutated: %w", err)
	}
	rec, scanErr := scanProjectFirstRow(rows)
	rows.Close()
	if scanErr != nil {
		return ProjectRecord{}, scanErr
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: commit mutate: %w", err)
	}
	return rec, nil
}

// mutateStatus is the shared read-modify-write helper for the reconciler-owned
// status column. It never touches authored config (payload) or the
// config_schema_ref version pointer.
func (s *ProjectsStore) mutateStatus(ctx context.Context, name string, mutate func(status map[string]any) error) (ProjectRecord, error) {
	if s == nil || s.pool == nil {
		return ProjectRecord{}, fmt.Errorf("projects store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: begin status mutate: %w", err)
	}
	defer tx.Rollback(ctx)

	const selectSQL = `SELECT status FROM projects WHERE kind = 'project' AND name = $1 FOR UPDATE`
	var statusBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, name).Scan(&statusBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProjectRecord{}, ErrProjectNotFound
		}
		return ProjectRecord{}, fmt.Errorf("projects: select status for update: %w", err)
	}

	status := map[string]any{}
	if len(statusBytes) > 0 {
		if err := json.Unmarshal(statusBytes, &status); err != nil {
			return ProjectRecord{}, fmt.Errorf("projects: unmarshal status: %w", err)
		}
	}
	if err := mutate(status); err != nil {
		return ProjectRecord{}, err
	}
	newStatus, err := json.Marshal(status)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: marshal mutated status: %w", err)
	}

	const updateSQL = `
		UPDATE projects
		SET status = $2, updated_at = now()
		WHERE kind = 'project' AND name = $1
		RETURNING ` + projectSelectColumns + `
	`
	rows, err := tx.Query(ctx, updateSQL, name, newStatus)
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: update status: %w", err)
	}
	rec, scanErr := scanProjectFirstRow(rows)
	rows.Close()
	if scanErr != nil {
		return ProjectRecord{}, scanErr
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: commit status mutate: %w", err)
	}
	return rec, nil
}

// SetTestEnvironmentCount updates metadata.native_standby_dns.count and
// strips the embedded `slots` array. The count under
// metadata.native_standby_workload_identity is mirrored when that nested map
// exists. This is authored config, so it re-versions.
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

// SetNativeWorkloadIdentityStatus sets the reconciler-owned
// native_standby_workload_identity_status in the status column.
func (s *ProjectsStore) SetNativeWorkloadIdentityStatus(ctx context.Context, name string, status any) (ProjectRecord, error) {
	return s.mutateStatus(ctx, name, func(current map[string]any) error {
		current["native_standby_workload_identity_status"] = status
		return nil
	})
}

// SetManagedAuthOriginStatus sets the reconciler-owned
// managed_auth_origin_status in the status column.
func (s *ProjectsStore) SetManagedAuthOriginStatus(ctx context.Context, name string, status any) (ProjectRecord, error) {
	return s.mutateStatus(ctx, name, func(current map[string]any) error {
		current["managed_auth_origin_status"] = status
		return nil
	})
}

// SetTestLeaseDefaultTTL updates the per-project TTL. ttlSeconds=nil
// clears the field (caller-controlled "use global default").
func (s *ProjectsStore) SetTestLeaseDefaultTTL(ctx context.Context, name string, ttlSeconds *int) (ProjectRecord, error) {
	return s.mutateProject(ctx, name, func(metadata map[string]any) error {
		// Drop the camelCase key; snake_case is the canonical field.
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
// Called by the one-shot slot-storage cleanup in internal/server/.
// Idempotent: re-running is harmless.
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

// BackfillConfigSchemas seeds config_schema_ref + project_config_schemas for
// any project row that has no version yet (config_schema_ref = ”). Computing
// the content hash in Go keeps it identical to the live write path — the
// migration deliberately does not try to reproduce the hash in SQL. Idempotent:
// rows that already carry a version are skipped, and the schema insert is a
// no-op on conflict. Returns the number of rows seeded.
func (s *ProjectsStore) BackfillConfigSchemas(ctx context.Context) (int, error) {
	if s == nil || s.pool == nil {
		return 0, fmt.Errorf("projects store not configured")
	}
	const selectSQL = `SELECT name, github_repo, payload FROM projects WHERE kind = 'project' AND config_schema_ref = ''`
	rows, err := s.pool.Query(ctx, selectSQL)
	if err != nil {
		return 0, fmt.Errorf("projects: backfill select: %w", err)
	}
	type pending struct {
		name      string
		schemaRef string
		payload   []byte
	}
	var todo []pending
	for rows.Next() {
		var name, githubRepo string
		var payloadBytes []byte
		if err := rows.Scan(&name, &githubRepo, &payloadBytes); err != nil {
			rows.Close()
			return 0, fmt.Errorf("projects: backfill scan: %w", err)
		}
		var doc map[string]any
		if len(payloadBytes) > 0 {
			if err := json.Unmarshal(payloadBytes, &doc); err != nil {
				rows.Close()
				return 0, fmt.Errorf("projects: backfill unmarshal %q: %w", name, err)
			}
		}
		authored := stripServerManagedStatus(mapFromMap(doc, "metadata"))
		canonicalRepo := stringFromMap(doc, "githubRepo", githubRepo)
		// Re-marshal so the stored version payload matches the live write
		// path (authored metadata only, status keys removed).
		versionPayload, err := json.Marshal(map[string]any{
			"name":       name,
			"githubRepo": canonicalRepo,
			"metadata":   authored,
		})
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("projects: backfill marshal %q: %w", name, err)
		}
		todo = append(todo, pending{
			name:      name,
			schemaRef: projectConfigSchemaRef(name, canonicalRepo, authored),
			payload:   versionPayload,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("projects: backfill iterate: %w", err)
	}
	rows.Close()

	seeded := 0
	for _, p := range todo {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return seeded, fmt.Errorf("projects: backfill begin %q: %w", p.name, err)
		}
		if _, err := tx.Exec(ctx, insertProjectConfigSchemaSQL, p.name, p.schemaRef, p.payload); err != nil {
			_ = tx.Rollback(ctx)
			return seeded, fmt.Errorf("projects: backfill mint %q: %w", p.name, err)
		}
		// Only move the pointer if it is still empty — avoid clobbering a
		// concurrent live write that already versioned this row.
		if _, err := tx.Exec(ctx,
			`UPDATE projects SET config_schema_ref = $2 WHERE kind = 'project' AND name = $1 AND config_schema_ref = ''`,
			p.name, p.schemaRef,
		); err != nil {
			_ = tx.Rollback(ctx)
			return seeded, fmt.Errorf("projects: backfill point %q: %w", p.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return seeded, fmt.Errorf("projects: backfill commit %q: %w", p.name, err)
		}
		seeded++
	}
	return seeded, nil
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
// global_ttl_seconds. nil clears by setting the value to 0.
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

func scanProjectFirstRow(rows pgx.Rows) (ProjectRecord, error) {
	if !rows.Next() {
		return ProjectRecord{}, fmt.Errorf("projects: returned no row")
	}
	rec, err := scanProjectRow(rows)
	if err != nil {
		return ProjectRecord{}, err
	}
	if err := rows.Err(); err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: rows err: %w", err)
	}
	return rec, nil
}

func scanProjectRow(rows pgx.Rows) (ProjectRecord, error) {
	var rec ProjectRecord
	var payloadBytes, statusBytes []byte
	if err := rows.Scan(&rec.Name, &rec.GitHubRepo, &payloadBytes, &statusBytes, &rec.ConfigSchemaRef, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return ProjectRecord{}, fmt.Errorf("projects: scan row: %w", err)
	}
	doc := map[string]any{}
	if len(payloadBytes) > 0 {
		if err := json.Unmarshal(payloadBytes, &doc); err != nil {
			return ProjectRecord{}, fmt.Errorf("projects: unmarshal payload: %w", err)
		}
	}
	status := map[string]any{}
	if len(statusBytes) > 0 {
		if err := json.Unmarshal(statusBytes, &status); err != nil {
			return ProjectRecord{}, fmt.Errorf("projects: unmarshal status: %w", err)
		}
	}
	rec.GitHubRepo = stringFromMap(doc, "githubRepo", rec.GitHubRepo)
	rec.ArgoCDApp = stringFromMap(doc, "argocdApp", "")
	authored := mapFromMap(doc, "metadata")
	rec.Status = status
	// Merge reconciler status back under metadata so the returned shape is
	// unchanged for every existing reader and the frontend.
	rec.Metadata = mergeStatusIntoMetadata(authored, status)
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

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

// WorkflowsStore is the Postgres-backed workflows + workflow_schemas
// store. Foundation only this stage (2f); Stage 2g cuts the public
// cosmos.Store workflow methods over to delegate to this.
//
// The cosmos `workflows` container held both kind='workflow' and
// kind='workflow_schema' docs differentiated by the embedded `kind`
// field. Postgres splits them into two real tables provisioned in
// Stage 2a's pg/migrations.go.
type WorkflowsStore struct {
	pool *pgxpool.Pool
}

// WorkflowRow is the per-project, per-name workflow row. Payload is the
// rest of the cosmos workflow doc (phases, pr, budget, metadata, etc.)
// stored as jsonb so this package doesn't reimplement every sub-type
// marshaler.
type WorkflowRow struct {
	Project   string
	Name      string
	SchemaRef string
	Payload   []byte // raw JSON of the cosmos workflowDoc
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WorkflowSchemaRow is the immutable workflow-schema row keyed by
// (project, schema_ref). Schemas accumulate over time; the same
// workflow re-registered with a different shape gets a new schema_ref.
type WorkflowSchemaRow struct {
	Project   string
	SchemaRef string
	Payload   []byte
	CreatedAt time.Time
}

var ErrWorkflowNotFound = errors.New("workflow not found")

func NewWorkflowsStore(pool *pgxpool.Pool) *WorkflowsStore {
	return &WorkflowsStore{pool: pool}
}

// List returns every workflow row across all projects.
func (s *WorkflowsStore) List(ctx context.Context) ([]WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `SELECT project, name, schema_ref, payload, created_at, updated_at FROM workflows ORDER BY project, name`
	rows, err := s.pool.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("workflows: list: %w", err)
	}
	defer rows.Close()
	return scanWorkflowRows(rows)
}

// ListByProject returns workflow rows scoped to one project.
func (s *WorkflowsStore) ListByProject(ctx context.Context, project string) ([]WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `SELECT project, name, schema_ref, payload, created_at, updated_at FROM workflows WHERE project = $1 ORDER BY name`
	rows, err := s.pool.Query(ctx, sql, project)
	if err != nil {
		return nil, fmt.Errorf("workflows: list by project: %w", err)
	}
	defer rows.Close()
	return scanWorkflowRows(rows)
}

// GetByName point-reads one workflow. Returns ErrWorkflowNotFound when
// no row exists.
func (s *WorkflowsStore) GetByName(ctx context.Context, project, name string) (WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return WorkflowRow{}, fmt.Errorf("workflows store not configured")
	}
	const sql = `SELECT project, name, schema_ref, payload, created_at, updated_at FROM workflows WHERE project = $1 AND name = $2`
	rows, err := s.pool.Query(ctx, sql, project, name)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: get by name: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return WorkflowRow{}, ErrWorkflowNotFound
	}
	return scanWorkflowRow(rows)
}

// GetSchemaByRef point-reads one workflow_schemas row.
func (s *WorkflowsStore) GetSchemaByRef(ctx context.Context, project, schemaRef string) (WorkflowSchemaRow, error) {
	if s == nil || s.pool == nil {
		return WorkflowSchemaRow{}, fmt.Errorf("workflows store not configured")
	}
	const sql = `SELECT project, schema_ref, payload, created_at FROM workflow_schemas WHERE project = $1 AND schema_ref = $2`
	var row WorkflowSchemaRow
	if err := s.pool.QueryRow(ctx, sql, project, schemaRef).Scan(&row.Project, &row.SchemaRef, &row.Payload, &row.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkflowSchemaRow{}, ErrWorkflowNotFound
		}
		return WorkflowSchemaRow{}, fmt.Errorf("workflows: get schema: %w", err)
	}
	return row, nil
}

// Upsert creates or updates a workflow row inside a transaction that
// also writes the corresponding workflow_schemas row idempotently.
// CreatedAt is preserved on update (the ON CONFLICT DO UPDATE doesn't
// touch created_at).
func (s *WorkflowsStore) Upsert(ctx context.Context, row WorkflowRow, schema WorkflowSchemaRow) (WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return WorkflowRow{}, fmt.Errorf("workflows store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: begin upsert: %w", err)
	}
	defer tx.Rollback(ctx)

	const schemaSQL = `
		INSERT INTO workflow_schemas (project, schema_ref, payload, created_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (project, schema_ref) DO NOTHING
	`
	if _, err := tx.Exec(ctx, schemaSQL, schema.Project, schema.SchemaRef, schema.Payload); err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: upsert schema: %w", err)
	}

	const upsertSQL = `
		INSERT INTO workflows (project, name, schema_ref, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (project, name) DO UPDATE
		  SET schema_ref = EXCLUDED.schema_ref,
		      payload    = EXCLUDED.payload,
		      updated_at = now()
		RETURNING project, name, schema_ref, payload, created_at, updated_at
	`
	rows, err := tx.Query(ctx, upsertSQL, row.Project, row.Name, row.SchemaRef, row.Payload)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: upsert workflow: %w", err)
	}
	out, scanErr := scanWorkflowFirstRow(rows)
	rows.Close()
	if scanErr != nil {
		return WorkflowRow{}, scanErr
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: commit upsert: %w", err)
	}
	return out, nil
}

// Delete removes a workflow row and returns its prior state. The
// workflow_schemas row is NOT removed because run history may
// reference it by schema_ref.
func (s *WorkflowsStore) Delete(ctx context.Context, project, name string) (WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return WorkflowRow{}, fmt.Errorf("workflows store not configured")
	}
	const sql = `
		DELETE FROM workflows WHERE project = $1 AND name = $2
		RETURNING project, name, schema_ref, payload, created_at, updated_at
	`
	rows, err := s.pool.Query(ctx, sql, project, name)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: delete: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return WorkflowRow{}, ErrWorkflowNotFound
	}
	return scanWorkflowRow(rows)
}

// PatchPayload mutates the workflow row's jsonb payload via mutate
// inside a SELECT FOR UPDATE transaction. The mutator may modify the
// map in place; the result is serialized back to jsonb.
func (s *WorkflowsStore) PatchPayload(ctx context.Context, project, name string, mutate func(payload map[string]any) error) (WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return WorkflowRow{}, fmt.Errorf("workflows store not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: begin patch: %w", err)
	}
	defer tx.Rollback(ctx)

	const selectSQL = `SELECT payload FROM workflows WHERE project = $1 AND name = $2 FOR UPDATE`
	var payloadBytes []byte
	if err := tx.QueryRow(ctx, selectSQL, project, name).Scan(&payloadBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkflowRow{}, ErrWorkflowNotFound
		}
		return WorkflowRow{}, fmt.Errorf("workflows: select for patch: %w", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: unmarshal payload: %w", err)
	}
	if err := mutate(payload); err != nil {
		return WorkflowRow{}, err
	}
	newPayload, err := json.Marshal(payload)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: marshal patched payload: %w", err)
	}

	const updateSQL = `
		UPDATE workflows SET payload = $3, updated_at = now()
		WHERE project = $1 AND name = $2
		RETURNING project, name, schema_ref, payload, created_at, updated_at
	`
	rows, err := tx.Query(ctx, updateSQL, project, name, newPayload)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: update patched: %w", err)
	}
	out, scanErr := scanWorkflowFirstRow(rows)
	rows.Close()
	if scanErr != nil {
		return WorkflowRow{}, scanErr
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: commit patch: %w", err)
	}
	return out, nil
}

func scanWorkflowRows(rows pgx.Rows) ([]WorkflowRow, error) {
	out := []WorkflowRow{}
	for rows.Next() {
		row, err := scanWorkflowRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workflows: iterate: %w", err)
	}
	return out, nil
}

func scanWorkflowRow(rows pgx.Rows) (WorkflowRow, error) {
	var row WorkflowRow
	if err := rows.Scan(&row.Project, &row.Name, &row.SchemaRef, &row.Payload, &row.CreatedAt, &row.UpdatedAt); err != nil {
		return WorkflowRow{}, fmt.Errorf("workflows: scan: %w", err)
	}
	return row, nil
}

func scanWorkflowFirstRow(rows pgx.Rows) (WorkflowRow, error) {
	if !rows.Next() {
		return WorkflowRow{}, fmt.Errorf("workflows: returned no row")
	}
	return scanWorkflowRow(rows)
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

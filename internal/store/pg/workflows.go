package pg

import (
	"context"
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

// WorkflowMigrationSource is the narrow interface cosmos.Store
// satisfies for the one-shot Migrate. Implemented by
// cosmos.Store.ListAllWorkflowDocsForMigration. Stage 2i deletes it.
type WorkflowMigrationSource interface {
	ListAllWorkflowDocsForMigration(ctx context.Context) ([]WorkflowRow, []WorkflowSchemaRow, error)
}

var ErrWorkflowNotFound = errors.New("workflow not found")

func NewWorkflowsStore(pool *pgxpool.Pool) *WorkflowsStore {
	return &WorkflowsStore{pool: pool}
}

// Migrate copies every cosmos workflow doc into the appropriate
// Postgres table. Idempotent — ON CONFLICT DO NOTHING preserves any
// pg-side writes that may have raced in (none in Stage 2f since the
// cosmos.Store public methods still own writes).
func (s *WorkflowsStore) Migrate(ctx context.Context, source WorkflowMigrationSource) (copied int, skipped int, err error) {
	if s == nil || s.pool == nil {
		return 0, 0, fmt.Errorf("workflows store not configured")
	}
	if source == nil {
		return 0, 0, nil
	}
	workflows, schemas, err := source.ListAllWorkflowDocsForMigration(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("workflows: migrate: read source: %w", err)
	}
	const insertWorkflowSQL = `
		INSERT INTO workflows (project, name, schema_ref, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, COALESCE($5, now()), COALESCE($6, $5, now()))
		ON CONFLICT (project, name) DO NOTHING
	`
	for _, row := range workflows {
		createdAt := nullableTime(row.CreatedAt)
		updatedAt := nullableTime(row.UpdatedAt)
		tag, execErr := s.pool.Exec(ctx, insertWorkflowSQL,
			row.Project, row.Name, row.SchemaRef, row.Payload, createdAt, updatedAt,
		)
		if execErr != nil {
			return copied, skipped, fmt.Errorf("workflows: migrate workflow %s/%s: %w", row.Project, row.Name, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	const insertSchemaSQL = `
		INSERT INTO workflow_schemas (project, schema_ref, payload, created_at)
		VALUES ($1, $2, $3, COALESCE($4, now()))
		ON CONFLICT (project, schema_ref) DO NOTHING
	`
	for _, row := range schemas {
		createdAt := nullableTime(row.CreatedAt)
		tag, execErr := s.pool.Exec(ctx, insertSchemaSQL,
			row.Project, row.SchemaRef, row.Payload, createdAt,
		)
		if execErr != nil {
			return copied, skipped, fmt.Errorf("workflows: migrate schema %s/%s: %w", row.Project, row.SchemaRef, execErr)
		}
		if tag.RowsAffected() == 1 {
			copied++
		} else {
			skipped++
		}
	}
	return copied, skipped, nil
}

// ListAllForFoundation returns every migrated workflow row. Used by
// pg_query during the 2f→2g window for verification; no cosmos.Store
// consumer reads this yet.
func (s *WorkflowsStore) ListAllForFoundation(ctx context.Context) ([]WorkflowRow, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	const sql = `SELECT project, name, schema_ref, payload, created_at, updated_at FROM workflows ORDER BY project, name`
	rows, err := s.pool.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("workflows: list all: %w", err)
	}
	defer rows.Close()
	out := []WorkflowRow{}
	for rows.Next() {
		var row WorkflowRow
		if err := rows.Scan(&row.Project, &row.Name, &row.SchemaRef, &row.Payload, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("workflows: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workflows: iterate: %w", err)
	}
	return out, nil
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// silence linter: ErrWorkflowNotFound is exported for Stage 2g's
// cutover delegations to reference; not used inside the foundation
// stage.
var _ = pgx.ErrNoRows

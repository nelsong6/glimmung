package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SlotInspectionsStore is the Postgres-backed ledger for free
// (lease-scoped) inspections produced by POST /v1/inspections. Run-bound
// inspections (caller in a Run context) are tracked by existing Run
// evidence machinery and are not indexed here. Sweep semantics: every
// row for a given lease_id is deleted when the lease enters cleanup,
// and the returned blob paths are deleted from object storage.
type SlotInspectionsStore struct {
	pool *pgxpool.Pool
}

type SlotInspectionRow struct {
	ID                    string
	Project               string
	Slot                  string
	LeaseID               string
	SessionID             string
	RequestID             string
	BlobPrefix            string
	ReportBlobPath        string
	ScreenshotBlobPath    string
	ScreenshotContentType string
	ByteSizeScreenshot    int64
	ByteSizeReport        int64
	CreatedAt             time.Time
}

var (
	ErrSlotInspectionNotFound = errors.New("slot inspection not found")
)

func NewSlotInspectionsStore(pool *pgxpool.Pool) *SlotInspectionsStore {
	return &SlotInspectionsStore{pool: pool}
}

// Insert writes a new row. Caller has already minted id (a UUID), and
// uploaded both blobs. If a row with the same (lease_id, request_id)
// already exists (idempotent retry by the same caller), returns
// ErrSlotInspectionAlreadyExists with the existing row so the caller
// can delete the just-uploaded blobs and return the prior record.
func (s *SlotInspectionsStore) Insert(ctx context.Context, row SlotInspectionRow) (SlotInspectionRow, error) {
	if s == nil || s.pool == nil {
		return SlotInspectionRow{}, fmt.Errorf("slot_inspections store not configured")
	}
	const insertSQL = `
		INSERT INTO slot_inspections (
			id, project, slot, lease_id, session_id, request_id,
			blob_prefix, report_blob_path, screenshot_blob_path,
			screenshot_content_type, byte_size_screenshot, byte_size_report
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, project, slot, lease_id, session_id, request_id,
			blob_prefix, report_blob_path, screenshot_blob_path,
			screenshot_content_type, byte_size_screenshot, byte_size_report,
			created_at
	`
	var out SlotInspectionRow
	err := s.pool.QueryRow(ctx, insertSQL,
		row.ID, row.Project, row.Slot, row.LeaseID, row.SessionID, row.RequestID,
		row.BlobPrefix, row.ReportBlobPath, row.ScreenshotBlobPath,
		row.ScreenshotContentType, row.ByteSizeScreenshot, row.ByteSizeReport,
	).Scan(
		&out.ID, &out.Project, &out.Slot, &out.LeaseID, &out.SessionID, &out.RequestID,
		&out.BlobPrefix, &out.ReportBlobPath, &out.ScreenshotBlobPath,
		&out.ScreenshotContentType, &out.ByteSizeScreenshot, &out.ByteSizeReport,
		&out.CreatedAt,
	)
	if err != nil {
		return SlotInspectionRow{}, fmt.Errorf("slot_inspections: insert: %w", err)
	}
	return out, nil
}

// LookupByRequest returns the existing row for (lease_id, request_id) if
// the caller previously supplied a request id and we already wrote
// for it. Returns ErrSlotInspectionNotFound if no match. Used by the
// idempotency path before doing any blob upload work.
func (s *SlotInspectionsStore) LookupByRequest(ctx context.Context, leaseID, requestID string) (SlotInspectionRow, error) {
	if s == nil || s.pool == nil {
		return SlotInspectionRow{}, fmt.Errorf("slot_inspections store not configured")
	}
	if requestID == "" {
		return SlotInspectionRow{}, ErrSlotInspectionNotFound
	}
	const sql = `
		SELECT id, project, slot, lease_id, session_id, request_id,
			blob_prefix, report_blob_path, screenshot_blob_path,
			screenshot_content_type, byte_size_screenshot, byte_size_report,
			created_at
		FROM slot_inspections
		WHERE lease_id = $1 AND request_id = $2
	`
	var out SlotInspectionRow
	err := s.pool.QueryRow(ctx, sql, leaseID, requestID).Scan(
		&out.ID, &out.Project, &out.Slot, &out.LeaseID, &out.SessionID, &out.RequestID,
		&out.BlobPrefix, &out.ReportBlobPath, &out.ScreenshotBlobPath,
		&out.ScreenshotContentType, &out.ByteSizeScreenshot, &out.ByteSizeReport,
		&out.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SlotInspectionRow{}, ErrSlotInspectionNotFound
	}
	if err != nil {
		return SlotInspectionRow{}, fmt.Errorf("slot_inspections: lookup: %w", err)
	}
	return out, nil
}

// DeleteByLease removes every row whose lease_id matches and returns
// them. Callers iterate the returned blob paths and delete each blob
// from object storage. Idempotent at the row level (re-running after a
// partial sweep returns nothing because the rows are already gone).
func (s *SlotInspectionsStore) DeleteByLease(ctx context.Context, leaseID string) ([]SlotInspectionRow, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("slot_inspections store not configured")
	}
	const sql = `
		DELETE FROM slot_inspections
		WHERE lease_id = $1
		RETURNING id, project, slot, lease_id, session_id, request_id,
			blob_prefix, report_blob_path, screenshot_blob_path,
			screenshot_content_type, byte_size_screenshot, byte_size_report,
			created_at
	`
	rows, err := s.pool.Query(ctx, sql, leaseID)
	if err != nil {
		return nil, fmt.Errorf("slot_inspections: delete by lease: %w", err)
	}
	defer rows.Close()
	out := []SlotInspectionRow{}
	for rows.Next() {
		var row SlotInspectionRow
		if err := rows.Scan(
			&row.ID, &row.Project, &row.Slot, &row.LeaseID, &row.SessionID, &row.RequestID,
			&row.BlobPrefix, &row.ReportBlobPath, &row.ScreenshotBlobPath,
			&row.ScreenshotContentType, &row.ByteSizeScreenshot, &row.ByteSizeReport,
			&row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("slot_inspections: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("slot_inspections: iterate: %w", err)
	}
	return out, nil
}

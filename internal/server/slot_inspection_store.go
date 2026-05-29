package server

import (
	"context"
	"errors"
	"time"
)

// SlotInspectionRecord is the server-domain view of a row in the
// slot_inspections ledger. Both lease-scoped and run-scoped inspections
// are indexed here; the `Scope` + `RunID` fields distinguish them at
// query time. Run-scoped rows survive lease-cleanup sweeps; their
// retention follows Run evidence semantics.
type SlotInspectionRecord struct {
	ID                    string
	Project               string
	Slot                  string
	LeaseID               string
	SessionID             string
	RequestID             string
	Scope                 string
	RunID                 string
	BlobPrefix            string
	ReportBlobPath        string
	ScreenshotBlobPath    string
	ScreenshotContentType string
	ByteSizeScreenshot    int64
	ByteSizeReport        int64
	CreatedAt             time.Time
}

// ErrSlotInspectionNotFound is the sentinel returned by
// LookupSlotInspectionByRequest when no record exists for the supplied
// (lease_id, request_id) pair.
var ErrSlotInspectionNotFound = errors.New("slot inspection not found")

// SlotInspectionStore is the interface server-side handlers use to
// reach the slot_inspections ledger.
type SlotInspectionStore interface {
	InsertSlotInspection(ctx context.Context, row SlotInspectionRecord) (SlotInspectionRecord, error)
	LookupSlotInspectionByRequest(ctx context.Context, leaseID, requestID string) (SlotInspectionRecord, error)
	DeleteSlotInspectionsByLease(ctx context.Context, leaseID string) ([]SlotInspectionRecord, error)
	GetSlotInspectionByID(ctx context.Context, id string) (SlotInspectionRecord, error)
	ListSlotInspections(ctx context.Context, filter SlotInspectionFilter) ([]SlotInspectionRecord, error)
}

// SlotInspectionFilter narrows a list query. Empty filter returns
// every row up to the limit, newest first.
type SlotInspectionFilter struct {
	Project string
	LeaseID string
	RunID   string
	Scope   string
	Limit   int
}

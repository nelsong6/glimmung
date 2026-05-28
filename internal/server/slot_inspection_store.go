package server

import (
	"context"
	"errors"
	"time"
)

// SlotInspectionRecord is the server-domain view of a row in the
// slot_inspections ledger. Free (lease-scoped) inspections only;
// run-bound inspections flow through the existing Run evidence
// machinery and are not indexed here.
type SlotInspectionRecord struct {
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
}

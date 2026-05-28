package server

import (
	"context"
	"testing"
	"time"
)

// inspectionSweepFakeStore is an inline store that satisfies just enough
// of ReadStore and SlotInspectionStore for the sweep tests. The wider
// reader interfaces are not needed because sweepLeaseInspections only
// type-asserts to SlotInspectionStore.
type inspectionSweepFakeStore struct {
	rows []SlotInspectionRecord
}

func (s *inspectionSweepFakeStore) ListProjects(context.Context) ([]Project, error)   { return nil, nil }
func (s *inspectionSweepFakeStore) ListWorkflows(context.Context) ([]Workflow, error) { return nil, nil }

func (s *inspectionSweepFakeStore) InsertSlotInspection(_ context.Context, row SlotInspectionRecord) (SlotInspectionRecord, error) {
	row.CreatedAt = time.Now()
	s.rows = append(s.rows, row)
	return row, nil
}

func (s *inspectionSweepFakeStore) LookupSlotInspectionByRequest(_ context.Context, leaseID, requestID string) (SlotInspectionRecord, error) {
	for _, row := range s.rows {
		if row.LeaseID == leaseID && row.RequestID == requestID && requestID != "" {
			return row, nil
		}
	}
	return SlotInspectionRecord{}, ErrSlotInspectionNotFound
}

func (s *inspectionSweepFakeStore) DeleteSlotInspectionsByLease(_ context.Context, leaseID string) ([]SlotInspectionRecord, error) {
	out := []SlotInspectionRecord{}
	remaining := s.rows[:0]
	for _, row := range s.rows {
		if row.LeaseID == leaseID {
			out = append(out, row)
			continue
		}
		remaining = append(remaining, row)
	}
	s.rows = remaining
	return out, nil
}

func TestSweepLeaseInspectionsDeletesRowsAndBlobs(t *testing.T) {
	prevWriter := inspectionSweepArtifactWriter()
	t.Cleanup(func() { SetInspectionSweepArtifactWriter(prevWriter) })

	store := &inspectionSweepFakeStore{}
	store.rows = []SlotInspectionRecord{
		{ID: "i-1", LeaseID: "L-1", BlobPrefix: "inspections/L-1/i-1", ReportBlobPath: "inspections/L-1/i-1/report.json", ScreenshotBlobPath: "inspections/L-1/i-1/screenshot.png"},
		{ID: "i-2", LeaseID: "L-1", BlobPrefix: "inspections/L-1/i-2", ReportBlobPath: "inspections/L-1/i-2/report.json", ScreenshotBlobPath: "inspections/L-1/i-2/screenshot.png"},
		{ID: "i-3", LeaseID: "L-other", BlobPrefix: "inspections/L-other/i-3", ReportBlobPath: "inspections/L-other/i-3/report.json", ScreenshotBlobPath: "inspections/L-other/i-3/screenshot.png"},
	}
	writer := newInspectionFakeWriter()
	writer.uploads["inspections/L-1/i-1/report.json"] = []byte("r1")
	writer.uploads["inspections/L-1/i-1/screenshot.png"] = []byte("s1")
	writer.uploads["inspections/L-1/i-2/report.json"] = []byte("r2")
	writer.uploads["inspections/L-1/i-2/screenshot.png"] = []byte("s2")
	writer.uploads["inspections/L-other/i-3/report.json"] = []byte("r3")
	writer.uploads["inspections/L-other/i-3/screenshot.png"] = []byte("s3")
	SetInspectionSweepArtifactWriter(writer)

	logs := []string{}
	sweepLeaseInspections(context.Background(), store, Lease{ID: "L-1"}, func(format string, args ...any) {
		logs = append(logs, format)
	})

	if len(store.rows) != 1 || store.rows[0].LeaseID != "L-other" {
		t.Fatalf("L-1 rows not pruned: %+v", store.rows)
	}
	if _, ok := writer.uploads["inspections/L-1/i-1/report.json"]; ok {
		t.Fatalf("L-1 i-1 report not deleted")
	}
	if _, ok := writer.uploads["inspections/L-1/i-1/screenshot.png"]; ok {
		t.Fatalf("L-1 i-1 screenshot not deleted")
	}
	if _, ok := writer.uploads["inspections/L-1/i-2/screenshot.png"]; ok {
		t.Fatalf("L-1 i-2 screenshot not deleted")
	}
	if _, ok := writer.uploads["inspections/L-other/i-3/report.json"]; !ok {
		t.Fatalf("other-lease report wrongly deleted")
	}
	if got, want := len(writer.deletes), 4; got != want {
		t.Fatalf("delete count=%d want=%d (%v)", got, want, writer.deletes)
	}
}

func TestSweepLeaseInspectionsHandlesNoRows(t *testing.T) {
	prevWriter := inspectionSweepArtifactWriter()
	t.Cleanup(func() { SetInspectionSweepArtifactWriter(prevWriter) })

	store := &inspectionSweepFakeStore{}
	writer := newInspectionFakeWriter()
	SetInspectionSweepArtifactWriter(writer)

	sweepLeaseInspections(context.Background(), store, Lease{ID: "L-1"}, nil)
	if len(writer.deletes) != 0 {
		t.Fatalf("expected no deletes, got %v", writer.deletes)
	}
}

func TestSweepLeaseInspectionsNoStoreNoCrash(t *testing.T) {
	prevWriter := inspectionSweepArtifactWriter()
	t.Cleanup(func() { SetInspectionSweepArtifactWriter(prevWriter) })

	// store is nil — sweep should bail without panicking.
	sweepLeaseInspections(context.Background(), nil, Lease{ID: "L-1"}, nil)
}

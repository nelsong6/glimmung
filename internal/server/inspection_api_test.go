package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// inspectionFakeStore implements SlotInspectionStore for the
// inspection handler tests. Insert/Lookup are ledger-only; nothing
// here touches blob storage.
type inspectionFakeStore struct {
	mu             sync.Mutex
	rows           []SlotInspectionRecord
	insertErr      error
	insertCount    int
	insertRecorded SlotInspectionRecord
}

func (s *inspectionFakeStore) InsertSlotInspection(_ context.Context, row SlotInspectionRecord) (SlotInspectionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertCount++
	s.insertRecorded = row
	if s.insertErr != nil {
		return SlotInspectionRecord{}, s.insertErr
	}
	row.CreatedAt, _ = time.Parse(time.RFC3339, "2026-05-28T00:00:00Z")
	s.rows = append(s.rows, row)
	return row, nil
}

func (s *inspectionFakeStore) LookupSlotInspectionByRequest(_ context.Context, leaseID, requestID string) (SlotInspectionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.rows {
		if row.LeaseID == leaseID && row.RequestID == requestID && requestID != "" {
			return row, nil
		}
	}
	return SlotInspectionRecord{}, ErrSlotInspectionNotFound
}

func (s *inspectionFakeStore) DeleteSlotInspectionsByLease(_ context.Context, leaseID string) ([]SlotInspectionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *inspectionFakeStore) GetSlotInspectionByID(_ context.Context, id string) (SlotInspectionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.rows {
		if row.ID == id {
			return row, nil
		}
	}
	return SlotInspectionRecord{}, ErrSlotInspectionNotFound
}

func (s *inspectionFakeStore) ListSlotInspections(_ context.Context, filter SlotInspectionFilter) ([]SlotInspectionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []SlotInspectionRecord{}
	for _, row := range s.rows {
		if filter.Project != "" && row.Project != filter.Project {
			continue
		}
		if filter.LeaseID != "" && row.LeaseID != filter.LeaseID {
			continue
		}
		if filter.RunID != "" && row.RunID != filter.RunID {
			continue
		}
		if filter.Scope != "" && row.Scope != filter.Scope {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// inspectionFakeRunResolver lets tests stub the run-id validation
// hook without spinning up a real runs store.
type inspectionFakeRunResolver struct {
	knownRuns map[string]string // run_id -> project
	err       error
}

func (r *inspectionFakeRunResolver) ResolveInspectionRunProject(_ context.Context, _, runID string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	project, ok := r.knownRuns[runID]
	if !ok {
		return "", ErrNotFound
	}
	return project, nil
}

// inspectionFakeWriter implements ArtifactWriter for the handler
// tests; in-memory only.
type inspectionFakeWriter struct {
	mu              sync.Mutex
	uploads         map[string][]byte
	contentTypes    map[string]string
	uploadErrOnPath map[string]error
	deletes         []string
}

func newInspectionFakeWriter() *inspectionFakeWriter {
	return &inspectionFakeWriter{
		uploads:         map[string][]byte{},
		contentTypes:    map[string]string{},
		uploadErrOnPath: map[string]error{},
	}
}

func (w *inspectionFakeWriter) Upload(_ context.Context, blobName string, body []byte, contentType string) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err, ok := w.uploadErrOnPath[blobName]; ok {
		return 0, err
	}
	w.uploads[blobName] = append([]byte(nil), body...)
	w.contentTypes[blobName] = contentType
	return int64(len(body)), nil
}

func (w *inspectionFakeWriter) Delete(_ context.Context, blobName string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deletes = append(w.deletes, blobName)
	delete(w.uploads, blobName)
	delete(w.contentTypes, blobName)
	return nil
}

type inspectionFakeLeaseResolver struct {
	lease Lease
	err   error
}

func (r *inspectionFakeLeaseResolver) ResolveTestSlotLeaseByTankSession(_ context.Context, _, _ string) (Lease, error) {
	if r.err != nil {
		return Lease{}, r.err
	}
	return r.lease, nil
}

func buildInspectionRequest(t *testing.T, tankSessionID, project string, reportJSON, screenshot []byte, screenshotType string, headers map[string]string) *http.Request {
	return buildInspectionRequestWithRun(t, tankSessionID, project, "", reportJSON, screenshot, screenshotType, headers)
}

func buildInspectionRequestWithRun(t *testing.T, tankSessionID, project, runID string, reportJSON, screenshot []byte, screenshotType string, headers map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if tankSessionID != "" {
		if err := writer.WriteField("tank_session_id", tankSessionID); err != nil {
			t.Fatalf("write tank_session_id: %v", err)
		}
	}
	if project != "" {
		if err := writer.WriteField("project", project); err != nil {
			t.Fatalf("write project: %v", err)
		}
	}
	if runID != "" {
		if err := writer.WriteField("run_id", runID); err != nil {
			t.Fatalf("write run_id: %v", err)
		}
	}
	if reportJSON != nil {
		part, err := writer.CreatePart(textHeaders("report", "application/json"))
		if err != nil {
			t.Fatalf("create report part: %v", err)
		}
		if _, err := part.Write(reportJSON); err != nil {
			t.Fatalf("write report bytes: %v", err)
		}
	}
	if screenshot != nil {
		part, err := writer.CreatePart(textHeaders("screenshot", screenshotType))
		if err != nil {
			t.Fatalf("create screenshot part: %v", err)
		}
		if _, err := part.Write(screenshot); err != nil {
			t.Fatalf("write screenshot bytes: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inspections", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func textHeaders(name, contentType string) map[string][]string {
	return map[string][]string{
		"Content-Disposition": {fmt.Sprintf(`form-data; name=%q`, name)},
		"Content-Type":        {contentType},
	}
}

func TestCreateInspectionWritesBlobsAndLedger(t *testing.T) {
	store := &inspectionFakeStore{}
	writer := newInspectionFakeWriter()
	resolver := &inspectionFakeLeaseResolver{lease: Lease{ID: "lease-1", Project: "p1", Metadata: map[string]any{"native_slot_name": "p1-slot-1"}}}

	handler := createInspection(createInspectionDeps{store: store, leases: resolver, artifactWrite: writer})
	rec := httptest.NewRecorder()
	req := buildInspectionRequest(t, "sess-1", "p1", []byte(`{"final_url":"https://example.test/"}`), []byte("PNG-BYTES"), "image/png", map[string]string{
		inspectionRequestIDHeader: "req-A",
	})
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp inspectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Scope != "lease" || resp.ScopeRef != "lease-1" {
		t.Fatalf("scope=%q ref=%q", resp.Scope, resp.ScopeRef)
	}
	wantReport := "/v1/artifacts/inspections/lease-1/" + resp.InspectionID + "/report.json"
	if resp.ReportURL != wantReport {
		t.Fatalf("report url=%q want=%q", resp.ReportURL, wantReport)
	}
	wantScreenshot := "/v1/artifacts/inspections/lease-1/" + resp.InspectionID + "/screenshot.png"
	if resp.ScreenshotURL != wantScreenshot {
		t.Fatalf("screenshot url=%q want=%q", resp.ScreenshotURL, wantScreenshot)
	}
	if store.insertCount != 1 {
		t.Fatalf("insertCount=%d", store.insertCount)
	}
	if store.insertRecorded.RequestID != "req-A" {
		t.Fatalf("request id not persisted: %q", store.insertRecorded.RequestID)
	}
	if store.insertRecorded.Slot != "p1-slot-1" {
		t.Fatalf("slot=%q", store.insertRecorded.Slot)
	}
	reportBlob := "inspections/lease-1/" + resp.InspectionID + "/report.json"
	if got := writer.uploads[reportBlob]; string(got) == "" {
		t.Fatalf("report blob missing: %v", writer.uploads)
	}
	screenshotBlob := "inspections/lease-1/" + resp.InspectionID + "/screenshot.png"
	if got := writer.uploads[screenshotBlob]; string(got) != "PNG-BYTES" {
		t.Fatalf("screenshot bytes=%q", got)
	}
	if ct := writer.contentTypes[screenshotBlob]; ct != "image/png" {
		t.Fatalf("screenshot content-type=%q", ct)
	}
}

func TestCreateInspectionIdempotentOnSameRequestID(t *testing.T) {
	store := &inspectionFakeStore{}
	writer := newInspectionFakeWriter()
	resolver := &inspectionFakeLeaseResolver{lease: Lease{ID: "lease-1"}}
	handler := createInspection(createInspectionDeps{store: store, leases: resolver, artifactWrite: writer})

	req1 := buildInspectionRequest(t, "sess", "p1", []byte(`{"a":1}`), []byte("png-1"), "image/png", map[string]string{inspectionRequestIDHeader: "same"})
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	var first inspectionResponse
	_ = json.Unmarshal(rec1.Body.Bytes(), &first)

	req2 := buildInspectionRequest(t, "sess", "p1", []byte(`{"a":2}`), []byte("png-2"), "image/png", map[string]string{inspectionRequestIDHeader: "same"})
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var second inspectionResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &second)
	if first.InspectionID != second.InspectionID {
		t.Fatalf("idempotent retry returned different inspection: %s vs %s", first.InspectionID, second.InspectionID)
	}
	if store.insertCount != 1 {
		t.Fatalf("second request triggered a second insert: %d", store.insertCount)
	}
}

func TestCreateInspectionRollsBackOnLedgerFailure(t *testing.T) {
	store := &inspectionFakeStore{insertErr: errors.New("boom")}
	writer := newInspectionFakeWriter()
	resolver := &inspectionFakeLeaseResolver{lease: Lease{ID: "lease-1"}}
	handler := createInspection(createInspectionDeps{store: store, leases: resolver, artifactWrite: writer})

	req := buildInspectionRequest(t, "sess", "p1", []byte(`{}`), []byte("png"), "image/png", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(writer.deletes) != 2 {
		t.Fatalf("expected 2 rollback deletes, got %v", writer.deletes)
	}
	if len(writer.uploads) != 0 {
		t.Fatalf("expected uploads to be rolled back, still have %v", writer.uploads)
	}
}

func TestCreateInspectionRejectsMissingParts(t *testing.T) {
	store := &inspectionFakeStore{}
	writer := newInspectionFakeWriter()
	resolver := &inspectionFakeLeaseResolver{lease: Lease{ID: "lease-1"}}
	handler := createInspection(createInspectionDeps{store: store, leases: resolver, artifactWrite: writer})

	for _, tc := range []struct {
		name           string
		tankSessionID  string
		project        string
		report         []byte
		screenshot     []byte
		screenshotType string
		wantStatus     int
	}{
		{name: "no_session", project: "p", report: []byte("{}"), screenshot: []byte("x"), screenshotType: "image/png", wantStatus: http.StatusBadRequest},
		{name: "no_project", tankSessionID: "s", report: []byte("{}"), screenshot: []byte("x"), screenshotType: "image/png", wantStatus: http.StatusBadRequest},
		{name: "no_report", tankSessionID: "s", project: "p", screenshot: []byte("x"), screenshotType: "image/png", wantStatus: http.StatusBadRequest},
		{name: "no_screenshot", tankSessionID: "s", project: "p", report: []byte("{}"), wantStatus: http.StatusBadRequest},
		{name: "non_image_screenshot", tankSessionID: "s", project: "p", report: []byte("{}"), screenshot: []byte("xx"), screenshotType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := buildInspectionRequest(t, tc.tankSessionID, tc.project, tc.report, tc.screenshot, tc.screenshotType, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreateInspectionRejectsInvalidJSONReport(t *testing.T) {
	store := &inspectionFakeStore{}
	writer := newInspectionFakeWriter()
	resolver := &inspectionFakeLeaseResolver{lease: Lease{ID: "lease-1"}}
	handler := createInspection(createInspectionDeps{store: store, leases: resolver, artifactWrite: writer})

	req := buildInspectionRequest(t, "s", "p", []byte("not-json"), []byte("png"), "image/png", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not valid JSON") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCreateInspectionNotFoundWhenLeaseMissing(t *testing.T) {
	store := &inspectionFakeStore{}
	writer := newInspectionFakeWriter()
	resolver := &inspectionFakeLeaseResolver{err: ErrNotFound}
	handler := createInspection(createInspectionDeps{store: store, leases: resolver, artifactWrite: writer})

	req := buildInspectionRequest(t, "s", "p", []byte(`{}`), []byte("png"), "image/png", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(writer.uploads) != 0 {
		t.Fatalf("uploads happened despite missing lease: %v", writer.uploads)
	}
}

// inspectionFakeStateStore satisfies StateStore so we can drive the
// resolver in tests without spinning up the real store.
type inspectionFakeStateStore struct {
	leases []Lease
}

func (s *inspectionFakeStateStore) ListProjects(context.Context) ([]Project, error) { return nil, nil }
func (s *inspectionFakeStateStore) ListWorkflows(context.Context) ([]Workflow, error) {
	return nil, nil
}
func (s *inspectionFakeStateStore) AnyLockHeld(context.Context, string) (bool, error) {
	return false, nil
}
func (s *inspectionFakeStateStore) ListLeases(context.Context) ([]Lease, error) {
	out := make([]Lease, len(s.leases))
	copy(out, s.leases)
	return out, nil
}

func TestCreateInspectionRunScopedWritesUnderRunPrefix(t *testing.T) {
	store := &inspectionFakeStore{}
	writer := newInspectionFakeWriter()
	resolver := &inspectionFakeLeaseResolver{lease: Lease{ID: "lease-1", Project: "p1", Metadata: map[string]any{"native_slot_name": "p1-slot-1"}}}
	runs := &inspectionFakeRunResolver{knownRuns: map[string]string{"01R": "p1"}}

	handler := createInspection(createInspectionDeps{store: store, leases: resolver, runs: runs, artifactWrite: writer})
	rec := httptest.NewRecorder()
	req := buildInspectionRequestWithRun(t, "sess-1", "p1", "01R", []byte(`{"final_url":"https://example.test/"}`), []byte("PNG-BYTES"), "image/png", map[string]string{
		inspectionRequestIDHeader: "req-run",
	})
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp inspectionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Scope != "run" {
		t.Fatalf("scope=%q want run", resp.Scope)
	}
	if resp.ScopeRef != "01R" {
		t.Fatalf("scope_ref=%q want 01R", resp.ScopeRef)
	}
	wantReport := "/v1/artifacts/runs/p1/01R/inspections/" + resp.InspectionID + "/report.json"
	if resp.ReportURL != wantReport {
		t.Fatalf("report url=%q want=%q", resp.ReportURL, wantReport)
	}
	wantScreenshot := "/v1/artifacts/runs/p1/01R/inspections/" + resp.InspectionID + "/screenshot.png"
	if resp.ScreenshotURL != wantScreenshot {
		t.Fatalf("screenshot url=%q want=%q", resp.ScreenshotURL, wantScreenshot)
	}
	// Ledger persisted with run-scoped fields.
	if store.insertRecorded.Scope != "run" || store.insertRecorded.RunID != "01R" {
		t.Fatalf("ledger row: scope=%q run=%q", store.insertRecorded.Scope, store.insertRecorded.RunID)
	}
}

func TestCreateInspectionRunScopedRejectsMissingRun(t *testing.T) {
	store := &inspectionFakeStore{}
	writer := newInspectionFakeWriter()
	resolver := &inspectionFakeLeaseResolver{lease: Lease{ID: "lease-1", Project: "p1"}}
	runs := &inspectionFakeRunResolver{knownRuns: map[string]string{}}
	handler := createInspection(createInspectionDeps{store: store, leases: resolver, runs: runs, artifactWrite: writer})

	req := buildInspectionRequestWithRun(t, "sess-1", "p1", "ghost", []byte("{}"), []byte("png"), "image/png", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(writer.uploads) != 0 {
		t.Fatalf("uploads happened despite unknown run: %v", writer.uploads)
	}
}

func TestCreateInspectionRunScopedRejectsCrossProject(t *testing.T) {
	store := &inspectionFakeStore{}
	writer := newInspectionFakeWriter()
	resolver := &inspectionFakeLeaseResolver{lease: Lease{ID: "lease-1", Project: "p1"}}
	// Run is registered under a different project.
	runs := &inspectionFakeRunResolver{knownRuns: map[string]string{"01R": "p2"}}
	handler := createInspection(createInspectionDeps{store: store, leases: resolver, runs: runs, artifactWrite: writer})

	req := buildInspectionRequestWithRun(t, "sess-1", "p1", "01R", []byte("{}"), []byte("png"), "image/png", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(writer.uploads) != 0 {
		t.Fatalf("uploads happened despite cross-project run: %v", writer.uploads)
	}
}

func TestGetInspectionByIDReturnsDetail(t *testing.T) {
	store := &inspectionFakeStore{rows: []SlotInspectionRecord{
		{
			ID:                    "i-1",
			Project:               "p1",
			Slot:                  "p1-slot-1",
			LeaseID:               "L-1",
			SessionID:             "sess-1",
			RequestID:             "req-A",
			BlobPrefix:            "inspections/L-1/i-1",
			ReportBlobPath:        "inspections/L-1/i-1/report.json",
			ScreenshotBlobPath:    "inspections/L-1/i-1/screenshot.png",
			ScreenshotContentType: "image/png",
			ByteSizeScreenshot:    1234,
			ByteSizeReport:        567,
		},
	}}

	handler := getInspectionByID(store)
	req := httptest.NewRequest(http.MethodGet, "/v1/inspections/i-1", nil)
	req.SetPathValue("inspection_id", "i-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp inspectionDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.InspectionID != "i-1" || resp.LeaseID != "L-1" {
		t.Fatalf("got %+v", resp)
	}
	if resp.ReportURL != "/v1/artifacts/inspections/L-1/i-1/report.json" {
		t.Fatalf("report url=%q", resp.ReportURL)
	}
	if resp.ScreenshotURL != "/v1/artifacts/inspections/L-1/i-1/screenshot.png" {
		t.Fatalf("screenshot url=%q", resp.ScreenshotURL)
	}
	if resp.Scope != "lease" {
		t.Fatalf("scope=%q", resp.Scope)
	}
	if resp.ByteSizeScreenshot != 1234 || resp.ByteSizeReport != 567 {
		t.Fatalf("byte sizes wrong: %+v", resp)
	}
}

func TestGetInspectionByIDMissing(t *testing.T) {
	store := &inspectionFakeStore{}
	handler := getInspectionByID(store)
	req := httptest.NewRequest(http.MethodGet, "/v1/inspections/nope", nil)
	req.SetPathValue("inspection_id", "nope")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetInspectionByIDRequiresID(t *testing.T) {
	store := &inspectionFakeStore{}
	handler := getInspectionByID(store)
	req := httptest.NewRequest(http.MethodGet, "/v1/inspections/", nil)
	// No PathValue set — empty inspection_id.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListInspectionsFiltersByLease(t *testing.T) {
	store := &inspectionFakeStore{rows: []SlotInspectionRecord{
		{ID: "i-1", Project: "p1", LeaseID: "L-1", BlobPrefix: "inspections/L-1/i-1", ReportBlobPath: "inspections/L-1/i-1/report.json", ScreenshotBlobPath: "inspections/L-1/i-1/screenshot.png"},
		{ID: "i-2", Project: "p1", LeaseID: "L-2", BlobPrefix: "inspections/L-2/i-2", ReportBlobPath: "inspections/L-2/i-2/report.json", ScreenshotBlobPath: "inspections/L-2/i-2/screenshot.png"},
		{ID: "i-3", Project: "p2", LeaseID: "L-3", BlobPrefix: "inspections/L-3/i-3", ReportBlobPath: "inspections/L-3/i-3/report.json", ScreenshotBlobPath: "inspections/L-3/i-3/screenshot.png"},
	}}
	handler := listInspections(store)

	t.Run("no_filter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/inspections", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp inspectionListResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if len(resp.Inspections) != 3 {
			t.Fatalf("got %d inspections", len(resp.Inspections))
		}
	})

	t.Run("filter_by_lease", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/inspections?lease=L-2", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var resp inspectionListResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if len(resp.Inspections) != 1 || resp.Inspections[0].LeaseID != "L-2" {
			t.Fatalf("got %+v", resp.Inspections)
		}
	})

	t.Run("filter_by_project", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/inspections?project=p2", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var resp inspectionListResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if len(resp.Inspections) != 1 || resp.Inspections[0].Project != "p2" {
			t.Fatalf("got %+v", resp.Inspections)
		}
	})

	t.Run("rejects_invalid_limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/inspections?limit=abc", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

func TestInspectionLeaseResolverMatchesTankSession(t *testing.T) {
	store := &inspectionFakeStateStore{leases: []Lease{
		{ID: "L1", Project: "p1", State: "claimed", Metadata: map[string]any{
			"test_slot_checkout": true,
			"tank_session_id":    "sess-A",
		}},
		{ID: "L2", Project: "p1", State: "claimed", Metadata: map[string]any{
			"test_slot_checkout": true,
			"tank_session_id":    "sess-B",
		}},
	}}
	resolver := newInspectionLeaseResolver(store)
	got, err := resolver.ResolveTestSlotLeaseByTankSession(context.Background(), "p1", "sess-B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "L2" {
		t.Fatalf("got lease=%s want L2", got.ID)
	}
	_, missErr := resolver.ResolveTestSlotLeaseByTankSession(context.Background(), "p1", "sess-missing")
	if !errors.Is(missErr, ErrNotFound) {
		t.Fatalf("expected ErrNotFound got %v", missErr)
	}
}


package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nelsong6/glimmung/internal/metrics"
)

// Inspection upload size caps. The screenshot bound mirrors typical
// Playwright full-page PNG sizes; the report bound is the soft ceiling
// on the structured payload mcp-glimmung emits (everything outside
// `screenshot_base64` from the previous shape, including the full
// elements list and accessibility tree).
const (
	maxInspectionScreenshotBytes = 25 * 1024 * 1024
	maxInspectionReportBytes     = 2 * 1024 * 1024
	inspectionRequestIDHeader    = "X-Inspection-Request-Id"
)

// InspectionLeaseResolver looks up the caller's claimed test-slot lease
// from the supplied tank_session_id. Implementations live in the
// server-side test-slot package — the handler depends only on this
// minimal contract so it stays unit-testable without spinning up the
// full lease store.
type InspectionLeaseResolver interface {
	ResolveTestSlotLeaseByTankSession(ctx context.Context, project, tankSessionID string) (Lease, error)
}

// inspectionResponse is the wire shape POST /v1/inspections returns to
// the caller. report_url / screenshot_url are relative paths that
// dereference through GET /v1/artifacts/{blob_path...}; scope_ref is
// the lease ref in V1 (lease-scoped only; run-scoped is a documented
// follow-up tracked on glimmung#143).
type inspectionResponse struct {
	InspectionID  string `json:"inspection_id"`
	ReportURL     string `json:"report_url"`
	ScreenshotURL string `json:"screenshot_url"`
	Scope         string `json:"scope"`
	ScopeRef      string `json:"scope_ref"`
	BlobPrefix    string `json:"blob_prefix"`
	CreatedAt     string `json:"created_at"`
}

// createInspectionDeps bundles the collaborators the handler needs.
type createInspectionDeps struct {
	store         InspectionStore
	leases        InspectionLeaseResolver
	artifactWrite ArtifactWriter
}

// InspectionStore is the server-side handle to slot_inspections, plus
// the lookup needed for cleanup (which lives in slot_inspection_store.go).
type InspectionStore = SlotInspectionStore

// createInspection is the POST /v1/inspections handler.
//
// Body: multipart/form-data with three parts —
//   - `tank_session_id` form field identifying the caller's session
//   - `report` part (Content-Type: application/json) carrying the full
//     inspection record mcp-glimmung produced
//   - `screenshot` part (Content-Type: image/png) carrying the PNG bytes
//
// Header: optional `X-Inspection-Request-Id` for idempotent retry.
//
// Server-side prefix decision: lease-scoped (`inspections/<lease_id>/<id>/...`)
// for V1. Run-scoped inspections — bytes under `runs/<project>/<run>/inspections/<id>/...`
// — are a documented follow-up because lease metadata does not carry a
// stable `run_id` today; see glimmung#143.
//
// Atomic write semantics: report + screenshot + ledger row all succeed
// together or get rolled back. If the ledger insert fails after both
// uploads, the handler deletes both blobs and counts an
// inspections_write_errors_total{phase=ledger}.
func createInspection(deps createInspectionDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.store == nil {
			writeProblem(w, http.StatusServiceUnavailable, "slot_inspections store not configured")
			return
		}
		if deps.leases == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease resolver not configured")
			return
		}
		if deps.artifactWrite == nil {
			writeProblem(w, http.StatusServiceUnavailable, "artifact writer not configured")
			return
		}

		// Cap the request body before parsing. http.MaxBytesReader returns a
		// real *http.MaxBytesError on overrun so we can distinguish 413 from
		// other parse failures below.
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxInspectionScreenshotBytes+maxInspectionReportBytes+1*1024*1024))

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
			writeProblem(w, http.StatusBadRequest, "content-type must be multipart/form-data")
			return
		}
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
			writeProblem(w, http.StatusBadRequest, "multipart boundary missing")
			return
		}

		var (
			tankSessionID string
			project       string
			reportBytes   []byte
			reportType    string
			screenshot    []byte
			screenshotCT  string
		)

		reader := multipart.NewReader(r.Body, boundary)
		for {
			part, perr := reader.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				if isMaxBytesError(perr) {
					metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
					writeProblem(w, http.StatusRequestEntityTooLarge, "inspection request body exceeds limit")
					return
				}
				metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
				writeProblem(w, http.StatusBadRequest, "invalid multipart body: "+perr.Error())
				return
			}
			switch part.FormName() {
			case "tank_session_id":
				value, rerr := readPartBytes(part, 4*1024)
				_ = part.Close()
				if rerr != nil {
					metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
					writeProblem(w, http.StatusBadRequest, "invalid tank_session_id part: "+rerr.Error())
					return
				}
				tankSessionID = strings.TrimSpace(string(value))
			case "project":
				value, rerr := readPartBytes(part, 1024)
				_ = part.Close()
				if rerr != nil {
					metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
					writeProblem(w, http.StatusBadRequest, "invalid project part: "+rerr.Error())
					return
				}
				project = strings.TrimSpace(string(value))
			case "report":
				buf, rerr := readPartBytes(part, maxInspectionReportBytes)
				partType := part.Header.Get("Content-Type")
				_ = part.Close()
				if rerr != nil {
					metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
					if errors.Is(rerr, errPartTooLarge) {
						writeProblem(w, http.StatusRequestEntityTooLarge, "report part exceeds limit")
						return
					}
					writeProblem(w, http.StatusBadRequest, "invalid report part: "+rerr.Error())
					return
				}
				reportBytes = buf
				reportType = strings.TrimSpace(partType)
			case "screenshot":
				buf, rerr := readPartBytes(part, maxInspectionScreenshotBytes)
				partType := part.Header.Get("Content-Type")
				_ = part.Close()
				if rerr != nil {
					metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
					if errors.Is(rerr, errPartTooLarge) {
						writeProblem(w, http.StatusRequestEntityTooLarge, "screenshot part exceeds limit")
						return
					}
					writeProblem(w, http.StatusBadRequest, "invalid screenshot part: "+rerr.Error())
					return
				}
				screenshot = buf
				screenshotCT = strings.TrimSpace(partType)
			default:
				_ = part.Close()
			}
		}

		if tankSessionID == "" {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
			writeProblem(w, http.StatusBadRequest, "tank_session_id part required")
			return
		}
		if project == "" {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
			writeProblem(w, http.StatusBadRequest, "project part required")
			return
		}
		if len(reportBytes) == 0 {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
			writeProblem(w, http.StatusBadRequest, "report part required")
			return
		}
		if len(screenshot) == 0 {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
			writeProblem(w, http.StatusBadRequest, "screenshot part required")
			return
		}
		if !strings.HasPrefix(strings.ToLower(reportType), "application/json") {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
			writeProblem(w, http.StatusUnsupportedMediaType, "report part must be application/json")
			return
		}
		// Validate JSON shape early so a malformed report doesn't leak a
		// half-uploaded inspection into blob storage.
		if !json.Valid(reportBytes) {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
			writeProblem(w, http.StatusBadRequest, "report part is not valid JSON")
			return
		}
		if screenshotCT == "" {
			screenshotCT = "image/png"
		}
		if !strings.HasPrefix(strings.ToLower(screenshotCT), "image/") {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseParse)
			writeProblem(w, http.StatusUnsupportedMediaType, "screenshot part must be an image/* content type")
			return
		}

		requestID := strings.TrimSpace(r.Header.Get(inspectionRequestIDHeader))

		// Resolve the caller's claimed lease. mcp-glimmung's session pod
		// already carries its Tank session id; the resolver looks up the
		// matching active test-slot lease (the same pattern as
		// `extend_test_slot_lease`).
		lease, leaseErr := deps.leases.ResolveTestSlotLeaseByTankSession(r.Context(), project, tankSessionID)
		if leaseErr != nil {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseLease)
			if errors.Is(leaseErr, ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "no active test-slot lease for the supplied tank_session_id")
				return
			}
			if errors.Is(leaseErr, ErrForbidden) {
				writeProblem(w, http.StatusForbidden, "tank_session_id does not match lease requester")
				return
			}
			writeInternalError(w, r, leaseErr, "resolve inspection lease failed")
			return
		}
		leaseID := strings.TrimSpace(lease.ID)
		if leaseID == "" {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseLease)
			writeInternalError(w, r, errors.New("resolved lease has empty id"), "resolve inspection lease failed")
			return
		}

		// Idempotency: if the caller already wrote for this
		// (lease_id, request_id), return the prior record verbatim and
		// drop the just-received bytes on the floor.
		if requestID != "" {
			existing, lookupErr := deps.store.LookupSlotInspectionByRequest(r.Context(), leaseID, requestID)
			if lookupErr == nil {
				writeJSON(w, http.StatusOK, inspectionResponseFromRecord(existing))
				return
			}
			if !errors.Is(lookupErr, ErrSlotInspectionNotFound) {
				metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseLedger)
				writeInternalError(w, r, lookupErr, "lookup existing inspection failed")
				return
			}
		}

		inspectionID := uuid.NewString()
		slotName := ""
		if v, ok := stringFromMap(lease.Metadata, "native_slot_name"); ok {
			slotName = strings.TrimSpace(v)
		}
		sessionID := tankSessionID

		// V1 prefix decision: lease-scoped. Run-scoped follow-up tracked
		// on glimmung#143.
		blobPrefix := path.Join("inspections", leaseID, inspectionID)
		if !strings.HasPrefix(blobPrefix, "inspections/") {
			// Belt-and-braces check that none of the components escape
			// the inspections/ prefix once joined.
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhasePrefix)
			writeInternalError(w, r, errors.New("derived prefix outside inspections/"), "inspection prefix derivation failed")
			return
		}
		reportBlobPath := path.Join(blobPrefix, "report.json")
		screenshotBlobPath := path.Join(blobPrefix, "screenshot.png")

		uploadCtx, cancelUpload := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancelUpload()
		reportSize, uperr := deps.artifactWrite.Upload(uploadCtx, reportBlobPath, reportBytes, "application/json")
		if uperr != nil {
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseUploadReport)
			writeInternalError(w, r, uperr, "upload inspection report failed")
			return
		}
		screenshotSize, uperr := deps.artifactWrite.Upload(uploadCtx, screenshotBlobPath, screenshot, screenshotCT)
		if uperr != nil {
			// Roll back the report upload so we don't leak orphan blobs.
			delCtx, cancelDel := context.WithTimeout(context.Background(), 30*time.Second)
			if delErr := deps.artifactWrite.Delete(delCtx, reportBlobPath); delErr != nil {
				slog.WarnContext(r.Context(), "inspection rollback failed (report)", "err", delErr, "blob", reportBlobPath)
			}
			cancelDel()
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseUploadScreenshot)
			writeInternalError(w, r, uperr, "upload inspection screenshot failed")
			return
		}

		record := SlotInspectionRecord{
			ID:                    inspectionID,
			Project:               project,
			Slot:                  slotName,
			LeaseID:               leaseID,
			SessionID:             sessionID,
			RequestID:             requestID,
			BlobPrefix:            blobPrefix,
			ReportBlobPath:        reportBlobPath,
			ScreenshotBlobPath:    screenshotBlobPath,
			ScreenshotContentType: screenshotCT,
			ByteSizeScreenshot:    screenshotSize,
			ByteSizeReport:        reportSize,
		}
		written, insertErr := deps.store.InsertSlotInspection(r.Context(), record)
		if insertErr != nil {
			// Roll both blobs back. The caller can safely retry with the
			// same X-Inspection-Request-Id; without a ledger row the
			// idempotency lookup will miss and the second attempt does
			// the work from scratch.
			delCtx, cancelDel := context.WithTimeout(context.Background(), 30*time.Second)
			if delErr := deps.artifactWrite.Delete(delCtx, reportBlobPath); delErr != nil {
				slog.WarnContext(r.Context(), "inspection rollback failed (report)", "err", delErr, "blob", reportBlobPath)
			}
			if delErr := deps.artifactWrite.Delete(delCtx, screenshotBlobPath); delErr != nil {
				slog.WarnContext(r.Context(), "inspection rollback failed (screenshot)", "err", delErr, "blob", screenshotBlobPath)
			}
			cancelDel()
			metrics.RecordInspectionWriteError(metrics.InspectionWritePhaseLedger)
			writeInternalError(w, r, insertErr, "record inspection ledger row failed")
			return
		}

		metrics.RecordInspectionWritten(metrics.InspectionScopeLease)
		writeJSON(w, http.StatusOK, inspectionResponseFromRecord(written))
	}
}

// inspectionDetailResponse is the wire shape for GET /v1/inspections/{id}.
// It carries the full ledger row alongside the URLs the read surface
// already exposes via the inspectionResponse summary.
type inspectionDetailResponse struct {
	InspectionID          string `json:"inspection_id"`
	Project               string `json:"project"`
	Slot                  string `json:"slot"`
	LeaseID               string `json:"lease_id"`
	SessionID             string `json:"session_id"`
	RequestID             string `json:"request_id,omitempty"`
	BlobPrefix            string `json:"blob_prefix"`
	ReportURL             string `json:"report_url"`
	ScreenshotURL         string `json:"screenshot_url"`
	ScreenshotContentType string `json:"screenshot_content_type"`
	ByteSizeScreenshot    int64  `json:"byte_size_screenshot"`
	ByteSizeReport        int64  `json:"byte_size_report"`
	Scope                 string `json:"scope"`
	ScopeRef              string `json:"scope_ref"`
	CreatedAt             string `json:"created_at"`
}

// inspectionListResponse wraps a slice of detail responses so future
// fields (cursor, total counts, etc.) can be added without breaking the
// wire shape.
type inspectionListResponse struct {
	Inspections []inspectionDetailResponse `json:"inspections"`
}

func inspectionDetailFromRecord(r SlotInspectionRecord) inspectionDetailResponse {
	return inspectionDetailResponse{
		InspectionID:          r.ID,
		Project:               r.Project,
		Slot:                  r.Slot,
		LeaseID:               r.LeaseID,
		SessionID:             r.SessionID,
		RequestID:             r.RequestID,
		BlobPrefix:            r.BlobPrefix,
		ReportURL:             "/v1/artifacts/" + r.ReportBlobPath,
		ScreenshotURL:         "/v1/artifacts/" + r.ScreenshotBlobPath,
		ScreenshotContentType: r.ScreenshotContentType,
		ByteSizeScreenshot:    r.ByteSizeScreenshot,
		ByteSizeReport:        r.ByteSizeReport,
		Scope:                 metrics.InspectionScopeLease,
		ScopeRef:              r.LeaseID,
		CreatedAt:             r.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// getInspectionByID is the GET /v1/inspections/{id} handler. Returns
// 404 when the ledger has no row with that id — including after a
// lease-cleanup sweep already removed the row and its blobs, which
// keeps the read surface honest about durability.
func getInspectionByID(store SlotInspectionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeProblem(w, http.StatusServiceUnavailable, "slot_inspections store not configured")
			return
		}
		id := strings.TrimSpace(r.PathValue("inspection_id"))
		if id == "" {
			writeProblem(w, http.StatusBadRequest, "inspection_id required")
			return
		}
		record, err := store.GetSlotInspectionByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, ErrSlotInspectionNotFound) {
				writeProblem(w, http.StatusNotFound, "inspection not found")
				return
			}
			writeInternalError(w, r, err, "read inspection failed")
			return
		}
		writeJSON(w, http.StatusOK, inspectionDetailFromRecord(record))
	}
}

// listInspections is the GET /v1/inspections handler. Filters:
//
//   - ?project=<name>
//   - ?lease=<lease_id>
//   - ?limit=<n>   (1..200, default 50)
//
// Run-bound inspections are not surfaced here in V1 — they live under
// `runs/<project>/<run>/inspections/<id>/...` and flow through the
// existing Run evidence machinery instead of slot_inspections. When the
// run-scoped path lands, this list will continue to enumerate only the
// free (lease-scoped) inspections; the run view exposes its own
// inspections through the existing Run report surface.
func listInspections(store SlotInspectionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeProblem(w, http.StatusServiceUnavailable, "slot_inspections store not configured")
			return
		}
		q := r.URL.Query()
		filter := SlotInspectionFilter{
			Project: strings.TrimSpace(q.Get("project")),
			LeaseID: strings.TrimSpace(q.Get("lease")),
		}
		if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed <= 0 {
				writeProblem(w, http.StatusBadRequest, "limit must be a positive integer")
				return
			}
			filter.Limit = parsed
		}
		records, err := store.ListSlotInspections(r.Context(), filter)
		if err != nil {
			writeInternalError(w, r, err, "list inspections failed")
			return
		}
		body := inspectionListResponse{
			Inspections: make([]inspectionDetailResponse, 0, len(records)),
		}
		for _, record := range records {
			body.Inspections = append(body.Inspections, inspectionDetailFromRecord(record))
		}
		writeJSON(w, http.StatusOK, body)
	}
}

func inspectionResponseFromRecord(r SlotInspectionRecord) inspectionResponse {
	return inspectionResponse{
		InspectionID:  r.ID,
		ReportURL:     "/v1/artifacts/" + r.ReportBlobPath,
		ScreenshotURL: "/v1/artifacts/" + r.ScreenshotBlobPath,
		Scope:         metrics.InspectionScopeLease,
		ScopeRef:      r.LeaseID,
		BlobPrefix:    r.BlobPrefix,
		CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// errPartTooLarge is returned by readPartBytes when a multipart part
// exceeds its caller-supplied byte cap. The handler maps this to a 413.
var errPartTooLarge = errors.New("multipart part too large")

func readPartBytes(part *multipart.Part, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	// Read one extra byte past the limit so we can distinguish "fits"
	// from "would have overflowed."
	buf := make([]byte, limit+1)
	n, err := io.ReadFull(part, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		if isMaxBytesError(err) {
			return nil, errPartTooLarge
		}
		return nil, err
	}
	if n > limit {
		return nil, errPartTooLarge
	}
	return buf[:n], nil
}

func isMaxBytesError(err error) bool {
	if err == nil {
		return false
	}
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return true
	}
	// Older Go stdlib raised "http: request body too large" as a plain
	// error before the typed MaxBytesError landed; the inspection
	// handler keeps the substring check for parity with environments
	// where the typed error isn't yet wired through every reader.
	return strings.Contains(err.Error(), "request body too large")
}


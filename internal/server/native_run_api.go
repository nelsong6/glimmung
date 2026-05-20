package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// NativeRunStore handles native k8s_job runner event recording and status.
type NativeRunStore interface {
	// GetNativeRunStatusByID returns the run state for the native runner.
	GetNativeRunStatusByID(ctx context.Context, project, runID string) (NativeRunStatusResponse, error)
	// RecordNativeEventByID writes one idempotent native event. The store resolves attempt context from the run doc.
	RecordNativeEventByID(ctx context.Context, project, runID string, req NativeRunEventRequest) (NativeRunEventResult, error)
	// ListNativeEventsByID returns ordered native events for a run with optional filters.
	ListNativeEventsByID(ctx context.Context, project, runID string, attemptIndex *int, jobID *string, limit *int) (NativeRunLogsResponse, error)
}

// NativeRunEventRequest is the body for POST /native/events.
type NativeRunEventRequest struct {
	JobID        string         `json:"job_id"`
	Seq          int            `json:"seq"`
	Event        string         `json:"event"`
	AttemptIndex *int           `json:"attempt_index,omitempty"`
	StepSlug     *string        `json:"step_slug"`
	Message      *string        `json:"message"`
	ExitCode     *int           `json:"exit_code"`
	Metadata     map[string]any `json:"metadata"`
}

// NativeRunEventResult is the response for POST /native/events.
type NativeRunEventResult struct {
	RunRef   string `json:"run_ref"`
	JobID    string `json:"job_id"`
	Seq      int    `json:"seq"`
	Accepted bool   `json:"accepted"`
}

// NativeRunStatusResponse is the response for GET /native/status.
type NativeRunStatusResponse struct {
	Project           string     `json:"project"`
	RunRef            string     `json:"run_ref"`
	State             string     `json:"state"`
	AttemptIndex      int        `json:"attempt_index"`
	CancelRequested   bool       `json:"cancel_requested"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at"`
	CancelReason      *string    `json:"cancel_reason"`
}

// NativeRunLogEvent is one event record in a native run log stream.
type NativeRunLogEvent struct {
	Project      string         `json:"project"`
	RunRef       string         `json:"run_ref"`
	AttemptIndex int            `json:"attempt_index"`
	Phase        string         `json:"phase"`
	JobID        string         `json:"job_id"`
	Seq          int            `json:"seq"`
	Event        string         `json:"event"`
	StepSlug     string         `json:"step_slug"`
	Message      string         `json:"message"`
	ExitCode     *int           `json:"exit_code"`
	Metadata     map[string]any `json:"metadata"`
	CreatedAt    string         `json:"created_at"`
}

// NativeRunLogsResponse is the response for GET /native/events.
type NativeRunLogsResponse struct {
	Project      string              `json:"project"`
	RunRef       string              `json:"run_ref"`
	AttemptIndex *int                `json:"attempt_index"`
	JobID        *string             `json:"job_id"`
	Events       []NativeRunLogEvent `json:"events"`
	ArchiveURL   *string             `json:"archive_url"`
}

// nativeRunEventsByNumber handles GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events
func nativeRunEventsByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nativeStore, mutStore, ok := requireNativeStores(w, store)
		if !ok {
			return
		}
		runID, project, ok := resolveRunByNumber(w, r, mutStore)
		if !ok {
			return
		}
		resp, err := nativeStore.ListNativeEventsByID(r.Context(), project, runID,
			parseOptionalIntQuery(r, "attempt_index"),
			parseOptionalStringQuery(r, "job_id"),
			parseOptionalIntQuery(r, "limit"),
		)
		writeNativeLogsOrError(w, r, resp, err)
	}
}

// nativeRunEventWriteByNumber handles POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events
func nativeRunEventWriteByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nativeStore, mutStore, ok := requireNativeStores(w, store)
		if !ok {
			return
		}
		runID, project, ok := resolveRunByNumber(w, r, mutStore)
		if !ok {
			return
		}
		postNativeEvent(w, r, nativeStore, project, runID)
	}
}

// nativeRunEventWriteByCallbackToken handles POST /v1/run-callbacks/{callback_token}/native/events
func nativeRunEventWriteByCallbackToken(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nativeStore, mutStore, ok := requireNativeStores(w, store)
		if !ok {
			return
		}
		runID, project, ok := resolveRunByCallbackToken(w, r, mutStore)
		if !ok {
			return
		}
		postNativeEvent(w, r, nativeStore, project, runID)
	}
}

// nativeRunStatusByNumber handles GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/status
func nativeRunStatusByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nativeStore, mutStore, ok := requireNativeStores(w, store)
		if !ok {
			return
		}
		runID, project, ok := resolveRunByNumber(w, r, mutStore)
		if !ok {
			return
		}
		getNativeStatus(w, r, nativeStore, project, runID)
	}
}

// nativeRunStatusByCallbackToken handles GET /v1/run-callbacks/{callback_token}/native/status
func nativeRunStatusByCallbackToken(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nativeStore, mutStore, ok := requireNativeStores(w, store)
		if !ok {
			return
		}
		runID, project, ok := resolveRunByCallbackToken(w, r, mutStore)
		if !ok {
			return
		}
		getNativeStatus(w, r, nativeStore, project, runID)
	}
}

// --- shared helpers ---

func requireNativeStores(w http.ResponseWriter, store ReadStore) (NativeRunStore, RunMutationStore, bool) {
	nativeStore, ok := store.(NativeRunStore)
	if !ok || nativeStore == nil {
		writeProblem(w, http.StatusServiceUnavailable, "native run store not configured")
		return nil, nil, false
	}
	mutStore, ok := store.(RunMutationStore)
	if !ok || mutStore == nil {
		writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
		return nil, nil, false
	}
	return nativeStore, mutStore, true
}

func resolveRunByNumber(w http.ResponseWriter, r *http.Request, mutStore RunMutationStore) (runID, project string, ok bool) {
	project = r.PathValue("project")
	issueNumber, err := strconv.Atoi(r.PathValue("issue_number"))
	if err != nil || issueNumber < 1 {
		writeProblem(w, http.StatusBadRequest, "issue_number must be a positive integer")
		return "", "", false
	}
	runID, _, err = mutStore.ReadRunIDForNumber(r.Context(), project, issueNumber, r.PathValue("run_number"))
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return "", "", false
	}
	if err != nil {
		writeInternalError(w, r, err, "read run failed")
		return "", "", false
	}
	return runID, project, true
}

func resolveRunByCallbackToken(w http.ResponseWriter, r *http.Request, mutStore RunMutationStore) (runID, project string, ok bool) {
	token := r.PathValue("callback_token")
	var err error
	runID, project, _, err = mutStore.ReadRunIDForCallbackToken(r.Context(), token)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run callback token not found")
		return "", "", false
	}
	if err != nil {
		writeInternalError(w, r, err, "read run by callback token failed")
		return "", "", false
	}
	return runID, project, true
}

func postNativeEvent(w http.ResponseWriter, r *http.Request, store NativeRunStore, project, runID string) {
	var req NativeRunEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.JobID == "" {
		writeProblem(w, http.StatusBadRequest, "job_id required")
		return
	}
	if req.Seq < 1 {
		writeProblem(w, http.StatusBadRequest, "seq must be >= 1")
		return
	}
	if req.Event == "" {
		writeProblem(w, http.StatusBadRequest, "event required")
		return
	}
	result, err := store.RecordNativeEventByID(r.Context(), project, runID, req)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if errors.Is(err, ErrConflict) {
		writeProblem(w, http.StatusConflict, "duplicate event with different payload")
		return
	}
	var validationErr ValidationError
	if errors.As(err, &validationErr) {
		writeProblem(w, http.StatusBadRequest, validationErr.Message)
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "record native event failed")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func getNativeStatus(w http.ResponseWriter, r *http.Request, store NativeRunStore, project, runID string) {
	resp, err := store.GetNativeRunStatusByID(r.Context(), project, runID)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if errors.Is(err, ErrConflict) {
		writeProblem(w, http.StatusConflict, "latest attempt is not a native k8s_job")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "get native run status failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeNativeLogsOrError(w http.ResponseWriter, r *http.Request, resp NativeRunLogsResponse, err error) {
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "list native events failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseOptionalIntQuery parses an optional integer query parameter, returning nil if absent or invalid.
func parseOptionalIntQuery(r *http.Request, name string) *int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	return &v
}

// parseOptionalStringQuery returns a non-empty query parameter value, or nil.
func parseOptionalStringQuery(r *http.Request, name string) *string {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil
	}
	return &raw
}

package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

// fakeRunMutationStore implements RunMutationStore + NativeRunStore for tests.
type fakeRunMutationStore struct {
	fakeReadStore
	runID    string
	runRef   string
	notFound bool

	abortResult AbortRunResult
	abortErr    error

	nativeStatus      NativeRunStatusResponse
	nativeStatusErr   error
	nativeEventResult NativeRunEventResult
	nativeEventErr    error
	nativeEvents      NativeRunLogsResponse
	nativeEventsErr   error
	nativeAfterSeq    *int
	nativeLimit       *int
}

func (s *fakeRunMutationStore) ReadRunIDForNumber(_ context.Context, project string, issueNumber int, runNumber string) (string, string, error) {
	if s.notFound {
		return "", "", ErrNotFound
	}
	return s.runID, s.runRef, nil
}

func (s *fakeRunMutationStore) ReadRunIDForCallbackToken(_ context.Context, token string) (string, string, string, error) {
	if s.notFound {
		return "", "", "", ErrNotFound
	}
	return s.runID, "myproject", s.runRef, nil
}

func (s *fakeRunMutationStore) AbortRunByID(_ context.Context, project, runID, reason string) (AbortRunResult, error) {
	return s.abortResult, s.abortErr
}

func (s *fakeRunMutationStore) GetNativeRunStatusByID(_ context.Context, project, runID string) (NativeRunStatusResponse, error) {
	return s.nativeStatus, s.nativeStatusErr
}

func (s *fakeRunMutationStore) RecordNativeEventByID(_ context.Context, project, runID string, req NativeRunEventRequest) (NativeRunEventResult, error) {
	return s.nativeEventResult, s.nativeEventErr
}

func (s *fakeRunMutationStore) ListNativeEventsByID(_ context.Context, project, runID string, attemptIndex *int, jobID *string, afterSeq *int, limit *int) (NativeRunLogsResponse, error) {
	s.nativeAfterSeq = afterSeq
	s.nativeLimit = limit
	return s.nativeEvents, s.nativeEventsErr
}

func newRunMutHandlerAdmin(store *fakeRunMutationStore) http.Handler {
	return NewWithSyncClient(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)
}

func newRunMutHandlerNoAuth(store *fakeRunMutationStore) http.Handler {
	return NewWithSyncClient(Settings{}, store, nil, nil)
}

// --- abort tests ---

func TestAbortRunByNumber(t *testing.T) {
	runRef := "myproject#42/runs/1"
	store := &fakeRunMutationStore{
		runID:       "run-123",
		runRef:      runRef,
		abortResult: AbortRunResult{State: "aborted", RunRef: runRef},
	}
	handler := newRunMutHandlerAdmin(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/42/runs/1/abort", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state":"aborted"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestAbortRunByNumberAlreadyTerminal(t *testing.T) {
	runRef := "myproject#42/runs/2"
	store := &fakeRunMutationStore{
		runID:       "run-456",
		runRef:      runRef,
		abortResult: AbortRunResult{State: "already_terminal", RunRef: runRef},
	}
	handler := newRunMutHandlerAdmin(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/42/runs/2/abort", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"already_terminal"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestAbortRunByNumberNotFound(t *testing.T) {
	store := &fakeRunMutationStore{notFound: true}
	handler := newRunMutHandlerAdmin(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/42/runs/99/abort", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAbortRunByNumberRequiresAdmin(t *testing.T) {
	store := &fakeRunMutationStore{runID: "x", runRef: "y", abortResult: AbortRunResult{State: "aborted", RunRef: "y"}}
	handler := newRunMutHandlerNoAuth(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/42/runs/1/abort", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 without admin auth, got 200")
	}
}

func TestAbortRunByNumberBadIssueNumber(t *testing.T) {
	store := &fakeRunMutationStore{runID: "x", runRef: "y"}
	handler := newRunMutHandlerAdmin(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/zero/runs/1/abort", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// --- webhook tests ---

func TestGitHubWebhookNoSecret(t *testing.T) {
	handler := New(Settings{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook/github", strings.NewReader(`{"action":"opened"}`))
	req.Header.Set("x-github-event", "pull_request")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"accepted"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestGitHubWebhookValidSignature(t *testing.T) {
	secret := "webhook-secret"
	body := []byte(`{"action":"created"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	handler := New(Settings{GitHubWebhookSecret: secret})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook/github", bytes.NewReader(body))
	req.Header.Set("x-hub-signature-256", sig)
	req.Header.Set("x-github-event", "push")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGitHubWebhookInvalidSignature(t *testing.T) {
	handler := New(Settings{GitHubWebhookSecret: "real-secret"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook/github", strings.NewReader(`{}`))
	req.Header.Set("x-hub-signature-256", "sha256=badhash")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGitHubWebhookMissingSignature(t *testing.T) {
	handler := New(Settings{GitHubWebhookSecret: "real-secret"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook/github", strings.NewReader(`{}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// --- native run tests ---

func TestNativeRunStatusByNumber(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:  "run-native",
		runRef: "proj#10/runs/1",
		nativeStatus: NativeRunStatusResponse{
			Project:      "proj",
			RunRef:       "proj#10/runs/1",
			State:        "in_progress",
			AttemptIndex: 0,
		},
	}
	handler := newRunMutHandlerNoAuth(store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/projects/proj/issues/10/runs/1/native/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state":"in_progress"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestNativeRunStatusByCallbackToken(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:  "run-native",
		runRef: "proj#10/runs/1",
		nativeStatus: NativeRunStatusResponse{
			Project:      "proj",
			RunRef:       "proj#10/runs/1",
			State:        "in_progress",
			AttemptIndex: 0,
		},
	}
	handler := newRunMutHandlerNoAuth(store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/run-callbacks/mytoken/native/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNativeRunStatusNotFoundByNumber(t *testing.T) {
	store := &fakeRunMutationStore{notFound: true}
	handler := newRunMutHandlerNoAuth(store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/projects/proj/issues/10/runs/1/native/status", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNativeRunEventsListByNumber(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:  "run-ev",
		runRef: "proj#11/runs/2",
		nativeEvents: NativeRunLogsResponse{
			Project: "proj",
			RunRef:  "proj#11/runs/2",
			Events:  []NativeRunLogEvent{{JobID: "job1", Seq: 1, Event: "log", Message: "hello"}},
		},
	}
	handler := newRunMutHandlerNoAuth(store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/projects/proj/issues/11/runs/2/native/events", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"job1"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestNativeRunEventsListByNumberPassesSeqCursor(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:  "run-ev",
		runRef: "proj#11/runs/2",
		nativeEvents: NativeRunLogsResponse{
			Project: "proj",
			RunRef:  "proj#11/runs/2",
			Events:  []NativeRunLogEvent{{JobID: "job1", Seq: 201, Event: "log", Message: "next"}},
		},
	}
	handler := newRunMutHandlerNoAuth(store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/projects/proj/issues/11/runs/2/native/events?after_seq=200&limit=200", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.nativeAfterSeq == nil || *store.nativeAfterSeq != 200 {
		t.Fatalf("after_seq=%v, want 200", store.nativeAfterSeq)
	}
	if store.nativeLimit == nil || *store.nativeLimit != 200 {
		t.Fatalf("limit=%v, want 200", store.nativeLimit)
	}
}

func TestNativeRunEventWriteByCallbackToken(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:  "run-ev",
		runRef: "proj#11/runs/2",
		nativeStatus: NativeRunStatusResponse{
			Project:      "proj",
			RunRef:       "proj#11/runs/2",
			State:        "in_progress",
			AttemptIndex: 0,
		},
		nativeEventResult: NativeRunEventResult{
			RunRef:   "proj#11/runs/2",
			JobID:    "myjob",
			Seq:      5,
			Accepted: true,
		},
	}
	handler := newRunMutHandlerNoAuth(store)

	body, _ := json.Marshal(NativeRunEventRequest{JobID: "myjob", Seq: 5, Event: "log"})
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/tok/native/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"accepted":true`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestNativeRunEventWriteByNumberValidation(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:             "run-ev",
		runRef:            "proj#11/runs/2",
		nativeStatus:      NativeRunStatusResponse{AttemptIndex: 0, State: "in_progress"},
		nativeEventResult: NativeRunEventResult{Accepted: true},
	}
	handler := newRunMutHandlerNoAuth(store)

	// Missing job_id → 400
	body, _ := json.Marshal(NativeRunEventRequest{Seq: 1, Event: "log"})
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj/issues/11/runs/2/native/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing job_id: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// seq=0 → 400
	body, _ = json.Marshal(NativeRunEventRequest{JobID: "job1", Seq: 0, Event: "log"})
	req = httptest.NewRequest(http.MethodPost, "/v1/projects/proj/issues/11/runs/2/native/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("seq=0: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
	"github.com/nelsong6/glimmung/internal/domain/budget"
)

// fakeDispatchStore implements RunDispatchStore.
type fakeDispatchStore struct {
	fakeReadStore

	githubRepo    string
	githubRepoErr error

	issue    *IssueDispatchData
	issueErr error

	wf    *Workflow
	wfErr error

	workflows    []Workflow
	workflowsErr error

	lockErr error

	run    *CreatedRun
	runErr error

	leaseResult Lease
	leaseHost   *Host
	leaseErr    error

	abortResult AbortRunResult
	abortErr    error

	lockReleased bool
}

func (s *fakeDispatchStore) ReadProjectGitHubRepo(_ context.Context, _ string) (string, error) {
	return s.githubRepo, s.githubRepoErr
}

func (s *fakeDispatchStore) ReadIssueForDispatch(_ context.Context, _ string, _ int) (IssueDispatchData, error) {
	if s.issueErr != nil {
		return IssueDispatchData{}, s.issueErr
	}
	if s.issue == nil {
		return IssueDispatchData{}, ErrNotFound
	}
	return *s.issue, nil
}

func (s *fakeDispatchStore) GetWorkflowByName(_ context.Context, _, _ string) (*Workflow, error) {
	return s.wf, s.wfErr
}

func (s *fakeDispatchStore) ListProjectWorkflows(_ context.Context, _ string) ([]Workflow, error) {
	return s.workflows, s.workflowsErr
}

func (s *fakeDispatchStore) ClaimIssueLock(_ context.Context, _ string, _ int, _ string, _ int) error {
	return s.lockErr
}

func (s *fakeDispatchStore) ReleaseIssueLock(_ context.Context, _ string, _ int, _ string) {
	s.lockReleased = true
}

func (s *fakeDispatchStore) CreateRun(_ context.Context, _ CreateRunRequest) (CreatedRun, error) {
	if s.runErr != nil {
		return CreatedRun{}, s.runErr
	}
	if s.run == nil {
		return CreatedRun{ID: "run-1", RunNumber: 1, RunDisplay: "1", CallbackToken: "tok"}, nil
	}
	return *s.run, nil
}

func (s *fakeDispatchStore) AcquireLease(_ context.Context, _ LeaseAcquireRequest) (Lease, *Host, error) {
	return s.leaseResult, s.leaseHost, s.leaseErr
}

func (s *fakeDispatchStore) AbortRunByID(_ context.Context, _, _, _ string) (AbortRunResult, error) {
	return s.abortResult, s.abortErr
}

// helpers

func newDispatchHandler(store *fakeDispatchStore, dispatch GHADispatchClient) http.Handler {
	return NewWithSyncClient(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)
}

func newDispatchHandlerWithDispatch(store *fakeDispatchStore, dispatch GHADispatchClient) http.Handler {
	type combo struct {
		*fakeDispatchStore
		WorkflowSyncClient
		GHADispatchClient
	}
	_ = combo{} // suppress unused
	// The dispatch client is extracted from the ghClient in newHandler via type assertion.
	// Since fakeDispatchStore doesn't implement WorkflowSyncClient, pass nil for ghClient
	// and wire ghDispatch separately via a thin wrapper.
	return newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, dispatch)
}

// newHandlerWithDispatch is a test-only constructor that wires ghDispatch directly.
func newHandlerWithDispatch(settings Settings, store ReadStore, authResolver AuthResolver, ghDispatch GHADispatchClient) http.Handler {
	adminAuthenticator, _ := authResolver.(AdminAuthenticator)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/runs/dispatch",
		requireAdmin(adminAuthenticator, http.HandlerFunc(dispatchRunHandler(store, ghDispatch))))
	return mux
}

func minimalDispatchStore() *fakeDispatchStore {
	leaseNum := 1
	wf := &Workflow{
		Name:    "main",
		Project: "proj",
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{
			{
				Name:             "impl",
				Kind:             "gha_dispatch",
				WorkflowFilename: "impl.yml",
				WorkflowRef:      "main",
			},
		},
		DefaultRequirements: map[string]any{},
		Metadata:            map[string]any{},
	}
	return &fakeDispatchStore{
		githubRepo: "owner/repo",
		issue: &IssueDispatchData{
			ID:    "issue-1",
			Title: "Test issue",
			Body:  "body",
		},
		wf:        wf,
		workflows: []Workflow{*wf}, // used when no workflow_name is specified
		leaseResult: Lease{
			Project:     "proj",
			LeaseNumber: &leaseNum,
			Metadata:    map[string]any{"lease_callback_token": "lctok"},
		},
	}
}

func dispatchRequest(project string, issueNumber int) *http.Request {
	body, _ := json.Marshal(DispatchRunRequest{Project: project, IssueNumber: issueNumber})
	req := httptest.NewRequest("POST", "/v1/runs/dispatch", bytes.NewReader(body))
	return req
}

// --- tests ---

func TestDispatchRun_MissingProject(t *testing.T) {
	store := minimalDispatchStore()
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	body, _ := json.Marshal(DispatchRunRequest{IssueNumber: 1})
	req := httptest.NewRequest("POST", "/v1/runs/dispatch", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestDispatchRun_MissingIssueNumber(t *testing.T) {
	store := minimalDispatchStore()
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	body, _ := json.Marshal(DispatchRunRequest{Project: "proj"})
	req := httptest.NewRequest("POST", "/v1/runs/dispatch", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestDispatchRun_ProjectNotFound(t *testing.T) {
	store := minimalDispatchStore()
	store.githubRepo = ""
	store.githubRepoErr = ErrNotFound
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, dispatchRequest("proj", 1))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result PublicDispatchResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "no_project" {
		t.Errorf("expected no_project, got %q", result.State)
	}
}

func TestDispatchRun_IssueNotFound(t *testing.T) {
	store := minimalDispatchStore()
	store.issue = nil
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, dispatchRequest("proj", 99))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result PublicDispatchResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "no_project" {
		t.Errorf("expected no_project, got %q", result.State)
	}
}

func TestDispatchRun_NoWorkflowRegistered(t *testing.T) {
	store := minimalDispatchStore()
	store.wf = nil
	store.workflows = nil
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, dispatchRequest("proj", 1))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result PublicDispatchResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "no_workflow" {
		t.Errorf("expected no_workflow, got %q", result.State)
	}
}

func TestDispatchRun_AlreadyRunning(t *testing.T) {
	store := minimalDispatchStore()
	store.lockErr = &AlreadyRunningError{
		HeldBy:    "holder-123",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, dispatchRequest("proj", 1))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result PublicDispatchResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "already_running" {
		t.Errorf("expected already_running, got %q", result.State)
	}
}

func TestDispatchRun_AlreadyRunning_ErrIs(t *testing.T) {
	// Verify that errors.Is works for the AlreadyRunningError wrapper.
	err := &AlreadyRunningError{HeldBy: "x", ExpiresAt: time.Now()}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Error("expected errors.Is(AlreadyRunningError, ErrAlreadyRunning) to be true")
	}
}

func TestDispatchRun_Pending_NoHost(t *testing.T) {
	store := minimalDispatchStore()
	store.leaseHost = nil // no host available
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, dispatchRequest("proj", 1))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result PublicDispatchResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "pending" {
		t.Errorf("expected pending, got %q", result.State)
	}
	if result.RunNumber == nil || *result.RunNumber != 1 {
		t.Errorf("expected run_number=1, got %v", result.RunNumber)
	}
	if result.Lease != "claimed" {
		t.Errorf("expected lease=claimed, got %q", result.Lease)
	}
}

func TestDispatchRun_Dispatched_WithHost(t *testing.T) {
	store := minimalDispatchStore()
	store.leaseHost = &Host{Name: "host-1"}
	dispatch := &fakeDispatchClient{}
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, dispatch)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, dispatchRequest("proj", 1))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result PublicDispatchResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "dispatched" {
		t.Errorf("expected dispatched, got %q", result.State)
	}
	if result.Host == nil || *result.Host != "host-1" {
		t.Errorf("expected host=host-1, got %v", result.Host)
	}
	if !dispatch.called {
		t.Error("expected DispatchWorkflow to be called")
	}
}

func TestDispatchRun_Pending_NoDispatchClient(t *testing.T) {
	store := minimalDispatchStore()
	store.leaseHost = &Host{Name: "host-1"}
	// No dispatch client — even with a host, result should be pending.
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, dispatchRequest("proj", 1))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result PublicDispatchResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "pending" {
		t.Errorf("expected pending (no dispatch client), got %q", result.State)
	}
}

func TestDispatchRun_DispatchFailed(t *testing.T) {
	store := minimalDispatchStore()
	store.leaseHost = &Host{Name: "host-1"}
	dispatch := &fakeDispatchClient{err: errors.New("422 unprocessable")}
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, dispatch)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, dispatchRequest("proj", 1))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result PublicDispatchResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "dispatch_failed" {
		t.Errorf("expected dispatch_failed, got %q", result.State)
	}
	if result.Detail == nil {
		t.Error("expected detail to be set")
	}
}

func TestDispatchRun_CreateRunFailReleasesLock(t *testing.T) {
	store := minimalDispatchStore()
	store.runErr = errors.New("cosmos unavailable")
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, dispatchRequest("proj", 1))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
	if !store.lockReleased {
		t.Error("expected ReleaseIssueLock to be called on CreateRun failure")
	}
}

func TestDispatchRun_MultipleWorkflows_NoName(t *testing.T) {
	store := minimalDispatchStore()
	store.wf = nil
	leaseNum := 1
	store.workflows = []Workflow{
		{Name: "wf-a", Project: "proj", Phases: []PhaseSpec{{Name: "impl", WorkflowFilename: "a.yml"}}, Budget: budget.Config{Total: 25},
			Metadata: map[string]any{}, DefaultRequirements: map[string]any{}},
		{Name: "wf-b", Project: "proj", Phases: []PhaseSpec{{Name: "impl", WorkflowFilename: "b.yml"}}, Budget: budget.Config{Total: 25},
			Metadata: map[string]any{}, DefaultRequirements: map[string]any{}},
	}
	store.leaseResult = Lease{Project: "proj", LeaseNumber: &leaseNum, Metadata: map[string]any{}}
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, dispatchRequest("proj", 1))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result PublicDispatchResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "no_workflow" {
		t.Errorf("expected no_workflow (ambiguous), got %q", result.State)
	}
}

func TestDispatchRun_WorkflowByName(t *testing.T) {
	store := minimalDispatchStore()
	// wf is already set in minimalDispatchStore and GetWorkflowByName returns it
	h := newHandlerWithDispatch(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)

	body, _ := json.Marshal(DispatchRunRequest{Project: "proj", IssueNumber: 1, WorkflowName: "main"})
	req := httptest.NewRequest("POST", "/v1/runs/dispatch", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result PublicDispatchResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "pending" && result.State != "dispatched" {
		t.Errorf("expected pending or dispatched, got %q", result.State)
	}
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// resumeFakeStore implements both ReadStore (via fakeDispatchStore) and RunResumeStore.
type resumeFakeStore struct {
	fakeDispatchStore
	runIDByNumber      map[string]string // "project#N#runNum" -> runID
	runsByID           map[string]RunForResume
	resumeWorkflows    map[string]*Workflow
	claimLockErr       error
	createResumedRunFn func(req CreateResumedRunRequest) (CreatedRun, error)
	resumeLeaseResult  Lease
	resumeLeaseHost    *Host
	resumeLeaseErr     error
	substituteErr      error
}

func (s *resumeFakeStore) ReadRunByNumber(_ context.Context, project string, issueNumber int, runNumber string) (string, error) {
	key := fmt.Sprintf("%s#%d#%s", project, issueNumber, runNumber)
	id, ok := s.runIDByNumber[key]
	if !ok {
		return "", ErrNotFound
	}
	return id, nil
}
func (s *resumeFakeStore) ReadRunForResume(_ context.Context, _, runID string) (RunForResume, error) {
	r, ok := s.runsByID[runID]
	if !ok {
		return RunForResume{}, ErrNotFound
	}
	return r, nil
}
func (s *resumeFakeStore) GetWorkflowByName(_ context.Context, _, name string) (*Workflow, error) {
	wf, ok := s.resumeWorkflows[name]
	if !ok {
		return nil, nil
	}
	return wf, nil
}
func (s *resumeFakeStore) ClaimIssueLock(_ context.Context, _ string, _ int, _ string, _ int) error {
	return s.claimLockErr
}
func (s *resumeFakeStore) ReleaseIssueLock(_ context.Context, _ string, _ int, _ string) {}
func (s *resumeFakeStore) CreateResumedRun(_ context.Context, req CreateResumedRunRequest) (CreatedRun, error) {
	if s.createResumedRunFn != nil {
		return s.createResumedRunFn(req)
	}
	return CreatedRun{ID: "new-run-id", RunNumber: 2, RunDisplay: "1.1", CallbackToken: "tok"}, nil
}
func (s *resumeFakeStore) AcquireLease(_ context.Context, _ LeaseAcquireRequest) (Lease, *Host, error) {
	return s.resumeLeaseResult, s.resumeLeaseHost, s.resumeLeaseErr
}
func (s *resumeFakeStore) AbortRunByID(_ context.Context, _, _, _ string) (AbortRunResult, error) {
	return AbortRunResult{}, nil
}
func (s *resumeFakeStore) SubstitutePhaseInputs(phase PhaseSpec, priorOutputs map[string]map[string]string) (map[string]string, error) {
	if s.substituteErr != nil {
		return nil, s.substituteErr
	}
	resolved := map[string]string{}
	for inputName := range phase.Inputs {
		resolved[inputName] = "resolved"
	}
	return resolved, nil
}
func (s *resumeFakeStore) CollectPriorOutputs(attempts []AttemptForResume) map[string]map[string]string {
	result := map[string]map[string]string{}
	for _, a := range attempts {
		if len(a.PhaseOutputs) > 0 {
			result[a.Phase] = a.PhaseOutputs
		}
	}
	return result
}

func newResumeTestHandler(store ReadStore, ghDispatch GHADispatchClient) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/resume",
		resumeRunHandler(store, ghDispatch))
	return mux
}

func minimalResumeStore() *resumeFakeStore {
	wf := &Workflow{
		Name: "ci",
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "gha_dispatch", WorkflowFilename: "env-prep.yml"},
			{Name: "agent-execute", Kind: "gha_dispatch", WorkflowFilename: "agent-execute.yml"},
		},
	}
	leaseNum := 7
	s := &resumeFakeStore{
		runIDByNumber: map[string]string{
			"myproject#10#1": "prior-run-id",
		},
		runsByID: map[string]RunForResume{
			"prior-run-id": {
				ID:               "prior-run-id",
				Project:          "myproject",
				Workflow:         "ci",
				State:            "completed",
				IssueID:          "issue-abc",
				IssueRepo:        "org/repo",
				IssueNumber:      10,
				RunDisplayNumber: strPtr("1"),
				Attempts: []AttemptForResume{
					{Phase: "env-prep", PhaseOutputs: map[string]string{"namespace": "ns-10"}},
				},
			},
		},
		resumeWorkflows:   map[string]*Workflow{"ci": wf},
		resumeLeaseResult: Lease{Project: "myproject", LeaseNumber: &leaseNum},
	}
	return s
}

func resumeReq(phase string) *http.Request {
	body, _ := json.Marshal(ResumeRunRequest{EntrypointPhase: phase})
	r := httptest.NewRequest(http.MethodPost,
		"/v1/projects/myproject/issues/10/runs/1/resume",
		bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestResumeRun_RunNotFound(t *testing.T) {
	store := minimalResumeStore()
	store.runIDByNumber = map[string]string{}
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("agent-execute"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestResumeRun_PriorInProgress(t *testing.T) {
	store := minimalResumeStore()
	prior := store.runsByID["prior-run-id"]
	prior.State = "in_progress"
	store.runsByID["prior-run-id"] = prior
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("agent-execute"))
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", w.Code)
	}
}

func TestResumeRun_WorkflowMissing(t *testing.T) {
	store := minimalResumeStore()
	store.resumeWorkflows = map[string]*Workflow{}
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("agent-execute"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestResumeRun_PhaseInvalid(t *testing.T) {
	store := minimalResumeStore()
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("no-such-phase"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", w.Code)
	}
}

func TestResumeRun_AlreadyRunning(t *testing.T) {
	store := minimalResumeStore()
	store.claimLockErr = &AlreadyRunningError{HeldBy: "other"}
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("agent-execute"))
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", w.Code)
	}
}

func TestResumeRun_OutputsMissing(t *testing.T) {
	store := minimalResumeStore()
	store.createResumedRunFn = func(_ CreateResumedRunRequest) (CreatedRun, error) {
		return CreatedRun{}, fmt.Errorf("%w: env-prep has no outputs", ErrOutputsMissing)
	}
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("agent-execute"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", w.Code)
	}
}

func TestResumeRun_AcquireLeaseFails(t *testing.T) {
	store := minimalResumeStore()
	store.resumeLeaseErr = errors.New("no capacity")
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("agent-execute"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestResumeRun_Pending(t *testing.T) {
	store := minimalResumeStore()
	store.resumeLeaseHost = nil
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("agent-execute"))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var result PublicResumeResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.State != "pending" {
		t.Errorf("want pending, got %q", result.State)
	}
	if result.Lease == nil || *result.Lease != "claimed" {
		t.Errorf("want lease=claimed, got %v", result.Lease)
	}
}

func TestResumeRun_Dispatched(t *testing.T) {
	store := minimalResumeStore()
	store.resumeLeaseHost = &Host{Name: "worker-1"}
	gh := &fakeDispatchClient{}
	h := newResumeTestHandler(store, gh)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("agent-execute"))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var result PublicResumeResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.State != "dispatched" {
		t.Errorf("want dispatched, got %q", result.State)
	}
	if !gh.called {
		t.Error("expected DispatchWorkflow to be called")
	}
	if result.Host == nil || *result.Host != "worker-1" {
		t.Errorf("want host=worker-1, got %v", result.Host)
	}
}

func TestResumeRun_DispatchFailed(t *testing.T) {
	store := minimalResumeStore()
	store.resumeLeaseHost = &Host{Name: "worker-1"}
	gh := &fakeDispatchClient{err: errors.New("github unavailable")}
	h := newResumeTestHandler(store, gh)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("agent-execute"))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var result PublicResumeResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.State != "dispatch_failed" {
		t.Errorf("want dispatch_failed, got %q", result.State)
	}
}

func TestResumeRun_MissingEntrypointPhase(t *testing.T) {
	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/10/runs/1/resume",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	h := newResumeTestHandler(minimalResumeStore(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestResumeRun_StepBoundaryNonK8s(t *testing.T) {
	store := minimalResumeStore()
	jobID := "job-1"
	body, _ := json.Marshal(ResumeRunRequest{
		EntrypointPhase: "agent-execute",
		EntrypointJobID: &jobID,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/10/runs/1/resume",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", w.Code, w.Body.String())
	}
}

func TestResumeRun_SubstitutionFails(t *testing.T) {
	store := minimalResumeStore()
	store.substituteErr = errors.New("missing ref")
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("agent-execute"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", w.Code, w.Body.String())
	}
}

func TestResumeRun_EntriesAtPhaseZero(t *testing.T) {
	// Resuming at the very first phase — no skipped phases, no outputs needed.
	store := minimalResumeStore()
	store.resumeLeaseHost = nil
	h := newResumeTestHandler(store, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, resumeReq("env-prep")) // first phase
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var result PublicResumeResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.State != "pending" {
		t.Errorf("want pending, got %q", result.State)
	}
}

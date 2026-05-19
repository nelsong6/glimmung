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

type resumeFakeStore struct {
	fakeDispatchStore
	runIDByNumber      map[string]string
	runsByID           map[string]RunForResume
	resumeWorkflows    map[string]*Workflow
	claimLockErr       error
	createResumedRunFn func(req CreateResumedRunRequest) (CreatedRun, error)
	resumeLeaseResult  Lease
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

func (s *resumeFakeStore) ReadRunForResume(_ context.Context, _ string, runID string) (RunForResume, error) {
	r, ok := s.runsByID[runID]
	if !ok {
		return RunForResume{}, ErrNotFound
	}
	return r, nil
}

func (s *resumeFakeStore) GetWorkflowByName(_ context.Context, _ string, name string) (*Workflow, error) {
	return s.resumeWorkflows[name], nil
}

func (s *resumeFakeStore) ClaimIssueLock(context.Context, string, int, string, int) error {
	return s.claimLockErr
}

func (s *resumeFakeStore) ReleaseIssueLock(context.Context, string, int, string) {}

func (s *resumeFakeStore) CreateResumedRun(_ context.Context, req CreateResumedRunRequest) (CreatedRun, error) {
	if s.createResumedRunFn != nil {
		return s.createResumedRunFn(req)
	}
	return CreatedRun{ID: "new-run-id", RunNumber: 2, RunDisplay: "1.1", CallbackToken: "tok"}, nil
}

func (s *resumeFakeStore) AcquireLease(context.Context, LeaseAcquireRequest) (Lease, error) {
	return s.resumeLeaseResult, s.resumeLeaseErr
}

func (s *resumeFakeStore) AbortRunByID(context.Context, string, string, string) (AbortRunResult, error) {
	return AbortRunResult{}, nil
}

func (s *resumeFakeStore) SubstitutePhaseInputs(phase PhaseSpec, _ map[string]map[string]string) (map[string]string, error) {
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

func newResumeTestHandler(store ReadStore, nativeLauncher NativeLauncher) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/resume", resumeRunHandler(store, nativeLauncher))
	return mux
}

func minimalResumeStore() *resumeFakeStore {
	wf := &Workflow{
		Name: "ci",
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job", WorkflowFilename: "k8s_job:env-prep", Jobs: []NativeJobSpec{{ID: "env-prep"}}},
			{Name: "agent-execute", Kind: "k8s_job", WorkflowFilename: "k8s_job:agent-execute", Jobs: []NativeJobSpec{{ID: "agent"}}},
		},
	}
	leaseNum := 7
	return &resumeFakeStore{
		runIDByNumber: map[string]string{"myproject#10#1": "prior-run-id"},
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
		resumeWorkflows: map[string]*Workflow{"ci": wf},
		resumeLeaseResult: Lease{
			Project:     "myproject",
			LeaseNumber: &leaseNum,
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata:    map[string]any{"native_k8s": true, "lease_callback_token": "lease-token"},
		},
	}
}

func resumeReq(phase string) *http.Request {
	body, _ := json.Marshal(ResumeRunRequest{EntrypointPhase: phase})
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/10/runs/1/resume", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func readResumeResult(t *testing.T, rec *httptest.ResponseRecorder) PublicResumeResult {
	t.Helper()
	var result PublicResumeResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestResumeRunRunNotFound(t *testing.T) {
	store := minimalResumeStore()
	store.runIDByNumber = map[string]string{}
	rec := httptest.NewRecorder()
	newResumeTestHandler(store, nil).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeRunPriorInProgress(t *testing.T) {
	store := minimalResumeStore()
	prior := store.runsByID["prior-run-id"]
	prior.State = "in_progress"
	store.runsByID["prior-run-id"] = prior
	rec := httptest.NewRecorder()
	newResumeTestHandler(store, nil).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeRunWorkflowMissing(t *testing.T) {
	store := minimalResumeStore()
	store.resumeWorkflows = map[string]*Workflow{}
	rec := httptest.NewRecorder()
	newResumeTestHandler(store, nil).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeRunPhaseInvalid(t *testing.T) {
	rec := httptest.NewRecorder()
	newResumeTestHandler(minimalResumeStore(), nil).ServeHTTP(rec, resumeReq("no-such-phase"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeRunRequiresNativeLauncher(t *testing.T) {
	rec := httptest.NewRecorder()
	newResumeTestHandler(minimalResumeStore(), nil).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeRunAlreadyRunning(t *testing.T) {
	store := minimalResumeStore()
	store.claimLockErr = &AlreadyRunningError{HeldBy: "other"}
	rec := httptest.NewRecorder()
	newResumeTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeRunOutputsMissing(t *testing.T) {
	store := minimalResumeStore()
	store.createResumedRunFn = func(CreateResumedRunRequest) (CreatedRun, error) {
		return CreatedRun{}, fmt.Errorf("%w: env-prep has no outputs", ErrOutputsMissing)
	}
	rec := httptest.NewRecorder()
	newResumeTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeRunQueuesWithoutCapacityCheck(t *testing.T) {
	store := minimalResumeStore()
	store.resumeLeaseErr = errors.New("cosmos unavailable")
	rec := httptest.NewRecorder()
	newResumeTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readResumeResult(t, rec).State; got != "queued" {
		t.Fatalf("state=%q", got)
	}
}

func TestResumeRunQueued(t *testing.T) {
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	newResumeTestHandler(minimalResumeStore(), launcher).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := readResumeResult(t, rec)
	if result.State != "queued" {
		t.Fatalf("state=%q", result.State)
	}
	if launcher.called {
		t.Fatalf("resume request must not launch native work directly: %#v", launcher.req)
	}
}

func TestResumeRunLauncherFailureDoesNotFailQueueing(t *testing.T) {
	rec := httptest.NewRecorder()
	newResumeTestHandler(minimalResumeStore(), &fakeNativeLauncher{err: errors.New("kube unavailable")}).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readResumeResult(t, rec).State; got != "queued" {
		t.Fatalf("state=%q", got)
	}
}

func TestResumeRunMissingEntrypointPhase(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/10/runs/1/resume", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	newResumeTestHandler(minimalResumeStore(), nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeRunRejectsNonNativeEntrypoint(t *testing.T) {
	store := minimalResumeStore()
	store.resumeWorkflows["ci"].Phases[1].Kind = "container"
	rec := httptest.NewRecorder()
	newResumeTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeRunSubstitutionFails(t *testing.T) {
	store := minimalResumeStore()
	store.substituteErr = errors.New("missing ref")
	rec := httptest.NewRecorder()
	newResumeTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, resumeReq("agent-execute"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeRunEntriesAtPhaseZero(t *testing.T) {
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	newResumeTestHandler(minimalResumeStore(), launcher).ServeHTTP(rec, resumeReq("env-prep"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readResumeResult(t, rec).State; got != "queued" {
		t.Fatalf("state=%q", got)
	}
}

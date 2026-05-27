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

type fakeDispatchStore struct {
	fakeReadStore

	githubRepo    string
	githubRepoErr error

	issue    *IssueDispatchData
	issueErr error

	wf           *Workflow
	wfErr        error
	workflows    []Workflow
	workflowsErr error

	lockErr      error
	lockReleased bool

	run    *CreatedRun
	runErr error
	runReq *CreateRunRequest

	leaseResult Lease
	leaseErr    error
	leaseReq    *LeaseAcquireRequest
	startReq    *StartRunCycleRequest

	abortReason string
}

type fakeNativeLauncher struct {
	called        bool
	req           NativeLaunchRequest
	err           error
	ctxErrOnEntry error
}

func (l *fakeNativeLauncher) LaunchNativePhase(ctx context.Context, req NativeLaunchRequest) ([]string, error) {
	l.called = true
	l.req = req
	l.ctxErrOnEntry = ctx.Err()
	if l.err != nil {
		return nil, l.err
	}
	return []string{"native-job"}, nil
}

func (s *fakeDispatchStore) ReadProjectGitHubRepo(context.Context, string) (string, error) {
	return s.githubRepo, s.githubRepoErr
}

func (s *fakeDispatchStore) ReadIssueForDispatch(context.Context, string, int) (IssueDispatchData, error) {
	if s.issueErr != nil {
		return IssueDispatchData{}, s.issueErr
	}
	if s.issue == nil {
		return IssueDispatchData{}, ErrNotFound
	}
	return *s.issue, nil
}

func (s *fakeDispatchStore) GetWorkflowByName(context.Context, string, string) (*Workflow, error) {
	return s.wf, s.wfErr
}

func (s *fakeDispatchStore) ListProjectWorkflows(context.Context, string) ([]Workflow, error) {
	return s.workflows, s.workflowsErr
}

func (s *fakeDispatchStore) ClaimIssueLock(context.Context, string, int, string, int) error {
	return s.lockErr
}

func (s *fakeDispatchStore) ReleaseIssueLock(context.Context, string, int, string) {
	s.lockReleased = true
}

func (s *fakeDispatchStore) CreateRun(_ context.Context, req CreateRunRequest) (CreatedRun, error) {
	s.runReq = &req
	if s.runErr != nil {
		return CreatedRun{}, s.runErr
	}
	if s.run != nil {
		return *s.run, nil
	}
	return CreatedRun{ID: "run-1", RunNumber: 1, CycleNumber: 1, RunCycle: 1, RunDisplay: "1.1", CallbackToken: "tok"}, nil
}

func (s *fakeDispatchStore) StartRunCycle(_ context.Context, req StartRunCycleRequest) (int, error) {
	s.startReq = &req
	return 0, nil
}

func (s *fakeDispatchStore) AcquireLease(_ context.Context, req LeaseAcquireRequest) (Lease, error) {
	s.leaseReq = &req
	return s.leaseResult, s.leaseErr
}

func (s *fakeDispatchStore) ReadLeaseByRef(context.Context, string, string) (Lease, error) {
	return s.leaseResult, s.leaseErr
}

func (s *fakeDispatchStore) CancelLeaseByRef(context.Context, string, string) (CancelLeaseResult, error) {
	return CancelLeaseResult{}, nil
}

func (s *fakeDispatchStore) AbortRunByID(context.Context, string, string, string) (AbortRunResult, error) {
	return AbortRunResult{}, nil
}

func newDispatchTestHandler(store ReadStore, nativeLauncher NativeLauncher) http.Handler {
	adminAuthenticator := fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/runs/dispatch", requireAdmin(adminAuthenticator, http.HandlerFunc(dispatchRunHandler(store, nativeLauncher))))
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
				Name:             "prep",
				Kind:             "k8s_job",
				WorkflowFilename: "k8s_job:prep",
				Jobs:             []NativeJobSpec{{ID: "prep", Image: "runner:latest"}},
			},
			{
				Name:             "verify",
				Kind:             "k8s_job",
				WorkflowFilename: "k8s_job:verify",
				DependsOn:        []string{"prep"},
				Verify:           true,
				Jobs:             []NativeJobSpec{{ID: "verify", Image: "runner:latest"}},
			},
			{
				Name:             "cleanup",
				Kind:             "k8s_job",
				WorkflowFilename: "k8s_job:cleanup",
				DependsOn:        []string{"verify"},
				Always:           true,
				Jobs:             []NativeJobSpec{{ID: "cleanup", Image: "runner:latest"}},
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
		workflows: []Workflow{*wf},
		leaseResult: Lease{
			Project:     "proj",
			LeaseNumber: &leaseNum,
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"native_k8s":           true,
				"native_slot_index":    "1",
				"native_slot_name":     "proj-1",
				"lease_callback_token": "lctok",
			},
		},
	}
}

func dispatchRequest(project string, issueNumber int) *http.Request {
	body, _ := json.Marshal(DispatchRunRequest{Project: project, IssueNumber: issueNumber})
	return httptest.NewRequest(http.MethodPost, "/v1/runs/dispatch", bytes.NewReader(body))
}

func readDispatchResult(t *testing.T, rec *httptest.ResponseRecorder) PublicDispatchResult {
	t.Helper()
	var result PublicDispatchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDispatchRunMissingProject(t *testing.T) {
	store := minimalDispatchStore()
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(DispatchRunRequest{IssueNumber: 1})
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/dispatch", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDispatchRunProjectNotFound(t *testing.T) {
	store := minimalDispatchStore()
	store.githubRepoErr = ErrNotFound
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, nil).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "no_project" {
		t.Fatalf("state=%q", got)
	}
}

func TestDispatchRunNoWorkflowRegistered(t *testing.T) {
	store := minimalDispatchStore()
	store.wf = nil
	store.workflows = nil
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, nil).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "no_workflow" {
		t.Fatalf("state=%q", got)
	}
}

func TestDispatchRunAlreadyRunning(t *testing.T) {
	store := minimalDispatchStore()
	store.lockErr = &AlreadyRunningError{HeldBy: "holder-123", ExpiresAt: time.Now().Add(time.Hour)}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "already_running" {
		t.Fatalf("state=%q", got)
	}
}

func TestDispatchRunAlreadyRunningErrIs(t *testing.T) {
	err := &AlreadyRunningError{HeldBy: "x", ExpiresAt: time.Now()}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatal("errors.Is should match ErrAlreadyRunning")
	}
}

func TestDispatchRunRequiresNativeLauncher(t *testing.T) {
	store := minimalDispatchStore()
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, nil).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.runReq != nil || store.leaseReq != nil {
		t.Fatalf("request should fail before creating run or lease")
	}
}

func TestDispatchRunDispatchedNativeK8sJob(t *testing.T) {
	store := minimalDispatchStore()
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, launcher).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := readDispatchResult(t, rec)
	if result.State != "dispatched" {
		t.Fatalf("state=%q", result.State)
	}
	if !launcher.called {
		t.Fatal("native launcher was not called")
	}
	if launcher.req.Phase.Name != "prep" || launcher.req.Run.ID != "run-1" {
		t.Fatalf("launch request=%#v", launcher.req)
	}
	if store.runReq == nil || store.runReq.InitialPhaseKind != "k8s_job" {
		t.Fatalf("run request=%#v", store.runReq)
	}
	if store.runReq.SlotLeaseRef == "" || store.startReq == nil || store.startReq.SlotLeaseRef != store.runReq.SlotLeaseRef {
		t.Fatalf("lease should be attached before run admission: run=%#v start=%#v", store.runReq, store.startReq)
	}
	if store.leaseReq == nil || store.leaseReq.Metadata["native_k8s"] != true {
		t.Fatalf("lease request=%#v", store.leaseReq)
	}
}

func TestDispatchRunSnapshotsVideoEvidenceRequirementFromIssueLabel(t *testing.T) {
	store := minimalDispatchStore()
	store.issue.Labels = []string{"evidence:video"}
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, launcher).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.runReq == nil || len(store.runReq.EvidenceRequirements) != 1 {
		t.Fatalf("run request=%#v", store.runReq)
	}
	if store.runReq.EvidenceRequirements[0].Kind != "video" {
		t.Fatalf("evidence requirements=%#v", store.runReq.EvidenceRequirements)
	}
	if store.leaseReq == nil || store.leaseReq.Metadata["evidence_requirements"] == nil {
		t.Fatalf("lease metadata=%#v", store.leaseReq)
	}
	if len(launcher.req.Run.EvidenceRequirements) != 1 || launcher.req.Run.EvidenceRequirements[0].Kind != "video" {
		t.Fatalf("launch run=%#v", launcher.req.Run)
	}
}

func TestDispatchRunLaunchUsesPostCommitContext(t *testing.T) {
	store := minimalDispatchStore()
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	req := dispatchRequest("proj", 1)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	newDispatchTestHandler(store, launcher).ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !launcher.called {
		t.Fatal("native launcher was not called")
	}
	if launcher.ctxErrOnEntry != nil {
		t.Fatalf("launch context err=%v, want nil", launcher.ctxErrOnEntry)
	}
}

func TestDispatchRunNoCapacity(t *testing.T) {
	store := minimalDispatchStore()
	store.leaseErr = ErrUnavailable
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "no_capacity" {
		t.Fatalf("state=%q", got)
	}
	if store.runReq != nil || store.startReq != nil {
		t.Fatalf("no-capacity dispatch should not create or start a run: run=%#v start=%#v", store.runReq, store.startReq)
	}
	if !store.lockReleased {
		t.Fatal("expected issue lock release after no-capacity dispatch")
	}
}

func TestDispatchRunNativeDispatchFailed(t *testing.T) {
	store := minimalDispatchStore()
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, &fakeNativeLauncher{err: errors.New("kube unavailable")}).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := readDispatchResult(t, rec)
	if result.State != "dispatch_failed" || result.Detail == nil {
		t.Fatalf("result=%#v", result)
	}
}

func TestDispatchRunCreateRunFailReleasesLock(t *testing.T) {
	store := minimalDispatchStore()
	store.runErr = errors.New("store unavailable")
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !store.lockReleased {
		t.Fatal("expected ReleaseIssueLock after CreateRun failure")
	}
}

func TestDispatchRunMultipleWorkflowsRequiresName(t *testing.T) {
	store := minimalDispatchStore()
	phases := store.wf.Phases
	store.wf = nil
	store.workflows = []Workflow{
		{Name: "wf-a", Project: "proj", Phases: phases, Budget: budget.Config{Total: 25}},
		{Name: "wf-b", Project: "proj", Phases: phases, Budget: budget.Config{Total: 25}},
	}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, nil).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "no_workflow" {
		t.Fatalf("state=%q", got)
	}
}

func TestDispatchRunWorkflowAlias(t *testing.T) {
	store := minimalDispatchStore()
	store.workflows = []Workflow{
		{Name: "other", Project: "proj", Phases: store.wf.Phases, Budget: budget.Config{Total: 25}},
		*store.wf,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/dispatch", bytes.NewBufferString(`{"project":"proj","issue_number":1,"workflow":"main"}`))
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "dispatched" {
		t.Fatalf("state=%q", got)
	}
}

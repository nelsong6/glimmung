package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/decision"
)

type fakeCompletionStore struct {
	fakeReadStore

	tokenRunID   string
	tokenProject string
	tokenRef     string
	tokenErr     error

	abortResult AbortRunResult
	abortErr    error

	run     *RunReplayData
	readErr error

	wf    *Workflow
	wfErr error

	stampErr error

	decisionErr error

	terminalResult AbortRunResult
	terminalErr    error
	terminalState  string
	terminalReason *string

	appendIdx   int
	appendErr   error
	appendPhase string
	appendKind  string
	appendFile  string

	leaseResult Lease
	leaseErr    error

	nativeExpectedJobs []string
	nativeCompletions  map[string]CompletionPayload
	nativeErr          error

	recycleReq *CreateRecycleCycleRequest

	issue         IssueDispatchData
	linkPRNumber  int
	linkPRErr     error
	touchpointReq *TouchpointCreate
	touchpointErr error
}

func (s *fakeCompletionStore) ReadRunIDForCallbackToken(context.Context, string) (string, string, string, error) {
	if s.tokenErr != nil {
		return "", "", "", s.tokenErr
	}
	if s.tokenRunID == "" {
		return "", "", "", ErrNotFound
	}
	return s.tokenRunID, s.tokenProject, s.tokenRef, nil
}

func (s *fakeCompletionStore) ReadRunIDForNumber(_ context.Context, project string, _ int, _ string) (string, string, error) {
	if s.tokenErr != nil {
		return "", "", s.tokenErr
	}
	if s.tokenRunID == "" {
		return "", "", ErrNotFound
	}
	if s.tokenProject != "" && s.tokenProject != project {
		return "", "", ErrNotFound
	}
	return s.tokenRunID, firstNonEmpty(s.tokenRef, "proj#7/runs/1"), nil
}

func (s *fakeCompletionStore) AbortRunByID(context.Context, string, string, string) (AbortRunResult, error) {
	return s.abortResult, s.abortErr
}

func (s *fakeCompletionStore) ReadRunForReplay(context.Context, string, string) (RunReplayData, error) {
	if s.readErr != nil {
		return RunReplayData{}, s.readErr
	}
	if s.run == nil {
		return RunReplayData{}, ErrNotFound
	}
	return *s.run, nil
}

func (s *fakeCompletionStore) GetWorkflowByName(context.Context, string, string) (*Workflow, error) {
	return s.wf, s.wfErr
}

func (s *fakeCompletionStore) GetWorkflowBySchemaRef(context.Context, string, string) (*Workflow, error) {
	return s.wf, s.wfErr
}

func (s *fakeCompletionStore) StampRunCompletion(_ context.Context, _, _ string, p CompletionPayload) (RunReplayData, error) {
	if s.stampErr != nil {
		return RunReplayData{}, s.stampErr
	}
	if s.run == nil {
		return RunReplayData{}, ErrNotFound
	}
	copy := *s.run
	copy.Attempts = append([]RunAttemptData{}, s.run.Attempts...)
	if len(copy.Attempts) > 0 {
		last := copy.Attempts[len(copy.Attempts)-1]
		last.Conclusion = p.Conclusion
		last.Completed = true
		if p.PhaseOutputs != nil {
			last.PhaseOutputs = p.PhaseOutputs
		}
		if p.VerificationStatus != "" {
			last.Verification = &RunVerificationData{Status: p.VerificationStatus, Reasons: p.VerificationReasons}
		} else {
			last.Verification = nil
		}
		copy.Attempts[len(copy.Attempts)-1] = last
	}
	return copy, nil
}

func (s *fakeCompletionStore) StampRunDecision(context.Context, string, string, string) error {
	return s.decisionErr
}

func (s *fakeCompletionStore) SetRunTerminalState(_ context.Context, _, _ string, state string, abortReason *string) (AbortRunResult, error) {
	s.terminalState = state
	s.terminalReason = abortReason
	return s.terminalResult, s.terminalErr
}

func (s *fakeCompletionStore) AppendRunAttempt(_ context.Context, _, _, phase, phaseKind, workflowFilename string) (int, error) {
	s.appendPhase = phase
	s.appendKind = phaseKind
	s.appendFile = workflowFilename
	return s.appendIdx, s.appendErr
}

func (s *fakeCompletionStore) CreateRecycleCycle(_ context.Context, req CreateRecycleCycleRequest) (CreatedRun, error) {
	s.recycleReq = &req
	return CreatedRun{
		ID:                   "recycle-run",
		RunNumber:            1,
		CycleNumber:          2,
		RunCycle:             2,
		RunDisplay:           "1.2",
		CallbackToken:        "tok2",
		CarryForwardAttempts: req.CarryForwardAttempts,
	}, nil
}

func (s *fakeCompletionStore) StartRunCycle(_ context.Context, req StartRunCycleRequest) (int, error) {
	s.appendPhase = req.PhaseName
	s.appendKind = req.PhaseKind
	s.appendFile = req.WorkflowFilename
	return s.appendIdx, s.appendErr
}

func (s *fakeCompletionStore) ReadLeaseByRef(context.Context, string, string) (Lease, error) {
	return s.leaseResult, s.leaseErr
}

func (s *fakeCompletionStore) ListProjectRuns(context.Context, string, int) ([]RunReport, error) {
	return nil, nil
}

func (s *fakeCompletionStore) CancelLeaseByRef(context.Context, string, string) (CancelLeaseResult, error) {
	return CancelLeaseResult{}, nil
}

func (s *fakeCompletionStore) RecordNativeJobCompletion(_ context.Context, _, _ string, p CompletionPayload) (NativeJobCompletionResult, error) {
	if s.nativeErr != nil {
		return NativeJobCompletionResult{}, s.nativeErr
	}
	if s.run == nil {
		return NativeJobCompletionResult{}, ErrNotFound
	}
	jobID := ""
	if p.JobID != nil {
		jobID = *p.JobID
	}
	if jobID == "" {
		return NativeJobCompletionResult{}, ValidationError{Message: "job_id required"}
	}
	expected := append([]string{}, s.nativeExpectedJobs...)
	if len(expected) == 0 {
		expected = append(expected, jobID)
	}
	if !containsTestString(expected, jobID) {
		return NativeJobCompletionResult{}, ValidationError{Message: "unknown job"}
	}
	if s.nativeCompletions == nil {
		s.nativeCompletions = map[string]CompletionPayload{}
	}
	_, existed := s.nativeCompletions[jobID]
	s.nativeCompletions[jobID] = p

	completed := make([]string, 0, len(expected))
	pending := make([]string, 0)
	failed := make([]string, 0)
	phaseComplete := true
	for _, id := range expected {
		completion, ok := s.nativeCompletions[id]
		if !ok {
			phaseComplete = false
			pending = append(pending, id)
			continue
		}
		completed = append(completed, id)
		if completion.Conclusion != "success" {
			failed = append(failed, id)
		}
	}
	return NativeJobCompletionResult{
		Run:             *s.run,
		PhaseComplete:   phaseComplete,
		CompletionReady: phaseComplete && !existed,
		CompletedJobIDs: completed,
		PendingJobIDs:   pending,
		FailedJobIDs:    failed,
		PhasePayload:    aggregateFakeNativePayload(expected, s.nativeCompletions),
	}, nil
}

func (s *fakeCompletionStore) ReadIssueForDispatch(context.Context, string, int) (IssueDispatchData, error) {
	if s.issue.ID == "" && s.issue.Title == "" {
		return IssueDispatchData{ID: "issue-7", Title: "Fix thing", Body: "body"}, nil
	}
	return s.issue, nil
}

func (s *fakeCompletionStore) LinkRunPullRequest(_ context.Context, _, _ string, prNumber int) error {
	s.linkPRNumber = prNumber
	if s.run != nil {
		s.run.PRNumber = &prNumber
	}
	return s.linkPRErr
}

func (s *fakeCompletionStore) EnsureTouchpoint(_ context.Context, req TouchpointCreate) (TouchpointDetail, error) {
	s.touchpointReq = &req
	if s.touchpointErr != nil {
		return TouchpointDetail{}, s.touchpointErr
	}
	return TouchpointDetail{
		Ref:      req.Repo + "#" + strconv.Itoa(req.Number),
		Project:  req.Project,
		Repo:     req.Repo,
		PRNumber: req.Number,
		Title:    req.Title,
		State:    "ready",
		Evidence: req.Evidence,
	}, nil
}

type fakePullRequestClient struct {
	req PullRequestEnsureRequest
	pr  PullRequest
	err error
}

func (c *fakePullRequestClient) EnsurePullRequest(_ context.Context, req PullRequestEnsureRequest) (PullRequest, error) {
	c.req = req
	if c.err != nil {
		return PullRequest{}, c.err
	}
	if c.pr.Number == 0 {
		c.pr = PullRequest{
			Number:  123,
			Title:   req.Title,
			Body:    req.Body,
			Branch:  req.Head,
			BaseRef: req.Base,
			HeadSHA: "abc123",
			HTMLURL: "https://github.com/" + req.Repo + "/pull/123",
			State:   "open",
		}
	}
	return c.pr, nil
}

func (c *fakePullRequestClient) FetchWorkflowFile(context.Context, string, string, string) ([]byte, int, error) {
	return nil, 0, ErrNotFound
}

func aggregateFakeNativePayload(expected []string, completions map[string]CompletionPayload) CompletionPayload {
	payload := CompletionPayload{Conclusion: "success", PhaseOutputs: map[string]string{}}
	for _, id := range expected {
		completion, ok := completions[id]
		if !ok {
			continue
		}
		if completion.Conclusion != "success" && payload.Conclusion == "success" {
			payload.Conclusion = completion.Conclusion
		}
		if completion.VerificationStatus != "" {
			payload.VerificationStatus = completion.VerificationStatus
			payload.VerificationReasons = append(payload.VerificationReasons, completion.VerificationReasons...)
			payload.EvidenceRefs = append(payload.EvidenceRefs, completion.EvidenceRefs...)
		}
		payload.CostUSD += completion.CostUSD
		for key, value := range completion.PhaseOutputs {
			payload.PhaseOutputs[key] = value
		}
	}
	return payload
}

func TestCompletionPayloadFromNativePrefersPositiveVerificationCost(t *testing.T) {
	jobID := "verify"
	payload := completionPayloadFromNative(NativeRunCompletedRequest{
		JobID:      &jobID,
		Conclusion: "success",
		CostUSD:    2.5,
		Verification: map[string]any{
			"status":   "pass",
			"cost_usd": 3.75,
		},
	})
	if payload.CostUSD != 3.75 {
		t.Fatalf("cost=%v", payload.CostUSD)
	}
}

func TestCompletionPayloadFromNativeKeepsObservedCostWhenVerificationCostIsZero(t *testing.T) {
	jobID := "verify"
	payload := completionPayloadFromNative(NativeRunCompletedRequest{
		JobID:      &jobID,
		Conclusion: "success",
		CostUSD:    2.5,
		Verification: map[string]any{
			"status":   "pass",
			"cost_usd": 0.0,
		},
	})
	if payload.CostUSD != 2.5 {
		t.Fatalf("cost=%v", payload.CostUSD)
	}
}

func containsTestString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func newCompletionHandler(store *fakeCompletionStore, nativeLauncher NativeLauncher) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/completed", nativeRunCompletedByCallbackToken(store, nativeLauncher))
	return mux
}

func newPRTouchpointHandler(store *fakeCompletionStore, prClient PullRequestClient) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/pr-touchpoint", nativePRTouchpointByCallbackToken(store, prClient, nil))
	return mux
}

func singlePhaseWorkflowForCompletion(name string, verify bool) *Workflow {
	return &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{{
			Name:          name,
			Kind:          "k8s_job",
			Jobs:          []NativeJobSpec{{ID: name, Image: "runner:latest"}},
			Verify:        verify,
			RecyclePolicy: &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}},
		}},
	}
}

func prWorkflowForCompletion(name string) *Workflow {
	wf := singlePhaseWorkflowForCompletion(name, false)
	wf.PR.Enabled = true
	wf.Phases = append(wf.Phases, PhaseSpec{
		Name:      "cleanup",
		Kind:      "k8s_job",
		Always:    true,
		DependsOn: []string{name},
		Jobs:      []NativeJobSpec{{ID: PRTouchpointJobID, Primitive: JobPrimitivePRTouchpoint}},
	})
	canonical := CanonicalWorkflow(*wf)
	return &canonical
}

func runDataForCompletion(phase string) *RunReplayData {
	callback := "run-token"
	leaseRef := "proj/leases/proj-1/1"
	runNumber := 1
	runDisplay := "1"
	return &RunReplayData{
		ID:               "run-1",
		Project:          "proj",
		WorkflowName:     "wf",
		IssueNumber:      7,
		IssueRepo:        "owner/repo",
		RunNumber:        &runNumber,
		RunDisplayNumber: &runDisplay,
		CallbackToken:    &callback,
		SlotLeaseRef:     &leaseRef,
		Attempts: []RunAttemptData{
			{AttemptIndex: 0, Phase: phase, Conclusion: "failure"},
		},
		CumulativeCostUSD: 0.1,
	}
}

func nativeCompletionRequest(token string, body NativeRunCompletedRequest) *http.Request {
	data, _ := json.Marshal(body)
	return httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/"+token+"/native/completed", bytes.NewReader(data))
}

func completedJob(id, conclusion string, verification map[string]any, outputs map[string]string) NativeRunCompletedRequest {
	return NativeRunCompletedRequest{
		JobID:        &id,
		Conclusion:   conclusion,
		Verification: verification,
		Outputs:      outputs,
	}
}

func assertPhaseTargets(t *testing.T, phases []PhaseSpec, want ...string) {
	t.Helper()
	got := make([]string, 0, len(phases))
	for _, phase := range phases {
		got = append(got, phase.Name)
	}
	if len(got) != len(want) {
		t.Fatalf("targets=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets=%v, want %v", got, want)
		}
	}
}

func readCallbackResult(t *testing.T, rec *httptest.ResponseRecorder) RunCallbackResult {
	t.Helper()
	var result RunCallbackResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNativeRunCompletedByCallbackTokenTokenNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	newCompletionHandler(&fakeCompletionStore{}, nil).ServeHTTP(rec, nativeCompletionRequest("badtoken", completedJob("impl", "success", nil, nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNativeRunCompletedByCallbackTokenMissingJobID(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.wf = singlePhaseWorkflowForCompletion("impl", false)
	rec := httptest.NewRecorder()
	newCompletionHandler(store, nil).ServeHTTP(rec, nativeCompletionRequest("tok", NativeRunCompletedRequest{Conclusion: "success"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNativeRunCompletedByCallbackTokenAdvancePassed(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.wf = singlePhaseWorkflowForCompletion("impl", false)
	store.terminalResult = AbortRunResult{State: "passed", RunRef: "proj#7/runs/1"}
	rec := httptest.NewRecorder()
	newCompletionHandler(store, nil).ServeHTTP(rec, nativeCompletionRequest("tok", completedJob("impl", "success", nil, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := readCallbackResult(t, rec)
	if result.Decision == nil || *result.Decision != "advance" {
		t.Fatalf("decision=%v", result.Decision)
	}
	if result.PhaseComplete == nil || !*result.PhaseComplete {
		t.Fatalf("phase_complete=%v", result.PhaseComplete)
	}
}

func TestNativeRunCompletedByCallbackTokenAdvanceReviewRequired(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("cleanup")
	store.run.Attempts = []RunAttemptData{
		{AttemptIndex: 0, Phase: "impl", Conclusion: "success", Decision: string(decision.Advance), Completed: true, PhaseOutputs: map[string]string{"branch_name": "issue-7-run-1"}},
		{AttemptIndex: 1, Phase: "cleanup", Conclusion: "failure"},
	}
	prNumber := 123
	store.run.PRNumber = &prNumber
	store.wf = prWorkflowForCompletion("impl")
	store.terminalResult = AbortRunResult{State: "review_required", RunRef: "proj#7/runs/1"}
	rec := httptest.NewRecorder()
	newCompletionHandler(store, nil).ServeHTTP(rec, nativeCompletionRequest("tok", completedJob(PRTouchpointJobID, "success", nil, map[string]string{"pr_number": "123"})))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readCallbackResult(t, rec).Decision; got == nil || *got != "advance" {
		t.Fatalf("decision=%v", got)
	}
	if store.terminalState != "review_required" {
		t.Fatalf("terminal state=%q, want review_required", store.terminalState)
	}
}

func TestCompletionPayloadFromNativeExtractsEvidenceRefs(t *testing.T) {
	id := "verify"
	req := NativeRunCompletedRequest{
		JobID:      &id,
		Conclusion: "success",
		Verification: map[string]any{
			"status":        "pass",
			"reasons":       []any{"screenshots ok"},
			"evidence_refs": []any{"screenshots/default.png", "", 42},
			"cost_usd":      1.25,
		},
	}

	payload := completionPayloadFromNative(req)

	if payload.VerificationStatus != "pass" || len(payload.VerificationReasons) != 1 {
		t.Fatalf("verification=%#v", payload)
	}
	if len(payload.EvidenceRefs) != 1 || payload.EvidenceRefs[0] != "screenshots/default.png" {
		t.Fatalf("evidence_refs=%#v", payload.EvidenceRefs)
	}
}

func TestNativeRunCompletedByCallbackTokenMissingPRPrimitiveLinkAborts(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("cleanup")
	store.run.Attempts = []RunAttemptData{
		{AttemptIndex: 0, Phase: "impl", Conclusion: "success", Decision: string(decision.Advance), Completed: true, PhaseOutputs: map[string]string{"branch_name": "issue-7-run-1"}},
		{AttemptIndex: 1, Phase: "cleanup", Conclusion: "failure"},
	}
	store.wf = prWorkflowForCompletion("impl")
	store.terminalResult = AbortRunResult{State: "aborted", RunRef: "proj#7/runs/1"}

	rec := httptest.NewRecorder()
	newCompletionHandler(store, nil).ServeHTTP(rec, nativeCompletionRequest("tok", completedJob(PRTouchpointJobID, "success", nil, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.terminalState != "aborted" {
		t.Fatalf("terminal state=%q, want aborted", store.terminalState)
	}
	if store.terminalReason == nil || !strings.Contains(*store.terminalReason, "PR primitive: touchpoint job completed without linking a PR") {
		t.Fatalf("terminal reason=%v", store.terminalReason)
	}
}

func TestNativePRTouchpointByCallbackTokenEnsuresPRAndTouchpoint(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.run.Attempts[0].Completed = true
	store.run.Attempts[0].Conclusion = "success"
	store.run.Attempts[0].Decision = string(decision.Advance)
	store.run.Attempts[0].PhaseOutputs = map[string]string{"branch_name": "issue-7-run-1"}
	store.wf = prWorkflowForCompletion("impl")
	prClient := &fakePullRequestClient{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/tok/native/pr-touchpoint", nil)
	newPRTouchpointHandler(store, prClient).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result PRPrimitiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ensured" || result.PRNumber != 123 || result.TouchpointRef != "owner/repo#123" {
		t.Fatalf("result=%#v", result)
	}
	if prClient.req.Repo != "owner/repo" || prClient.req.Head != "issue-7-run-1" || prClient.req.Base != "main" {
		t.Fatalf("pr request=%#v", prClient.req)
	}
	if store.linkPRNumber != 123 {
		t.Fatalf("linked pr=%d, want 123", store.linkPRNumber)
	}
	if store.touchpointReq == nil || store.touchpointReq.Number != 123 || store.touchpointReq.LinkedIssueRef != "proj#7" || store.touchpointReq.LinkedRunRef != "proj#7/runs/1" {
		t.Fatalf("touchpoint req=%#v", store.touchpointReq)
	}
}

func TestNativePRTouchpointByCallbackTokenSkipsAbortPath(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.run.Attempts[0].Completed = true
	store.run.Attempts[0].Decision = string(decision.AbortMalformed)
	store.wf = prWorkflowForCompletion("impl")
	prClient := &fakePullRequestClient{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/tok/native/pr-touchpoint", nil)
	newPRTouchpointHandler(store, prClient).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result PRPrimitiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "skipped" || !strings.Contains(result.Reason, "abort") {
		t.Fatalf("result=%#v", result)
	}
	if prClient.req.Repo != "" || store.touchpointReq != nil {
		t.Fatalf("unexpected PR materialization req=%#v touchpoint=%#v", prClient.req, store.touchpointReq)
	}
}

func TestFinalizeRunTouchpointByNumberEnsuresPRAndTouchpoint(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "run-1", tokenProject: "proj", tokenRef: "proj#7/runs/1"}
	store.run = runDataForCompletion("impl")
	store.run.Attempts[0].Completed = true
	store.run.Attempts[0].Conclusion = "success"
	store.run.Attempts[0].Decision = string(decision.Advance)
	store.run.Attempts[0].PhaseOutputs = map[string]string{"branch_name": "issue-7-run-1"}
	store.wf = prWorkflowForCompletion("impl")
	prClient := &fakePullRequestClient{}
	handler := NewWithRuntimeClients(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, prClient, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj/issues/7/runs/1/touchpoint/finalize", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result PRPrimitiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ensured" || result.PRNumber != 123 || result.TouchpointRef != "owner/repo#123" {
		t.Fatalf("result=%#v", result)
	}
	if store.linkPRNumber != 123 {
		t.Fatalf("linked pr=%d, want 123", store.linkPRNumber)
	}
	if store.touchpointReq == nil || store.touchpointReq.LinkedIssueRef != "proj#7" || store.touchpointReq.LinkedRunRef != "proj#7/runs/1" {
		t.Fatalf("touchpoint req=%#v", store.touchpointReq)
	}
}

func TestFinalizeRunTouchpointByNumberPersistsStructuredScreenshotEvidence(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "run-1", tokenProject: "proj", tokenRef: "proj#7/runs/1"}
	store.run = runDataForCompletion("verify")
	store.run.ScreenshotsMarkdown = stringPtr("![old](https://example.test/old.png)")
	store.run.Attempts = []RunAttemptData{
		{
			AttemptIndex: 0,
			Phase:        "plan",
			Completed:    true,
			Decision:     string(decision.Advance),
			PhaseOutputs: map[string]string{
				"test_plan": `{"required_evidence":[{"id":"default","kind":"screenshot","url_path":"/dev/demo","must_show":"default render"}]}`,
			},
		},
		{
			AttemptIndex: 1,
			Phase:        "verify",
			Completed:    true,
			Conclusion:   "success",
			Decision:     string(decision.Advance),
			PhaseOutputs: map[string]string{
				"branch_name":  "issue-7-run-1",
				"verification": `{"status":"pass","evidence_refs":["screenshots/default.png"]}`,
			},
		},
	}
	store.wf = prWorkflowForCompletion("verify")
	prClient := &fakePullRequestClient{}
	artifacts := &fakeArtifactStore{artifact: Artifact{Body: []byte("png"), ContentType: "image/png"}}
	handler := NewWithRuntimeClients(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, prClient, nil, artifacts)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj/issues/7/runs/1/touchpoint/finalize", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.touchpointReq == nil || len(store.touchpointReq.Evidence) != 1 {
		t.Fatalf("touchpoint evidence=%#v", store.touchpointReq)
	}
	ev := store.touchpointReq.Evidence[0]
	if ev.Kind != "screenshot" || ev.ArtifactPath != "runs/proj/run-1/screenshots/default.png" {
		t.Fatalf("evidence=%#v", ev)
	}
	if ev.URL != "/v1/artifacts/runs/proj/run-1/screenshots/default.png" || ev.Ref != "blob://artifacts/runs/proj/run-1/screenshots/default.png" {
		t.Fatalf("evidence URLs=%#v", ev)
	}
	if ev.SourceAttemptIndex == nil || *ev.SourceAttemptIndex != 1 || ev.SourcePhase != "verify" {
		t.Fatalf("evidence source=%#v", ev)
	}
	if len(artifacts.downloads) != 1 || artifacts.downloads[0] != "runs/proj/run-1/screenshots/default.png" {
		t.Fatalf("artifact downloads=%#v", artifacts.downloads)
	}
	if strings.Contains(prClient.req.Body, "![") {
		t.Fatalf("PR body should not include image markdown: %s", prClient.req.Body)
	}
}

func TestFinalizeRunTouchpointByNumberRejectsMissingRequiredScreenshotArtifact(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "run-1", tokenProject: "proj", tokenRef: "proj#7/runs/1"}
	store.run = runDataForCompletion("verify")
	store.run.Attempts = []RunAttemptData{
		{
			AttemptIndex: 0,
			Phase:        "plan",
			Completed:    true,
			Decision:     string(decision.Advance),
			PhaseOutputs: map[string]string{
				"test_plan": `{"required_evidence":[{"id":"default","kind":"screenshot","url_path":"/dev/demo","must_show":"default render"}]}`,
			},
		},
		{
			AttemptIndex: 1,
			Phase:        "verify",
			Completed:    true,
			Conclusion:   "success",
			Decision:     string(decision.Advance),
			PhaseOutputs: map[string]string{
				"branch_name":  "issue-7-run-1",
				"verification": `{"status":"pass","evidence_refs":["screenshots/default.png"]}`,
			},
		},
	}
	store.wf = prWorkflowForCompletion("verify")
	prClient := &fakePullRequestClient{}
	handler := NewWithRuntimeClients(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, prClient, nil, &fakeArtifactStore{err: ErrArtifactNotFound})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj/issues/7/runs/1/touchpoint/finalize", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "screenshot artifact not found") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if prClient.req.Repo != "" || store.touchpointReq != nil {
		t.Fatalf("unexpected side effects pr=%#v touchpoint=%#v", prClient.req, store.touchpointReq)
	}
}

func TestFinalizeRunTouchpointByNumberRejectsAbortPath(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "run-1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.run.Attempts[0].Completed = true
	store.run.Attempts[0].Decision = string(decision.AbortMalformed)
	store.wf = prWorkflowForCompletion("impl")
	prClient := &fakePullRequestClient{}
	handler := NewWithRuntimeClients(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, prClient, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj/issues/7/runs/1/touchpoint/finalize", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if prClient.req.Repo != "" || store.touchpointReq != nil {
		t.Fatalf("unexpected PR materialization req=%#v touchpoint=%#v", prClient.req, store.touchpointReq)
	}
}

func TestFinalizeRunTouchpointByNumberRequiresBranchOutput(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "run-1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.run.RunNumber = nil
	store.run.RunDisplayNumber = nil
	store.run.Attempts[0].Completed = true
	store.run.Attempts[0].Conclusion = "success"
	store.run.Attempts[0].Decision = string(decision.Advance)
	store.wf = prWorkflowForCompletion("impl")
	prClient := &fakePullRequestClient{}
	handler := NewWithRuntimeClients(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, prClient, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj/issues/7/runs/1/touchpoint/finalize", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "branch_name") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestFinalizeRunTouchpointByNumberRequiresAdmin(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "run-1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.wf = prWorkflowForCompletion("impl")
	handler := NewWithRuntimeClients(Settings{}, store, nil, &fakePullRequestClient{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj/issues/7/runs/1/touchpoint/finalize", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 without admin auth, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNativeRunCompletedByCallbackTokenAdvanceDispatchesNextPhase(t *testing.T) {
	leaseNumber := 12
	store := &fakeCompletionStore{
		tokenRunID:   "r1",
		tokenProject: "proj",
		appendIdx:    1,
		leaseResult: Lease{
			Project:     "proj",
			LeaseNumber: &leaseNumber,
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata:    map[string]any{"lease_callback_token": "lease-token", "native_k8s": true},
		},
	}
	leaseRef := "proj/leases/proj-1/12"
	store.run = &RunReplayData{
		ID:           "r1",
		Project:      "proj",
		WorkflowName: "wf",
		IssueNumber:  7,
		IssueRepo:    "owner/repo",
		SlotLeaseRef: &leaseRef,
		Attempts:     []RunAttemptData{{AttemptIndex: 0, Phase: "env-prep", Conclusion: "failure"}},
	}
	store.wf = &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "env-prep"}}, Outputs: []string{"validation_url"}},
			{
				Name:             "agent-execute",
				Kind:             "k8s_job",
				WorkflowFilename: "k8s_job:agent-execute",
				DependsOn:        []string{"env-prep"},
				Jobs:             []NativeJobSpec{{ID: "agent", Image: "runner:latest"}},
				Inputs: map[string]string{
					"validation_url": "${{ phases.env-prep.outputs.validation_url }}",
				},
			},
		},
	}
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	newCompletionHandler(store, launcher).ServeHTTP(rec, nativeCompletionRequest("tok", completedJob("env-prep", "success", nil, map[string]string{"validation_url": "https://preview.example"})))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := readCallbackResult(t, rec)
	if result.Decision == nil || *result.Decision != "advance_phase" {
		t.Fatalf("decision=%v", result.Decision)
	}
	if store.appendPhase != "agent-execute" || store.appendKind != "k8s_job" || store.appendFile != "k8s_job:agent-execute" {
		t.Fatalf("append=(%q,%q,%q)", store.appendPhase, store.appendKind, store.appendFile)
	}
	if !launcher.called || launcher.req.Phase.Name != "agent-execute" {
		t.Fatalf("native launch=%#v", launcher.req)
	}
	phaseInputs, ok := launcher.req.Lease.Metadata["phase_inputs"].(map[string]string)
	if !ok || phaseInputs["validation_url"] != "https://preview.example" {
		t.Fatalf("phase_inputs=%#v", launcher.req.Lease.Metadata["phase_inputs"])
	}
	if launcher.req.Lease.Metadata["native_k8s"] != true {
		t.Fatalf("lease metadata=%#v", launcher.req.Lease.Metadata)
	}
}

func TestNativeRunCompletedByCallbackTokenFailureDispatchesCleanup(t *testing.T) {
	leaseRef := "proj/leases/proj-1/1"
	store := &fakeCompletionStore{
		tokenRunID:   "r1",
		tokenProject: "proj",
		appendIdx:    1,
		leaseResult:  Lease{Project: "proj", LeaseNumber: intPtr(1), State: "claimed", Metadata: map[string]any{}},
	}
	store.run = &RunReplayData{
		ID:           "r1",
		Project:      "proj",
		WorkflowName: "wf",
		IssueNumber:  7,
		IssueRepo:    "owner/repo",
		SlotLeaseRef: &leaseRef,
		Attempts:     []RunAttemptData{{AttemptIndex: 0, Phase: "env-prep"}},
	}
	store.wf = &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "env-prep"}}},
			{
				Name:             "env-destroy",
				Kind:             "k8s_job",
				WorkflowFilename: "k8s_job:env-destroy",
				Always:           true,
				DependsOn:        []string{"env-prep"},
				Jobs:             []NativeJobSpec{{ID: "env-destroy", Image: "runner:latest"}},
			},
		},
	}
	launcher := &fakeNativeLauncher{}
	req := completedJob("env-prep", "failure", nil, nil)
	summary := "contract failure"
	req.SummaryMarkdown = &summary
	rec := httptest.NewRecorder()
	newCompletionHandler(store, launcher).ServeHTTP(rec, nativeCompletionRequest("tok", req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := readCallbackResult(t, rec)
	if result.Decision == nil || *result.Decision != "advance_phase" {
		t.Fatalf("decision=%v", result.Decision)
	}
	if len(result.FailedJobIDs) != 1 || result.FailedJobIDs[0] != "env-prep" {
		t.Fatalf("failed jobs=%v", result.FailedJobIDs)
	}
	if store.appendPhase != "env-destroy" || store.appendKind != "k8s_job" || store.appendFile != "k8s_job:env-destroy" {
		t.Fatalf("append=(%q,%q,%q)", store.appendPhase, store.appendKind, store.appendFile)
	}
	if !launcher.called || launcher.req.Phase.Name != "env-destroy" {
		t.Fatalf("native launch=%#v", launcher.req)
	}
}

func TestNativeRunCompletedByCallbackTokenCleanupAfterAbortKeepsRunAborted(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = &RunReplayData{
		ID:           "r1",
		Project:      "proj",
		WorkflowName: "wf",
		IssueNumber:  7,
		IssueRepo:    "owner/repo",
		Attempts: []RunAttemptData{
			{
				AttemptIndex: 0,
				Phase:        "env-prep",
				Conclusion:   "failure",
				Decision:     string(decision.AbortMalformed),
				Completed:    true,
			},
			{AttemptIndex: 1, Phase: "env-destroy"},
		},
	}
	store.wf = &Workflow{
		Project: "proj",
		Name:    "wf",
		PR:      PrPrimitive{Enabled: true},
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "env-prep"}}},
			{
				Name:      "env-destroy",
				Kind:      "k8s_job",
				Always:    true,
				DependsOn: []string{"env-prep"},
				Jobs:      []NativeJobSpec{{ID: "env-destroy"}},
			},
		},
	}
	store.terminalResult = AbortRunResult{State: "aborted", RunRef: "proj#7/runs/1"}

	rec := httptest.NewRecorder()
	newCompletionHandler(store, nil).ServeHTTP(rec, nativeCompletionRequest("tok", completedJob("env-destroy", "success", nil, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readCallbackResult(t, rec).Decision; got == nil || *got != "advance" {
		t.Fatalf("decision=%v", got)
	}
	if store.terminalState != "aborted" {
		t.Fatalf("terminal state=%q, want aborted", store.terminalState)
	}
	if store.terminalReason == nil || !strings.Contains(*store.terminalReason, "verification.json") {
		t.Fatalf("terminal reason=%v", store.terminalReason)
	}
}

func TestAllReadyDispatchTargetsHandlesLinearPhasesAndTeardown(t *testing.T) {
	wf := &Workflow{Phases: []PhaseSpec{
		{Name: "prepare"},
		{Name: "work", DependsOn: []string{"prepare"}},
		{Name: "verify", Verify: true, DependsOn: []string{"work"}},
		{Name: "cleanup", Always: true, DependsOn: []string{"verify"}},
	}}
	run := RunReplayData{Attempts: []RunAttemptData{{AttemptIndex: 0, Phase: "prepare", Completed: true, Decision: string(decision.Advance)}}}
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.Advance), "work")

	run.Attempts = append(run.Attempts, RunAttemptData{AttemptIndex: 1, Phase: "work", Completed: true, Decision: string(decision.Advance)})
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.Advance), "verify")

	run.Attempts = append(run.Attempts, RunAttemptData{AttemptIndex: 2, Phase: "verify", Completed: true, Decision: string(decision.AbortBudgetAttempts)})
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.AbortBudgetAttempts), "cleanup")
}

func TestAllReadyDispatchTargetsUsesPhaseOrderNotDependencyDepth(t *testing.T) {
	wf := &Workflow{Phases: []PhaseSpec{
		{Name: "prepare"},
		{Name: "plan", DependsOn: []string{"prepare"}},
		{Name: "implement", DependsOn: []string{"prepare"}},
		{Name: "verify", Verify: true, DependsOn: []string{"plan", "implement"}},
		{Name: "cleanup", Always: true, DependsOn: []string{"verify"}},
	}}
	run := RunReplayData{Attempts: []RunAttemptData{{AttemptIndex: 0, Phase: "prepare", Completed: true, Decision: string(decision.Advance)}}}

	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.Advance), "plan")

	run.Attempts = append(run.Attempts, RunAttemptData{AttemptIndex: 1, Phase: "plan", Completed: true, Decision: string(decision.Advance)})
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.Advance), "implement")
}

func TestNativeRunCompletedByCallbackTokenAbortBudgetAttempts(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = &RunReplayData{
		ID: "r1", Project: "proj", WorkflowName: "wf", IssueNumber: 7,
		Attempts: []RunAttemptData{
			{Phase: "impl", Conclusion: "failure"},
			{Phase: "impl", Conclusion: "failure"},
			{Phase: "impl", Conclusion: "failure"},
		},
		CumulativeCostUSD: 1.0,
	}
	store.wf = singlePhaseWorkflowForCompletion("impl", true)
	store.terminalResult = AbortRunResult{State: "aborted", RunRef: "proj#7/runs/1"}
	rec := httptest.NewRecorder()
	newCompletionHandler(store, nil).ServeHTTP(rec, nativeCompletionRequest("tok", completedJob("impl", "failure", map[string]any{"status": "fail"}, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readCallbackResult(t, rec).Decision; got == nil || *got != "abort_budget_attempts" {
		t.Fatalf("decision=%v", got)
	}
}

func TestNativeRunCompletedByCallbackTokenRetryRequiresNativeLauncher(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.wf = singlePhaseWorkflowForCompletion("impl", true)
	store.terminalResult = AbortRunResult{State: "aborted", RunRef: "proj#7/runs/1"}
	rec := httptest.NewRecorder()
	newCompletionHandler(store, nil).ServeHTTP(rec, nativeCompletionRequest("tok", completedJob("impl", "failure", map[string]any{"status": "fail"}, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readCallbackResult(t, rec).Decision; got == nil || *got != "abort_malformed" {
		t.Fatalf("decision=%v", got)
	}
}

func TestNativeRunCompletedByCallbackTokenCycleOrdinalCountsRecycleAttempts(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.run.RunCycleNumber = intPtr(3)
	store.wf = singlePhaseWorkflowForCompletion("impl", true)
	store.terminalResult = AbortRunResult{State: "aborted", RunRef: "proj#7/runs/1.3"}
	rec := httptest.NewRecorder()
	newCompletionHandler(store, nil).ServeHTTP(rec, nativeCompletionRequest("tok", completedJob("impl", "failure", map[string]any{"status": "fail"}, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readCallbackResult(t, rec).Decision; got == nil || *got != "abort_budget_attempts" {
		t.Fatalf("decision=%v", got)
	}
}

func TestNativeRunCompletedByCallbackTokenStampError(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj", stampErr: errors.New("store unavailable")}
	store.run = runDataForCompletion("impl")
	rec := httptest.NewRecorder()
	newCompletionHandler(store, nil).ServeHTTP(rec, nativeCompletionRequest("tok", completedJob("impl", "success", nil, nil)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNativeRunCompletedByCallbackTokenWaitsForSiblingJobs(t *testing.T) {
	store := &fakeCompletionStore{
		tokenRunID:         "r1",
		tokenProject:       "proj",
		nativeExpectedJobs: []string{"plan", "impl"},
	}
	store.run = runDataForCompletion("work")
	store.wf = &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases:  []PhaseSpec{{Name: "work", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "plan"}, {ID: "impl"}}}},
	}
	store.terminalResult = AbortRunResult{State: "passed", RunRef: "proj#7/runs/1"}
	handler := newCompletionHandler(store, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, nativeCompletionRequest("tok", completedJob("plan", "success", nil, map[string]string{"plan": "ready"})))
	if rec.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", rec.Code, rec.Body.String())
	}
	first := readCallbackResult(t, rec)
	if first.Decision == nil || *first.Decision != "wait_jobs" || first.PhaseComplete == nil || *first.PhaseComplete {
		t.Fatalf("first result=%#v", first)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, nativeCompletionRequest("tok", completedJob("impl", "success", nil, map[string]string{"impl": "done"})))
	if rec.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", rec.Code, rec.Body.String())
	}
	second := readCallbackResult(t, rec)
	if second.Decision == nil || *second.Decision != "advance" || second.PhaseComplete == nil || !*second.PhaseComplete {
		t.Fatalf("second result=%#v", second)
	}
	if len(second.CompletedJobIDs) != 2 {
		t.Fatalf("completed_job_ids=%v", second.CompletedJobIDs)
	}
}

func TestNativeRunCompletedByCallbackTokenEvidenceGateRetryCarriesPriorOutputs(t *testing.T) {
	leaseRef := "proj/leases/proj-1/1"
	store := &fakeCompletionStore{
		tokenRunID:         "r1",
		tokenProject:       "proj",
		appendIdx:          1,
		nativeExpectedJobs: []string{EvidenceGateJobID},
		leaseResult:        Lease{Project: "proj", LeaseNumber: intPtr(1), State: "claimed", Metadata: map[string]any{}},
	}
	store.run = &RunReplayData{
		ID:           "r1",
		Project:      "proj",
		WorkflowName: "wf",
		IssueNumber:  7,
		IssueRepo:    "owner/repo",
		SlotLeaseRef: &leaseRef,
		Attempts: []RunAttemptData{
			{
				AttemptIndex: 0,
				Phase:        "env-prep",
				Conclusion:   "success",
				Decision:     string(decision.Advance),
				Completed:    true,
				PhaseOutputs: map[string]string{
					"namespace":      "ambience-slot-1",
					"validation_url": "https://slot.example",
				},
			},
			{AttemptIndex: 1, Phase: "llm-work", Conclusion: "success", Decision: string(decision.Advance), Completed: true},
			{AttemptIndex: 2, Phase: "llm-verify", Conclusion: "success", Decision: string(decision.Advance), Completed: true},
			{AttemptIndex: 3, Phase: "evidence-gate"},
		},
	}
	store.wf = &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "env-prep"}}, Outputs: []string{"namespace", "validation_url"}},
			{
				Name:      "llm-work",
				Kind:      "k8s_job",
				DependsOn: []string{"env-prep"},
				Inputs: map[string]string{
					"namespace":      "${{ phases.env-prep.outputs.namespace }}",
					"validation_url": "${{ phases.env-prep.outputs.validation_url }}",
				},
				Jobs: []NativeJobSpec{{ID: "llm-work", Managed: true, Steps: []NativeStepSpec{{Slug: "run", Run: "true"}}}},
			},
			{Name: "llm-verify", Kind: "k8s_job", Verify: true, DependsOn: []string{"llm-work"}, Jobs: []NativeJobSpec{{ID: "llm-verify"}}},
			{
				Name:                     "evidence-gate",
				Kind:                     "k8s_job",
				EvidenceVerificationGate: true,
				DependsOn:                []string{"llm-verify"},
				RecyclePolicy:            &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}, LandsAt: "llm-work"},
				Jobs:                     []NativeJobSpec{{ID: EvidenceGateJobID}},
			},
			{Name: "cleanup", Kind: "k8s_job", Always: true, DependsOn: []string{"evidence-gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	launcher := &fakeNativeLauncher{}
	req := completedJob(EvidenceGateJobID, "failure", nil, nil)
	rec := httptest.NewRecorder()
	newCompletionHandler(store, launcher).ServeHTTP(rec, nativeCompletionRequest("tok", req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := readCallbackResult(t, rec)
	if result.Decision == nil || *result.Decision != "retry" {
		t.Fatalf("decision=%v", result.Decision)
	}
	if !launcher.called || launcher.req.Phase.Name != "llm-work" {
		t.Fatalf("native launch=%#v", launcher.req)
	}
	phaseInputs, ok := launcher.req.Lease.Metadata["phase_inputs"].(map[string]string)
	if !ok {
		t.Fatalf("phase_inputs=%#v", launcher.req.Lease.Metadata["phase_inputs"])
	}
	if phaseInputs["namespace"] != "ambience-slot-1" || phaseInputs["validation_url"] != "https://slot.example" {
		t.Fatalf("phase_inputs=%#v", phaseInputs)
	}
	if len(launcher.req.Run.Attempts) != 2 || !launcher.req.Run.Attempts[0].CarryForward || launcher.req.Run.Attempts[1].Phase != "llm-work" {
		t.Fatalf("recycle attempts=%#v", launcher.req.Run.Attempts)
	}
	if store.recycleReq == nil || store.recycleReq.TargetPhaseName != "llm-work" || len(store.recycleReq.CarryForwardAttempts) != 1 {
		t.Fatalf("recycle request=%#v", store.recycleReq)
	}
}

func TestNativeRunCompletedByCallbackTokenEvidenceGateRetryCanRestartAtEnvPrep(t *testing.T) {
	leaseRef := "proj/leases/proj-1/1"
	store := &fakeCompletionStore{
		tokenRunID:         "r1",
		tokenProject:       "proj",
		appendIdx:          0,
		nativeExpectedJobs: []string{EvidenceGateJobID},
		leaseResult:        Lease{Project: "proj", LeaseNumber: intPtr(1), State: "claimed", Metadata: map[string]any{}},
	}
	store.run = &RunReplayData{
		ID:           "r1",
		Project:      "proj",
		WorkflowName: "wf",
		IssueNumber:  7,
		IssueRepo:    "owner/repo",
		SlotLeaseRef: &leaseRef,
		Attempts: []RunAttemptData{
			{
				AttemptIndex: 0,
				Phase:        "env-prep",
				Conclusion:   "success",
				Decision:     string(decision.Advance),
				Completed:    true,
				PhaseOutputs: map[string]string{
					"namespace":      "ambience-slot-1",
					"validation_url": "https://slot.example",
				},
			},
			{AttemptIndex: 1, Phase: "llm-work", Conclusion: "success", Decision: string(decision.Advance), Completed: true},
			{AttemptIndex: 2, Phase: "llm-verify", Conclusion: "success", Decision: string(decision.Advance), Completed: true},
			{AttemptIndex: 3, Phase: "evidence-gate"},
		},
	}
	store.wf = &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "env-prep"}}, Outputs: []string{"namespace", "validation_url"}},
			{
				Name:      "llm-work",
				Kind:      "k8s_job",
				DependsOn: []string{"env-prep"},
				Inputs: map[string]string{
					"namespace":      "${{ phases.env-prep.outputs.namespace }}",
					"validation_url": "${{ phases.env-prep.outputs.validation_url }}",
				},
				Jobs: []NativeJobSpec{{ID: "llm-work", Managed: true, Steps: []NativeStepSpec{{Slug: "run", Run: "true"}}}},
			},
			{Name: "llm-verify", Kind: "k8s_job", Verify: true, DependsOn: []string{"llm-work"}, Jobs: []NativeJobSpec{{ID: "llm-verify"}}},
			{
				Name:                     "evidence-gate",
				Kind:                     "k8s_job",
				EvidenceVerificationGate: true,
				DependsOn:                []string{"llm-verify"},
				RecyclePolicy:            &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}, LandsAt: "env-prep"},
				Jobs:                     []NativeJobSpec{{ID: EvidenceGateJobID}},
			},
			{Name: "cleanup", Kind: "k8s_job", Always: true, DependsOn: []string{"evidence-gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	launcher := &fakeNativeLauncher{}
	req := completedJob(EvidenceGateJobID, "failure", nil, nil)
	rec := httptest.NewRecorder()
	newCompletionHandler(store, launcher).ServeHTTP(rec, nativeCompletionRequest("tok", req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := readCallbackResult(t, rec)
	if result.Decision == nil || *result.Decision != "retry" {
		t.Fatalf("decision=%v", result.Decision)
	}
	if !launcher.called || launcher.req.Phase.Name != "env-prep" {
		t.Fatalf("native launch=%#v", launcher.req)
	}
	phaseInputs, ok := launcher.req.Lease.Metadata["phase_inputs"].(map[string]string)
	if !ok {
		t.Fatalf("phase_inputs=%#v", launcher.req.Lease.Metadata["phase_inputs"])
	}
	if len(phaseInputs) != 0 {
		t.Fatalf("phase_inputs=%#v", phaseInputs)
	}
	if len(launcher.req.Run.Attempts) != 1 || launcher.req.Run.Attempts[0].Phase != "env-prep" || launcher.req.Run.Attempts[0].CarryForward {
		t.Fatalf("recycle attempts=%#v", launcher.req.Run.Attempts)
	}
	if store.recycleReq == nil || store.recycleReq.TargetPhaseName != "env-prep" || len(store.recycleReq.CarryForwardAttempts) != 0 {
		t.Fatalf("recycle request=%#v", store.recycleReq)
	}
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/decision"
)

// fakeCompletionStore implements RunCompletionStore.
type fakeCompletionStore struct {
	fakeReadStore
	// token → (runID, project, runRef)
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

	stampResult RunReplayData
	stampErr    error

	decisionErr error

	terminalResult AbortRunResult
	terminalErr    error

	appendIdx   int
	appendErr   error
	appendPhase string
	appendKind  string
	appendFile  string

	leaseResult Lease
	leaseHost   *Host
	leaseErr    error
	leaseReq    LeaseAcquireRequest

	nativeExpectedJobs []string
	nativeCompletions  map[string]CompletionPayload
	nativeErr          error
}

func (s *fakeCompletionStore) ReadRunIDForCallbackToken(_ context.Context, token string) (string, string, string, error) {
	if s.tokenErr != nil {
		return "", "", "", s.tokenErr
	}
	if s.tokenRunID == "" {
		return "", "", "", ErrNotFound
	}
	return s.tokenRunID, s.tokenProject, s.tokenRef, nil
}

func (s *fakeCompletionStore) AbortRunByID(_ context.Context, _, _, _ string) (AbortRunResult, error) {
	return s.abortResult, s.abortErr
}

func (s *fakeCompletionStore) ReadRunForReplay(_ context.Context, _, _ string) (RunReplayData, error) {
	if s.readErr != nil {
		return RunReplayData{}, s.readErr
	}
	if s.run == nil {
		return RunReplayData{}, ErrNotFound
	}
	return *s.run, nil
}

func (s *fakeCompletionStore) GetWorkflowByName(_ context.Context, _, _ string) (*Workflow, error) {
	return s.wf, s.wfErr
}

func (s *fakeCompletionStore) StampRunCompletion(_ context.Context, _, _ string, p CompletionPayload) (RunReplayData, error) {
	if s.stampErr != nil {
		return RunReplayData{}, s.stampErr
	}
	if s.run == nil {
		return RunReplayData{}, ErrNotFound
	}
	// Apply the completion payload to a copy of the run so the decision engine sees updated state.
	copy := *s.run
	if len(copy.Attempts) > 0 {
		last := copy.Attempts[len(copy.Attempts)-1]
		last.Conclusion = p.Conclusion
		last.Completed = true
		if p.PhaseOutputs != nil {
			last.PhaseOutputs = p.PhaseOutputs
		}
		if p.VerificationStatus != "" {
			last.Verification = &RunVerificationData{
				Status:  p.VerificationStatus,
				Reasons: p.VerificationReasons,
			}
		} else {
			last.Verification = nil
		}
		copy.Attempts[len(copy.Attempts)-1] = last
	}
	return copy, nil
}

func (s *fakeCompletionStore) StampRunDecision(_ context.Context, _, _, _ string) error {
	return s.decisionErr
}

func (s *fakeCompletionStore) SetRunTerminalState(_ context.Context, _, _, _ string, _ *string) (AbortRunResult, error) {
	return s.terminalResult, s.terminalErr
}

func (s *fakeCompletionStore) AppendRunAttempt(_ context.Context, _, _, phase, phaseKind, workflowFilename string) (int, error) {
	s.appendPhase = phase
	s.appendKind = phaseKind
	s.appendFile = workflowFilename
	return s.appendIdx, s.appendErr
}

func (s *fakeCompletionStore) AcquireLease(_ context.Context, req LeaseAcquireRequest) (Lease, *Host, error) {
	s.leaseReq = req
	return s.leaseResult, s.leaseHost, s.leaseErr
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
	phasePayload := aggregateFakeNativePayload(expected, s.nativeCompletions)
	return NativeJobCompletionResult{
		Run:             *s.run,
		PhaseComplete:   phaseComplete,
		CompletionReady: phaseComplete && !existed,
		CompletedJobIDs: completed,
		PendingJobIDs:   pending,
		FailedJobIDs:    failed,
		PhasePayload:    phasePayload,
	}, nil
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
		}
		payload.CostUSD += completion.CostUSD
		for key, value := range completion.PhaseOutputs {
			payload.PhaseOutputs[key] = value
		}
	}
	return payload
}

func containsTestString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// fakeDispatchClient records dispatch calls.
type fakeDispatchClient struct {
	called bool
	repo   string
	file   string
	ref    string
	inputs map[string]string
	err    error
}

func (f *fakeDispatchClient) DispatchWorkflow(_ context.Context, repo, file, ref string, inputs map[string]string) error {
	f.called = true
	f.repo = repo
	f.file = file
	f.ref = ref
	f.inputs = inputs
	return f.err
}

type fakeGitHubClient struct {
	fakeDispatchClient
}

func (f *fakeGitHubClient) FetchWorkflowFile(context.Context, string, string, string) ([]byte, int, error) {
	return nil, 404, ErrNotFound
}

// --- helpers ---

func newCompletionHandler(store *fakeCompletionStore, ghClient WorkflowSyncClient) http.Handler {
	return NewWithSyncClient(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, ghClient)
}

func singlePhaseWorkflowForCompletion(name string, verify bool) *Workflow {
	return &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{
			{
				Name:          name,
				Verify:        verify,
				RecyclePolicy: &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}},
			},
		},
	}
}

func runDataForCompletion(phase string) *RunReplayData {
	return &RunReplayData{
		ID:           "run-1",
		Project:      "proj",
		WorkflowName: "wf",
		IssueNumber:  7,
		IssueRepo:    "owner/repo",
		Attempts: []RunAttemptData{
			{AttemptIndex: 0, Phase: phase, Conclusion: "failure"},
		},
		CumulativeCostUSD: 0.1,
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

// --- run_completed tests ---

func TestRunCompletedByCallbackToken_TokenNotFound(t *testing.T) {
	store := &fakeCompletionStore{}
	h := newCompletionHandler(store, nil)

	body, _ := json.Marshal(RunCompletedRequest{WorkflowRunID: 1, Conclusion: "success"})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/badtoken/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestRunCompletedByCallbackToken_MissingConclusion(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.wf = singlePhaseWorkflowForCompletion("impl", false)
	h := newCompletionHandler(store, nil)

	body, _ := json.Marshal(RunCompletedRequest{WorkflowRunID: 1})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/tok/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestRunCompletedByCallbackToken_Advance_Passed(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.wf = singlePhaseWorkflowForCompletion("impl", false)
	store.terminalResult = AbortRunResult{State: "passed", RunRef: "proj#7/runs/1"}
	h := newCompletionHandler(store, nil)

	body, _ := json.Marshal(RunCompletedRequest{WorkflowRunID: 1, Conclusion: "success"})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/tok/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result RunCallbackResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision == nil || *result.Decision != "advance" {
		t.Errorf("expected decision=advance, got %v", result.Decision)
	}
}

func TestRunCompletedByCallbackToken_Advance_ReviewRequired(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	wf := singlePhaseWorkflowForCompletion("impl", false)
	wf.PR.Enabled = true
	store.wf = wf
	store.terminalResult = AbortRunResult{State: "review_required", RunRef: "proj#7/runs/1"}
	h := newCompletionHandler(store, nil)

	body, _ := json.Marshal(RunCompletedRequest{WorkflowRunID: 1, Conclusion: "success"})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/tok/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result RunCallbackResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision == nil || *result.Decision != "advance" {
		t.Errorf("expected decision=advance, got %v", result.Decision)
	}
}

func TestRunCompletedByCallbackToken_AdvanceDispatchesNextPhase(t *testing.T) {
	leaseNumber := 12
	token := "lease-token"
	runCallback := "run-token"
	store := &fakeCompletionStore{
		tokenRunID:   "r1",
		tokenProject: "proj",
		appendIdx:    1,
		leaseResult: Lease{
			Project:     "proj",
			LeaseNumber: &leaseNumber,
			Metadata:    map[string]any{"lease_callback_token": token},
		},
		leaseHost: &Host{Name: "runner-1"},
	}
	store.run = &RunReplayData{
		ID:            "r1",
		Project:       "proj",
		WorkflowName:  "wf",
		IssueNumber:   7,
		IssueRepo:     "owner/repo",
		CallbackToken: &runCallback,
		Attempts: []RunAttemptData{
			{AttemptIndex: 0, Phase: "env-prep", Conclusion: "failure"},
		},
		CumulativeCostUSD: 0.1,
	}
	store.wf = &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{
			{Name: "env-prep", Outputs: []string{"validation_url"}},
			{
				Name:             "agent-execute",
				Kind:             "gha_dispatch",
				WorkflowFilename: "agent.yml",
				WorkflowRef:      "feature",
				DependsOn:        []string{"env-prep"},
				Inputs: map[string]string{
					"validation_url": "${{ phases.env-prep.outputs.validation_url }}",
				},
			},
		},
	}
	gh := &fakeGitHubClient{}
	h := newCompletionHandler(store, gh)

	body, _ := json.Marshal(RunCompletedRequest{
		WorkflowRunID: 1,
		Conclusion:    "success",
		Outputs:       map[string]string{"validation_url": "https://preview.example"},
	})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/tok/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result RunCallbackResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision == nil || *result.Decision != "advance_phase" {
		t.Fatalf("decision=%v, want advance_phase", result.Decision)
	}
	if store.appendPhase != "agent-execute" || store.appendKind != "gha_dispatch" || store.appendFile != "agent.yml" {
		t.Fatalf("append=(%q,%q,%q)", store.appendPhase, store.appendKind, store.appendFile)
	}
	phaseInputs, ok := store.leaseReq.Metadata["phase_inputs"].(map[string]string)
	if !ok || phaseInputs["validation_url"] != "https://preview.example" {
		t.Fatalf("phase_inputs=%#v", store.leaseReq.Metadata["phase_inputs"])
	}
	if !gh.called {
		t.Fatal("expected workflow dispatch")
	}
	if gh.repo != "owner/repo" || gh.file != "agent.yml" || gh.ref != "feature" {
		t.Fatalf("dispatch=(%q,%q,%q)", gh.repo, gh.file, gh.ref)
	}
	if gh.inputs["validation_url"] != "https://preview.example" {
		t.Fatalf("dispatch inputs=%#v", gh.inputs)
	}
	if gh.inputs["lease_callback_token"] != token {
		t.Fatalf("lease callback input=%q", gh.inputs["lease_callback_token"])
	}
}

func TestAllReadyDispatchTargetsHandlesFanOutFanInAndTeardown(t *testing.T) {
	wf := &Workflow{
		Phases: []PhaseSpec{
			{Name: "prepare"},
			{Name: "work-a", DependsOn: []string{"prepare"}},
			{Name: "work-b", DependsOn: []string{"prepare"}},
			{Name: "verify", Verify: true, DependsOn: []string{"work-a", "work-b"}},
			{Name: "cleanup", Always: true},
		},
	}

	run := RunReplayData{Attempts: []RunAttemptData{
		{AttemptIndex: 0, Phase: "prepare", Completed: true, Decision: string(decision.Advance)},
	}}
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.Advance), "work-a", "work-b")

	run.Attempts = append(run.Attempts,
		RunAttemptData{AttemptIndex: 1, Phase: "work-a", Completed: true, Decision: string(decision.Advance)},
	)
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.Advance), "work-b")

	run.Attempts = append(run.Attempts,
		RunAttemptData{AttemptIndex: 2, Phase: "work-b", Completed: true, Decision: string(decision.Advance)},
	)
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.Advance), "verify")

	run.Attempts = append(run.Attempts,
		RunAttemptData{AttemptIndex: 3, Phase: "verify", Completed: true, Decision: string(decision.AbortBudgetAttempts)},
	)
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.AbortBudgetAttempts), "cleanup")
}

func TestRunCompletedByCallbackToken_AbortBudgetAttempts(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	// 3 attempts already at max
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
	h := newCompletionHandler(store, nil)

	body, _ := json.Marshal(RunCompletedRequest{
		WorkflowRunID: 1,
		Conclusion:    "failure",
		Verification:  map[string]any{"status": "fail"},
	})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/tok/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result RunCallbackResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision == nil || *result.Decision != "abort_budget_attempts" {
		t.Errorf("expected abort_budget_attempts, got %v", result.Decision)
	}
}

func TestRunCompletedByCallbackToken_Retry_NoDispatchClient(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.wf = singlePhaseWorkflowForCompletion("impl", true)
	store.abortResult = AbortRunResult{State: "aborted", RunRef: "proj#7/runs/1"}
	h := newCompletionHandler(store, nil)

	body, _ := json.Marshal(RunCompletedRequest{
		WorkflowRunID: 1,
		Conclusion:    "failure",
		Verification:  map[string]any{"status": "fail"},
	})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/tok/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Without a dispatch client, retry falls back to abort
	var result RunCallbackResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision == nil {
		t.Errorf("expected a decision in response, got nil")
	}
}

func TestRunCompletedByCallbackToken_StampError(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.stampErr = errors.New("cosmos unavailable")
	h := newCompletionHandler(store, nil)

	body, _ := json.Marshal(RunCompletedRequest{WorkflowRunID: 1, Conclusion: "success"})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/tok/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// --- native_completed tests ---

func TestNativeRunCompletedByCallbackToken_Advance(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.wf = singlePhaseWorkflowForCompletion("impl", false)
	store.terminalResult = AbortRunResult{State: "passed", RunRef: "proj#7/runs/1"}
	h := newCompletionHandler(store, nil)

	jobID := "impl"
	body, _ := json.Marshal(NativeRunCompletedRequest{JobID: &jobID, Conclusion: "success"})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/tok/native/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result RunCallbackResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision == nil || *result.Decision != "advance" {
		t.Errorf("expected advance, got %v", result.Decision)
	}
	if result.PhaseComplete == nil || !*result.PhaseComplete {
		t.Errorf("phase_complete=%v, want true", result.PhaseComplete)
	}
	if len(result.CompletedJobIDs) != 1 || result.CompletedJobIDs[0] != "impl" {
		t.Errorf("completed_job_ids=%v", result.CompletedJobIDs)
	}
}

func TestNativeRunCompletedByCallbackToken_TokenNotFound(t *testing.T) {
	store := &fakeCompletionStore{}
	h := newCompletionHandler(store, nil)

	body, _ := json.Marshal(NativeRunCompletedRequest{Conclusion: "success"})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/badtoken/native/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestNativeRunCompletedByCallbackToken_MissingJobID(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.wf = singlePhaseWorkflowForCompletion("impl", false)
	h := newCompletionHandler(store, nil)

	body, _ := json.Marshal(NativeRunCompletedRequest{Conclusion: "success"})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/tok/native/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestNativeRunCompletedByCallbackToken_WaitsForSiblingJobs(t *testing.T) {
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
		Phases: []PhaseSpec{{
			Name: "work",
			Kind: "k8s_job",
			Jobs: []NativeJobSpec{{ID: "plan"}, {ID: "impl"}},
		}},
	}
	store.terminalResult = AbortRunResult{State: "passed", RunRef: "proj#7/runs/1"}
	h := newCompletionHandler(store, nil)

	planJob := "plan"
	body, _ := json.Marshal(NativeRunCompletedRequest{
		JobID:      &planJob,
		Conclusion: "success",
		Outputs:    map[string]string{"plan": "ready"},
	})
	req := httptest.NewRequest("POST", "/v1/run-callbacks/tok/native/completed", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("first completion expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var first RunCallbackResult
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Decision == nil || *first.Decision != "wait_jobs" {
		t.Fatalf("first decision=%v, want wait_jobs", first.Decision)
	}
	if first.PhaseComplete == nil || *first.PhaseComplete {
		t.Fatalf("first phase_complete=%v, want false", first.PhaseComplete)
	}
	if len(first.PendingJobIDs) != 1 || first.PendingJobIDs[0] != "impl" {
		t.Fatalf("pending_job_ids=%v, want [impl]", first.PendingJobIDs)
	}

	implJob := "impl"
	body, _ = json.Marshal(NativeRunCompletedRequest{
		JobID:      &implJob,
		Conclusion: "success",
		Outputs:    map[string]string{"impl": "done"},
	})
	req = httptest.NewRequest("POST", "/v1/run-callbacks/tok/native/completed", bytes.NewReader(body))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("second completion expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var second RunCallbackResult
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.Decision == nil || *second.Decision != "advance" {
		t.Fatalf("second decision=%v, want advance", second.Decision)
	}
	if second.PhaseComplete == nil || !*second.PhaseComplete {
		t.Fatalf("second phase_complete=%v, want true", second.PhaseComplete)
	}
	if len(second.CompletedJobIDs) != 2 {
		t.Fatalf("completed_job_ids=%v, want two jobs", second.CompletedJobIDs)
	}
}

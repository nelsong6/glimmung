package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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

	appendIdx   int
	appendErr   error
	appendPhase string
	appendKind  string
	appendFile  string

	leaseResult Lease
	leaseErr    error
	leaseReq    LeaseAcquireRequest

	nativeExpectedJobs []string
	nativeCompletions  map[string]CompletionPayload
	nativeErr          error
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

func (s *fakeCompletionStore) SetRunTerminalState(context.Context, string, string, string, *string) (AbortRunResult, error) {
	return s.terminalResult, s.terminalErr
}

func (s *fakeCompletionStore) AppendRunAttempt(_ context.Context, _, _, phase, phaseKind, workflowFilename string) (int, error) {
	s.appendPhase = phase
	s.appendKind = phaseKind
	s.appendFile = workflowFilename
	return s.appendIdx, s.appendErr
}

func (s *fakeCompletionStore) AcquireLease(_ context.Context, req LeaseAcquireRequest) (Lease, error) {
	s.leaseReq = req
	return s.leaseResult, s.leaseErr
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

func newCompletionHandler(store *fakeCompletionStore, nativeLauncher NativeLauncher) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/completed", nativeRunCompletedByCallbackToken(store, nativeLauncher))
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

func runDataForCompletion(phase string) *RunReplayData {
	callback := "run-token"
	return &RunReplayData{
		ID:            "run-1",
		Project:       "proj",
		WorkflowName:  "wf",
		IssueNumber:   7,
		IssueRepo:     "owner/repo",
		CallbackToken: &callback,
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
	store.run = runDataForCompletion("impl")
	wf := singlePhaseWorkflowForCompletion("impl", false)
	wf.PR.Enabled = true
	store.wf = wf
	store.terminalResult = AbortRunResult{State: "review_required", RunRef: "proj#7/runs/1"}
	rec := httptest.NewRecorder()
	newCompletionHandler(store, nil).ServeHTTP(rec, nativeCompletionRequest("tok", completedJob("impl", "success", nil, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readCallbackResult(t, rec).Decision; got == nil || *got != "advance" {
		t.Fatalf("decision=%v", got)
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
	store.run = &RunReplayData{
		ID:           "r1",
		Project:      "proj",
		WorkflowName: "wf",
		IssueNumber:  7,
		IssueRepo:    "owner/repo",
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
	phaseInputs, ok := store.leaseReq.Metadata["phase_inputs"].(map[string]string)
	if !ok || phaseInputs["validation_url"] != "https://preview.example" {
		t.Fatalf("phase_inputs=%#v", store.leaseReq.Metadata["phase_inputs"])
	}
	if store.leaseReq.Metadata["native_k8s"] != true {
		t.Fatalf("lease metadata=%#v", store.leaseReq.Metadata)
	}
}

func TestAllReadyDispatchTargetsHandlesFanOutFanInAndTeardown(t *testing.T) {
	wf := &Workflow{Phases: []PhaseSpec{
		{Name: "prepare"},
		{Name: "work-a", DependsOn: []string{"prepare"}},
		{Name: "work-b", DependsOn: []string{"prepare"}},
		{Name: "verify", Verify: true, DependsOn: []string{"work-a", "work-b"}},
		{Name: "cleanup", Always: true},
	}}
	run := RunReplayData{Attempts: []RunAttemptData{{AttemptIndex: 0, Phase: "prepare", Completed: true, Decision: string(decision.Advance)}}}
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.Advance), "work-a", "work-b")

	run.Attempts = append(run.Attempts, RunAttemptData{AttemptIndex: 1, Phase: "work-a", Completed: true, Decision: string(decision.Advance)})
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.Advance), "work-b")

	run.Attempts = append(run.Attempts, RunAttemptData{AttemptIndex: 2, Phase: "work-b", Completed: true, Decision: string(decision.Advance)})
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.Advance), "verify")

	run.Attempts = append(run.Attempts, RunAttemptData{AttemptIndex: 3, Phase: "verify", Completed: true, Decision: string(decision.AbortBudgetAttempts)})
	assertPhaseTargets(t, allReadyDispatchTargets(wf, run, decision.AbortBudgetAttempts), "cleanup")
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
	store.abortResult = AbortRunResult{State: "aborted", RunRef: "proj#7/runs/1"}
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
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj", stampErr: errors.New("cosmos unavailable")}
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

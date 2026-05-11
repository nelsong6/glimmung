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
)

// fakeCompletionStore implements RunCompletionStore.
type fakeCompletionStore struct {
	fakeReadStore
	// token → (runID, project, runRef)
	tokenRunID  string
	tokenProject string
	tokenRef    string
	tokenErr    error

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

	appendIdx int
	appendErr error

	leaseResult Lease
	leaseHost   *Host
	leaseErr    error
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

func (s *fakeCompletionStore) AppendRunAttempt(_ context.Context, _, _, _, _, _ string) (int, error) {
	return s.appendIdx, s.appendErr
}

func (s *fakeCompletionStore) AcquireLease(_ context.Context, _ LeaseAcquireRequest) (Lease, *Host, error) {
	return s.leaseResult, s.leaseHost, s.leaseErr
}

// fakeDispatchClient records dispatch calls.
type fakeDispatchClient struct {
	called bool
	err    error
}

func (f *fakeDispatchClient) DispatchWorkflow(_ context.Context, _, _, _ string, _ map[string]string) error {
	f.called = true
	return f.err
}

// --- helpers ---

func newCompletionHandler(store *fakeCompletionStore, _ GHADispatchClient) http.Handler {
	return NewWithSyncClient(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)
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

	body, _ := json.Marshal(NativeRunCompletedRequest{Conclusion: "success"})
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

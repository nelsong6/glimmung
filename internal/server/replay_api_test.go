package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
	"github.com/nelsong6/glimmung/internal/domain/budget"
)

// fakeRunReplayStore extends fakeRunMutationStore with RunReplayStore methods.
type fakeRunReplayStore struct {
	fakeRunMutationStore
	run     *RunReplayData
	wf      *Workflow
	readErr error
}

func (s *fakeRunReplayStore) ReadRunForReplay(_ context.Context, _, _ string) (RunReplayData, error) {
	if s.readErr != nil {
		return RunReplayData{}, s.readErr
	}
	if s.run == nil {
		return RunReplayData{}, ErrNotFound
	}
	return *s.run, nil
}

func (s *fakeRunReplayStore) GetWorkflowByName(_ context.Context, _, _ string) (*Workflow, error) {
	return s.wf, nil
}

func (s *fakeRunReplayStore) GetWorkflowBySchemaRef(_ context.Context, _, _ string) (*Workflow, error) {
	return s.wf, nil
}

func newReplayHandlerAdmin(store *fakeRunReplayStore) http.Handler {
	return NewWithSyncClient(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)
}

func newReplayHandlerNoAuth(store *fakeRunReplayStore) http.Handler {
	return NewWithSyncClient(Settings{}, store, nil, nil)
}

// --- helpers ---

func singlePhaseWorkflow(name string, verify bool) *Workflow {
	return &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases:  []PhaseSpec{{Name: name, Verify: verify, RecyclePolicy: &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}}}},
	}
}

func twoPhaseWorkflow(p1, p2 string) *Workflow {
	return &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{
			{Name: p1, Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}}},
			{Name: p2, Verify: false},
		},
	}
}

func minimalRun(phase string) *RunReplayData {
	return &RunReplayData{
		ID:           "run-id-1",
		Project:      "proj",
		WorkflowName: "wf",
		Attempts: []RunAttemptData{
			{AttemptIndex: 0, Phase: phase, Conclusion: "success"},
		},
		CumulativeCostUSD: 0.5,
	}
}

// --- replay tests ---

func TestReplayRunDecision_RequiresAdmin(t *testing.T) {
	store := &fakeRunReplayStore{}
	store.runID = "run-abc"
	h := newReplayHandlerNoAuth(store)

	body, _ := json.Marshal(RunReplayRequest{SyntheticCompletion: SyntheticCompletion{Conclusion: "success"}})
	req := httptest.NewRequest("POST", "/v1/projects/proj/issues/1/runs/r1/replay", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("expected non-200 without admin auth, got 200")
	}
}

func TestReplayRunDecision_Advance(t *testing.T) {
	store := &fakeRunReplayStore{}
	store.runID = "run-abc"
	store.run = minimalRun("implement")
	store.wf = twoPhaseWorkflow("implement", "review")
	h := newReplayHandlerAdmin(store)

	body, _ := json.Marshal(RunReplayRequest{
		SyntheticCompletion: SyntheticCompletion{
			Conclusion:   "success",
			Verification: map[string]any{"status": "pass"},
		},
	})
	req := httptest.NewRequest("POST", "/v1/projects/proj/issues/1/runs/r1/replay", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result ReplayResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision != "advance" {
		t.Errorf("expected advance, got %s", result.Decision)
	}
	if result.WouldAdvanceToPhase == nil || *result.WouldAdvanceToPhase != "review" {
		t.Errorf("expected would_advance_to_phase=review, got %v", result.WouldAdvanceToPhase)
	}
	if result.WorkflowSource != "registered" {
		t.Errorf("expected workflow_source=registered, got %s", result.WorkflowSource)
	}
}

func TestReplayRunDecision_Retry(t *testing.T) {
	store := &fakeRunReplayStore{}
	store.runID = "run-abc"
	store.run = minimalRun("implement")
	store.wf = singlePhaseWorkflow("implement", true)
	h := newReplayHandlerAdmin(store)

	body, _ := json.Marshal(RunReplayRequest{
		SyntheticCompletion: SyntheticCompletion{
			Conclusion:   "failure",
			Verification: map[string]any{"status": "fail", "reasons": []any{"test red"}},
		},
	})
	req := httptest.NewRequest("POST", "/v1/projects/proj/issues/1/runs/r1/replay", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result ReplayResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision != "retry" {
		t.Errorf("expected retry, got %s", result.Decision)
	}
	if result.WouldRetryTargetPhase == nil {
		t.Errorf("expected would_retry_target_phase to be set")
	}
}

func TestReplayRunDecision_RetryReportsExplicitEnvPrepTarget(t *testing.T) {
	store := &fakeRunReplayStore{}
	store.runID = "run-abc"
	store.run = &RunReplayData{
		ID:           "run-id-1",
		Project:      "proj",
		WorkflowName: "wf",
		Attempts: []RunAttemptData{
			{AttemptIndex: 0, Phase: "evidence-gate", Conclusion: "success"},
		},
	}
	store.wf = &Workflow{
		Project: "proj",
		Name:    "wf",
		Budget:  budget.Config{Total: 25},
		Phases: []PhaseSpec{
			{Name: "env-prep"},
			{Name: "llm-work", DependsOn: []string{"env-prep"}},
			{
				Name:          "evidence-gate",
				Verify:        true,
				DependsOn:     []string{"llm-work"},
				RecyclePolicy: &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}, LandsAt: "env-prep"},
			},
		},
	}
	h := newReplayHandlerAdmin(store)

	body, _ := json.Marshal(RunReplayRequest{
		SyntheticCompletion: SyntheticCompletion{
			Conclusion:   "failure",
			Verification: map[string]any{"status": "fail", "reasons": []any{"evidence missing"}},
		},
	})
	req := httptest.NewRequest("POST", "/v1/projects/proj/issues/1/runs/r1/replay", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result ReplayResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision != "retry" {
		t.Errorf("expected retry, got %s", result.Decision)
	}
	if result.WouldRetryTargetPhase == nil || *result.WouldRetryTargetPhase != "env-prep" {
		t.Errorf("would_retry_target_phase=%v, want env-prep", result.WouldRetryTargetPhase)
	}
}

func TestReplayRunDecision_AbortBudgetAttempts(t *testing.T) {
	// 3 attempts already, max_attempts=3 → abort
	store := &fakeRunReplayStore{}
	store.runID = "run-abc"
	store.run = &RunReplayData{
		ID:           "run-id-1",
		Project:      "proj",
		WorkflowName: "wf",
		Attempts: []RunAttemptData{
			{AttemptIndex: 0, Phase: "implement", Conclusion: "failure"},
			{AttemptIndex: 1, Phase: "implement", Conclusion: "failure"},
			{AttemptIndex: 2, Phase: "implement", Conclusion: "failure"},
		},
		CumulativeCostUSD: 1.0,
	}
	store.wf = singlePhaseWorkflow("implement", true)
	h := newReplayHandlerAdmin(store)

	body, _ := json.Marshal(RunReplayRequest{
		SyntheticCompletion: SyntheticCompletion{
			Conclusion:   "failure",
			Verification: map[string]any{"status": "fail"},
		},
	})
	req := httptest.NewRequest("POST", "/v1/projects/proj/issues/1/runs/r1/replay", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result ReplayResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision != "abort_budget_attempts" {
		t.Errorf("expected abort_budget_attempts, got %s", result.Decision)
	}
	if result.AbortReason == nil {
		t.Errorf("expected abort_reason to be set")
	}
}

func TestReplayRunDecision_CycleOrdinalCountsRecycleAttempts(t *testing.T) {
	store := &fakeRunReplayStore{}
	store.runID = "run-abc"
	store.run = minimalRun("implement")
	store.run.RunCycleNumber = intPtr(3)
	store.wf = singlePhaseWorkflow("implement", true)
	h := newReplayHandlerAdmin(store)

	body, _ := json.Marshal(RunReplayRequest{
		SyntheticCompletion: SyntheticCompletion{
			Conclusion:   "failure",
			Verification: map[string]any{"status": "fail"},
		},
	})
	req := httptest.NewRequest("POST", "/v1/projects/proj/issues/1/runs/r1/replay", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result ReplayResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision != "abort_budget_attempts" {
		t.Errorf("expected abort_budget_attempts, got %s", result.Decision)
	}
	if result.AttemptsInPhaseAfter != 3 {
		t.Errorf("attempts_in_phase_after=%d, want 3", result.AttemptsInPhaseAfter)
	}
}

func TestReplayRunDecision_OverrideWorkflow(t *testing.T) {
	store := &fakeRunReplayStore{}
	store.runID = "run-abc"
	store.run = minimalRun("implement")
	// wf is nil — override_workflow should be used instead
	h := newReplayHandlerAdmin(store)

	body, _ := json.Marshal(RunReplayRequest{
		SyntheticCompletion: SyntheticCompletion{Conclusion: "success"},
		OverrideWorkflow: &WorkflowReplayOverride{
			Phases: []PhaseSpec{{Name: "implement", Verify: false}},
			Budget: budget.Config{Total: 10},
		},
	})
	req := httptest.NewRequest("POST", "/v1/projects/proj/issues/1/runs/r1/replay", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result ReplayResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.WorkflowSource != "override" {
		t.Errorf("expected workflow_source=override, got %s", result.WorkflowSource)
	}
}

func TestReplayRunDecision_RunNotFound(t *testing.T) {
	store := &fakeRunReplayStore{}
	store.runID = "run-abc"
	// run is nil → ErrNotFound from ReadRunForReplay
	h := newReplayHandlerAdmin(store)

	body, _ := json.Marshal(RunReplayRequest{SyntheticCompletion: SyntheticCompletion{Conclusion: "success"}})
	req := httptest.NewRequest("POST", "/v1/projects/proj/issues/1/runs/r1/replay", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestReplayRunDecision_NoAttempts(t *testing.T) {
	store := &fakeRunReplayStore{}
	store.runID = "run-abc"
	store.run = &RunReplayData{ID: "run-id-1", Project: "proj", WorkflowName: "wf", Attempts: nil}
	store.wf = singlePhaseWorkflow("implement", true)
	h := newReplayHandlerAdmin(store)

	body, _ := json.Marshal(RunReplayRequest{SyntheticCompletion: SyntheticCompletion{Conclusion: "success"}})
	req := httptest.NewRequest("POST", "/v1/projects/proj/issues/1/runs/r1/replay", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", w.Code)
	}
}

func TestReplayRunDecision_WorkflowNotFound(t *testing.T) {
	store := &fakeRunReplayStore{}
	store.runID = "run-abc"
	store.run = minimalRun("implement")
	// wf is nil, no override → 404
	h := newReplayHandlerAdmin(store)

	body, _ := json.Marshal(RunReplayRequest{SyntheticCompletion: SyntheticCompletion{Conclusion: "success"}})
	req := httptest.NewRequest("POST", "/v1/projects/proj/issues/1/runs/r1/replay", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

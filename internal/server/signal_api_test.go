package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
	"github.com/nelsong6/glimmung/internal/domain/budget"
)

type fakeSignalStore struct {
	fakeReadStore
	result PublicSignal
	err    error
}

func (s *fakeSignalStore) EnqueueSignal(_ context.Context, _ SignalEnqueue) (PublicSignal, error) {
	if s.err != nil {
		return PublicSignal{}, s.err
	}
	return s.result, nil
}

func TestCreateSignal(t *testing.T) {
	store := &fakeSignalStore{result: PublicSignal{
		Ref:        "signal:pr:owner/repo:42:2026-01-01T00:00:00Z",
		TargetType: "pr",
		TargetRepo: "owner/repo",
		TargetRef:  "42",
		Source:     "glimmung_ui",
		State:      "pending",
		EnqueuedAt: time.Now(),
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	body := `{"target_type":"pr","target_repo":"owner/repo","target_ref":"42","source":"glimmung_ui"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/signals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"target_type":"pr"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCreateSignalValidates(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeSignalStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	cases := []struct {
		body   string
		status int
		desc   string
	}{
		{`{"target_repo":"owner/repo","target_ref":"main"}`, http.StatusBadRequest, "missing target_type"},
		{`{"target_type":"pr","target_ref":"main"}`, http.StatusBadRequest, "missing target_repo"},
		{`{"target_type":"pr","target_repo":"owner/repo"}`, http.StatusBadRequest, "missing target_ref"},
		{`{"target_type":"pr","target_repo":"owner/repo","target_ref":"main"}`, http.StatusBadRequest, "invalid pr target_ref"},
		{`{"target_type":"pr","target_repo":"glimmung","target_ref":"42"}`, http.StatusBadRequest, "invalid pr target_repo"},
		{`{"target_type":"pr","target_repo":"owner/repo","target_ref":"42","source":"bad_source"}`, http.StatusBadRequest, "invalid source"},
		{`{"target_type":"bad","target_repo":"owner/repo","target_ref":"main"}`, http.StatusBadRequest, "invalid target_type"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/signals", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer admin")
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.desc, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateSignalNotFound(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeSignalStore{err: ErrNotFound}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"target_type":"issue","target_repo":"myproject","target_ref":"myproject#999"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/signals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateSignalRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/signals", strings.NewReader(`{}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type fakeSignalDrainStore struct {
	fakeReadStore
	pending           []QueuedSignal
	processedDecision string
	prLockReleased    bool
	issueLockClaimed  bool
	reopened          bool
}

func (s *fakeSignalDrainStore) ListPendingSignals(context.Context, int) ([]QueuedSignal, error) {
	return s.pending, nil
}

func (s *fakeSignalDrainStore) MarkSignalProcessing(_ context.Context, signal QueuedSignal) (QueuedSignal, bool, error) {
	signal.State = "processing"
	return signal, true, nil
}

func (s *fakeSignalDrainStore) MarkSignalProcessed(_ context.Context, signal QueuedSignal, decision string) (QueuedSignal, error) {
	s.processedDecision = decision
	signal.State = "processed"
	return signal, nil
}

func (s *fakeSignalDrainStore) MarkSignalFailed(context.Context, QueuedSignal, string) error {
	return nil
}

func (s *fakeSignalDrainStore) ClaimLock(context.Context, string, string, string, int, map[string]any) error {
	return nil
}

func (s *fakeSignalDrainStore) ReleaseLock(_ context.Context, scope, _, _ string) bool {
	if scope == "pr" {
		s.prLockReleased = true
	}
	return true
}

func (s *fakeSignalDrainStore) FindRunForPR(context.Context, string, int) (RunReplayData, error) {
	pr := 42
	callback := "tok"
	return RunReplayData{
		ID:                "run-1",
		Project:           "glimmung",
		WorkflowName:      "agent",
		IssueRepo:         "owner/repo",
		IssueNumber:       7,
		PRNumber:          &pr,
		CallbackToken:     &callback,
		Budget:            defaultBudgetForTest(),
		CumulativeCostUSD: 1,
		Attempts:          []RunAttemptData{{AttemptIndex: 0, Phase: "impl", Completed: true}},
	}, nil
}

func (s *fakeSignalDrainStore) GetWorkflowByName(context.Context, string, string) (*Workflow, error) {
	return &Workflow{
		Name: "agent",
		Phases: []PhaseSpec{{
			Name: "impl",
			Kind: "k8s_job",
		}},
		PR: PrPrimitive{RecyclePolicy: &RecyclePolicy{MaxAttempts: 3, LandsAt: "impl"}},
	}, nil
}

func (s *fakeSignalDrainStore) ClaimIssueLock(context.Context, string, int, string, int) error {
	s.issueLockClaimed = true
	return nil
}

func (s *fakeSignalDrainStore) ReleaseIssueLock(context.Context, string, int, string) {}

func (s *fakeSignalDrainStore) ReopenRunForTriage(_ context.Context, req TriageReopenRequest) (RunReplayData, int, error) {
	s.reopened = true
	pr := 42
	callback := "tok"
	return RunReplayData{
		ID:            req.RunID,
		Project:       req.Project,
		WorkflowName:  "agent",
		IssueRepo:     "owner/repo",
		IssueNumber:   7,
		PRNumber:      &pr,
		CallbackToken: &callback,
	}, 1, nil
}

func (s *fakeSignalDrainStore) AcquireLease(context.Context, LeaseAcquireRequest) (Lease, error) {
	return Lease{Project: "glimmung", Host: stringPtr("native-k8s"), State: "claimed", Metadata: map[string]any{"native_k8s": true}}, nil
}

func (s *fakeSignalDrainStore) AbortRunByID(context.Context, string, string, string) (AbortRunResult, error) {
	return AbortRunResult{}, nil
}

func defaultBudgetForTest() budget.Config {
	return budget.Config{Total: 10}
}

func TestDrainSignalsDispatchesRequestChangesTriage(t *testing.T) {
	store := &fakeSignalDrainStore{pending: []QueuedSignal{{
		ID:         "signal-1",
		TargetType: "pr",
		TargetRepo: "owner/repo",
		TargetID:   "42",
		Source:     "glimmung_ui",
		Payload:    map[string]any{"kind": "reject", "feedback": "fix it"},
		State:      "pending",
		EnqueuedAt: time.Now(),
	}}}

	launcher := &fakeNativeLauncher{}
	result, err := DrainSignals(context.Background(), store, launcher, 10)
	if err != nil {
		t.Fatalf("DrainSignals: %v", err)
	}
	if launcher.called {
		t.Fatalf("signal drain must not launch native work directly: %#v", launcher.req)
	}
	if result.Processed != 1 || store.processedDecision != triageDispatch {
		t.Fatalf("result=%#v decision=%q", result, store.processedDecision)
	}
	if !store.issueLockClaimed || !store.reopened {
		t.Fatalf("issueLockClaimed=%v reopened=%v", store.issueLockClaimed, store.reopened)
	}
	if store.prLockReleased {
		t.Fatal("PR lock should stay held until the triage run reaches terminal state")
	}
}

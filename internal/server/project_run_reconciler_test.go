package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeProjectRunQueueStore struct {
	fakeReadStore

	queued []QueuedProjectRun
	wf     *Workflow

	leaseResult Lease
	leaseErr    error
	leaseReq    *LeaseAcquireRequest
	pending     []LaunchPendingProjectRun

	startReq *StartQueuedRunRequest
	startErr error

	cancelledLease string
	failedRunID    string
	failedReason   string
	launching      *RunAttemptLaunchStateRequest
	launched       *RunAttemptLaunchedRequest
	launchFailed   *RunAttemptLaunchStateRequest
}

func (s *fakeProjectRunQueueStore) GetWorkflowByName(context.Context, string, string) (*Workflow, error) {
	return s.wf, nil
}

func (s *fakeProjectRunQueueStore) ListQueuedProjectRuns(context.Context, string) ([]QueuedProjectRun, error) {
	return append([]QueuedProjectRun{}, s.queued...), nil
}

func (s *fakeProjectRunQueueStore) AcquireLease(_ context.Context, req LeaseAcquireRequest) (Lease, error) {
	s.leaseReq = &req
	return s.leaseResult, s.leaseErr
}

func (s *fakeProjectRunQueueStore) ReadLeaseByRef(context.Context, string, string) (Lease, error) {
	return s.leaseResult, nil
}

func (s *fakeProjectRunQueueStore) ListLaunchPendingProjectRuns(context.Context, string) ([]LaunchPendingProjectRun, error) {
	return append([]LaunchPendingProjectRun{}, s.pending...), nil
}

func (s *fakeProjectRunQueueStore) CancelLeaseByRef(_ context.Context, _ string, ref string) (CancelLeaseResult, error) {
	s.cancelledLease = ref
	return CancelLeaseResult{State: "released", LeaseRef: ref}, nil
}

func (s *fakeProjectRunQueueStore) StartQueuedRun(_ context.Context, req StartQueuedRunRequest) (RunReplayData, error) {
	s.startReq = &req
	if s.startErr != nil {
		return RunReplayData{}, s.startErr
	}
	run := s.queued[0]
	return RunReplayData{
		ID:               run.ID,
		Project:          run.Project,
		WorkflowName:     run.WorkflowName,
		IssueNumber:      run.IssueNumber,
		RunNumber:        run.RunNumber,
		RunDisplayNumber: run.RunDisplayNumber,
		IssueRepo:        run.IssueRepo,
		CallbackToken:    run.CallbackToken,
		Attempts: []RunAttemptData{{
			AttemptIndex: 0,
			Phase:        req.Phase.Name,
		}},
	}, nil
}

func (s *fakeProjectRunQueueStore) FailQueuedRunAdmission(_ context.Context, project, runID, reason string) (AbortRunResult, error) {
	s.failedRunID = runID
	s.failedReason = reason
	return AbortRunResult{State: "failed_to_start", RunRef: project + "#1/runs/1"}, nil
}

func (s *fakeProjectRunQueueStore) MarkRunAttemptLaunching(_ context.Context, req RunAttemptLaunchStateRequest) error {
	s.launching = &req
	return nil
}

func (s *fakeProjectRunQueueStore) MarkRunAttemptLaunched(_ context.Context, req RunAttemptLaunchedRequest) error {
	s.launched = &req
	return nil
}

func (s *fakeProjectRunQueueStore) MarkRunAttemptLaunchFailed(_ context.Context, req RunAttemptLaunchStateRequest) error {
	s.launchFailed = &req
	return nil
}

func TestReconcileProjectRunQueueAdmitsAndLaunchesEntryPhase(t *testing.T) {
	wf := minimalDispatchStore().wf
	leaseNum := 7
	runNumber := 1
	display := "1"
	callbackToken := "run-callback"
	holder := "issue-lock-holder"
	store := &fakeProjectRunQueueStore{
		wf: wf,
		queued: []QueuedProjectRun{{
			ID:                "run-1",
			Project:           "proj",
			WorkflowName:      wf.Name,
			WorkflowSnapshot:  *wf,
			IssueRepo:         "owner/repo",
			IssueNumber:       42,
			RunNumber:         &runNumber,
			RunDisplayNumber:  &display,
			CallbackToken:     &callbackToken,
			IssueLockHolderID: &holder,
			CreatedAt:         time.Now(),
		}},
		leaseResult: Lease{
			ID:          "lease-1",
			Project:     "proj",
			LeaseNumber: &leaseNum,
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"native_k8s":           true,
				"native_slot_index":    "1",
				"native_slot_name":     "proj-1",
				"lease_callback_token": "lease-callback",
			},
		},
	}
	launcher := &fakeNativeLauncher{}

	result, err := ReconcileProjectRunQueue(context.Background(), store, launcher, "proj")
	if err != nil {
		t.Fatalf("ReconcileProjectRunQueue: %v", err)
	}
	if result.Admitted != 1 || result.Launched != 1 || result.CapacityBlocked {
		t.Fatalf("result=%#v", result)
	}
	if store.leaseReq == nil || store.leaseReq.Metadata["glimmung_run_test_slot"] != true || store.leaseReq.Metadata["reserved_unprovisioned"] != true {
		t.Fatalf("lease request=%#v", store.leaseReq)
	}
	if store.startReq == nil || store.startReq.Phase.Name != "prep" {
		t.Fatalf("start request=%#v", store.startReq)
	}
	if !launcher.called || launcher.req.Run.ID != "run-1" || launcher.req.Phase.Name != "prep" {
		t.Fatalf("launch request=%#v", launcher.req)
	}
}

func TestReconcileProjectRunQueueStopsWhenCapacityUnavailable(t *testing.T) {
	wf := minimalDispatchStore().wf
	store := &fakeProjectRunQueueStore{
		wf: wf,
		queued: []QueuedProjectRun{{
			ID:               "run-1",
			Project:          "proj",
			WorkflowName:     wf.Name,
			WorkflowSnapshot: *wf,
			IssueNumber:      42,
		}},
		leaseErr: ErrUnavailable,
	}
	launcher := &fakeNativeLauncher{}

	result, err := ReconcileProjectRunQueue(context.Background(), store, launcher, "proj")
	if err != nil {
		t.Fatalf("ReconcileProjectRunQueue: %v", err)
	}
	if !result.CapacityBlocked || result.Admitted != 0 || launcher.called {
		t.Fatalf("result=%#v launcher.called=%v", result, launcher.called)
	}
	if store.failedRunID != "" {
		t.Fatalf("run should stay queued when capacity is unavailable: %s", store.failedRunID)
	}
}

func TestReconcileProjectRunQueueFailsInvalidSnapshotBeforeLease(t *testing.T) {
	store := &fakeProjectRunQueueStore{
		queued: []QueuedProjectRun{{
			ID:               "run-1",
			Project:          "proj",
			WorkflowName:     "main",
			WorkflowSnapshot: Workflow{Name: "main", Project: "proj"},
			IssueNumber:      42,
		}},
	}

	result, err := ReconcileProjectRunQueue(context.Background(), store, &fakeNativeLauncher{}, "proj")
	if err != nil {
		t.Fatalf("ReconcileProjectRunQueue: %v", err)
	}
	if result.FailedToStart != 1 || store.failedRunID != "run-1" {
		t.Fatalf("result=%#v failed=%s", result, store.failedRunID)
	}
	if store.leaseReq != nil {
		t.Fatalf("invalid snapshot must fail before lease request: %#v", store.leaseReq)
	}
}

func TestReconcileProjectRunQueueReleasesLeaseWhenStartLosesRace(t *testing.T) {
	wf := minimalDispatchStore().wf
	store := &fakeProjectRunQueueStore{
		wf: wf,
		queued: []QueuedProjectRun{{
			ID:               "run-1",
			Project:          "proj",
			WorkflowName:     wf.Name,
			WorkflowSnapshot: *wf,
			IssueNumber:      42,
		}},
		leaseResult: Lease{
			ID:      "lease-1",
			Project: "proj",
			State:   "claimed",
			Metadata: map[string]any{
				"native_slot_index": "1",
				"native_slot_name":  "proj-1",
			},
		},
		startErr: ErrConflict,
	}

	result, err := ReconcileProjectRunQueue(context.Background(), store, &fakeNativeLauncher{}, "proj")
	if err != nil {
		t.Fatalf("ReconcileProjectRunQueue: %v", err)
	}
	if result.Admitted != 0 || store.cancelledLease == "" {
		t.Fatalf("result=%#v cancelled=%q", result, store.cancelledLease)
	}
}

func TestReconcileProjectRunQueueReleasesLeaseWhenLaunchFails(t *testing.T) {
	wf := minimalDispatchStore().wf
	store := &fakeProjectRunQueueStore{
		wf: wf,
		queued: []QueuedProjectRun{{
			ID:               "run-1",
			Project:          "proj",
			WorkflowName:     wf.Name,
			WorkflowSnapshot: *wf,
			IssueNumber:      42,
		}},
		leaseResult: Lease{
			ID:      "lease-1",
			Project: "proj",
			State:   "claimed",
			Metadata: map[string]any{
				"native_slot_index": "1",
				"native_slot_name":  "proj-1",
			},
		},
	}

	result, err := ReconcileProjectRunQueue(context.Background(), store, &fakeNativeLauncher{err: errors.New("kube failed")}, "proj")
	if err != nil {
		t.Fatalf("ReconcileProjectRunQueue: %v", err)
	}
	if result.FailedToStart != 1 || store.cancelledLease == "" || store.failedRunID != "run-1" {
		t.Fatalf("result=%#v cancelled=%q failed=%q", result, store.cancelledLease, store.failedRunID)
	}
}

func TestReconcileProjectRunQueueLaunchesPendingAttemptFromRunLease(t *testing.T) {
	wf := minimalDispatchStore().wf
	leaseNum := 4
	store := &fakeProjectRunQueueStore{
		wf: wf,
		leaseResult: Lease{
			ID:          "lease-4",
			Project:     "proj",
			LeaseNumber: &leaseNum,
			State:       "claimed",
			Metadata: map[string]any{
				"native_slot_index": "1",
				"native_slot_name":  "proj-1",
			},
		},
		pending: []LaunchPendingProjectRun{{
			Run: RunReplayData{
				ID:               "run-1",
				Project:          "proj",
				WorkflowName:     wf.Name,
				IssueNumber:      42,
				RunDisplayNumber: stringPtr("1"),
				Attempts: []RunAttemptData{{
					AttemptIndex: 1,
					Phase:        "verify",
				}},
			},
			WorkflowSnapshot: *wf,
			LeaseRef:         "proj/test-slots/proj-1/leases/4",
			AttemptIndex:     1,
			Phase:            wf.Phases[1],
			PhaseKind:        "k8s_job",
			WorkflowFilename: "k8s_job:verify",
			PhaseInputs:      map[string]string{"validation_url": "https://preview.example"},
		}},
	}
	launcher := &fakeNativeLauncher{}

	result, err := ReconcileProjectRunQueue(context.Background(), store, launcher, "proj")
	if err != nil {
		t.Fatalf("ReconcileProjectRunQueue: %v", err)
	}
	if result.Launched != 1 || store.launching == nil || store.launched == nil {
		t.Fatalf("result=%#v launching=%#v launched=%#v", result, store.launching, store.launched)
	}
	if !launcher.called || launcher.req.Phase.Name != "verify" {
		t.Fatalf("launch request=%#v", launcher.req)
	}
	if got := launcher.req.Lease.Metadata["attempt_index"]; got != "1" {
		t.Fatalf("attempt metadata=%#v", launcher.req.Lease.Metadata)
	}
	phaseInputs, ok := launcher.req.Lease.Metadata["phase_inputs"].(map[string]string)
	if !ok || phaseInputs["validation_url"] != "https://preview.example" {
		t.Fatalf("phase inputs=%#v", launcher.req.Lease.Metadata["phase_inputs"])
	}
}

func TestRecoverProjectRunQueuesReconcilesProjects(t *testing.T) {
	wf := minimalDispatchStore().wf
	store := &fakeProjectRunQueueStore{
		fakeReadStore: fakeReadStore{projects: []Project{{Name: "proj"}}},
		wf:            wf,
		queued: []QueuedProjectRun{{
			ID:               "run-1",
			Project:          "proj",
			WorkflowName:     wf.Name,
			WorkflowSnapshot: *wf,
			IssueNumber:      42,
		}},
		leaseResult: Lease{
			ID:      "lease-1",
			Project: "proj",
			State:   "claimed",
			Metadata: map[string]any{
				"native_slot_index": "1",
				"native_slot_name":  "proj-1",
			},
		},
	}
	launcher := &fakeNativeLauncher{}

	RecoverProjectRunQueues(context.Background(), store, launcher, nil)
	if !launcher.called {
		t.Fatal("startup recovery did not reconcile queued run")
	}
}

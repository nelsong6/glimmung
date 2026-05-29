package server

import (
	"context"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/budget"
)

type fakeRunDispatchTimeoutStore struct {
	projects []Project
	runs     []RunReport
	aborted  []string
}

func (s *fakeRunDispatchTimeoutStore) ListProjects(context.Context) ([]Project, error) {
	return s.projects, nil
}

func (s *fakeRunDispatchTimeoutStore) ListProjectRuns(_ context.Context, project string, _ int) ([]RunReport, error) {
	out := make([]RunReport, 0, len(s.runs))
	for _, run := range s.runs {
		if run.Project == project {
			out = append(out, run)
		}
	}
	return out, nil
}

func (s *fakeRunDispatchTimeoutStore) AbortRunByID(_ context.Context, project, runID, reason string) (AbortRunResult, error) {
	s.aborted = append(s.aborted, project+"/"+runID+"/"+reason)
	return AbortRunResult{State: "aborted"}, nil
}

func TestExpireRunDispatchTimeoutsAbortsStaleDispatchingPhase(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-11 * time.Minute).Format(time.RFC3339Nano)
	recent := now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
	store := &fakeRunDispatchTimeoutStore{
		projects: []Project{{ID: "glimmung"}},
		runs: []RunReport{
			{
				ID:      "run-stale",
				Project: "glimmung",
				State:   "in_progress",
				PhaseExecutions: []RunPhaseExecution{{
					Name:         "env-prep",
					State:        "dispatching",
					DispatchedAt: &stale,
				}},
			},
			{
				ID:      "run-recent",
				Project: "glimmung",
				State:   "in_progress",
				PhaseExecutions: []RunPhaseExecution{{
					Name:         "env-prep",
					State:        "dispatching",
					DispatchedAt: &recent,
				}},
			},
			{
				ID:      "run-active",
				Project: "glimmung",
				State:   "in_progress",
				PhaseExecutions: []RunPhaseExecution{{
					Name:         "env-prep",
					State:        "active",
					DispatchedAt: &stale,
				}},
			},
		},
	}

	expired, err := ExpireRunDispatchTimeouts(context.Background(), store, nil, 10*time.Minute, now)
	if err != nil {
		t.Fatalf("ExpireRunDispatchTimeouts: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired=%d, want 1", expired)
	}
	if len(store.aborted) != 1 || store.aborted[0] != "glimmung/run-stale/dispatch_timeout" {
		t.Fatalf("aborted=%#v", store.aborted)
	}
}

func TestExpireRunDispatchTimeoutsAbortsLegacyStaleAttempt(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	completedAt := now.Add(-time.Minute)
	store := &fakeRunDispatchTimeoutStore{
		projects: []Project{{ID: "glimmung"}},
		runs: []RunReport{
			{
				ID:      "run-legacy-stale",
				Project: "glimmung",
				State:   "in_progress",
				Attempts: []RunReportAttempt{{
					Phase:        "env-prep",
					DispatchedAt: now.Add(-11 * time.Minute),
				}},
			},
			{
				ID:      "run-legacy-recent",
				Project: "glimmung",
				State:   "in_progress",
				Attempts: []RunReportAttempt{{
					Phase:        "env-prep",
					DispatchedAt: now.Add(-2 * time.Minute),
				}},
			},
			{
				ID:      "run-legacy-complete",
				Project: "glimmung",
				State:   "in_progress",
				Attempts: []RunReportAttempt{{
					Phase:        "env-prep",
					DispatchedAt: now.Add(-11 * time.Minute),
					CompletedAt:  &completedAt,
				}},
			},
		},
	}

	expired, err := ExpireRunDispatchTimeouts(context.Background(), store, nil, 10*time.Minute, now)
	if err != nil {
		t.Fatalf("ExpireRunDispatchTimeouts: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired=%d, want 1", expired)
	}
	if len(store.aborted) != 1 || store.aborted[0] != "glimmung/run-legacy-stale/dispatch_timeout" {
		t.Fatalf("aborted=%#v", store.aborted)
	}
}

func TestCompleteDispatchTimedOutPhaseUsesCompletionPathForCleanup(t *testing.T) {
	leaseRef := "proj/leases/proj-1/1"
	store := &fakeCompletionStore{
		tokenRunID:         "r1",
		tokenProject:       "proj",
		appendIdx:          1,
		nativeExpectedJobs: []string{"env-prep"},
		leaseResult:        Lease{Project: "proj", LeaseNumber: intPtr(1), State: "claimed", Metadata: map[string]any{}},
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
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "env-prep"}}},
			{Name: "cleanup", Kind: "k8s_job", Always: true, DependsOn: []string{"env-prep"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
		Budget: budget.Config{Total: 25},
	}
	launcher := &fakeNativeLauncher{}
	run := RunReport{
		ID:      "r1",
		Project: "proj",
		State:   "in_progress",
		PhaseExecutions: []RunPhaseExecution{{
			Name:  "env-prep",
			State: "dispatching",
			Jobs:  []RunJobExecution{{ID: "env-prep", State: "dispatching"}},
		}},
	}

	completed, err := completeDispatchTimedOutPhase(context.Background(), store, launcher, run, "env-prep", 10*time.Minute)
	if err != nil {
		t.Fatalf("completeDispatchTimedOutPhase: %v", err)
	}
	if !completed {
		t.Fatal("timeout should have been completed through native completion")
	}
	if !launcher.called || launcher.req.Phase.Name != "cleanup" {
		t.Fatalf("native launch=%#v", launcher.req)
	}
}

// fakeJobStatusGetter satisfies NativeJobStatusGetter for reconciler tests.
type fakeJobStatusGetter struct {
	statuses map[string]NativeJobStatus
	calls    int
	err      error
}

func (f *fakeJobStatusGetter) GetNativeJobStatus(_ context.Context, _, name string) (NativeJobStatus, error) {
	f.calls++
	if f.err != nil {
		return NativeJobStatus{}, f.err
	}
	status, ok := f.statuses[name]
	if !ok {
		return NativeJobStatus{Found: false}, nil
	}
	return status, nil
}

func TestExpireFailedActiveJobsSynthesizesTimedOutCompletion(t *testing.T) {
	leaseRef := "proj/leases/proj-1/1"
	store := &fakeCompletionStore{
		tokenRunID:         "r1",
		tokenProject:       "proj",
		appendIdx:          1,
		nativeExpectedJobs: []string{"llm-verify"},
		leaseResult:        Lease{Project: "proj", LeaseNumber: intPtr(1), State: "claimed", Metadata: map[string]any{}},
	}
	store.run = &RunReplayData{
		ID:           "r1",
		Project:      "proj",
		WorkflowName: "wf",
		IssueNumber:  170,
		IssueRepo:    "owner/repo",
		SlotLeaseRef: &leaseRef,
		Attempts:     []RunAttemptData{{AttemptIndex: 2, Phase: "llm-verify"}},
	}
	store.wf = &Workflow{
		Project: "proj",
		Name:    "wf",
		Phases: []PhaseSpec{
			{Name: "llm-verify", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "llm-verify"}}},
			{Name: "cleanup", Kind: "k8s_job", Always: true, DependsOn: []string{"llm-verify"}, Jobs: []NativeJobSpec{{ID: "env-destroy"}}},
		},
		Budget: budget.Config{Total: 25},
	}
	launcher := &fakeNativeLauncher{}
	now := time.Date(2026, 5, 28, 18, 30, 0, 0, time.UTC)
	terminal := now.Add(-5 * time.Minute)
	jobName := "glim-proj-170-runs-1-1-2-llm-verify"
	statusGetter := &fakeJobStatusGetter{
		statuses: map[string]NativeJobStatus{
			jobName: {
				Found:              true,
				Failed:             1,
				LastTransitionTime: terminal,
				CompletionTime:     time.Time{},
				Conditions: []NativeJobCondition{
					{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit", LastTransitionTime: terminal},
				},
			},
		},
	}

	listStore := &runReportListStore{
		fakeCompletionStore: store,
		runs: []RunReport{{
			ID:      "r1",
			Project: "proj",
			State:   "in_progress",
			PhaseExecutions: []RunPhaseExecution{{
				Name:  "llm-verify",
				State: "active",
				Jobs: []RunJobExecution{{
					ID:         "llm-verify",
					State:      "active",
					K8sJobName: stringPtr(jobName),
				}},
			}},
		}},
	}

	count, err := ExpireFailedActiveJobs(context.Background(), listStore, launcher, statusGetter, "glimmung-runs", time.Minute, now)
	if err != nil {
		t.Fatalf("ExpireFailedActiveJobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("count=%d, want 1", count)
	}
	if statusGetter.calls != 1 {
		t.Fatalf("status getter calls=%d, want 1", statusGetter.calls)
	}
	if got := store.nativeCompletions["llm-verify"]; got.Conclusion != "timed_out" {
		t.Fatalf("conclusion=%q, want timed_out", got.Conclusion)
	}
	if got := store.nativeCompletions["llm-verify"]; got.TerminalReason != JobTerminalReasonBackoffExceeded {
		t.Fatalf("terminal_reason=%q, want %q", got.TerminalReason, JobTerminalReasonBackoffExceeded)
	}
	if !launcher.called || launcher.req.Phase.Name != "cleanup" {
		t.Fatalf("synthetic completion should have triggered cleanup phase; launcher.req=%#v", launcher.req)
	}
}

func TestEvaluateActiveJobFailureMapsK8sReasonToEnum(t *testing.T) {
	now := time.Date(2026, 5, 28, 18, 30, 0, 0, time.UTC)
	terminal := now.Add(-2 * time.Minute)
	cases := []struct {
		name           string
		status         NativeJobStatus
		wantReady      bool
		wantConclusion string
		wantTerminal   string
	}{
		{
			name: "DeadlineExceeded maps to deadline_exceeded",
			status: NativeJobStatus{
				Found:              true,
				Failed:             1,
				LastTransitionTime: terminal,
				Conditions: []NativeJobCondition{
					{Type: "Failed", Status: "True", Reason: "DeadlineExceeded", LastTransitionTime: terminal},
				},
			},
			wantReady:      true,
			wantConclusion: "timed_out",
			wantTerminal:   JobTerminalReasonDeadlineExceeded,
		},
		{
			name: "BackoffLimitExceeded maps to backoff_exceeded",
			status: NativeJobStatus{
				Found:              true,
				Failed:             1,
				LastTransitionTime: terminal,
				Conditions: []NativeJobCondition{
					{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded", LastTransitionTime: terminal},
				},
			},
			wantReady:      true,
			wantConclusion: "timed_out",
			wantTerminal:   JobTerminalReasonBackoffExceeded,
		},
		{
			name: "Job TTL-collected maps to pod_gone",
			status: NativeJobStatus{
				Found: false,
			},
			wantReady:      true,
			wantConclusion: "failed",
			wantTerminal:   JobTerminalReasonPodGone,
		},
		{
			name: "Completed-but-callback-lost maps to callback_lost",
			status: NativeJobStatus{
				Found:              true,
				Succeeded:          1,
				CompletionTime:     terminal,
				LastTransitionTime: terminal,
				Conditions: []NativeJobCondition{
					{Type: "Complete", Status: "True", LastTransitionTime: terminal},
				},
			},
			wantReady:      true,
			wantConclusion: "failed",
			wantTerminal:   JobTerminalReasonCallbackLost,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getter := &fakeJobStatusGetter{statuses: map[string]NativeJobStatus{"job": tc.status}}
			ready, conclusion, terminal, _, err := evaluateActiveJobFailure(context.Background(), getter, "glimmung-runs", "job", time.Minute, now)
			if err != nil {
				t.Fatalf("evaluateActiveJobFailure: %v", err)
			}
			if ready != tc.wantReady {
				t.Fatalf("ready=%v, want %v", ready, tc.wantReady)
			}
			if conclusion != tc.wantConclusion {
				t.Fatalf("conclusion=%q, want %q", conclusion, tc.wantConclusion)
			}
			if terminal != tc.wantTerminal {
				t.Fatalf("terminal=%q, want %q", terminal, tc.wantTerminal)
			}
			if !IsKnownJobTerminalReason(terminal) {
				t.Fatalf("terminal reason %q is not in the closed enum", terminal)
			}
		})
	}
}

func TestNormalizeJobTerminalReasonCollapsesUnknownInputs(t *testing.T) {
	for _, in := range []string{"", "deadline_exceeded", "backoff_exceeded", "pod_gone", "callback_lost", "job_failed", "timeout", "cancelled", "verification_failed", "verification_error", "unknown"} {
		if got := NormalizeJobTerminalReason(in); got != in {
			t.Fatalf("NormalizeJobTerminalReason(%q)=%q, want %q", in, got, in)
		}
	}
	for _, in := range []string{"unexpected", "DeadlineExceeded", "free text"} {
		if got := NormalizeJobTerminalReason(in); got != JobTerminalReasonUnknown {
			t.Fatalf("NormalizeJobTerminalReason(%q)=%q, want %q", in, got, JobTerminalReasonUnknown)
		}
	}
}

func TestExpireFailedActiveJobsRespectsGracePeriod(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	now := time.Date(2026, 5, 28, 18, 30, 0, 0, time.UTC)
	recent := now.Add(-10 * time.Second)
	jobName := "glim-proj-170-runs-1-1-2-llm-verify"
	statusGetter := &fakeJobStatusGetter{
		statuses: map[string]NativeJobStatus{
			jobName: {
				Found:              true,
				Failed:             1,
				LastTransitionTime: recent,
				Conditions: []NativeJobCondition{
					{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded", LastTransitionTime: recent},
				},
			},
		},
	}

	listStore := &runReportListStore{
		fakeCompletionStore: store,
		runs: []RunReport{{
			ID:      "r1",
			Project: "proj",
			State:   "in_progress",
			PhaseExecutions: []RunPhaseExecution{{
				Name:  "llm-verify",
				State: "active",
				Jobs:  []RunJobExecution{{ID: "llm-verify", State: "active", K8sJobName: stringPtr(jobName)}},
			}},
		}},
	}

	count, err := ExpireFailedActiveJobs(context.Background(), listStore, &fakeNativeLauncher{}, statusGetter, "glimmung-runs", time.Minute, now)
	if err != nil {
		t.Fatalf("ExpireFailedActiveJobs: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected grace period to defer completion; count=%d", count)
	}
	if _, ok := store.nativeCompletions["llm-verify"]; ok {
		t.Fatal("did not expect a synthetic completion within the grace period")
	}
}

func TestExpireFailedActiveJobsIgnoresActiveAndSucceededJobs(t *testing.T) {
	leaseRef := "proj/leases/proj-1/1"
	store := &fakeCompletionStore{
		tokenRunID:         "r1",
		tokenProject:       "proj",
		appendIdx:          1,
		nativeExpectedJobs: []string{"already-done", "still-running"},
		leaseResult:        Lease{Project: "proj", LeaseNumber: intPtr(1), State: "claimed", Metadata: map[string]any{}},
	}
	store.run = &RunReplayData{
		ID:           "r1",
		Project:      "proj",
		WorkflowName: "wf",
		IssueNumber:  170,
		IssueRepo:    "owner/repo",
		SlotLeaseRef: &leaseRef,
		Attempts:     []RunAttemptData{{AttemptIndex: 2, Phase: "llm-verify"}},
	}
	store.wf = &Workflow{
		Project: "proj",
		Name:    "wf",
		Phases: []PhaseSpec{
			{Name: "llm-verify", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "already-done"}, {ID: "still-running"}}},
			{Name: "cleanup", Kind: "k8s_job", Always: true, DependsOn: []string{"llm-verify"}, Jobs: []NativeJobSpec{{ID: "env-destroy"}}},
		},
		Budget: budget.Config{Total: 25},
	}
	now := time.Date(2026, 5, 28, 18, 30, 0, 0, time.UTC)
	terminal := now.Add(-5 * time.Minute)
	statusGetter := &fakeJobStatusGetter{
		statuses: map[string]NativeJobStatus{
			"glim-proj-still-running": {
				Found:  true,
				Active: 1,
			},
			"glim-proj-already-done": {
				Found:          true,
				Succeeded:      1,
				CompletionTime: terminal,
				Conditions: []NativeJobCondition{
					{Type: "Complete", Status: "True", LastTransitionTime: terminal},
				},
			},
		},
	}

	listStore := &runReportListStore{
		fakeCompletionStore: store,
		runs: []RunReport{{
			ID:      "r1",
			Project: "proj",
			State:   "in_progress",
			PhaseExecutions: []RunPhaseExecution{{
				Name:  "llm-verify",
				State: "active",
				Jobs: []RunJobExecution{
					{ID: "still-running", State: "active", K8sJobName: stringPtr("glim-proj-still-running")},
					{ID: "already-done", State: "active", K8sJobName: stringPtr("glim-proj-already-done")},
				},
			}},
		}},
	}

	count, err := ExpireFailedActiveJobs(context.Background(), listStore, &fakeNativeLauncher{}, statusGetter, "glimmung-runs", time.Minute, now)
	if err != nil {
		t.Fatalf("ExpireFailedActiveJobs: %v", err)
	}
	// The actively-running Job is skipped; the succeeded-without-callback
	// Job is past the grace period and gets synthesized as failed (callback lost).
	if count != 1 {
		t.Fatalf("count=%d, want 1 (succeeded-without-callback)", count)
	}
	got, ok := store.nativeCompletions["already-done"]
	if !ok {
		t.Fatal("expected synthetic completion for already-done")
	}
	if got.Conclusion != "failed" {
		t.Fatalf("conclusion=%q, want failed for callback-lost Job", got.Conclusion)
	}
}

func TestParseNativeJobStatusExtractsConditions(t *testing.T) {
	raw := map[string]any{
		"status": map[string]any{
			"active":         0,
			"succeeded":      0,
			"failed":         1,
			"completionTime": "2026-05-28T17:52:15Z",
			"conditions": []any{
				map[string]any{
					"type":               "FailureTarget",
					"status":             "True",
					"reason":             "BackoffLimitExceeded",
					"message":            "Job has reached the specified backoff limit",
					"lastTransitionTime": "2026-05-28T17:52:15Z",
				},
				map[string]any{
					"type":               "Failed",
					"status":             "True",
					"reason":             "BackoffLimitExceeded",
					"message":            "Job has reached the specified backoff limit",
					"lastTransitionTime": "2026-05-28T17:52:15Z",
				},
			},
		},
	}
	status := parseNativeJobStatus(raw)
	if !status.Found {
		t.Fatal("expected Found=true")
	}
	if status.Failed != 1 {
		t.Fatalf("Failed=%d, want 1", status.Failed)
	}
	if !status.IsTerminallyFailed() {
		t.Fatal("expected IsTerminallyFailed")
	}
	if status.IsTerminallySucceeded() {
		t.Fatal("did not expect IsTerminallySucceeded")
	}
	if got := status.FailureReason(); got != "BackoffLimitExceeded" {
		t.Fatalf("FailureReason=%q", got)
	}
	if status.TerminalTime().IsZero() {
		t.Fatal("expected non-zero TerminalTime")
	}
}

// runReportListStore wraps a fakeCompletionStore + a static list of runs to
// drive the reconciler's per-project scan. We only need the ListProjects and
// ListProjectRuns methods to satisfy RunDispatchTimeoutStore; the rest is
// inherited via embedding so the type also satisfies RunCompletionStore and
// NativeJobCompletionStore.
type runReportListStore struct {
	*fakeCompletionStore
	runs []RunReport
}

func (s *runReportListStore) store_() {}

func (s *runReportListStore) ListProjects(_ context.Context) ([]Project, error) {
	seen := map[string]struct{}{}
	out := make([]Project, 0, len(s.runs))
	for _, r := range s.runs {
		if _, ok := seen[r.Project]; ok {
			continue
		}
		seen[r.Project] = struct{}{}
		out = append(out, Project{ID: r.Project})
	}
	return out, nil
}

func (s *runReportListStore) ListProjectRuns(_ context.Context, project string, _ int) ([]RunReport, error) {
	out := make([]RunReport, 0, len(s.runs))
	for _, r := range s.runs {
		if r.Project == project {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *runReportListStore) AbortRunByID(_ context.Context, project, runID, reason string) (AbortRunResult, error) {
	if s.fakeCompletionStore == nil {
		return AbortRunResult{}, nil
	}
	return s.fakeCompletionStore.AbortRunByID(context.Background(), project, runID, reason)
}


// innerJobEventStore is a minimal NativeRunStore stand-in for the
// inner-job watcher tests. It records events that were submitted plus
// fakes ErrConflict on idempotency-key collision.
type innerJobEventStore struct {
	*runReportListStore
	events    []NativeRunEventRequest
	idSeen    map[string]bool
	recordErr error
}

func newInnerJobEventStore(store *runReportListStore) *innerJobEventStore {
	return &innerJobEventStore{runReportListStore: store, idSeen: map[string]bool{}}
}

func (s *innerJobEventStore) GetNativeRunStatusByID(_ context.Context, _, _ string) (NativeRunStatusResponse, error) {
	return NativeRunStatusResponse{}, nil
}

func (s *innerJobEventStore) ListNativeEventsByID(_ context.Context, _, _ string, _ *int, _ *string, _ *int) (NativeRunLogsResponse, error) {
	return NativeRunLogsResponse{}, nil
}

func (s *innerJobEventStore) RecordNativeEventByID(_ context.Context, project, runID string, req NativeRunEventRequest) (NativeRunEventResult, error) {
	if s.recordErr != nil {
		return NativeRunEventResult{}, s.recordErr
	}
	jobID := ""
	if req.JobID != "" {
		jobID = req.JobID
	}
	key := project + "::" + runID + "::" + jobID + "::" + strconvItoa(req.Seq)
	if s.idSeen[key] {
		return NativeRunEventResult{}, ErrConflict
	}
	s.idSeen[key] = true
	s.events = append(s.events, req)
	return NativeRunEventResult{Accepted: true, JobID: req.JobID, Seq: req.Seq}, nil
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestExpireInnerJobTerminationsEmitsForTerminalSucceededJob(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	now := time.Date(2026, 5, 29, 1, 30, 0, 0, time.UTC)
	terminal := now.Add(-5 * time.Minute)
	statusGetter := &fakeJobStatusGetter{
		statuses: map[string]NativeJobStatus{
			"agent-ve-2": {
				Found:              true,
				Succeeded:          1,
				CompletionTime:     terminal,
				LastTransitionTime: terminal,
				Conditions: []NativeJobCondition{
					{Type: "Complete", Status: "True", LastTransitionTime: terminal},
				},
			},
		},
	}
	parentStep := "run-verification"
	listStore := &runReportListStore{
		fakeCompletionStore: store,
		runs: []RunReport{{
			ID:      "r1",
			Project: "proj",
			State:   "in_progress",
			PhaseExecutions: []RunPhaseExecution{{
				Name:  "llm-verify",
				State: "active",
				InnerJobs: []InnerJobRef{{
					ParentJobID:    "llm-verify",
					ParentStepSlug: &parentStep,
					Namespace:      "ambience-slot-3",
					JobName:        "agent-ve-2",
					Intent:         "verification_agent",
					State:          "active",
					RegisteredAt:   "2026-05-29T01:00:00Z",
				}},
			}},
		}},
	}
	events := newInnerJobEventStore(listStore)

	emitted, err := ExpireInnerJobTerminations(context.Background(), events, statusGetter, time.Minute, now)
	if err != nil {
		t.Fatalf("ExpireInnerJobTerminations: %v", err)
	}
	if emitted != 1 {
		t.Fatalf("emitted=%d, want 1", emitted)
	}
	if len(events.events) != 1 {
		t.Fatalf("events=%#v", events.events)
	}
	ev := events.events[0]
	if ev.Event != "inner_job_terminated" {
		t.Fatalf("event=%q", ev.Event)
	}
	if ev.JobID != "llm-verify" {
		t.Fatalf("parent job=%q", ev.JobID)
	}
	if got := ev.Metadata["state"]; got != "succeeded" {
		t.Fatalf("state=%v", got)
	}
	if ev.Seq < 1<<30 {
		t.Fatalf("seq=%d, want >= 2^30", ev.Seq)
	}

	// Re-running the reconciler must not emit a duplicate event.
	emitted2, _ := ExpireInnerJobTerminations(context.Background(), events, statusGetter, time.Minute, now)
	if emitted2 != 0 {
		t.Fatalf("re-emission count=%d, want 0 (idempotency)", emitted2)
	}
}

func TestExpireInnerJobTerminationsEmitsForFailedConditionWithMappedReason(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	now := time.Date(2026, 5, 29, 1, 30, 0, 0, time.UTC)
	terminal := now.Add(-5 * time.Minute)
	statusGetter := &fakeJobStatusGetter{
		statuses: map[string]NativeJobStatus{
			"agent-stuck": {
				Found:              true,
				Failed:             1,
				LastTransitionTime: terminal,
				Conditions: []NativeJobCondition{
					{Type: "Failed", Status: "True", Reason: "DeadlineExceeded", LastTransitionTime: terminal},
				},
			},
		},
	}
	listStore := &runReportListStore{
		fakeCompletionStore: store,
		runs: []RunReport{{
			ID:      "r1",
			Project: "proj",
			State:   "in_progress",
			PhaseExecutions: []RunPhaseExecution{{
				Name:  "llm-verify",
				State: "active",
				InnerJobs: []InnerJobRef{{
					ParentJobID:  "llm-verify",
					Namespace:    "ambience-slot-3",
					JobName:      "agent-stuck",
					Intent:       "verification_agent",
					State:        "active",
					RegisteredAt: "2026-05-29T01:00:00Z",
				}},
			}},
		}},
	}
	events := newInnerJobEventStore(listStore)

	emitted, err := ExpireInnerJobTerminations(context.Background(), events, statusGetter, time.Minute, now)
	if err != nil {
		t.Fatalf("ExpireInnerJobTerminations: %v", err)
	}
	if emitted != 1 {
		t.Fatalf("emitted=%d", emitted)
	}
	ev := events.events[0]
	if got := ev.Metadata["state"]; got != "failed" {
		t.Fatalf("state=%v", got)
	}
	if got := ev.Metadata["reason"]; got != JobTerminalReasonDeadlineExceeded {
		t.Fatalf("reason=%v, want %q", got, JobTerminalReasonDeadlineExceeded)
	}
}

func TestExpireInnerJobTerminationsSkipsAlreadyTerminatedAndStillActive(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	now := time.Date(2026, 5, 29, 1, 30, 0, 0, time.UTC)
	statusGetter := &fakeJobStatusGetter{
		statuses: map[string]NativeJobStatus{
			"still-running": {Found: true, Active: 1},
		},
	}
	listStore := &runReportListStore{
		fakeCompletionStore: store,
		runs: []RunReport{{
			ID:      "r1",
			Project: "proj",
			State:   "in_progress",
			PhaseExecutions: []RunPhaseExecution{{
				Name:  "llm-verify",
				State: "active",
				InnerJobs: []InnerJobRef{
					// Already terminated — must skip.
					{ParentJobID: "llm-verify", Namespace: "ambience-slot-3", JobName: "done-already", State: "succeeded", RegisteredAt: "..."},
					// Still active in k8s — no terminal event yet.
					{ParentJobID: "llm-verify", Namespace: "ambience-slot-3", JobName: "still-running", State: "active", RegisteredAt: "..."},
				},
			}},
		}},
	}
	events := newInnerJobEventStore(listStore)

	emitted, err := ExpireInnerJobTerminations(context.Background(), events, statusGetter, time.Minute, now)
	if err != nil {
		t.Fatalf("ExpireInnerJobTerminations: %v", err)
	}
	if emitted != 0 {
		t.Fatalf("emitted=%d, want 0", emitted)
	}
	if statusGetter.calls != 1 {
		t.Fatalf("status getter calls=%d, want 1 (only the still-active child)", statusGetter.calls)
	}
}

func TestInnerJobTerminationSeqIsDeterministicAndAbove2to30(t *testing.T) {
	ij := InnerJobRef{Namespace: "ambience-slot-3", JobName: "agent-ve-2"}
	got1 := innerJobTerminationSeq(ij)
	got2 := innerJobTerminationSeq(ij)
	if got1 != got2 {
		t.Fatalf("seq is not deterministic: %d vs %d", got1, got2)
	}
	if got1 < 1<<30 {
		t.Fatalf("seq=%d below the reconciler base 2^30", got1)
	}
	// Different identity should produce a different seq with extremely
	// high probability (FNV-1a on different inputs).
	other := innerJobTerminationSeq(InnerJobRef{Namespace: "ambience-slot-3", JobName: "agent-other"})
	if other == got1 {
		t.Fatalf("two different inner-Jobs collided on seq=%d", got1)
	}
}

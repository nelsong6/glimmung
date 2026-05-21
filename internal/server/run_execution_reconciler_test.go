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

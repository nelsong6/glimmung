package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTempSAToken writes a fake k8s SA token to a tempfile and
// returns the path. Used by tests that drive the watcher's authed
// HTTP request through an httptest.Server.
func writeTempSAToken(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("test-token"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func TestWatchPathIncludesLabelSelectorAndBookmarks(t *testing.T) {
	outer := &k8sJobWatcher{labelSelector: watchOuterSelector}
	got := outer.watchPath("12345")
	for _, want := range []string{"watch=true", "allowWatchBookmarks=true", "resourceVersion=12345", "labelSelector="} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	// The outer selector pins both managed-by=glimmung and the
	// native-job=true qualifier that distinguishes phase Jobs from
	// test-slot installer Jobs (which also carry managed-by=glimmung).
	if !strings.Contains(got, "managed-by%3Dglimmung") || !strings.Contains(got, "native-job%3Dtrue") {
		t.Fatalf("outer selector missing phase-Job filters: %s", got)
	}
	inner := &k8sJobWatcher{labelSelector: watchInnerSelector}
	gotInner := inner.watchPath("12345")
	if !strings.Contains(gotInner, "managed-by%3Dglimmung-inner") {
		t.Fatalf("inner selector missing managed-by=glimmung-inner: %s", gotInner)
	}
}

func TestKindClassifiesOuterAndInnerByLabel(t *testing.T) {
	outer := map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/managed-by": "glimmung"}},
	}
	inner := map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/managed-by": "glimmung-inner"}},
	}
	stranger := map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/managed-by": "something-else"}},
	}
	if got := kind(outer); got != "outer" {
		t.Fatalf("outer kind=%q", got)
	}
	if got := kind(inner); got != "inner" {
		t.Fatalf("inner kind=%q", got)
	}
	if got := kind(stranger); got != "control" {
		t.Fatalf("stranger kind=%q, want control", got)
	}
}

func TestDeriveTerminalFromStatusMapsK8sReasonsToEnum(t *testing.T) {
	cases := []struct {
		name           string
		status         NativeJobStatus
		wantConclusion string
		wantReason     string
	}{
		{
			name: "Complete=True with no Failed -> callback_lost",
			status: NativeJobStatus{
				Conditions: []NativeJobCondition{{Type: "Complete", Status: "True"}},
			},
			wantConclusion: "failed",
			wantReason:     JobTerminalReasonCallbackLost,
		},
		{
			name: "Failed=True reason=DeadlineExceeded -> deadline_exceeded",
			status: NativeJobStatus{
				Conditions: []NativeJobCondition{{Type: "Failed", Status: "True", Reason: "DeadlineExceeded"}},
			},
			wantConclusion: "timed_out",
			wantReason:     JobTerminalReasonDeadlineExceeded,
		},
		{
			name: "Failed=True reason=BackoffLimitExceeded -> backoff_exceeded",
			status: NativeJobStatus{
				Conditions: []NativeJobCondition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded"}},
			},
			wantConclusion: "timed_out",
			wantReason:     JobTerminalReasonBackoffExceeded,
		},
		{
			name: "Failed=True reason= -> job_failed",
			status: NativeJobStatus{
				Conditions: []NativeJobCondition{{Type: "Failed", Status: "True"}},
			},
			wantConclusion: "failed",
			wantReason:     JobTerminalReasonJobFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conclusion, reason, _ := deriveTerminalFromStatus(tc.status, "job")
			if conclusion != tc.wantConclusion {
				t.Fatalf("conclusion=%q, want %q", conclusion, tc.wantConclusion)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason=%q, want %q", reason, tc.wantReason)
			}
		})
	}
}

func TestWatchListAndSyncDispatchesTerminalJobsAndReturnsResourceVersion(t *testing.T) {
	// Build a fake apiserver that serves a list with one terminal
	// outer Job + one still-running Job. The watcher's listAndSync
	// should call into the synthesis path only for the terminal one
	// and return the list's resourceVersion.
	terminalJob := map[string]any{
		"metadata": map[string]any{
			"name":      "glim-proj-1-runs-1-1-0-env-prep",
			"namespace": "glimmung-runs",
			"labels": map[string]any{
				"app.kubernetes.io/managed-by":  "glimmung",
				"glimmung.romaine.life/project": "proj",
				"glimmung.romaine.life/run-ref": labelValue("proj#7/runs/1.1"),
				"glimmung.romaine.life/phase":   "env-prep",
				"glimmung.romaine.life/job-id":  "env-prep",
			},
		},
		"status": map[string]any{
			"failed":         1,
			"completionTime": "2026-05-29T01:00:00Z",
			"conditions": []any{
				map[string]any{"type": "Failed", "status": "True", "reason": "DeadlineExceeded", "lastTransitionTime": "2026-05-29T01:00:00Z"},
			},
		},
	}
	runningJob := map[string]any{
		"metadata": map[string]any{
			"name":      "glim-proj-1-runs-1-1-1-llm-implement",
			"namespace": "glimmung-runs",
			"labels": map[string]any{
				"app.kubernetes.io/managed-by":  "glimmung",
				"glimmung.romaine.life/project": "proj",
				"glimmung.romaine.life/run-ref": "proj#7/runs/1.1",
				"glimmung.romaine.life/phase":   "llm-work",
				"glimmung.romaine.life/job-id":  "llm-implement",
			},
		},
		"status": map[string]any{"active": 1},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/apis/batch/v1/jobs") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("watch") == "true" {
			// Not exercised in this test.
			http.Error(w, "not implemented", 501)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"resourceVersion": "999"},
			"items":    []any{terminalJob, runningJob},
		})
	}))
	defer srv.Close()
	tokenPath := writeTempSAToken(t)

	// Wire the synthesis path through a fakeCompletionStore. The
	// run-ref label below has to round-trip through labelValue() and
	// match the runReportListStore entry's RunRef.
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
			{Name: "cleanup", Kind: "k8s_job", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"env-prep"}, Jobs: []NativeJobSpec{{ID: "env-destroy"}}},
		},
	}
	listStore := &runReportListStore{
		fakeCompletionStore: store,
		runs: []RunReport{{
			ID:      "r1",
			Project: "proj",
			Ref:     "proj#7/runs/1.1",
			RunRef:  labelValue("proj#7/runs/1.1"),
			State:   "in_progress",
			PhaseExecutions: []RunPhaseExecution{{
				Name:  "env-prep",
				State: "active",
				Jobs:  []RunJobExecution{{ID: "env-prep", State: "active"}},
			}},
		}},
	}
	events := newInnerJobEventStore(listStore)
	launcher := &fakeNativeLauncher{}

	wch := &k8sJobWatcher{
		watcherDeps: watcherDeps{
			settings: Settings{
				K8sAPIHost:     srv.URL,
				K8sSATokenPath: tokenPath,
			},
			store:           events,
			completionStore: store,
			jobStore:        store,
			eventStore:      events,
			nativeLauncher:  launcher,
			statusGetter:    &fakeJobStatusGetter{},
			namespace:       "glimmung-runs",
		},
		labelSelector: watchOuterSelector,
	}
	rv, err := wch.listAndSync(context.Background())
	if err != nil {
		t.Fatalf("listAndSync: %v", err)
	}
	if rv != "999" {
		t.Fatalf("resourceVersion=%q, want 999", rv)
	}
	got, ok := store.nativeCompletions["env-prep"]
	if !ok {
		t.Fatalf("expected env-prep to be synthesized; completions=%v", store.nativeCompletions)
	}
	if got.Conclusion != "timed_out" {
		t.Fatalf("conclusion=%q", got.Conclusion)
	}
	if got.TerminalReason != JobTerminalReasonDeadlineExceeded {
		t.Fatalf("terminal_reason=%q", got.TerminalReason)
	}
}

func TestWatchStreamDispatchesModifiedTerminalEvent(t *testing.T) {
	// Drive a Watch stream that pushes one MODIFIED event with a
	// Failed=True condition. The handler should call into the
	// synthesis path and the stream should close gracefully.
	terminalEvent := map[string]any{
		"type": "MODIFIED",
		"object": map[string]any{
			"metadata": map[string]any{
				"name":            "glim-proj-1-runs-1-1-0-env-prep",
				"namespace":       "glimmung-runs",
				"resourceVersion": "1001",
				"labels": map[string]any{
					"app.kubernetes.io/managed-by":  "glimmung",
					"glimmung.romaine.life/project": "proj",
					"glimmung.romaine.life/run-ref": labelValue("proj#7/runs/1.1"),
					"glimmung.romaine.life/phase":   "env-prep",
					"glimmung.romaine.life/job-id":  "env-prep",
				},
			},
			"status": map[string]any{
				"failed":         1,
				"completionTime": "2026-05-29T01:00:00Z",
				"conditions": []any{
					map[string]any{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded", "lastTransitionTime": "2026-05-29T01:00:00Z"},
				},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(terminalEvent)
		if flusher != nil {
			flusher.Flush()
		}
		// Close after one event by returning.
	}))
	defer srv.Close()
	tokenPath := writeTempSAToken(t)

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
			{Name: "cleanup", Kind: "k8s_job", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"env-prep"}, Jobs: []NativeJobSpec{{ID: "env-destroy"}}},
		},
	}
	listStore := &runReportListStore{
		fakeCompletionStore: store,
		runs: []RunReport{{
			ID:      "r1",
			Project: "proj",
			RunRef:  labelValue("proj#7/runs/1.1"),
			State:   "in_progress",
			PhaseExecutions: []RunPhaseExecution{{
				Name:  "env-prep",
				State: "active",
				Jobs:  []RunJobExecution{{ID: "env-prep", State: "active"}},
			}},
		}},
	}
	events := newInnerJobEventStore(listStore)
	wch := &k8sJobWatcher{
		watcherDeps: watcherDeps{
			settings: Settings{
				K8sAPIHost:     srv.URL,
				K8sSATokenPath: tokenPath,
			},
			store:           events,
			completionStore: store,
			jobStore:        store,
			eventStore:      events,
			nativeLauncher:  &fakeNativeLauncher{},
			statusGetter:    &fakeJobStatusGetter{},
			namespace:       "glimmung-runs",
		},
		labelSelector: watchOuterSelector,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := wch.watch(ctx, "999")
	if err != errWatchClosedNormally && err != nil {
		t.Fatalf("watch: %v", err)
	}
	got, ok := store.nativeCompletions["env-prep"]
	if !ok {
		t.Fatalf("synthesis did not fire; completions=%v", store.nativeCompletions)
	}
	if got.TerminalReason != JobTerminalReasonBackoffExceeded {
		t.Fatalf("terminal_reason=%q", got.TerminalReason)
	}
}

func TestWatchHandlesGoneResponse(t *testing.T) {
	// 410 Gone must surface as errWatchGone so run() re-lists.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Gone"}`))
	}))
	defer srv.Close()
	tokenPath := writeTempSAToken(t)

	wch := &k8sJobWatcher{
		watcherDeps: watcherDeps{
			settings: Settings{
				K8sAPIHost:     srv.URL,
				K8sSATokenPath: tokenPath,
			},
		},
		labelSelector: watchOuterSelector,
	}
	err := wch.watch(context.Background(), "stale")
	if err != errWatchGone {
		t.Fatalf("err=%v, want errWatchGone", err)
	}
}

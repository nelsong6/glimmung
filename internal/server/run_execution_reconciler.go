package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const defaultRunDispatchTimeoutSeconds = 600

type RunDispatchTimeoutStore interface {
	ListProjects(ctx context.Context) ([]Project, error)
	ListProjectRuns(ctx context.Context, project string, limit int) ([]RunReport, error)
	AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
}

// activeJobFailureGracePeriod gives the native runner a small window after
// k8s marks a Job terminally Failed before the reconciler synthesizes a
// completion. The completion callback may already be in flight; without a
// grace period we race and double-complete. 60s is well inside the runner's
// http retry budget.
const activeJobFailureGracePeriod = 60 * time.Second

func StartRunDispatchTimeoutReconciler(ctx context.Context, settings Settings, store ReadStore, nativeLauncher NativeLauncher, logf func(string, ...any)) {
	timeout := time.Duration(settings.NativeRunnerDispatchTimeoutSeconds) * time.Second
	timeoutStore, _ := store.(RunDispatchTimeoutStore)
	jobStatusGetter, _ := nativeLauncher.(NativeJobStatusGetter)
	if timeout <= 0 && jobStatusGetter == nil {
		return
	}
	if timeoutStore == nil {
		return
	}
	namespace := strings.TrimSpace(settings.NativeRunnerNamespace)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			if timeout > 0 {
				expired, err := ExpireRunDispatchTimeouts(ctx, timeoutStore, nativeLauncher, timeout, time.Now().UTC())
				if err != nil && logf != nil {
					logf("run dispatch-timeout reconcile failed: %v", err)
				}
				if expired > 0 && logf != nil {
					logf("run dispatch-timeout reconciled expired=%d", expired)
				}
			}
			if jobStatusGetter != nil && namespace != "" {
				completed, err := ExpireFailedActiveJobs(ctx, timeoutStore, nativeLauncher, jobStatusGetter, namespace, activeJobFailureGracePeriod, time.Now().UTC())
				if err != nil && logf != nil {
					logf("run active-job failure reconcile failed: %v", err)
				}
				if completed > 0 && logf != nil {
					logf("run active-job failure reconciled completed=%d", completed)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func ExpireRunDispatchTimeouts(ctx context.Context, store RunDispatchTimeoutStore, nativeLauncher NativeLauncher, timeout time.Duration, now time.Time) (int, error) {
	if timeout <= 0 {
		return 0, nil
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, project := range projects {
		name := firstNonEmpty(project.Name, project.ID)
		if name == "" {
			continue
		}
		runs, err := store.ListProjectRuns(ctx, name, 500)
		if err != nil {
			return expired, err
		}
		for _, run := range runs {
			if run.State != "in_progress" || run.ID == "" {
				continue
			}
			phase, ok := dispatchTimedOutPhase(run, timeout, now)
			if !ok {
				continue
			}
			completed, err := completeDispatchTimedOutPhase(ctx, store, nativeLauncher, run, phase, timeout)
			if err != nil {
				return expired, err
			}
			if !completed {
				if _, err := store.AbortRunByID(ctx, run.Project, run.ID, "dispatch_timeout"); err != nil {
					return expired, err
				}
			}
			expired++
		}
	}
	return expired, nil
}

func completeDispatchTimedOutPhase(ctx context.Context, store RunDispatchTimeoutStore, nativeLauncher NativeLauncher, run RunReport, phaseName string, timeout time.Duration) (bool, error) {
	completionStore, ok := any(store).(RunCompletionStore)
	if !ok || completionStore == nil {
		return false, nil
	}
	jobStore, ok := any(store).(NativeJobCompletionStore)
	if !ok || jobStore == nil || nativeLauncher == nil {
		return false, nil
	}
	jobIDs := timedOutJobIDs(run, phaseName)
	if len(jobIDs) == 0 {
		return false, nil
	}
	summary := fmt.Sprintf("native phase %q exceeded dispatch timeout after %s", phaseName, timeout)
	var ready *NativeJobCompletionResult
	for _, id := range jobIDs {
		jobID := id
		payload := CompletionPayload{
			JobID:           &jobID,
			Conclusion:      "timed_out",
			SummaryMarkdown: &summary,
		}
		result, err := jobStore.RecordNativeJobCompletion(ctx, run.Project, run.ID, payload)
		if err != nil {
			return false, err
		}
		if result.CompletionReady {
			ready = &result
		}
	}
	if ready == nil {
		return true, nil
	}
	_, err := processSyntheticRunCompletion(ctx, completionStore, nativeLauncher, run.Project, run.ID, ready.PhasePayload)
	return true, err
}

func timedOutJobIDs(run RunReport, phaseName string) []string {
	for _, phase := range run.PhaseExecutions {
		if phase.Name != phaseName {
			continue
		}
		ids := make([]string, 0, len(phase.Jobs))
		for _, job := range phase.Jobs {
			switch job.State {
			case "", "not_started", "dispatching", "active":
				if job.ID != "" {
					ids = append(ids, job.ID)
				}
			}
		}
		return ids
	}
	return nil
}

func processSyntheticRunCompletion(ctx context.Context, store RunCompletionStore, nativeLauncher NativeLauncher, project, runID string, payload CompletionPayload) (*RunCallbackResult, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	w := &captureResponseWriter{header: http.Header{}}
	result := processRunCompletion(ctx, w, req, store, nativeLauncher, project, runID, payload)
	if result == nil {
		status := w.status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		return nil, fmt.Errorf("synthetic completion failed with HTTP %d: %s", status, w.body.String())
	}
	return result, nil
}

type captureResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *captureResponseWriter) Header() http.Header {
	return w.header
}

func (w *captureResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

func (w *captureResponseWriter) WriteHeader(status int) {
	w.status = status
}

func dispatchTimedOutPhase(run RunReport, timeout time.Duration, now time.Time) (string, bool) {
	for _, phase := range run.PhaseExecutions {
		if phase.State != "dispatching" {
			continue
		}
		dispatchedAt, ok := phaseDispatchTime(phase)
		if !ok {
			continue
		}
		if now.Sub(dispatchedAt) >= timeout {
			return phase.Name, true
		}
	}
	if len(run.PhaseExecutions) > 0 || len(run.Attempts) == 0 {
		return "", false
	}
	latest := run.Attempts[len(run.Attempts)-1]
	if latest.CompletedAt != nil {
		return "", false
	}
	if now.Sub(latest.DispatchedAt) >= timeout {
		return firstNonEmpty(latest.Phase, "phase"), true
	}
	return "", false
}

func phaseDispatchTime(phase RunPhaseExecution) (time.Time, bool) {
	if phase.DispatchedAt != nil && *phase.DispatchedAt != "" {
		if ts, err := time.Parse(time.RFC3339Nano, *phase.DispatchedAt); err == nil {
			return ts, true
		}
	}
	if phase.CreatedAt != "" {
		if ts, err := time.Parse(time.RFC3339Nano, phase.CreatedAt); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// ExpireFailedActiveJobs walks in-progress runs and, for each active job
// whose backing k8s Job has terminally failed without a completion callback,
// synthesizes a failed completion through the same path the native runner
// would have used. Without this, a runner pod killed mid-step (DeadlineExceeded,
// OOM, eviction) leaves the run permanently "in_progress" with the phase
// showing as "active" — invisible to dashboards and gates, and impossible for
// the verify-loop budget to retry. See run-execution-reconciler design notes.
func ExpireFailedActiveJobs(ctx context.Context, store RunDispatchTimeoutStore, nativeLauncher NativeLauncher, statusGetter NativeJobStatusGetter, namespace string, grace time.Duration, now time.Time) (int, error) {
	if statusGetter == nil {
		return 0, nil
	}
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return 0, nil
	}
	completionStore, _ := any(store).(RunCompletionStore)
	jobStore, _ := any(store).(NativeJobCompletionStore)
	if completionStore == nil || jobStore == nil {
		return 0, nil
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, project := range projects {
		name := firstNonEmpty(project.Name, project.ID)
		if name == "" {
			continue
		}
		runs, err := store.ListProjectRuns(ctx, name, 500)
		if err != nil {
			return completed, err
		}
		for _, run := range runs {
			if run.State != "in_progress" || run.ID == "" {
				continue
			}
			n, err := reconcileFailedActiveJobsForRun(ctx, completionStore, jobStore, nativeLauncher, statusGetter, run, namespace, grace, now)
			if err != nil {
				return completed, err
			}
			completed += n
		}
	}
	return completed, nil
}

func reconcileFailedActiveJobsForRun(ctx context.Context, completionStore RunCompletionStore, jobStore NativeJobCompletionStore, nativeLauncher NativeLauncher, statusGetter NativeJobStatusGetter, run RunReport, namespace string, grace time.Duration, now time.Time) (int, error) {
	completed := 0
	for _, phase := range run.PhaseExecutions {
		// Only "active" phases can have jobs stuck without callbacks.
		// "dispatching" is handled by ExpireRunDispatchTimeouts.
		if phase.State != "active" {
			continue
		}
		for _, job := range phase.Jobs {
			if job.State != "active" {
				continue
			}
			if job.K8sJobName == nil || strings.TrimSpace(*job.K8sJobName) == "" {
				continue
			}
			ready, conclusion, summary, err := evaluateActiveJobFailure(ctx, statusGetter, namespace, *job.K8sJobName, grace, now)
			if err != nil {
				return completed, err
			}
			if !ready {
				continue
			}
			jobID := job.ID
			payload := CompletionPayload{
				JobID:           &jobID,
				Conclusion:      conclusion,
				SummaryMarkdown: &summary,
			}
			result, err := jobStore.RecordNativeJobCompletion(ctx, run.Project, run.ID, payload)
			if err != nil {
				return completed, err
			}
			completed++
			if result.CompletionReady {
				if _, err := processSyntheticRunCompletion(ctx, completionStore, nativeLauncher, run.Project, run.ID, result.PhasePayload); err != nil {
					return completed, err
				}
			}
		}
	}
	return completed, nil
}

// evaluateActiveJobFailure asks k8s for the Job's status and, if it is past
// the grace period and terminally failed (or has been garbage-collected from
// k8s entirely), returns the synthetic completion conclusion plus a summary
// the dashboard can render.
func evaluateActiveJobFailure(ctx context.Context, statusGetter NativeJobStatusGetter, namespace, name string, grace time.Duration, now time.Time) (bool, string, string, error) {
	status, err := statusGetter.GetNativeJobStatus(ctx, namespace, name)
	if err != nil {
		return false, "", "", err
	}
	if !status.Found {
		// k8s no longer has the Job (TTL-collected). The run lost its
		// execution surface and cannot be observed further — fail it so
		// the verify loop or cleanup phase can run.
		summary := fmt.Sprintf("native job %q was garbage-collected from kubernetes without a completion callback", name)
		return true, "failed", summary, nil
	}
	if status.IsTerminallySucceeded() && !status.IsTerminallyFailed() {
		// Pod ran to completion but the callback was lost. Surface as
		// failed so the run leaves "in_progress"; cleanup phases still
		// run and a human can decide. We prefer this over silently
		// marking success because evidence/verification fields would
		// otherwise be missing.
		if grace > 0 && !status.TerminalTime().IsZero() && now.Sub(status.TerminalTime()) < grace {
			return false, "", "", nil
		}
		summary := fmt.Sprintf("native job %q completed in kubernetes but its completion callback was never received", name)
		return true, "failed", summary, nil
	}
	if !status.IsTerminallyFailed() {
		return false, "", "", nil
	}
	if grace > 0 && !status.TerminalTime().IsZero() && now.Sub(status.TerminalTime()) < grace {
		return false, "", "", nil
	}
	reason := status.FailureReason()
	message := status.FailureMessage()
	conclusion := "failed"
	switch reason {
	case "DeadlineExceeded", "BackoffLimitExceeded":
		// BackoffLimitExceeded with backoffLimit=0 most often comes from
		// a pod hitting activeDeadlineSeconds first; either way the runner
		// was killed without surfacing test evidence. Tag as timed_out so
		// dashboards distinguish wallclock kills from a runner-reported
		// failure.
		conclusion = "timed_out"
	}
	summary := fmt.Sprintf("native job %q ended with kubernetes condition Failed=true reason=%q: %s", name, reason, strings.TrimSpace(message))
	return true, conclusion, summary, nil
}

package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"
)

const defaultRunDispatchTimeoutSeconds = 600

type RunDispatchTimeoutStore interface {
	ListProjects(ctx context.Context) ([]Project, error)
	ListProjectRuns(ctx context.Context, project string, limit int) ([]RunReport, error)
	AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
}

func StartRunDispatchTimeoutReconciler(ctx context.Context, settings Settings, store ReadStore, nativeLauncher NativeLauncher, logf func(string, ...any)) {
	timeout := time.Duration(settings.NativeRunnerDispatchTimeoutSeconds) * time.Second
	if timeout <= 0 {
		return
	}
	timeoutStore, ok := store.(RunDispatchTimeoutStore)
	if !ok || timeoutStore == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			expired, err := ExpireRunDispatchTimeouts(ctx, timeoutStore, nativeLauncher, timeout, time.Now().UTC())
			if err != nil && logf != nil {
				logf("run dispatch-timeout reconcile failed: %v", err)
			}
			if expired > 0 && logf != nil {
				logf("run dispatch-timeout reconciled expired=%d", expired)
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

package server

import (
	"context"
	"time"
)

const defaultRunDispatchTimeoutSeconds = 600

type RunDispatchTimeoutStore interface {
	ListProjects(ctx context.Context) ([]Project, error)
	ListProjectRuns(ctx context.Context, project string, limit int) ([]RunReport, error)
	AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
}

func StartRunDispatchTimeoutReconciler(ctx context.Context, settings Settings, store ReadStore, logf func(string, ...any)) {
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
			expired, err := ExpireRunDispatchTimeouts(ctx, timeoutStore, timeout, time.Now().UTC())
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

func ExpireRunDispatchTimeouts(ctx context.Context, store RunDispatchTimeoutStore, timeout time.Duration, now time.Time) (int, error) {
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
			if _, ok := dispatchTimedOutPhase(run, timeout, now); !ok {
				continue
			}
			if _, err := store.AbortRunByID(ctx, run.Project, run.ID, "dispatch_timeout"); err != nil {
				return expired, err
			}
			expired++
		}
	}
	return expired, nil
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

package server

import (
	"context"
	"time"
)

const committedNativeLaunchTimeout = 2 * time.Minute

type NativeJobDispatchRecorder interface {
	RecordNativeJobsDispatched(ctx context.Context, project, runID, phase string, jobs map[string]string) error
}

func launchCommittedNativePhase(ctx context.Context, nativeLauncher NativeLauncher, req NativeLaunchRequest) ([]string, error) {
	launchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), committedNativeLaunchTimeout)
	defer cancel()
	return nativeLauncher.LaunchNativePhase(launchCtx, req)
}

func recordLaunchedNativeJobs(ctx context.Context, store any, run RunReplayData, phase PhaseSpec, launched []string) error {
	recorder, ok := store.(NativeJobDispatchRecorder)
	if !ok || recorder == nil {
		return nil
	}
	jobs := launchedNativeJobMap(phase, launched)
	if len(jobs) == 0 {
		return nil
	}
	return recorder.RecordNativeJobsDispatched(ctx, run.Project, run.ID, phase.Name, jobs)
}

func launchedNativeJobMap(phase PhaseSpec, launched []string) map[string]string {
	jobs := make(map[string]string, len(launched))
	for i, job := range phase.Jobs {
		if i >= len(launched) {
			break
		}
		if job.ID == "" || launched[i] == "" {
			continue
		}
		jobs[job.ID] = launched[i]
	}
	return jobs
}

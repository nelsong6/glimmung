package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/publicids"
)

type ProjectRunQueueStore interface {
	ReadStore
	GetWorkflowByName(ctx context.Context, project, name string) (*Workflow, error)
	ListQueuedProjectRuns(ctx context.Context, project string) ([]QueuedProjectRun, error)
	ListLaunchPendingProjectRuns(ctx context.Context, project string) ([]LaunchPendingProjectRun, error)
	AcquireLease(ctx context.Context, req LeaseAcquireRequest) (Lease, error)
	ReadLeaseByRef(ctx context.Context, project, ref string) (Lease, error)
	CancelLeaseByRef(ctx context.Context, project, ref string) (CancelLeaseResult, error)
	StartQueuedRun(ctx context.Context, req StartQueuedRunRequest) (RunReplayData, error)
	FailQueuedRunAdmission(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
	MarkRunAttemptLaunching(ctx context.Context, req RunAttemptLaunchStateRequest) error
	MarkRunAttemptLaunched(ctx context.Context, req RunAttemptLaunchedRequest) error
	MarkRunAttemptLaunchFailed(ctx context.Context, req RunAttemptLaunchStateRequest) error
}

type QueuedProjectRun struct {
	ID                string
	Project           string
	WorkflowName      string
	WorkflowSnapshot  Workflow
	IssueRepo         string
	IssueNumber       int
	RunNumber         *int
	RunDisplayNumber  *string
	CallbackToken     *string
	IssueLockHolderID *string
	EntrypointPhase   *string
	CreatedAt         time.Time
}

type StartQueuedRunRequest struct {
	Project          string
	RunID            string
	Phase            PhaseSpec
	PhaseKind        string
	WorkflowFilename string
	Lease            Lease
}

type QueueRunAttemptRequest struct {
	Project          string
	RunID            string
	Phase            PhaseSpec
	PhaseKind        string
	WorkflowFilename string
	PhaseInputs      map[string]string
	LaunchMetadata   map[string]any
}

type LaunchPendingProjectRun struct {
	Run              RunReplayData
	WorkflowSnapshot Workflow
	LeaseRef         string
	AttemptIndex     int
	Phase            PhaseSpec
	PhaseKind        string
	WorkflowFilename string
	PhaseInputs      map[string]string
	LaunchMetadata   map[string]any
	AdmissionAttempt bool
}

type RunAttemptLaunchStateRequest struct {
	Project      string
	RunID        string
	AttemptIndex int
	Reason       string
}

type RunAttemptLaunchedRequest struct {
	Project      string
	RunID        string
	AttemptIndex int
	JobNames     []string
}

type ProjectRunReconcileResult struct {
	Project         string
	QueuedSeen      int
	Admitted        int
	Launched        int
	FailedToStart   int
	CapacityBlocked bool
}

func WakeProjectRunReconciler(ctx context.Context, store ReadStore, nativeLauncher NativeLauncher, project string) {
	queueStore, ok := store.(ProjectRunQueueStore)
	if !ok || queueStore == nil || nativeLauncher == nil || strings.TrimSpace(project) == "" {
		return
	}
	go func() {
		reconcileCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		_, _ = ReconcileProjectRunQueue(reconcileCtx, queueStore, nativeLauncher, project)
	}()
}

func RecoverProjectRunQueues(ctx context.Context, store ReadStore, nativeLauncher NativeLauncher, logf func(string, ...any)) {
	queueStore, ok := store.(ProjectRunQueueStore)
	if !ok || queueStore == nil || nativeLauncher == nil {
		return
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		if logf != nil {
			logf("project run queue recovery list projects failed: %v", err)
		}
		return
	}
	for _, project := range projects {
		projectName := firstNonEmpty(project.Name, project.ID)
		if projectName == "" {
			continue
		}
		result, err := ReconcileProjectRunQueue(ctx, queueStore, nativeLauncher, projectName)
		if err != nil {
			if logf != nil {
				logf("project run queue recovery failed project=%s: %v", projectName, err)
			}
			continue
		}
		if logf != nil && (result.Admitted > 0 || result.Launched > 0 || result.FailedToStart > 0 || result.CapacityBlocked) {
			logf("project run queue recovery project=%s queued=%d admitted=%d launched=%d failed_to_start=%d capacity_blocked=%t",
				projectName, result.QueuedSeen, result.Admitted, result.Launched, result.FailedToStart, result.CapacityBlocked)
		}
	}
}

func ReconcileProjectRunQueue(ctx context.Context, store ProjectRunQueueStore, nativeLauncher NativeLauncher, project string) (ProjectRunReconcileResult, error) {
	result := ProjectRunReconcileResult{Project: project}
	if store == nil {
		return result, fmt.Errorf("project run queue store not configured")
	}
	if nativeLauncher == nil {
		return result, fmt.Errorf("native launcher not configured")
	}
	queued, err := store.ListQueuedProjectRuns(ctx, project)
	if err != nil {
		return result, err
	}
	result.QueuedSeen = len(queued)
	for _, run := range queued {
		wf := run.WorkflowSnapshot
		if strings.TrimSpace(wf.Name) == "" || len(wf.Phases) == 0 {
			_, _ = store.FailQueuedRunAdmission(ctx, run.Project, run.ID, "queued_run_missing_workflow_snapshot")
			result.FailedToStart++
			continue
		}
		if err := ValidateWorkflowRegister(WorkflowRegister{
			Project:             wf.Project,
			Name:                wf.Name,
			Phases:              wf.Phases,
			PR:                  wf.PR,
			Budget:              wf.Budget,
			DefaultRequirements: wf.DefaultRequirements,
			Metadata:            wf.Metadata,
		}); err != nil {
			_, _ = store.FailQueuedRunAdmission(ctx, run.Project, run.ID, "workflow_snapshot_invalid: "+err.Error())
			result.FailedToStart++
			continue
		}
		entry, ok := workflowAdmissionPhase(wf.Phases, run.EntrypointPhase)
		if !ok {
			_, _ = store.FailQueuedRunAdmission(ctx, run.Project, run.ID, "workflow_snapshot_has_no_entry_phase")
			result.FailedToStart++
			continue
		}
		phaseKind := workflowPhaseKind(entry.Kind)
		if err := validateNativeWorkflowKind(phaseKind); err != nil {
			_, _ = store.FailQueuedRunAdmission(ctx, run.Project, run.ID, err.Error())
			result.FailedToStart++
			continue
		}
		workflowFilename := entry.WorkflowFilename
		if workflowFilename == "" {
			workflowFilename = fmt.Sprintf("%s:%s", phaseKind, entry.Name)
		}
		lease, err := reserveRunTestSlot(ctx, store, run, wf, entry)
		if err != nil {
			if errors.Is(err, ErrUnavailable) {
				result.CapacityBlocked = true
				break
			}
			return result, err
		}
		started, err := store.StartQueuedRun(ctx, StartQueuedRunRequest{
			Project:          run.Project,
			RunID:            run.ID,
			Phase:            entry,
			PhaseKind:        phaseKind,
			WorkflowFilename: workflowFilename,
			Lease:            lease,
		})
		if err != nil {
			_, _ = store.CancelLeaseByRef(ctx, run.Project, LeasePublicRefFromLease(lease))
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
				continue
			}
			return result, err
		}
		result.Admitted++
		launch := LaunchPendingProjectRun{
			Run:              started,
			WorkflowSnapshot: wf,
			LeaseRef:         LeasePublicRefFromLease(lease),
			AttemptIndex:     latestAttemptIndex(started),
			Phase:            entry,
			PhaseKind:        phaseKind,
			WorkflowFilename: workflowFilename,
			AdmissionAttempt: true,
		}
		launched, failedToStart, err := launchPendingRun(ctx, store, nativeLauncher, launch)
		if err != nil {
			return result, err
		}
		if launched {
			result.Launched++
		}
		if failedToStart {
			result.FailedToStart++
		}
	}

	pending, err := store.ListLaunchPendingProjectRuns(ctx, project)
	if err != nil {
		return result, err
	}
	for _, launch := range pending {
		launched, failedToStart, err := launchPendingRun(ctx, store, nativeLauncher, launch)
		if err != nil {
			return result, err
		}
		if launched {
			result.Launched++
		}
		if failedToStart {
			result.FailedToStart++
		}
	}
	return result, nil
}

func launchPendingRun(ctx context.Context, store ProjectRunQueueStore, nativeLauncher NativeLauncher, launch LaunchPendingProjectRun) (bool, bool, error) {
	if launch.AttemptIndex < 0 {
		return false, false, nil
	}
	if strings.TrimSpace(launch.LeaseRef) == "" {
		reason := "run_missing_test_slot_lease"
		if launch.AdmissionAttempt {
			_, _ = store.FailQueuedRunAdmission(ctx, launch.Run.Project, launch.Run.ID, reason)
			return false, true, nil
		}
		_ = store.MarkRunAttemptLaunchFailed(ctx, RunAttemptLaunchStateRequest{
			Project:      launch.Run.Project,
			RunID:        launch.Run.ID,
			AttemptIndex: launch.AttemptIndex,
			Reason:       reason,
		})
		return false, false, nil
	}
	if strings.TrimSpace(launch.WorkflowSnapshot.Name) == "" || len(launch.WorkflowSnapshot.Phases) == 0 {
		reason := "run_missing_workflow_snapshot"
		if launch.AdmissionAttempt {
			_, _ = store.FailQueuedRunAdmission(ctx, launch.Run.Project, launch.Run.ID, reason)
			return false, true, nil
		}
		_ = store.MarkRunAttemptLaunchFailed(ctx, RunAttemptLaunchStateRequest{
			Project:      launch.Run.Project,
			RunID:        launch.Run.ID,
			AttemptIndex: launch.AttemptIndex,
			Reason:       reason,
		})
		return false, false, nil
	}
	if strings.TrimSpace(launch.Phase.Name) == "" || len(launch.Phase.Jobs) == 0 {
		reason := "run_launch_phase_not_in_workflow_snapshot"
		if launch.AdmissionAttempt {
			_, _ = store.FailQueuedRunAdmission(ctx, launch.Run.Project, launch.Run.ID, reason)
			return false, true, nil
		}
		_ = store.MarkRunAttemptLaunchFailed(ctx, RunAttemptLaunchStateRequest{
			Project:      launch.Run.Project,
			RunID:        launch.Run.ID,
			AttemptIndex: launch.AttemptIndex,
			Reason:       reason,
		})
		return false, false, nil
	}
	lease, err := store.ReadLeaseByRef(ctx, launch.Run.Project, launch.LeaseRef)
	if err != nil {
		if errors.Is(err, ErrNotFound) && launch.AdmissionAttempt {
			_, _ = store.FailQueuedRunAdmission(ctx, launch.Run.Project, launch.Run.ID, "run_test_slot_lease_not_found")
			return false, true, nil
		}
		return false, false, err
	}
	if lease.State != "claimed" {
		reason := "run_test_slot_lease_not_claimed"
		if launch.AdmissionAttempt {
			_, _ = store.FailQueuedRunAdmission(ctx, launch.Run.Project, launch.Run.ID, reason)
			return false, true, nil
		}
		_ = store.MarkRunAttemptLaunchFailed(ctx, RunAttemptLaunchStateRequest{
			Project:      launch.Run.Project,
			RunID:        launch.Run.ID,
			AttemptIndex: launch.AttemptIndex,
			Reason:       reason,
		})
		return false, false, nil
	}
	if err := store.MarkRunAttemptLaunching(ctx, RunAttemptLaunchStateRequest{
		Project:      launch.Run.Project,
		RunID:        launch.Run.ID,
		AttemptIndex: launch.AttemptIndex,
	}); err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			return false, false, nil
		}
		return false, false, err
	}
	jobNames, err := nativeLauncher.LaunchNativePhase(ctx, NativeLaunchRequest{
		Lease:    leaseForRunAttempt(lease, launch),
		Workflow: launch.WorkflowSnapshot,
		Phase:    launch.Phase,
		Run:      launch.Run,
	})
	if err != nil {
		reason := "native_dispatch_failed: " + err.Error()
		_ = store.MarkRunAttemptLaunchFailed(ctx, RunAttemptLaunchStateRequest{
			Project:      launch.Run.Project,
			RunID:        launch.Run.ID,
			AttemptIndex: launch.AttemptIndex,
			Reason:       reason,
		})
		if launch.AdmissionAttempt {
			_, _ = store.CancelLeaseByRef(ctx, launch.Run.Project, launch.LeaseRef)
			_, _ = store.FailQueuedRunAdmission(ctx, launch.Run.Project, launch.Run.ID, reason)
			return false, true, nil
		}
		return false, false, nil
	}
	if err := store.MarkRunAttemptLaunched(ctx, RunAttemptLaunchedRequest{
		Project:      launch.Run.Project,
		RunID:        launch.Run.ID,
		AttemptIndex: launch.AttemptIndex,
		JobNames:     jobNames,
	}); err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, false, nil
}

func leaseForRunAttempt(lease Lease, launch LaunchPendingProjectRun) Lease {
	out := lease
	metadata := map[string]any{}
	for k, v := range lease.Metadata {
		metadata[k] = v
	}
	for k, v := range launch.LaunchMetadata {
		metadata[k] = v
	}
	metadata["run_id"] = launch.Run.ID
	metadata["run_ref"] = runRefFromData(launch.Run)
	metadata["phase_name"] = launch.Phase.Name
	metadata["attempt_index"] = strconv.Itoa(launch.AttemptIndex)
	metadata["native_k8s"] = true
	if launch.Run.CallbackToken != nil && *launch.Run.CallbackToken != "" {
		metadata["run_callback_token"] = *launch.Run.CallbackToken
	}
	if launch.Run.IssueLockHolderID != nil && *launch.Run.IssueLockHolderID != "" {
		metadata["issue_lock_holder_id"] = *launch.Run.IssueLockHolderID
	}
	if launch.Run.IssueRepo != "" {
		metadata["issue_repo"] = launch.Run.IssueRepo
	}
	if launch.Run.IssueNumber > 0 {
		metadata["issue_ref"] = publicids.IssueRef(launch.Run.Project, positiveIssueNumber(launch.Run.IssueNumber))
		metadata["issue_number"] = strconv.Itoa(launch.Run.IssueNumber)
	}
	if len(launch.PhaseInputs) > 0 {
		metadata["phase_inputs"] = launch.PhaseInputs
	} else {
		delete(metadata, "phase_inputs")
	}
	if _, ok := metadata["work_context_branch"]; !ok && launch.Run.IssueNumber > 0 {
		display := "unknown"
		if launch.Run.RunDisplayNumber != nil && *launch.Run.RunDisplayNumber != "" {
			display = *launch.Run.RunDisplayNumber
		}
		metadata["work_context_branch"] = fmt.Sprintf("issue-%d-run-%s", launch.Run.IssueNumber, display)
	}
	out.Metadata = metadata
	return out
}

func latestAttemptIndex(run RunReplayData) int {
	if len(run.Attempts) == 0 {
		return -1
	}
	return run.Attempts[len(run.Attempts)-1].AttemptIndex
}

func workflowAdmissionPhase(phases []PhaseSpec, entrypoint *string) (PhaseSpec, bool) {
	if entrypoint != nil && strings.TrimSpace(*entrypoint) != "" {
		for _, phase := range phases {
			if phase.Name == *entrypoint {
				return phase, true
			}
		}
		return PhaseSpec{}, false
	}
	return workflowEntryPhase(phases)
}

func reserveRunTestSlot(ctx context.Context, store ProjectRunQueueStore, run QueuedProjectRun, wf Workflow, phase PhaseSpec) (Lease, error) {
	issueNumber := run.IssueNumber
	display := ""
	if run.RunDisplayNumber != nil {
		display = *run.RunDisplayNumber
	}
	runRef := publicids.RunRef(run.Project, positiveIssueNumber(issueNumber), display)
	metadata := map[string]any{
		"issue_ref":                    publicids.IssueRef(run.Project, positiveIssueNumber(issueNumber)),
		"issue_repo":                   run.IssueRepo,
		"issue_number":                 strconv.Itoa(issueNumber),
		"run_id":                       run.ID,
		"run_ref":                      runRef,
		"phase_name":                   phase.Name,
		"attempt_index":                "0",
		"work_context_branch":          fmt.Sprintf("issue-%d-run-%s", issueNumber, display),
		"native_k8s":                   true,
		"glimmung_run_test_slot":       true,
		"reserved_unprovisioned":       true,
		"test_slot_activation_owner":   "env-prep",
		"issue_lock_holder_id":         optionalStringValue(run.IssueLockHolderID),
		"workflow_snapshot_name":       wf.Name,
		"workflow_snapshot_project":    wf.Project,
		"workflow_snapshot_phase_name": phase.Name,
	}
	if run.CallbackToken != nil && *run.CallbackToken != "" {
		metadata["run_callback_token"] = *run.CallbackToken
	}
	requirements := phase.Requirements
	if len(requirements) == 0 {
		requirements = wf.DefaultRequirements
	}
	wfName := wf.Name
	return store.AcquireLease(ctx, LeaseAcquireRequest{
		Project:      run.Project,
		Workflow:     &wfName,
		Requirements: requirements,
		Metadata:     metadata,
		Requester: LeaseRequesterInput{
			Consumer: "run",
			Kind:     "admission",
			Ref:      runRef,
		},
	})
}

func optionalStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

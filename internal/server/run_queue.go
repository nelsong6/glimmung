package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/nelsong6/glimmung/internal/domain/publicids"
)

const defaultRunQueueBatchSize = 25

type RunQueueStore interface {
	ListProjects(ctx context.Context) ([]Project, error)
	ListQueuedRunCycles(ctx context.Context, project string, limit int) ([]RunReplayData, error)
	ReadProjectGitHubRepo(ctx context.Context, project string) (string, error)
	ReadIssueForDispatch(ctx context.Context, project string, issueNumber int) (IssueDispatchData, error)
	GetWorkflowByName(ctx context.Context, project, name string) (*Workflow, error)
	GetWorkflowBySchemaRef(ctx context.Context, project, schemaRef string) (*Workflow, error)
	StartRunCycle(ctx context.Context, req StartRunCycleRequest) (int, error)
	AcquireLease(ctx context.Context, req LeaseAcquireRequest) (Lease, error)
	CancelLeaseByRef(ctx context.Context, project, ref string) (CancelLeaseResult, error)
	AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
}

type RunCycleAdmissionResult struct {
	State  string
	Lease  string
	Host   *string
	Detail *string
}

type RunQueueDrainResult struct {
	Admitted int
	Queued   int
	Failed   int
	Skipped  int
}

var runQueueWake atomic.Value // stores func(project string)

func StartRunQueueReconciler(ctx context.Context, store ReadStore, nativeLauncher NativeLauncher, logf func(string, ...any)) {
	queueStore, ok := store.(RunQueueStore)
	if !ok || queueStore == nil || nativeLauncher == nil {
		return
	}
	wakeCh := make(chan string, 128)
	runQueueWake.Store(func(project string) {
		select {
		case wakeCh <- strings.TrimSpace(project):
		default:
		}
	})
	wakeRunQueue("")
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case project := <-wakeCh:
				result, err := DrainRunQueue(ctx, queueStore, nativeLauncher, project, defaultRunQueueBatchSize)
				if err != nil {
					if logf != nil {
						logf("run queue reconcile failed project=%q: %v", project, err)
					}
					continue
				}
				if logf != nil && (result.Admitted > 0 || result.Failed > 0) {
					logf("run queue reconciled project=%q admitted=%d queued=%d failed=%d skipped=%d", project, result.Admitted, result.Queued, result.Failed, result.Skipped)
				}
			}
		}
	}()
}

func wakeRunQueue(project string) {
	fn, ok := runQueueWake.Load().(func(string))
	if ok && fn != nil {
		fn(project)
	}
}

func DrainRunQueue(ctx context.Context, store RunQueueStore, nativeLauncher NativeLauncher, project string, limit int) (RunQueueDrainResult, error) {
	if store == nil {
		return RunQueueDrainResult{}, errors.New("run queue store not configured")
	}
	if nativeLauncher == nil {
		return RunQueueDrainResult{}, errors.New("native launcher not configured")
	}
	if limit <= 0 {
		limit = defaultRunQueueBatchSize
	}
	project = strings.TrimSpace(project)
	if project != "" {
		return drainProjectRunQueue(ctx, store, nativeLauncher, project, limit)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return RunQueueDrainResult{}, err
	}
	var total RunQueueDrainResult
	for _, row := range projects {
		name := firstNonEmpty(row.Name, row.ID)
		if name == "" {
			continue
		}
		result, err := drainProjectRunQueue(ctx, store, nativeLauncher, name, limit)
		if err != nil {
			return total, err
		}
		total.Admitted += result.Admitted
		total.Queued += result.Queued
		total.Failed += result.Failed
		total.Skipped += result.Skipped
	}
	return total, nil
}

func drainProjectRunQueue(ctx context.Context, store RunQueueStore, nativeLauncher NativeLauncher, project string, limit int) (RunQueueDrainResult, error) {
	queued, err := store.ListQueuedRunCycles(ctx, project, limit)
	if err != nil {
		return RunQueueDrainResult{}, err
	}
	var result RunQueueDrainResult
	for _, run := range queued {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}
		admission, err := AdmitQueuedRunCycle(ctx, store, nativeLauncher, run)
		if err != nil {
			result.Failed++
			return result, err
		}
		switch admission.State {
		case "dispatched":
			result.Admitted++
		case "queued":
			result.Queued++
			return result, nil
		case "admission_failed":
			result.Failed++
		case "dispatch_failed":
			result.Failed++
			return result, nil
		default:
			result.Skipped++
		}
	}
	return result, nil
}

func AdmitQueuedRunCycle(ctx context.Context, store RunQueueStore, nativeLauncher NativeLauncher, run RunReplayData) (RunCycleAdmissionResult, error) {
	wf, err := workflowForQueuedRun(ctx, store, run)
	if err != nil {
		return RunCycleAdmissionResult{}, err
	}
	if wf == nil {
		return abortQueuedAdmission(ctx, store, run, "workflow_schema_missing")
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
		return abortQueuedAdmission(ctx, store, run, "workflow_schema_invalid: "+err.Error())
	}
	issueRepo, err := store.ReadProjectGitHubRepo(ctx, run.Project)
	if errors.Is(err, ErrNotFound) {
		return abortQueuedAdmission(ctx, store, run, "project_missing")
	}
	if err != nil {
		return RunCycleAdmissionResult{}, err
	}
	issue, err := store.ReadIssueForDispatch(ctx, run.Project, run.IssueNumber)
	if errors.Is(err, ErrNotFound) {
		return abortQueuedAdmission(ctx, store, run, "issue_missing")
	}
	if err != nil {
		return RunCycleAdmissionResult{}, err
	}
	return admitRunCycle(ctx, store, nativeLauncher, run, wf, issue, issueRepo, LeasePurposeDispatch)
}

func workflowForQueuedRun(ctx context.Context, store RunQueueStore, run RunReplayData) (*Workflow, error) {
	if run.WorkflowSchemaRef != "" {
		return store.GetWorkflowBySchemaRef(ctx, run.Project, run.WorkflowSchemaRef)
	}
	return store.GetWorkflowByName(ctx, run.Project, run.WorkflowName)
}

func abortQueuedAdmission(ctx context.Context, store RunQueueStore, run RunReplayData, reason string) (RunCycleAdmissionResult, error) {
	_, err := store.AbortRunByID(ctx, run.Project, run.ID, "admission_failed: "+reason)
	if err != nil {
		return RunCycleAdmissionResult{}, err
	}
	detail := reason
	return RunCycleAdmissionResult{State: "admission_failed", Detail: &detail}, nil
}

func admitRunCycle(
	ctx context.Context,
	store interface {
		StartRunCycle(ctx context.Context, req StartRunCycleRequest) (int, error)
		AcquireLease(ctx context.Context, req LeaseAcquireRequest) (Lease, error)
		CancelLeaseByRef(ctx context.Context, project, ref string) (CancelLeaseResult, error)
		AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
	},
	nativeLauncher NativeLauncher,
	run RunReplayData,
	wf *Workflow,
	issue IssueDispatchData,
	issueRepo string,
	leasePurpose string,
) (RunCycleAdmissionResult, error) {
	if nativeLauncher == nil {
		return RunCycleAdmissionResult{}, errors.New("native launcher not configured")
	}
	initPhase, ok := runEntrypointPhase(wf.Phases, run)
	if !ok {
		return abortAdmissionForRun(ctx, store, run, "workflow has no entry phase")
	}
	phaseKind := workflowPhaseKind(initPhase.Kind)
	if err := validateNativeWorkflowKind(phaseKind); err != nil {
		return abortAdmissionForRun(ctx, store, run, err.Error())
	}
	workflowFilename := initPhase.WorkflowFilename
	if workflowFilename == "" {
		workflowFilename = fmt.Sprintf("%s:%s", phaseKind, initPhase.Name)
	}
	metadata := runCycleLeaseMetadata(run, issue, issueRepo, initPhase.Name, 0, nil)
	var lease Lease
	leaseRef := ""
	acquiredLease := false
	if run.SlotLeaseRef != nil && strings.TrimSpace(*run.SlotLeaseRef) != "" {
		reader, ok := any(store).(interface {
			ReadLeaseByRef(ctx context.Context, project, ref string) (Lease, error)
		})
		if !ok {
			return RunCycleAdmissionResult{}, errors.New("slot lease reader not configured")
		}
		leaseRef = strings.TrimSpace(*run.SlotLeaseRef)
		var err error
		lease, err = reader.ReadLeaseByRef(ctx, run.Project, leaseRef)
		if err != nil {
			return RunCycleAdmissionResult{}, err
		}
		merged := mapOrEmpty(lease.Metadata)
		for key, value := range metadata {
			merged[key] = value
		}
		lease.Metadata = merged
	} else {
		requirements := initPhase.Requirements
		if len(requirements) == 0 {
			requirements = wf.DefaultRequirements
		}
		wfName := wf.Name
		var err error
		lease, err = acquireLeaseInstrumented(ctx, leasePurpose, LeaseAcquireRequest{
			Project:      run.Project,
			Workflow:     &wfName,
			Requirements: requirements,
			Metadata:     metadata,
		}, store.AcquireLease)
		if err != nil {
			if errors.Is(err, ErrUnavailable) {
				detail := "queued awaiting project test slot capacity"
				return RunCycleAdmissionResult{State: "queued", Detail: &detail}, nil
			}
			return RunCycleAdmissionResult{}, err
		}
		leaseRef = LeasePublicRefFromLease(lease)
		acquiredLease = true
	}
	if lease.State != "claimed" {
		if acquiredLease {
			_, _ = store.CancelLeaseByRef(ctx, run.Project, leaseRef)
		}
		_, _ = store.AbortRunByID(ctx, run.Project, run.ID, "native_lease_not_claimed")
		detail := "native lease was not claimed"
		return RunCycleAdmissionResult{State: "dispatch_failed", Detail: &detail}, nil
	}
	attemptIdx, err := store.StartRunCycle(ctx, StartRunCycleRequest{
		Project:          run.Project,
		RunID:            run.ID,
		PhaseName:        initPhase.Name,
		PhaseKind:        phaseKind,
		WorkflowFilename: workflowFilename,
		SlotLeaseRef:     leaseRef,
	})
	if err != nil {
		if acquiredLease {
			_, _ = store.CancelLeaseByRef(ctx, run.Project, leaseRef)
		}
		if errors.Is(err, ErrConflict) {
			detail := "run cycle was already admitted"
			return RunCycleAdmissionResult{State: "skipped", Detail: &detail}, nil
		}
		_, _ = store.AbortRunByID(ctx, run.Project, run.ID, "start_cycle_failed: "+err.Error())
		return RunCycleAdmissionResult{}, fmt.Errorf("start run cycle: %w", err)
	}
	started := runWithAttempt(run, attemptIdx, initPhase.Name)
	started.SlotLeaseRef = &leaseRef
	if _, err := launchCommittedNativePhase(ctx, nativeLauncher, NativeLaunchRequest{
		Lease:    lease,
		Workflow: *wf,
		Phase:    initPhase,
		Run:      started,
	}); err != nil {
		if acquiredLease {
			_, _ = store.CancelLeaseByRef(ctx, run.Project, leaseRef)
		}
		_, _ = store.AbortRunByID(ctx, run.Project, run.ID, "native_dispatch_failed: "+err.Error())
		detail := fmt.Sprintf("native dispatch failed: %s", err)
		return RunCycleAdmissionResult{State: "dispatch_failed", Detail: &detail}, nil
	}
	return RunCycleAdmissionResult{
		State: "dispatched",
		Lease: "claimed",
		Host:  lease.Host,
	}, nil
}

func runEntrypointPhase(phases []PhaseSpec, run RunReplayData) (PhaseSpec, bool) {
	if run.EntrypointPhase != nil && strings.TrimSpace(*run.EntrypointPhase) != "" {
		if phase := phaseSpecByName(phases, strings.TrimSpace(*run.EntrypointPhase)); phase != nil {
			return *phase, true
		}
		return PhaseSpec{}, false
	}
	return workflowEntryPhase(phases)
}

func abortAdmissionForRun(
	ctx context.Context,
	store interface {
		AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
	},
	run RunReplayData,
	reason string,
) (RunCycleAdmissionResult, error) {
	_, err := store.AbortRunByID(ctx, run.Project, run.ID, "admission_failed: "+reason)
	if err != nil {
		return RunCycleAdmissionResult{}, err
	}
	detail := reason
	return RunCycleAdmissionResult{State: "admission_failed", Detail: &detail}, nil
}

func runCycleLeaseMetadata(run RunReplayData, issue IssueDispatchData, issueRepo, phaseName string, attemptIndex int, phaseInputs map[string]string) map[string]any {
	issueNum := run.IssueNumber
	issueRef := publicids.IssueRef(run.Project, &issueNum)
	runRef := runRefFromData(run)
	display := "unknown"
	if run.RunDisplayNumber != nil && *run.RunDisplayNumber != "" {
		display = *run.RunDisplayNumber
	}
	metadata := map[string]any{
		"issue_body":          issue.Body,
		"issue_ref":           issueRef,
		"issue_repo":          issueRepo,
		"issue_title":         issue.Title,
		"run_id":              run.ID,
		"run_ref":             runRef,
		"run_number":          positiveIntString(run.RunNumber),
		"cycle_number":        positiveIntString(run.CycleNumber),
		"run_cycle_number":    positiveIntString(run.RunCycleNumber),
		"run_display_number":  display,
		"attempt_index":       strconv.Itoa(attemptIndex),
		"phase_name":          phaseName,
		"issue_number":        strconv.Itoa(run.IssueNumber),
		"work_context_branch": fmt.Sprintf("issue-%d-run-%s", run.IssueNumber, display),
		"native_k8s":          true,
	}
	if run.CallbackToken != nil && *run.CallbackToken != "" {
		metadata["run_callback_token"] = *run.CallbackToken
	}
	if run.IssueLockHolderID != nil && *run.IssueLockHolderID != "" {
		metadata["issue_lock_holder_id"] = *run.IssueLockHolderID
	}
	if len(phaseInputs) > 0 {
		metadata["phase_inputs"] = phaseInputs
	}
	if len(run.TriggerSource) > 0 {
		metadata["trigger_source"] = run.TriggerSource
		if feedback := stringValue(run.TriggerSource["feedback"]); feedback != "" {
			metadata["feedback"] = feedback
		}
	}
	return metadata
}

func positiveIntString(value *int) string {
	if value == nil || *value <= 0 {
		return ""
	}
	return strconv.Itoa(*value)
}

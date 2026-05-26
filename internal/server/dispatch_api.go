package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/publicids"
	"github.com/nelsong6/glimmung/internal/metrics"
)

const defaultIssueLockTTLSeconds = 14400 // 4 hours

// ErrAlreadyRunning is the sentinel returned when the issue lock is already held.
var ErrAlreadyRunning = errors.New("already running")

// AlreadyRunningError wraps ErrAlreadyRunning with lock-holder details for the response body.
type AlreadyRunningError struct {
	HeldBy    string
	ExpiresAt time.Time
}

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("issue lock held by %s until %s", e.HeldBy, e.ExpiresAt.Format(time.RFC3339))
}

func (e *AlreadyRunningError) Unwrap() error { return ErrAlreadyRunning }

// IssueDispatchData holds the minimal issue fields needed to build dispatch metadata.
type IssueDispatchData struct {
	ID     string
	Title  string
	Body   string
	Labels []string
}

// CreateRunRequest carries all parameters for creating a new run document.
type CreateRunRequest struct {
	Project                 string
	Workflow                string
	WorkflowSchemaRef       string
	IssueID                 string
	IssueRepo               string
	IssueNumber             int
	Budget                  budget.Config
	InitialPhaseName        string
	InitialPhaseKind        string
	InitialWorkflowFilename string
	IssueLockHolderID       string
	SlotLeaseRef            string
	TriggerSource           map[string]any
}

// CreatedRun holds the identifiers returned after creating a run document.
type CreatedRun struct {
	ID                   string
	RunNumber            int
	CycleNumber          int
	RunCycle             int
	RunDisplay           string
	CallbackToken        string
	CarryForwardAttempts []RunAttemptData
}

type StartRunCycleRequest struct {
	Project          string
	RunID            string
	PhaseName        string
	PhaseKind        string
	WorkflowFilename string
	SlotLeaseRef     string
}

type CreateRecycleCycleRequest struct {
	Parent               RunReplayData
	WorkflowSchemaRef    string
	TargetPhaseName      string
	CarryForwardAttempts []RunAttemptData
	TriggerSource        map[string]any
}

// RunDispatchStore provides all store operations needed by the dispatch handler.
type RunDispatchStore interface {
	ReadProjectGitHubRepo(ctx context.Context, project string) (string, error)
	ReadIssueForDispatch(ctx context.Context, project string, issueNumber int) (IssueDispatchData, error)
	GetWorkflowByName(ctx context.Context, project, name string) (*Workflow, error)
	ListProjectWorkflows(ctx context.Context, project string) ([]Workflow, error)
	ClaimIssueLock(ctx context.Context, project string, issueNumber int, holderID string, ttlSeconds int) error
	ReleaseIssueLock(ctx context.Context, project string, issueNumber int, holderID string)
	CreateRun(ctx context.Context, req CreateRunRequest) (CreatedRun, error)
	StartRunCycle(ctx context.Context, req StartRunCycleRequest) (int, error)
	AcquireLease(ctx context.Context, req LeaseAcquireRequest) (Lease, error)
	ReadLeaseByRef(ctx context.Context, project, ref string) (Lease, error)
	CancelLeaseByRef(ctx context.Context, project, ref string) (CancelLeaseResult, error)
	AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
}

// DispatchRunRequest is the body for POST /v1/runs/dispatch.
type DispatchRunRequest struct {
	Project       string         `json:"project"`
	IssueNumber   int            `json:"issue_number"`
	WorkflowName  string         `json:"workflow_name"`
	Workflow      string         `json:"workflow"`
	TriggerSource map[string]any `json:"trigger_source"`
}

// PublicDispatchResult is the response for POST /v1/runs/dispatch.
type PublicDispatchResult struct {
	State       string  `json:"state"`
	Lease       string  `json:"lease,omitempty"`
	IssueRef    *string `json:"issue_ref,omitempty"`
	IssueNumber *int    `json:"issue_number,omitempty"`
	RunNumber   *int    `json:"run_number"`
	CycleNumber *int    `json:"cycle_number,omitempty"`
	RunCycle    *int    `json:"run_cycle_number,omitempty"`
	RunID       *string `json:"run_id,omitempty"`
	RunRef      *string `json:"run_ref,omitempty"`
	Host        *string `json:"host"`
	Workflow    *string `json:"workflow"`
	Detail      *string `json:"detail"`
}

// dispatchRunHandler handles POST /v1/runs/dispatch (admin-only).
func dispatchRunHandler(store ReadStore, nativeLauncher NativeLauncher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dispatchStore, ok := store.(RunDispatchStore)
		if !ok || dispatchStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "dispatch store not configured")
			return
		}

		var req DispatchRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid request body")
			return
		}
		result, problem := dispatchRun(r.Context(), dispatchStore, nativeLauncher, req)
		if problem != nil {
			writeProblem(w, problem.status, problem.message)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

type dispatchProblem struct {
	status  int
	message string
}

func dispatchRun(ctx context.Context, dispatchStore RunDispatchStore, nativeLauncher NativeLauncher, req DispatchRunRequest) (PublicDispatchResult, *dispatchProblem) {
	if req.Project == "" {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusBadRequest, message: "project required"}
	}
	if req.IssueNumber <= 0 {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusBadRequest, message: "issue_number required"}
	}

	issueRepo, err := dispatchStore.ReadProjectGitHubRepo(ctx, req.Project)
	if errors.Is(err, ErrNotFound) {
		return PublicDispatchResult{
			State:  "no_project",
			Detail: stringPtr(fmt.Sprintf("project %q not registered", req.Project)),
		}, nil
	}
	if err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "read project failed"}
	}

	issue, err := dispatchStore.ReadIssueForDispatch(ctx, req.Project, req.IssueNumber)
	if errors.Is(err, ErrNotFound) {
		return PublicDispatchResult{
			State:  "no_project",
			Detail: stringPtr(fmt.Sprintf("no issue %s#%d", req.Project, req.IssueNumber)),
		}, nil
	}
	if err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "read issue failed"}
	}

	wf, resolveDetail, err := resolveDispatchWorkflow(ctx, dispatchStore, req.Project, req.resolvedWorkflowName())
	if err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "read workflow failed"}
	}
	if wf == nil {
		return PublicDispatchResult{State: "no_workflow", Detail: &resolveDetail}, nil
	}
	if len(wf.Phases) == 0 {
		return PublicDispatchResult{
			State:    "no_workflow",
			Workflow: &wf.Name,
			Detail:   stringPtr("workflow has no phases"),
		}, nil
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
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: err.Error()}
	}

	if nativeLauncher == nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusServiceUnavailable, message: "native launcher not configured"}
	}

	initPhase, ok := workflowEntryPhase(wf.Phases)
	if !ok {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: "workflow has no entry phase"}
	}
	phaseKind := workflowPhaseKind(initPhase.Kind)
	if err := validateNativeWorkflowKind(phaseKind); err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: err.Error()}
	}

	holderID := newDispatchID()
	if err := dispatchStore.ClaimIssueLock(ctx, req.Project, req.IssueNumber, holderID, defaultIssueLockTTLSeconds); err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			return PublicDispatchResult{
				State:    "already_running",
				Workflow: &wf.Name,
				Detail:   stringPtr(err.Error()),
			}, nil
		}
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "claim issue lock failed"}
	}

	var wfBudget *budget.Config
	if wf.Budget.Total > 0 {
		c := wf.Budget
		wfBudget = &c
	}
	resolvedBudget := budget.ResolveBudget(issue.Labels, wfBudget)

	workflowFilename := initPhase.WorkflowFilename
	if workflowFilename == "" {
		workflowFilename = fmt.Sprintf("%s:%s", phaseKind, initPhase.Name)
	}
	issueNum := req.IssueNumber
	issueRef := publicids.IssueRef(req.Project, &issueNum)
	wfNameStr := wf.Name
	triggerSource := req.TriggerSource
	if triggerSource == nil {
		triggerSource = map[string]any{"kind": "dispatch"}
	}

	requirements := initPhase.Requirements
	if len(requirements) == 0 {
		requirements = wf.DefaultRequirements
	}
	initialLease, err := acquireLeaseInstrumented(ctx, LeasePurposeDispatch, LeaseAcquireRequest{
		Project:      req.Project,
		Workflow:     &wf.Name,
		Requirements: requirements,
		Metadata: runCycleLeaseMetadata(RunReplayData{
			Project:       req.Project,
			WorkflowName:  wf.Name,
			IssueNumber:   req.IssueNumber,
			IssueRepo:     issueRepo,
			TriggerSource: triggerSource,
		}, issue, issueRepo, initPhase.Name, 0, nil),
	}, dispatchStore.AcquireLease)
	if err != nil {
		dispatchStore.ReleaseIssueLock(ctx, req.Project, req.IssueNumber, holderID)
		if errors.Is(err, ErrUnavailable) {
			return PublicDispatchResult{
				State:       "no_capacity",
				IssueRef:    &issueRef,
				IssueNumber: &issueNum,
				Workflow:    &wfNameStr,
				Detail:      stringPtr("no project test slot capacity available; run was not created"),
			}, nil
		}
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "acquire test slot failed"}
	}
	initialLeaseRef := LeasePublicRefFromLease(initialLease)
	if initialLease.State != "claimed" {
		_, _ = dispatchStore.CancelLeaseByRef(ctx, req.Project, initialLeaseRef)
		dispatchStore.ReleaseIssueLock(ctx, req.Project, req.IssueNumber, holderID)
		return PublicDispatchResult{
			State:       "dispatch_failed",
			IssueRef:    &issueRef,
			IssueNumber: &issueNum,
			Workflow:    &wfNameStr,
			Detail:      stringPtr("native lease was not claimed; run was not created"),
		}, nil
	}

	run, err := dispatchStore.CreateRun(ctx, CreateRunRequest{
		Project:                 req.Project,
		Workflow:                wf.Name,
		WorkflowSchemaRef:       wf.SchemaRef,
		IssueID:                 issue.ID,
		IssueRepo:               issueRepo,
		IssueNumber:             req.IssueNumber,
		Budget:                  resolvedBudget,
		InitialPhaseName:        initPhase.Name,
		InitialPhaseKind:        phaseKind,
		InitialWorkflowFilename: workflowFilename,
		IssueLockHolderID:       holderID,
		SlotLeaseRef:            initialLeaseRef,
		TriggerSource:           triggerSource,
	})
	if err != nil {
		_, _ = dispatchStore.CancelLeaseByRef(ctx, req.Project, initialLeaseRef)
		dispatchStore.ReleaseIssueLock(ctx, req.Project, req.IssueNumber, holderID)
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "create run failed"}
	}
	metrics.RecordRunCreated(wf.Name)

	runRef := publicids.RunRef(req.Project, &issueNum, run.RunDisplay)
	runData := RunReplayData{
		ID:                run.ID,
		Project:           req.Project,
		WorkflowName:      wf.Name,
		WorkflowSchemaRef: wf.SchemaRef,
		IssueNumber:       req.IssueNumber,
		RunNumber:         &run.RunNumber,
		CycleNumber:       &run.CycleNumber,
		RunCycleNumber:    &run.RunCycle,
		RunDisplayNumber:  &run.RunDisplay,
		IssueRepo:         issueRepo,
		CallbackToken:     &run.CallbackToken,
		IssueLockHolderID: &holderID,
		SlotLeaseRef:      &initialLeaseRef,
		TriggerSource:     triggerSource,
	}
	admission, err := admitRunCycle(ctx, dispatchStore, nativeLauncher, runData, wf, issue, issueRepo, LeasePurposeDispatch)
	if err != nil {
		_, _ = dispatchStore.CancelLeaseByRef(ctx, req.Project, initialLeaseRef)
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "admit run cycle failed"}
	}
	if admission.State == "admission_failed" || admission.State == "dispatch_failed" {
		_, _ = dispatchStore.CancelLeaseByRef(ctx, req.Project, initialLeaseRef)
	}
	return PublicDispatchResult{
		State:       admission.State,
		IssueRef:    &issueRef,
		IssueNumber: &issueNum,
		RunNumber:   &run.RunNumber,
		CycleNumber: &run.CycleNumber,
		RunCycle:    &run.RunCycle,
		RunID:       &run.ID,
		RunRef:      &runRef,
		Workflow:    &wfNameStr,
		Lease:       admission.Lease,
		Host:        admission.Host,
		Detail:      admission.Detail,
	}, nil
}

func workflowEntryPhase(phases []PhaseSpec) (PhaseSpec, bool) {
	for _, phase := range phases {
		if len(phase.DependsOn) == 0 {
			return phase, true
		}
	}
	return PhaseSpec{}, false
}

func (req DispatchRunRequest) resolvedWorkflowName() string {
	if req.WorkflowName != "" {
		return req.WorkflowName
	}
	return req.Workflow
}

// resolveDispatchWorkflow picks the workflow to dispatch: explicit name or the project's sole workflow.
// Returns (nil, detail, nil) when no matching workflow is found (not an error, caller returns no_workflow).
func resolveDispatchWorkflow(ctx context.Context, store RunDispatchStore, project, workflowName string) (*Workflow, string, error) {
	if workflowName != "" {
		wf, err := store.GetWorkflowByName(ctx, project, workflowName)
		if err != nil {
			return nil, "", err
		}
		if wf == nil {
			return nil, fmt.Sprintf("workflow %s/%s not registered", project, workflowName), nil
		}
		canonical := CanonicalWorkflow(*wf)
		return &canonical, "", nil
	}
	workflows, err := store.ListProjectWorkflows(ctx, project)
	if err != nil {
		return nil, "", err
	}
	switch len(workflows) {
	case 0:
		return nil, fmt.Sprintf("project %q has no workflows registered", project), nil
	case 1:
		canonical := CanonicalWorkflow(workflows[0])
		return &canonical, "", nil
	default:
		names := make([]string, 0, len(workflows))
		for _, wf := range workflows {
			names = append(names, wf.Name)
		}
		return nil, fmt.Sprintf("project %q has multiple workflows; specify one of %v", project, names), nil
	}
}

// newDispatchID generates a random 32-char hex string to use as an issue lock holder ID.
func newDispatchID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

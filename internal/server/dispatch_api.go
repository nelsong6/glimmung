package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/publicids"
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
	IssueID                 string
	IssueRepo               string
	IssueNumber             int
	Budget                  budget.Config
	InitialPhaseName        string
	InitialPhaseKind        string
	InitialWorkflowFilename string
	IssueLockHolderID       string
	TriggerSource           map[string]any
}

// CreatedRun holds the identifiers returned after creating a run document.
type CreatedRun struct {
	ID            string
	RunNumber     int
	RunDisplay    string
	CallbackToken string
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
	AcquireLease(ctx context.Context, req LeaseAcquireRequest) (Lease, *Host, error)
	AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
}

// DispatchRunRequest is the body for POST /v1/runs/dispatch.
type DispatchRunRequest struct {
	Project       string         `json:"project"`
	IssueNumber   int            `json:"issue_number"`
	WorkflowName  string         `json:"workflow_name"`
	TriggerSource map[string]any `json:"trigger_source"`
}

// PublicDispatchResult is the response for POST /v1/runs/dispatch.
type PublicDispatchResult struct {
	State     string  `json:"state"`
	Lease     string  `json:"lease,omitempty"`
	RunNumber *int    `json:"run_number"`
	Host      *string `json:"host"`
	Workflow  *string `json:"workflow"`
	Detail    *string `json:"detail"`
}

// dispatchRunHandler handles POST /v1/runs/dispatch (admin-only).
func dispatchRunHandler(store ReadStore, ghDispatch GHADispatchClient) http.HandlerFunc {
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
		if req.Project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		if req.IssueNumber <= 0 {
			writeProblem(w, http.StatusBadRequest, "issue_number required")
			return
		}

		ctx := r.Context()

		// 1. Verify project exists and get its GitHub repo.
		issueRepo, err := dispatchStore.ReadProjectGitHubRepo(ctx, req.Project)
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusOK, PublicDispatchResult{
				State:  "no_project",
				Detail: stringPtr(fmt.Sprintf("project %q not registered", req.Project)),
			})
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "read project failed")
			return
		}

		// 2. Read the issue.
		issue, err := dispatchStore.ReadIssueForDispatch(ctx, req.Project, req.IssueNumber)
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusOK, PublicDispatchResult{
				State:  "no_project",
				Detail: stringPtr(fmt.Sprintf("no issue %s#%d", req.Project, req.IssueNumber)),
			})
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "read issue failed")
			return
		}

		// 3. Resolve workflow.
		wf, resolveDetail, err := resolveDispatchWorkflow(ctx, dispatchStore, req.Project, req.WorkflowName)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "read workflow failed")
			return
		}
		if wf == nil {
			writeJSON(w, http.StatusOK, PublicDispatchResult{State: "no_workflow", Detail: &resolveDetail})
			return
		}
		if len(wf.Phases) == 0 {
			writeJSON(w, http.StatusOK, PublicDispatchResult{
				State:    "no_workflow",
				Workflow: &wf.Name,
				Detail:   stringPtr("workflow has no phases"),
			})
			return
		}

		// 4. Claim the per-issue serialization lock.
		holderID := newDispatchID()
		if err := dispatchStore.ClaimIssueLock(ctx, req.Project, req.IssueNumber, holderID, defaultIssueLockTTLSeconds); err != nil {
			if errors.Is(err, ErrAlreadyRunning) {
				writeJSON(w, http.StatusOK, PublicDispatchResult{
					State:    "already_running",
					Workflow: &wf.Name,
					Detail:   stringPtr(err.Error()),
				})
				return
			}
			writeProblem(w, http.StatusInternalServerError, "claim issue lock failed")
			return
		}

		// 5. Resolve budget from issue labels + workflow default.
		var wfBudget *budget.Config
		if wf.Budget.Total > 0 {
			c := wf.Budget
			wfBudget = &c
		}
		resolvedBudget := budget.ResolveBudget(issue.Labels, wfBudget)

		// 6. Prepare initial phase parameters.
		initPhase := wf.Phases[0]
		phaseKind := initPhase.Kind
		if phaseKind == "" {
			phaseKind = "gha_dispatch"
		}
		workflowFilename := initPhase.WorkflowFilename
		if workflowFilename == "" {
			workflowFilename = fmt.Sprintf("%s:%s", phaseKind, initPhase.Name)
		}
		triggerSource := req.TriggerSource
		if triggerSource == nil {
			triggerSource = map[string]any{"kind": "dispatch"}
		}

		// 7. Create the run document (under the lock, so run-number allocation is serialized).
		run, err := dispatchStore.CreateRun(ctx, CreateRunRequest{
			Project:                 req.Project,
			Workflow:                wf.Name,
			IssueID:                 issue.ID,
			IssueRepo:               issueRepo,
			IssueNumber:             req.IssueNumber,
			Budget:                  resolvedBudget,
			InitialPhaseName:        initPhase.Name,
			InitialPhaseKind:        phaseKind,
			InitialWorkflowFilename: workflowFilename,
			IssueLockHolderID:       holderID,
			TriggerSource:           triggerSource,
		})
		if err != nil {
			// Run wasn't created — release the lock directly since AbortRunByID can't help.
			dispatchStore.ReleaseIssueLock(ctx, req.Project, req.IssueNumber, holderID)
			writeProblem(w, http.StatusInternalServerError, "create run failed")
			return
		}

		// 8. Build lease metadata.
		issueNum := req.IssueNumber
		runRef := publicids.RunRef(req.Project, &issueNum, run.RunDisplay)
		metadata := map[string]any{
			"issue_body":           issue.Body,
			"issue_title":          issue.Title,
			"issue_lock_holder_id": holderID,
			"run_id":               run.ID,
			"run_ref":              runRef,
			"run_callback_token":   run.CallbackToken,
			"run_number":           strconv.Itoa(run.RunNumber),
			"run_display_number":   run.RunDisplay,
			"attempt_index":        "0",
			"phase_name":           initPhase.Name,
			"issue_number":         strconv.Itoa(req.IssueNumber),
			"work_context_branch":  fmt.Sprintf("issue-%d-run-%s", req.IssueNumber, run.RunDisplay),
		}

		// 9. Acquire a lease for the initial phase.
		requirements := initPhase.Requirements
		if len(requirements) == 0 {
			requirements = wf.DefaultRequirements
		}
		wfName := wf.Name
		lease, host, err := dispatchStore.AcquireLease(ctx, LeaseAcquireRequest{
			Project:      req.Project,
			Workflow:     &wfName,
			Requirements: requirements,
			Metadata:     metadata,
		})
		if err != nil {
			// Abort the run — this releases the issue lock as a side-effect.
			dispatchStore.AbortRunByID(ctx, req.Project, run.ID, "lease_acquire_failed") //nolint:errcheck
			writeProblem(w, http.StatusInternalServerError, "acquire lease failed")
			return
		}

		wfNameStr := wf.Name
		result := PublicDispatchResult{
			RunNumber: &run.RunNumber,
			Workflow:  &wfNameStr,
			Lease:     "claimed",
		}

		// 10. If a host was immediately assigned and this is a GHA phase, fire workflow_dispatch.
		if host != nil && phaseKind == "gha_dispatch" && ghDispatch != nil {
			inputs := buildInitialDispatchInputs(lease, host, run, runRef, initPhase, req.IssueNumber)
			wfRef := initPhase.WorkflowRef
			if wfRef == "" {
				wfRef = "main"
			}
			if err := ghDispatch.DispatchWorkflow(ctx, issueRepo, workflowFilename, wfRef, inputs); err != nil {
				// Rollback: abort run (releases issue lock). Lease will expire via TTL.
				dispatchStore.AbortRunByID(ctx, req.Project, run.ID, "dispatch_failed: "+err.Error()) //nolint:errcheck
				detail := fmt.Sprintf("runner dispatch failed: %s", err)
				writeJSON(w, http.StatusOK, PublicDispatchResult{
					State:    "dispatch_failed",
					Workflow: &wfNameStr,
					Detail:   &detail,
				})
				return
			}
			result.State = "dispatched"
			hostName := host.Name
			result.Host = &hostName
		} else {
			result.State = "pending"
		}

		writeJSON(w, http.StatusOK, result)
	}
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
		return wf, "", nil
	}
	workflows, err := store.ListProjectWorkflows(ctx, project)
	if err != nil {
		return nil, "", err
	}
	switch len(workflows) {
	case 0:
		return nil, fmt.Sprintf("project %q has no workflows registered", project), nil
	case 1:
		return &workflows[0], "", nil
	default:
		names := make([]string, 0, len(workflows))
		for _, wf := range workflows {
			names = append(names, wf.Name)
		}
		return nil, fmt.Sprintf("project %q has multiple workflows; specify one of %v", project, names), nil
	}
}

// buildInitialDispatchInputs constructs the workflow_dispatch input map for the first attempt.
func buildInitialDispatchInputs(lease Lease, host *Host, run CreatedRun, runRef string, phase PhaseSpec, issueNumber int) map[string]string {
	inputs := map[string]string{
		"attempt_index":      "0",
		"issue_number":       strconv.Itoa(issueNumber),
		"run_callback_token": run.CallbackToken,
		"run_number":         strconv.Itoa(run.RunNumber),
		"run_display_number": run.RunDisplay,
		"run_ref":            runRef,
		"phase_name":         phase.Name,
	}
	if host != nil {
		inputs["host"] = host.Name
	}
	var slotName string
	if m, ok := lease.Metadata["native_slot_name"].(string); ok {
		slotName = m
	}
	inputs["lease_ref"] = publicids.LeaseRef(lease.Project, slotName, lease.LeaseNumber)
	if t, ok := lease.Metadata["lease_callback_token"].(string); ok && t != "" {
		inputs["lease_callback_token"] = t
	}
	if lease.LeaseNumber != nil {
		inputs["lease_number"] = strconv.Itoa(*lease.LeaseNumber)
	}
	// Phase-level inputs fill slots not already set by standard fields.
	for k, v := range phase.Inputs {
		if _, exists := inputs[k]; !exists {
			inputs[k] = v
		}
	}
	return inputs
}

// newDispatchID generates a random 32-char hex string to use as an issue lock holder ID.
func newDispatchID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/decision"
	"github.com/nelsong6/glimmung/internal/domain/publicids"
)

// GHADispatchClient dispatches GitHub Actions workflow runs.
type GHADispatchClient interface {
	DispatchWorkflow(ctx context.Context, repo, filename, ref string, inputs map[string]string) error
}

// CompletionPayload carries the completion data to stamp on a run attempt.
type CompletionPayload struct {
	WorkflowRunID       *int64
	Conclusion          string
	VerificationStatus  string
	VerificationReasons []string
	CostUSD             float64
	SummaryMarkdown     *string
	ScreenshotsMarkdown *string
	PhaseOutputs        map[string]string
	AttemptIndex        *int
}

// RunCompletionStore provides all store operations needed by completion handlers.
type RunCompletionStore interface {
	ReadRunIDForCallbackToken(ctx context.Context, token string) (string, string, string, error)
	AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
	ReadRunForReplay(ctx context.Context, project, runID string) (RunReplayData, error)
	GetWorkflowByName(ctx context.Context, project, name string) (*Workflow, error)
	StampRunCompletion(ctx context.Context, project, runID string, p CompletionPayload) (RunReplayData, error)
	StampRunDecision(ctx context.Context, project, runID, decision string) error
	SetRunTerminalState(ctx context.Context, project, runID, state string, abortReason *string) (AbortRunResult, error)
	AppendRunAttempt(ctx context.Context, project, runID, phase, phaseKind, workflowFilename string) (int, error)
	AcquireLease(ctx context.Context, req LeaseAcquireRequest) (Lease, *Host, error)
}

// RunCompletedRequest is the body for POST /run-callbacks/{token}/completed.
type RunCompletedRequest struct {
	WorkflowRunID       int64             `json:"workflow_run_id"`
	Conclusion          string            `json:"conclusion"`
	Verification        map[string]any    `json:"verification"`
	ScreenshotsMarkdown *string           `json:"screenshots_markdown"`
	SummaryMarkdown     *string           `json:"summary_markdown"`
	Outputs             map[string]string `json:"outputs"`
}

// NativeRunCompletedRequest is the body for POST /run-callbacks/{token}/native/completed.
type NativeRunCompletedRequest struct {
	JobID               *string           `json:"job_id"`
	Conclusion          string            `json:"conclusion"`
	Verification        map[string]any    `json:"verification"`
	ScreenshotsMarkdown *string           `json:"screenshots_markdown"`
	SummaryMarkdown     *string           `json:"summary_markdown"`
	Outputs             map[string]string `json:"outputs"`
}

// runCompletedByCallbackToken handles POST /v1/run-callbacks/{callback_token}/completed.
func runCompletedByCallbackToken(store ReadStore, ghDispatch GHADispatchClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		completionStore, ok := store.(RunCompletionStore)
		if !ok || completionStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "completion store not configured")
			return
		}
		token := r.PathValue("callback_token")
		runID, project, _, err := completionStore.ReadRunIDForCallbackToken(r.Context(), token)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run callback token not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "read run by callback token failed")
			return
		}

		var req RunCompletedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Conclusion == "" {
			writeProblem(w, http.StatusBadRequest, "conclusion required")
			return
		}

		wfRunID := req.WorkflowRunID
		result := processRunCompletion(r.Context(), w, completionStore, ghDispatch, project, runID, completionPayloadFromGHA(req), &wfRunID)
		if result != nil {
			writeJSON(w, http.StatusOK, result)
		}
	}
}

// nativeRunCompletedByCallbackToken handles POST /v1/run-callbacks/{callback_token}/native/completed.
func nativeRunCompletedByCallbackToken(store ReadStore, ghDispatch GHADispatchClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		completionStore, ok := store.(RunCompletionStore)
		if !ok || completionStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "completion store not configured")
			return
		}
		token := r.PathValue("callback_token")
		runID, project, _, err := completionStore.ReadRunIDForCallbackToken(r.Context(), token)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run callback token not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "read run by callback token failed")
			return
		}

		var req NativeRunCompletedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Conclusion == "" {
			writeProblem(w, http.StatusBadRequest, "conclusion required")
			return
		}

		result := processRunCompletion(r.Context(), w, completionStore, ghDispatch, project, runID, completionPayloadFromNative(req), nil)
		if result != nil {
			writeJSON(w, http.StatusOK, result)
		}
	}
}

// --- shared completion path ---

func completionPayloadFromGHA(req RunCompletedRequest) CompletionPayload {
	p := CompletionPayload{
		Conclusion:          req.Conclusion,
		SummaryMarkdown:     req.SummaryMarkdown,
		ScreenshotsMarkdown: req.ScreenshotsMarkdown,
		PhaseOutputs:        req.Outputs,
	}
	if v := req.WorkflowRunID; v != 0 {
		p.WorkflowRunID = &v
	}
	extractVerification(req.Verification, &p)
	return p
}

func completionPayloadFromNative(req NativeRunCompletedRequest) CompletionPayload {
	p := CompletionPayload{
		Conclusion:          req.Conclusion,
		SummaryMarkdown:     req.SummaryMarkdown,
		ScreenshotsMarkdown: req.ScreenshotsMarkdown,
		PhaseOutputs:        req.Outputs,
	}
	extractVerification(req.Verification, &p)
	return p
}

func extractVerification(raw map[string]any, p *CompletionPayload) {
	if raw == nil {
		return
	}
	if s, ok := raw["status"].(string); ok {
		p.VerificationStatus = s
	}
	if c, ok := raw["cost_usd"].(float64); ok {
		p.CostUSD = c
	}
	if reasons, ok := raw["reasons"].([]any); ok {
		for _, r := range reasons {
			if s, ok := r.(string); ok {
				p.VerificationReasons = append(p.VerificationReasons, s)
			}
		}
	}
}

// processRunCompletion is the shared decision-engine path for GHA and native completions.
// Returns the RunCallbackResult to write, or nil if it already wrote an error response.
func processRunCompletion(
	ctx context.Context,
	w http.ResponseWriter,
	store RunCompletionStore,
	ghDispatch GHADispatchClient,
	project, runID string,
	payload CompletionPayload,
	workflowRunID *int64,
) *RunCallbackResult {
	// 1. Record completion data on the latest attempt.
	run, err := store.StampRunCompletion(ctx, project, runID, payload)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return nil
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "record completion failed")
		return nil
	}

	// 2. Build the run ref for the response.
	runRef := runRefFromData(run)

	// 3. Read the workflow.
	wf, err := store.GetWorkflowByName(ctx, run.Project, run.WorkflowName)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "read workflow failed")
		return nil
	}
	if wf == nil {
		// Workflow was deleted. Abort the run.
		reason := "workflow_registration_deleted"
		store.AbortRunByID(ctx, project, runID, reason) //nolint:errcheck
		return &RunCallbackResult{RunRef: runRef, Decision: strPtr("abort_malformed")}
	}

	// 4. Validate the completing phase still exists on the workflow.
	if len(run.Attempts) == 0 {
		writeProblem(w, http.StatusUnprocessableEntity, "run has no attempts")
		return nil
	}
	lastAttempt := run.Attempts[len(run.Attempts)-1]
	decisionWorkflow := serverPhasesToDecisionWorkflow(wf.Phases)
	phaseFound := false
	for _, p := range decisionWorkflow.Phases {
		if p.Name == lastAttempt.Phase {
			phaseFound = true
			break
		}
	}
	if !phaseFound {
		// Phase removed from workflow — treat as malformed.
		reason := "completing_phase_not_in_workflow"
		store.AbortRunByID(ctx, project, runID, reason) //nolint:errcheck
		return &RunCallbackResult{RunRef: runRef, Decision: strPtr("abort_malformed")}
	}

	// 5. Build decision run and fire the engine.
	decisionAttempts := make([]decision.Attempt, len(run.Attempts))
	for i, a := range run.Attempts {
		decisionAttempts[i] = decision.Attempt{
			Phase:      a.Phase,
			Conclusion: a.Conclusion,
		}
		if a.Verification != nil {
			decisionAttempts[i].Verification = &decision.Verification{
				Status:  decision.VerificationStatus(a.Verification.Status),
				Reasons: a.Verification.Reasons,
			}
		}
	}
	decisionRun := decision.Run{
		Attempts:          decisionAttempts,
		CumulativeCostUSD: run.CumulativeCostUSD,
		Budget:            budget.Config{Total: wf.Budget.Total},
	}
	verdict, err := decision.Decide(decisionRun, decisionWorkflow)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, fmt.Sprintf("decision engine: %s", err))
		return nil
	}

	// 6. Record the decision on the attempt.
	_ = store.StampRunDecision(ctx, project, runID, string(verdict)) // best-effort

	verdictStr := string(verdict)

	// 7. Route on decision.
	switch verdict {
	case decision.Retry:
		err := dispatchRetry(ctx, store, ghDispatch, run, wf, lastAttempt.Phase)
		if err != nil {
			// Retry dispatch failed — abort the run to prevent it getting stuck.
			abortReason := fmt.Sprintf("retry_dispatch_failed: %s", err)
			result, _ := store.AbortRunByID(ctx, project, runID, abortReason)
			verdictStr = "abort_budget_attempts"
			_ = result
		}
		return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}

	case decision.Advance:
		// Find what would happen next.
		var nextPhase *string
		for i, p := range wf.Phases {
			if p.Name == lastAttempt.Phase && i+1 < len(wf.Phases) {
				next := wf.Phases[i+1].Name
				nextPhase = &next
				break
			}
		}
		if nextPhase != nil {
			// More phases remaining — dispatch the next phase (simplified: treat as pending).
			// Full multi-phase DAG dispatch is complex; for now mark run as passed and let
			// the operator trigger the next phase if needed.
			// TODO: implement multi-phase forward dispatch.
		}
		// Mark run passed (or review_required if PR primitive enabled).
		state := "passed"
		if wf.PR.Enabled {
			state = "review_required"
		}
		result, err := store.SetRunTerminalState(ctx, project, runID, state, nil)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "mark run terminal failed")
			return nil
		}
		return &RunCallbackResult{
			RunRef:            runRef,
			Decision:          &verdictStr,
			IssueLockReleased: result.IssueLockReleased,
			PRLockReleased:    result.PRLockReleased,
		}

	default: // abort_budget_attempts, abort_budget_cost, abort_malformed
		explanation, _ := decision.AbortExplanation(decisionRun, decisionWorkflow, verdict)
		var abortReason *string
		if explanation != "" {
			abortReason = &explanation
		}
		result, err := store.SetRunTerminalState(ctx, project, runID, "aborted", abortReason)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "mark run aborted failed")
			return nil
		}
		return &RunCallbackResult{
			RunRef:            runRef,
			Decision:          &verdictStr,
			IssueLockReleased: result.IssueLockReleased,
			PRLockReleased:    result.PRLockReleased,
		}
	}
}

// dispatchRetry appends a new attempt and fires workflow_dispatch for a GHA retry.
func dispatchRetry(
	ctx context.Context,
	store RunCompletionStore,
	ghDispatch GHADispatchClient,
	run RunReplayData,
	wf *Workflow,
	failingPhase string,
) error {
	// Find the target phase from the recycle policy.
	var targetPhase *PhaseSpec
	for _, p := range wf.Phases {
		if p.Name == failingPhase {
			if p.RecyclePolicy != nil {
				target := p.RecyclePolicy.LandsAt
				if target == "self" || target == "" {
					target = failingPhase
				}
				for _, tp := range wf.Phases {
					if tp.Name == target {
						copy := tp
						targetPhase = &copy
						break
					}
				}
			}
			break
		}
	}
	if targetPhase == nil {
		return fmt.Errorf("no retry target phase found for %q", failingPhase)
	}
	if ghDispatch == nil {
		return fmt.Errorf("no GHA dispatch client configured")
	}

	// Acquire a lease for the target phase.
	wfName := wf.Name
	lease, host, err := store.AcquireLease(ctx, LeaseAcquireRequest{
		Project:      run.Project,
		Workflow:     &wfName,
		Requirements: targetPhase.Requirements,
		Requester: LeaseRequesterInput{
			Consumer: "run",
			Kind:     "retry",
			Ref:      publicids.RunRef(run.Project, positiveIssueNumber(run.IssueNumber), fmt.Sprintf("%d", run.IssueNumber)),
		},
	})
	if err != nil {
		return fmt.Errorf("acquire lease for retry: %w", err)
	}

	// Append the new attempt to the run doc.
	newAttemptIdx, err := store.AppendRunAttempt(ctx, run.Project, run.ID, targetPhase.Name, "gha_dispatch", targetPhase.WorkflowFilename)
	if err != nil {
		return fmt.Errorf("append retry attempt: %w", err)
	}

	// Compute the lease_ref dispatch input.
	var slotName string
	if m, ok := lease.Metadata["native_slot_name"].(string); ok {
		slotName = m
	}
	leaseRef := publicids.LeaseRef(lease.Project, slotName, lease.LeaseNumber)

	// Build dispatch inputs.
	inputs := map[string]string{
		"attempt_index": strconv.Itoa(newAttemptIdx),
		"lease_ref":     leaseRef,
	}
	if host != nil {
		inputs["host"] = host.Name
	}
	if t, ok := lease.Metadata["lease_callback_token"].(string); ok && t != "" {
		inputs["lease_callback_token"] = t
	}
	if lease.LeaseNumber != nil {
		inputs["lease_number"] = strconv.Itoa(*lease.LeaseNumber)
	}
	if run.CallbackToken != nil && *run.CallbackToken != "" {
		inputs["run_callback_token"] = *run.CallbackToken
	}
	if run.IssueNumber > 0 {
		inputs["issue_number"] = strconv.Itoa(run.IssueNumber)
	}
	// Construct run_ref from the run data.
	inputs["run_ref"] = publicids.RunRef(run.Project, positiveIssueNumber(run.IssueNumber), "")

	// Dispatch the GHA workflow.
	ref := targetPhase.WorkflowRef
	if ref == "" {
		ref = "main"
	}
	if err := ghDispatch.DispatchWorkflow(ctx, run.IssueRepo, targetPhase.WorkflowFilename, ref, inputs); err != nil {
		return fmt.Errorf("workflow_dispatch: %w", err)
	}
	return nil
}

func runRefFromData(run RunReplayData) string {
	return publicids.RunRef(run.Project, positiveIssueNumber(run.IssueNumber), "")
}

func positiveIssueNumber(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

func strPtr(s string) *string {
	return &s
}

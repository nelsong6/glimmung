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
	"github.com/nelsong6/glimmung/internal/domain/phaserefs"
	"github.com/nelsong6/glimmung/internal/domain/publicids"
)

// CompletionPayload carries the completion data to stamp on a run attempt.
type CompletionPayload struct {
	JobID               *string
	Conclusion          string
	VerificationStatus  string
	VerificationReasons []string
	CostUSD             float64
	SummaryMarkdown     *string
	ScreenshotsMarkdown *string
	PhaseOutputs        map[string]string
	AttemptIndex        *int
}

type NativeJobCompletionResult struct {
	Run             RunReplayData
	PhaseComplete   bool
	CompletionReady bool
	CompletedJobIDs []string
	PendingJobIDs   []string
	FailedJobIDs    []string
	PhasePayload    CompletionPayload
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
	AcquireLease(ctx context.Context, req LeaseAcquireRequest) (Lease, error)
}

type NativeJobCompletionStore interface {
	RecordNativeJobCompletion(ctx context.Context, project, runID string, p CompletionPayload) (NativeJobCompletionResult, error)
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

// nativeRunCompletedByCallbackToken handles POST /v1/run-callbacks/{callback_token}/native/completed.
func nativeRunCompletedByCallbackToken(store ReadStore, nativeLauncher NativeLauncher) http.HandlerFunc {
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
		if req.JobID == nil || *req.JobID == "" {
			writeProblem(w, http.StatusBadRequest, "job_id required")
			return
		}

		payload := completionPayloadFromNative(req)
		jobStore, ok := store.(NativeJobCompletionStore)
		if !ok || jobStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "native job completion store not configured")
			return
		}
		jobResult, err := jobStore.RecordNativeJobCompletion(r.Context(), project, runID, payload)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		}
		if errors.Is(err, ErrConflict) {
			writeProblem(w, http.StatusConflict, "native job completion conflict")
			return
		}
		var validationErr ValidationError
		if errors.As(err, &validationErr) {
			writeProblem(w, http.StatusBadRequest, validationErr.Message)
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "record native job completion failed")
			return
		}
		if !jobResult.CompletionReady {
			phaseComplete := jobResult.PhaseComplete
			decision := "wait_jobs"
			if phaseComplete {
				decision = "already_completed"
			}
			writeJSON(w, http.StatusOK, &RunCallbackResult{
				RunRef:          runRefFromData(jobResult.Run),
				Decision:        &decision,
				PhaseComplete:   &phaseComplete,
				CompletedJobIDs: jobResult.CompletedJobIDs,
				PendingJobIDs:   jobResult.PendingJobIDs,
				FailedJobIDs:    jobResult.FailedJobIDs,
			})
			return
		}

		result := processRunCompletion(r.Context(), w, completionStore, nativeLauncher, project, runID, jobResult.PhasePayload)
		if result != nil {
			phaseComplete := true
			result.PhaseComplete = &phaseComplete
			result.CompletedJobIDs = jobResult.CompletedJobIDs
			result.PendingJobIDs = jobResult.PendingJobIDs
			result.FailedJobIDs = jobResult.FailedJobIDs
			writeJSON(w, http.StatusOK, result)
		}
	}
}

// --- shared completion path ---

func completionPayloadFromNative(req NativeRunCompletedRequest) CompletionPayload {
	p := CompletionPayload{
		JobID:               req.JobID,
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

// processRunCompletion is the shared decision-engine path for native completions.
// Returns the RunCallbackResult to write, or nil if it already wrote an error response.
func processRunCompletion(
	ctx context.Context,
	w http.ResponseWriter,
	store RunCompletionStore,
	nativeLauncher NativeLauncher,
	project, runID string,
	payload CompletionPayload,
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
	run = withStampedAttemptDecision(run, lastAttempt.AttemptIndex, verdict, payload)

	verdictStr := string(verdict)

	// 7. Route on decision.
	switch verdict {
	case decision.Retry:
		err := dispatchRetry(ctx, store, nativeLauncher, run, wf, lastAttempt.Phase)
		if err != nil {
			abortReason := fmt.Sprintf("retry_dispatch_failed: %s", err)
			return abortRunWithWorkflowCleanup(ctx, w, store, nativeLauncher, run, wf, runRef, decision.AbortMalformed, abortReason)
		}
		return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}

	case decision.Advance:
		targets := allReadyDispatchTargets(wf, run, verdict)
		if len(targets) > 0 {
			for _, target := range targets {
				if err := dispatchForwardPhase(ctx, store, nativeLauncher, run, wf, target); err != nil {
					abortReason := fmt.Sprintf("forward_dispatch_failed: %s", err)
					return abortRunWithWorkflowCleanup(ctx, w, store, nativeLauncher, run, wf, runRef, decision.AbortMalformed, abortReason)
				}
			}
			verdictStr = "advance_phase"
			return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}
		}
		if hasInFlightAttempts(run) {
			return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}
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
		advancePlaybooksForTerminalRun(ctx, store, nativeLauncher, project, runID)
		return &RunCallbackResult{
			RunRef:            runRef,
			Decision:          &verdictStr,
			IssueLockReleased: result.IssueLockReleased,
			PRLockReleased:    result.PRLockReleased,
		}

	default: // abort_budget_attempts, abort_budget_cost, abort_malformed
		targets := allReadyDispatchTargets(wf, run, verdict)
		if len(targets) > 0 {
			for _, target := range targets {
				if err := dispatchForwardPhase(ctx, store, nativeLauncher, run, wf, target); err != nil {
					abortReason := fmt.Sprintf("teardown_dispatch_failed: %s", err)
					return markRunAborted(ctx, w, store, nativeLauncher, run, runRef, decision.AbortMalformed, abortReason)
				}
			}
			verdictStr = "advance_phase"
			return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}
		}
		if hasInFlightAttempts(run) {
			return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}
		}
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
		advancePlaybooksForTerminalRun(ctx, store, nativeLauncher, project, runID)
		return &RunCallbackResult{
			RunRef:            runRef,
			Decision:          &verdictStr,
			IssueLockReleased: result.IssueLockReleased,
			PRLockReleased:    result.PRLockReleased,
		}
	}
}

func abortRunWithWorkflowCleanup(
	ctx context.Context,
	w http.ResponseWriter,
	store RunCompletionStore,
	nativeLauncher NativeLauncher,
	run RunReplayData,
	wf *Workflow,
	runRef string,
	verdict decision.RunDecision,
	abortReason string,
) *RunCallbackResult {
	verdictStr := string(verdict)
	if wf != nil {
		targets := allReadyDispatchTargets(wf, run, verdict)
		if len(targets) > 0 {
			for _, target := range targets {
				if err := dispatchForwardPhase(ctx, store, nativeLauncher, run, wf, target); err != nil {
					return markRunAborted(ctx, w, store, nativeLauncher, run, runRef, decision.AbortMalformed, abortReason+"; cleanup_dispatch_failed: "+err.Error())
				}
			}
			decisionStr := "advance_phase"
			return &RunCallbackResult{RunRef: runRef, Decision: &decisionStr}
		}
		if hasInFlightAttempts(run) {
			return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}
		}
	}
	return markRunAborted(ctx, w, store, nativeLauncher, run, runRef, verdict, abortReason)
}

func markRunAborted(
	ctx context.Context,
	w http.ResponseWriter,
	store RunCompletionStore,
	nativeLauncher NativeLauncher,
	run RunReplayData,
	runRef string,
	verdict decision.RunDecision,
	abortReason string,
) *RunCallbackResult {
	reason := abortReason
	result, err := store.SetRunTerminalState(ctx, run.Project, run.ID, "aborted", &reason)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "mark run aborted failed")
		return nil
	}
	advancePlaybooksForTerminalRun(ctx, store, nativeLauncher, run.Project, run.ID)
	verdictStr := string(verdict)
	return &RunCallbackResult{
		RunRef:            runRef,
		Decision:          &verdictStr,
		IssueLockReleased: result.IssueLockReleased,
		PRLockReleased:    result.PRLockReleased,
	}
}

func advancePlaybooksForTerminalRun(ctx context.Context, store RunCompletionStore, nativeLauncher NativeLauncher, project, runID string) {
	pbStore, ok := store.(PlaybookRunStore)
	if !ok || pbStore == nil {
		return
	}
	readStore, ok := store.(ReadStore)
	if !ok || readStore == nil {
		return
	}
	dispatcher, ok := playbookEntryDispatcher(readStore, nativeLauncher)
	if !ok {
		return
	}
	_ = pbStore.AdvancePlaybooksForRun(ctx, project, runID, dispatcher)
}

func withStampedAttemptDecision(run RunReplayData, attemptIndex int, verdict decision.RunDecision, payload CompletionPayload) RunReplayData {
	for i := range run.Attempts {
		if run.Attempts[i].AttemptIndex != attemptIndex {
			continue
		}
		run.Attempts[i].Decision = string(verdict)
		run.Attempts[i].Completed = true
		run.Attempts[i].Conclusion = payload.Conclusion
		if payload.PhaseOutputs != nil {
			run.Attempts[i].PhaseOutputs = payload.PhaseOutputs
		}
		return run
	}
	if len(run.Attempts) > 0 {
		i := len(run.Attempts) - 1
		run.Attempts[i].Decision = string(verdict)
		run.Attempts[i].Completed = true
		run.Attempts[i].Conclusion = payload.Conclusion
		if payload.PhaseOutputs != nil {
			run.Attempts[i].PhaseOutputs = payload.PhaseOutputs
		}
	}
	return run
}

func allReadyDispatchTargets(wf *Workflow, run RunReplayData, verdict decision.RunDecision) []PhaseSpec {
	if wf == nil || len(run.Attempts) == 0 {
		return nil
	}
	completed := run.Attempts[len(run.Attempts)-1]
	completedPhase := phaseSpecByName(wf.Phases, completed.Phase)
	if completedPhase != nil && completedPhase.Always {
		return listReadyPhases(wf.Phases, run, true)
	}
	onAbortPath := verdict != decision.Advance || runHasNonAlwaysAbort(wf.Phases, run)
	if onAbortPath {
		if hasInFlightAttempts(run) {
			return nil
		}
		if phase := firstUnattemptedAlwaysPhase(wf.Phases, run); phase != nil {
			return []PhaseSpec{*phase}
		}
		return nil
	}
	if ready := listReadyPhases(wf.Phases, run, false); len(ready) > 0 {
		return ready
	}
	if hasInFlightAttempts(run) {
		return nil
	}
	return listReadyPhases(wf.Phases, run, true)
}

func listReadyPhases(phases []PhaseSpec, run RunReplayData, includeAlways bool) []PhaseSpec {
	completed := completedAdvancePhases(run)
	attempted := attemptedPhases(run)
	ready := make([]PhaseSpec, 0)
	for _, phase := range phases {
		if attempted[phase.Name] {
			continue
		}
		if phase.Always && !includeAlways {
			continue
		}
		if allPhaseDepsAdvanced(phase, completed) {
			ready = append(ready, phase)
		}
	}
	return ready
}

func completedAdvancePhases(run RunReplayData) map[string]bool {
	latestByPhase := map[string]RunAttemptData{}
	for _, attempt := range run.Attempts {
		latestByPhase[attempt.Phase] = attempt
	}
	advanced := map[string]bool{}
	for phase, attempt := range latestByPhase {
		if attempt.Decision == string(decision.Advance) {
			advanced[phase] = true
			continue
		}
		if isAbortDecision(attempt.Decision) {
			continue
		}
		if attempt.Decision == "" && attempt.Completed && attempt.Conclusion == "success" {
			advanced[phase] = true
		}
	}
	return advanced
}

func attemptedPhases(run RunReplayData) map[string]bool {
	attempted := map[string]bool{}
	for _, attempt := range run.Attempts {
		attempted[attempt.Phase] = true
	}
	return attempted
}

func allPhaseDepsAdvanced(phase PhaseSpec, completed map[string]bool) bool {
	for _, dep := range phase.DependsOn {
		if !completed[dep] {
			return false
		}
	}
	return true
}

func runHasNonAlwaysAbort(phases []PhaseSpec, run RunReplayData) bool {
	for _, attempt := range run.Attempts {
		phase := phaseSpecByName(phases, attempt.Phase)
		if phase != nil && phase.Always {
			continue
		}
		if isAbortDecision(attempt.Decision) {
			return true
		}
	}
	return false
}

func hasInFlightAttempts(run RunReplayData) bool {
	for _, attempt := range run.Attempts {
		if !attempt.Completed && attempt.Decision == "" {
			return true
		}
	}
	return false
}

func firstUnattemptedAlwaysPhase(phases []PhaseSpec, run RunReplayData) *PhaseSpec {
	completed := completedAdvancePhases(run)
	attempted := attemptedPhases(run)
	alwaysNames := map[string]bool{}
	for _, phase := range phases {
		if phase.Always {
			alwaysNames[phase.Name] = true
		}
	}
	for _, phase := range phases {
		if !phase.Always || attempted[phase.Name] {
			continue
		}
		ok := true
		for _, dep := range phase.DependsOn {
			if alwaysNames[dep] && !completed[dep] {
				ok = false
				break
			}
		}
		if ok {
			copy := phase
			return &copy
		}
	}
	return nil
}

func phaseSpecByName(phases []PhaseSpec, name string) *PhaseSpec {
	for _, phase := range phases {
		if phase.Name == name {
			copy := phase
			return &copy
		}
	}
	return nil
}

func isAbortDecision(value string) bool {
	switch value {
	case string(decision.AbortBudgetAttempts), string(decision.AbortBudgetCost), string(decision.AbortMalformed):
		return true
	default:
		return false
	}
}

func dispatchForwardPhase(
	ctx context.Context,
	store RunCompletionStore,
	nativeLauncher NativeLauncher,
	run RunReplayData,
	wf *Workflow,
	targetPhase PhaseSpec,
) error {
	if nativeLauncher == nil {
		return fmt.Errorf("no native launcher configured")
	}
	phaseKind := workflowPhaseKind(targetPhase.Kind)
	if err := validateNativeWorkflowKind(phaseKind); err != nil {
		return err
	}
	workflowFilename := targetPhase.WorkflowFilename
	if workflowFilename == "" {
		workflowFilename = fmt.Sprintf("%s:%s", phaseKind, targetPhase.Name)
	}
	substituted, err := substituteCompletionPhaseInputs(targetPhase, run)
	if err != nil {
		return err
	}
	newAttemptIdx, err := store.AppendRunAttempt(ctx, run.Project, run.ID, targetPhase.Name, phaseKind, workflowFilename)
	if err != nil {
		return fmt.Errorf("append forward attempt: %w", err)
	}
	runRef := runRefFromData(run)
	wfName := wf.Name
	metadata := map[string]any{
		"run_id":              run.ID,
		"run_ref":             runRef,
		"phase_name":          targetPhase.Name,
		"attempt_index":       strconv.Itoa(newAttemptIdx),
		"phase_inputs":        substituted,
		"issue_ref":           publicids.IssueRef(run.Project, positiveIssueNumber(run.IssueNumber)),
		"issue_number":        strconv.Itoa(run.IssueNumber),
		"work_context_branch": fmt.Sprintf("issue-%d-run-unknown", run.IssueNumber),
		"native_k8s":          true,
	}
	if run.CallbackToken != nil && *run.CallbackToken != "" {
		metadata["run_callback_token"] = *run.CallbackToken
	}
	if run.IssueLockHolderID != nil && *run.IssueLockHolderID != "" {
		metadata["issue_lock_holder_id"] = *run.IssueLockHolderID
	}
	requirements := targetPhase.Requirements
	if len(requirements) == 0 {
		requirements = wf.DefaultRequirements
	}
	lease, err := store.AcquireLease(ctx, LeaseAcquireRequest{
		Project:      run.Project,
		Workflow:     &wfName,
		Requirements: requirements,
		Metadata:     metadata,
	})
	if err != nil {
		return fmt.Errorf("acquire lease for forward phase: %w", err)
	}
	if lease.State != "claimed" {
		return fmt.Errorf("native lease was not claimed")
	}
	_, err = nativeLauncher.LaunchNativePhase(ctx, NativeLaunchRequest{
		Lease:    lease,
		Workflow: *wf,
		Phase:    targetPhase,
		Run:      runWithAttempt(run, newAttemptIdx, targetPhase.Name),
	})
	if err != nil {
		return fmt.Errorf("native dispatch: %w", err)
	}
	return nil
}

func substituteCompletionPhaseInputs(phase PhaseSpec, run RunReplayData) (map[string]string, error) {
	if len(phase.Inputs) == 0 {
		return map[string]string{}, nil
	}
	priorOutputs := map[string]map[string]string{}
	for _, attempt := range run.Attempts {
		if len(attempt.PhaseOutputs) > 0 {
			priorOutputs[attempt.Phase] = attempt.PhaseOutputs
		}
	}
	return phaserefs.Substitute(phaserefs.Phase{
		Name:    phase.Name,
		Inputs:  phase.Inputs,
		Outputs: phase.Outputs,
	}, priorOutputs)
}

// dispatchRetry appends a new attempt and launches the native retry phase.
func dispatchRetry(
	ctx context.Context,
	store RunCompletionStore,
	nativeLauncher NativeLauncher,
	run RunReplayData,
	wf *Workflow,
	failingPhase string,
) error {
	if nativeLauncher == nil {
		return fmt.Errorf("no native launcher configured")
	}
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

	phaseKind := workflowPhaseKind(targetPhase.Kind)
	if err := validateNativeWorkflowKind(phaseKind); err != nil {
		return err
	}
	workflowFilename := targetPhase.WorkflowFilename
	if workflowFilename == "" {
		workflowFilename = fmt.Sprintf("%s:%s", phaseKind, targetPhase.Name)
	}
	newAttemptIdx, err := store.AppendRunAttempt(ctx, run.Project, run.ID, targetPhase.Name, phaseKind, workflowFilename)
	if err != nil {
		return fmt.Errorf("append retry attempt: %w", err)
	}

	// Acquire a lease for the target phase.
	wfName := wf.Name
	metadata := map[string]any{
		"run_id":              run.ID,
		"run_ref":             runRefFromData(run),
		"phase_name":          targetPhase.Name,
		"attempt_index":       strconv.Itoa(newAttemptIdx),
		"issue_ref":           publicids.IssueRef(run.Project, positiveIssueNumber(run.IssueNumber)),
		"issue_number":        strconv.Itoa(run.IssueNumber),
		"work_context_branch": fmt.Sprintf("issue-%d-run-unknown", run.IssueNumber),
		"native_k8s":          true,
	}
	if run.CallbackToken != nil && *run.CallbackToken != "" {
		metadata["run_callback_token"] = *run.CallbackToken
	}
	if run.IssueLockHolderID != nil && *run.IssueLockHolderID != "" {
		metadata["issue_lock_holder_id"] = *run.IssueLockHolderID
	}
	requirements := targetPhase.Requirements
	if len(requirements) == 0 {
		requirements = wf.DefaultRequirements
	}
	lease, err := store.AcquireLease(ctx, LeaseAcquireRequest{
		Project:      run.Project,
		Workflow:     &wfName,
		Requirements: requirements,
		Metadata:     metadata,
		Requester: LeaseRequesterInput{
			Consumer: "run",
			Kind:     "retry",
			Ref:      publicids.RunRef(run.Project, positiveIssueNumber(run.IssueNumber), fmt.Sprintf("%d", run.IssueNumber)),
		},
	})
	if err != nil {
		return fmt.Errorf("acquire lease for retry: %w", err)
	}
	if lease.State != "claimed" {
		return fmt.Errorf("native lease was not claimed")
	}
	_, err = nativeLauncher.LaunchNativePhase(ctx, NativeLaunchRequest{
		Lease:    lease,
		Workflow: *wf,
		Phase:    *targetPhase,
		Run:      runWithAttempt(run, newAttemptIdx, targetPhase.Name),
	})
	if err != nil {
		return fmt.Errorf("native dispatch: %w", err)
	}
	return nil
}

func runWithAttempt(run RunReplayData, attemptIndex int, phase string) RunReplayData {
	out := run
	out.Attempts = append(append([]RunAttemptData{}, run.Attempts...), RunAttemptData{
		AttemptIndex: attemptIndex,
		Phase:        phase,
	})
	return out
}

func runRefFromData(run RunReplayData) string {
	display := ""
	if run.RunDisplayNumber != nil {
		display = *run.RunDisplayNumber
	}
	if display == "" && run.RunNumber != nil {
		display = strconv.Itoa(*run.RunNumber)
	}
	return publicids.RunRef(run.Project, positiveIssueNumber(run.IssueNumber), display)
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

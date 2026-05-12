package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/decision"
)

// RunReplayStore provides run and workflow reads needed by the replay route.
type RunReplayStore interface {
	ReadRunForReplay(ctx context.Context, project, runID string) (RunReplayData, error)
	GetWorkflowByName(ctx context.Context, project, name string) (*Workflow, error)
}

// RunReplayData is the minimal run state required by the decision engine replay and completion handling.
type RunReplayData struct {
	ID                string
	Project           string
	WorkflowName      string
	Attempts          []RunAttemptData
	CumulativeCostUSD float64
	IssueNumber       int
	RunNumber         *int
	RunDisplayNumber  *string
	IssueRepo         string
	CallbackToken     *string
	IssueLockHolderID *string
	PRNumber          *int
	PRLockHolderID    *string
}

// RunAttemptData holds one attempt's decision-engine-relevant fields.
type RunAttemptData struct {
	AttemptIndex int
	Phase        string
	Conclusion   string
	Verification *RunVerificationData
	Decision     string
	Completed    bool
	PhaseOutputs map[string]string
}

// RunVerificationData holds the status and reasons from a verification result.
type RunVerificationData struct {
	Status  string
	Reasons []string
}

// NativeRunFailedRequest is the body for POST …/native/failed.
type NativeRunFailedRequest struct {
	JobID  *string `json:"job_id"`
	Reason string  `json:"reason"`
}

// SyntheticCompletion mirrors the /completed callback body for in-memory replay.
type SyntheticCompletion struct {
	Conclusion   string            `json:"conclusion"`
	Verification map[string]any    `json:"verification"`
	PhaseOutputs map[string]string `json:"phase_outputs"`
}

// WorkflowReplayOverride lets a caller supply an alternate workflow shape for replay.
type WorkflowReplayOverride struct {
	Phases []PhaseSpec   `json:"phases"`
	PR     PrPrimitive   `json:"pr"`
	Budget budget.Config `json:"budget"`
}

// RunReplayRequest is the request body for POST …/replay.
type RunReplayRequest struct {
	SyntheticCompletion SyntheticCompletion     `json:"synthetic_completion"`
	OverrideWorkflow    *WorkflowReplayOverride `json:"override_workflow"`
}

// ReplayResult is the response for POST …/replay.
type ReplayResult struct {
	RunID                  string  `json:"run_id"`
	AppliedToPhase         string  `json:"applied_to_phase"`
	AppliedToAttemptIndex  int     `json:"applied_to_attempt_index"`
	Decision               string  `json:"decision"`
	AbortReason            *string `json:"abort_reason"`
	WouldAdvanceToPhase    *string `json:"would_advance_to_phase"`
	WouldOpenPR            bool    `json:"would_open_pr"`
	WouldRetryTargetPhase  *string `json:"would_retry_target_phase"`
	CumulativeCostUSDAfter float64 `json:"cumulative_cost_usd_after"`
	AttemptsInPhaseAfter   int     `json:"attempts_in_phase_after"`
	WorkflowSource         string  `json:"workflow_source"`
}

// nativeRunFailedByNumber handles POST …/runs/{run_number}/native/failed.
func nativeRunFailedByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mutStore, ok := store.(RunMutationStore)
		if !ok || mutStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run mutation store not configured")
			return
		}
		runID, project, ok := resolveRunByNumber(w, r, mutStore)
		if !ok {
			return
		}
		postNativeFailed(w, r, mutStore, project, runID)
	}
}

// nativeRunFailedByCallbackToken handles POST …/native/failed via callback token.
func nativeRunFailedByCallbackToken(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mutStore, ok := store.(RunMutationStore)
		if !ok || mutStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run mutation store not configured")
			return
		}
		runID, project, ok := resolveRunByCallbackToken(w, r, mutStore)
		if !ok {
			return
		}
		postNativeFailed(w, r, mutStore, project, runID)
	}
}

func postNativeFailed(w http.ResponseWriter, r *http.Request, store RunMutationStore, project, runID string) {
	var req NativeRunFailedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request body")
		return
	}
	reason := req.Reason
	if reason == "" {
		reason = "native_run_failed"
	}
	result, err := store.AbortRunByID(r.Context(), project, runID, reason)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "abort run failed")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// replayRunDecisionByNumber handles POST …/runs/{run_number}/replay (admin-only).
func replayRunDecisionByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		replayStore, ok := store.(RunReplayStore)
		if !ok || replayStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "replay store not configured")
			return
		}
		mutStore, ok := store.(RunMutationStore)
		if !ok || mutStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run mutation store not configured")
			return
		}

		runID, project, ok := resolveRunByNumber(w, r, mutStore)
		if !ok {
			return
		}

		var req RunReplayRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.SyntheticCompletion.Conclusion == "" {
			req.SyntheticCompletion.Conclusion = "success"
		}

		run, err := replayStore.ReadRunForReplay(r.Context(), project, runID)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "read run failed")
			return
		}
		if len(run.Attempts) == 0 {
			writeProblem(w, http.StatusUnprocessableEntity, "run has no attempts to replay against")
			return
		}

		var decisionWorkflow decision.Workflow
		var serverPhases []PhaseSpec
		var budgetTotal float64
		var prEnabled bool
		var workflowSource string

		if req.OverrideWorkflow != nil {
			serverPhases = req.OverrideWorkflow.Phases
			decisionWorkflow = serverPhasesToDecisionWorkflow(serverPhases)
			budgetTotal = req.OverrideWorkflow.Budget.Total
			if budgetTotal <= 0 {
				budgetTotal = budget.DefaultConfig().Total
			}
			prEnabled = req.OverrideWorkflow.PR.Enabled
			workflowSource = "override"
		} else {
			wf, err := replayStore.GetWorkflowByName(r.Context(), run.Project, run.WorkflowName)
			if err != nil {
				writeProblem(w, http.StatusInternalServerError, "read workflow failed")
				return
			}
			if wf == nil {
				writeProblem(w, http.StatusNotFound,
					"workflow not found; pass override_workflow if the live registration is missing")
				return
			}
			serverPhases = wf.Phases
			decisionWorkflow = serverPhasesToDecisionWorkflow(serverPhases)
			budgetTotal = wf.Budget.Total
			prEnabled = wf.PR.Enabled
			workflowSource = "registered"
		}

		// Validate the latest attempt's phase exists on the workflow.
		lastAttempt := run.Attempts[len(run.Attempts)-1]
		phaseFound := false
		for _, p := range decisionWorkflow.Phases {
			if p.Name == lastAttempt.Phase {
				phaseFound = true
				break
			}
		}
		if !phaseFound {
			writeProblem(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("run's latest attempt phase %q not in workflow phases; cannot replay", lastAttempt.Phase))
			return
		}

		// Build decision.Attempt slice, overriding the last attempt with synthetic completion.
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
		last := &decisionAttempts[len(decisionAttempts)-1]
		last.Conclusion = req.SyntheticCompletion.Conclusion
		if req.SyntheticCompletion.Verification != nil {
			statusRaw, _ := req.SyntheticCompletion.Verification["status"].(string)
			var reasons []string
			if rawReasons, ok2 := req.SyntheticCompletion.Verification["reasons"].([]any); ok2 {
				for _, rr := range rawReasons {
					if s, ok3 := rr.(string); ok3 {
						reasons = append(reasons, s)
					}
				}
			}
			if statusRaw != "" {
				last.Verification = &decision.Verification{
					Status:  decision.VerificationStatus(statusRaw),
					Reasons: reasons,
				}
			} else {
				last.Verification = nil
			}
		} else {
			last.Verification = nil
		}

		decisionRun := decision.Run{
			Attempts:          decisionAttempts,
			CumulativeCostUSD: run.CumulativeCostUSD,
			Budget:            budget.Config{Total: budgetTotal},
		}

		verdict, err := decision.Decide(decisionRun, decisionWorkflow)
		if err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, fmt.Sprintf("decision engine: %s", err))
			return
		}

		result := ReplayResult{
			RunID:                  run.ID,
			AppliedToPhase:         lastAttempt.Phase,
			AppliedToAttemptIndex:  lastAttempt.AttemptIndex,
			Decision:               string(verdict),
			WorkflowSource:         workflowSource,
			CumulativeCostUSDAfter: decisionRun.CumulativeCostUSD,
			AttemptsInPhaseAfter:   replayAttemptsInPhase(decisionAttempts, lastAttempt.Phase),
		}

		switch verdict {
		case decision.Advance:
			for i, p := range decisionWorkflow.Phases {
				if p.Name == lastAttempt.Phase {
					if i+1 < len(decisionWorkflow.Phases) {
						next := decisionWorkflow.Phases[i+1].Name
						result.WouldAdvanceToPhase = &next
					} else {
						result.WouldOpenPR = prEnabled
					}
					break
				}
			}
		case decision.Retry:
			for _, p := range serverPhases {
				if p.Name == lastAttempt.Phase && p.RecyclePolicy != nil {
					target := p.RecyclePolicy.LandsAt
					if target == "self" || target == "" {
						target = lastAttempt.Phase
					}
					result.WouldRetryTargetPhase = &target
					break
				}
			}
		default:
			explanation, expErr := decision.AbortExplanation(decisionRun, decisionWorkflow, verdict)
			if expErr == nil && explanation != "" {
				result.AbortReason = &explanation
			}
		}

		writeJSON(w, http.StatusOK, result)
	}
}

func serverPhasesToDecisionWorkflow(phases []PhaseSpec) decision.Workflow {
	dPhases := make([]decision.PhaseSpec, 0, len(phases))
	for _, p := range phases {
		var rp *decision.RecyclePolicy
		if p.RecyclePolicy != nil {
			rp = &decision.RecyclePolicy{
				MaxAttempts: p.RecyclePolicy.MaxAttempts,
				On:          p.RecyclePolicy.On,
			}
		}
		dPhases = append(dPhases, decision.PhaseSpec{
			Name:                     p.Name,
			Verify:                   p.Verify,
			EvidenceVerificationGate: p.EvidenceVerificationGate,
			Always:                   p.Always,
			RecyclePolicy:            rp,
		})
	}
	return decision.Workflow{Phases: dPhases}
}

func replayAttemptsInPhase(attempts []decision.Attempt, phase string) int {
	count := 0
	for _, a := range attempts {
		if a.Phase == phase {
			count++
		}
	}
	return count
}

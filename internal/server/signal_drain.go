package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type SignalDrainStore interface {
	ListPendingSignals(ctx context.Context, limit int) ([]QueuedSignal, error)
	MarkSignalProcessing(ctx context.Context, signal QueuedSignal) (QueuedSignal, bool, error)
	MarkSignalProcessed(ctx context.Context, signal QueuedSignal, decision string) (QueuedSignal, error)
	MarkSignalFailed(ctx context.Context, signal QueuedSignal, reason string) error
	ClaimLock(ctx context.Context, scope, key, holderID string, ttlSeconds int, metadata map[string]any) error
	ReleaseLock(ctx context.Context, scope, key, holderID string) bool
	FindRunForPR(ctx context.Context, repo string, prNumber int) (RunReplayData, error)
	GetWorkflowByName(ctx context.Context, project, name string) (*Workflow, error)
	ClaimIssueLock(ctx context.Context, project string, issueNumber int, holderID string, ttlSeconds int) error
	ReleaseIssueLock(ctx context.Context, project string, issueNumber int, holderID string)
	ReopenRunForTriage(ctx context.Context, req TriageReopenRequest) (RunReplayData, int, error)
	AcquireLease(ctx context.Context, req LeaseAcquireRequest) (Lease, error)
	AbortRunByID(ctx context.Context, project, runID, reason string) (AbortRunResult, error)
}

type QueuedSignal struct {
	ID         string
	TargetType string
	TargetRepo string
	TargetID   string
	Source     string
	Payload    map[string]any
	State      string
	EnqueuedAt time.Time
}

type TriageReopenRequest struct {
	Project           string
	RunID             string
	PhaseName         string
	PhaseKind         string
	WorkflowFilename  string
	IssueLockHolderID string
	PRLockHolderID    string
}

type SignalDrainResult struct {
	Processed int                   `json:"processed"`
	Skipped   int                   `json:"skipped"`
	Failed    int                   `json:"failed"`
	Decisions []SignalDrainDecision `json:"decisions"`
}

type SignalDrainDecision struct {
	SignalID string  `json:"signal_id"`
	Decision string  `json:"decision"`
	Detail   *string `json:"detail,omitempty"`
}

type triageDecisionResult struct {
	Decision string
	HoldLock bool
	Detail   *string
	Run      RunReplayData
	Workflow *Workflow
	Target   *PhaseSpec
	Feedback string
}

const (
	triageDispatch              = "dispatch_triage"
	triageIgnore                = "ignore"
	triageAbortNoRun            = "abort_no_run"
	triageAbortBudgetAttempts   = "abort_budget_attempts"
	triageAbortBudgetCost       = "abort_budget_cost"
	defaultSignalDrainBatchSize = 50
)

func drainSignalsHandler(store ReadStore, nativeLauncher NativeLauncher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := DrainSignals(r.Context(), store, nativeLauncher, defaultSignalDrainBatchSize)
		if err != nil {
			writeProblem(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func DrainSignals(ctx context.Context, store ReadStore, nativeLauncher NativeLauncher, limit int) (SignalDrainResult, error) {
	drainStore, ok := store.(SignalDrainStore)
	if !ok || drainStore == nil {
		return SignalDrainResult{}, errors.New("signal drain store not configured")
	}
	if limit <= 0 {
		limit = defaultSignalDrainBatchSize
	}
	pending, err := drainStore.ListPendingSignals(ctx, limit)
	if err != nil {
		return SignalDrainResult{}, err
	}
	var result SignalDrainResult
	for _, signal := range pending {
		scope, key := signalLockScopeKey(signal)
		holderID := signal.ID
		if err := drainStore.ClaimLock(ctx, scope, key, holderID, defaultIssueLockTTLSeconds, map[string]any{
			"signal_id": signal.ID,
			"source":    signal.Source,
		}); err != nil {
			if errors.Is(err, ErrAlreadyRunning) {
				result.Skipped++
				continue
			}
			result.Failed++
			continue
		}

		claimed, ok, err := drainStore.MarkSignalProcessing(ctx, signal)
		if err != nil || !ok {
			drainStore.ReleaseLock(ctx, scope, key, holderID)
			if err != nil {
				result.Failed++
			} else {
				result.Skipped++
			}
			continue
		}
		signal = claimed

		decision, err := decideTriageSignal(ctx, drainStore, signal)
		if err != nil {
			_ = drainStore.MarkSignalFailed(ctx, signal, err.Error())
			drainStore.ReleaseLock(ctx, scope, key, holderID)
			result.Failed++
			continue
		}

		processed, err := drainStore.MarkSignalProcessed(ctx, signal, decision.Decision)
		if err != nil {
			drainStore.ReleaseLock(ctx, scope, key, holderID)
			result.Failed++
			continue
		}
		signal = processed

		if decision.Decision == triageDispatch {
			if err := dispatchTriage(ctx, drainStore, nativeLauncher, signal, holderID, decision); err != nil {
				_ = drainStore.MarkSignalFailed(ctx, signal, err.Error())
				drainStore.ReleaseLock(ctx, scope, key, holderID)
				result.Failed++
				continue
			}
		}

		if !decision.HoldLock {
			drainStore.ReleaseLock(ctx, scope, key, holderID)
		}
		result.Processed++
		result.Decisions = append(result.Decisions, SignalDrainDecision{
			SignalID: signal.ID,
			Decision: decision.Decision,
			Detail:   decision.Detail,
		})
	}
	return result, nil
}

func signalLockScopeKey(signal QueuedSignal) (string, string) {
	scope := signal.TargetType
	if scope == "" {
		scope = "signal"
	}
	return scope, signal.TargetRepo + "#" + signal.TargetID
}

func decideTriageSignal(ctx context.Context, store SignalDrainStore, signal QueuedSignal) (triageDecisionResult, error) {
	if signal.TargetType != "pr" {
		return triageDecisionResult{Decision: triageIgnore}, nil
	}
	if !triageActionable(signal) {
		return triageDecisionResult{Decision: triageIgnore}, nil
	}
	prNumber, err := strconv.Atoi(signal.TargetID)
	if err != nil || prNumber < 1 {
		return triageDecisionResult{Decision: triageAbortNoRun, Detail: stringPtr("PR target is not a positive number")}, nil
	}
	run, err := store.FindRunForPR(ctx, signal.TargetRepo, prNumber)
	if errors.Is(err, ErrNotFound) {
		return triageDecisionResult{Decision: triageAbortNoRun, Detail: stringPtr(triageAbortExplanation(triageAbortNoRun, signal, RunReplayData{}, nil))}, nil
	}
	if err != nil {
		return triageDecisionResult{}, err
	}
	wf, err := store.GetWorkflowByName(ctx, run.Project, run.WorkflowName)
	if err != nil {
		return triageDecisionResult{}, err
	}
	if wf == nil || wf.PR.RecyclePolicy == nil {
		return triageDecisionResult{Decision: triageIgnore}, nil
	}
	target := phaseSpecByName(wf.Phases, wf.PR.RecyclePolicy.LandsAt)
	if target == nil {
		return triageDecisionResult{Decision: triageAbortNoRun, Detail: stringPtr("recycle target phase is not registered")}, nil
	}
	budgetTotal := run.Budget.Total
	if budgetTotal <= 0 {
		budgetTotal = wf.Budget.Total
	}
	if budgetTotal > 0 && run.CumulativeCostUSD >= budgetTotal {
		return triageDecisionResult{Decision: triageAbortBudgetCost, Detail: stringPtr(triageAbortExplanation(triageAbortBudgetCost, signal, run, wf))}, nil
	}
	attempts := 0
	for _, attempt := range run.Attempts {
		if attempt.Phase == target.Name {
			attempts++
		}
	}
	if wf.PR.RecyclePolicy.MaxAttempts > 0 && attempts >= wf.PR.RecyclePolicy.MaxAttempts {
		return triageDecisionResult{Decision: triageAbortBudgetAttempts, Detail: stringPtr(triageAbortExplanation(triageAbortBudgetAttempts, signal, run, wf))}, nil
	}
	return triageDecisionResult{
		Decision: triageDispatch,
		HoldLock: true,
		Run:      run,
		Workflow: wf,
		Target:   target,
		Feedback: triageFeedbackText(signal),
	}, nil
}

func triageActionable(signal QueuedSignal) bool {
	switch signal.Source {
	case "glimmung_ui":
		return stringValue(signal.Payload["kind"]) == "reject"
	case "gh_review":
		return stringValue(signal.Payload["state"]) == "changes_requested" && strings.TrimSpace(stringValue(signal.Payload["body"])) != ""
	default:
		return false
	}
}

func triageFeedbackText(signal QueuedSignal) string {
	switch signal.Source {
	case "glimmung_ui":
		return stringValue(signal.Payload["feedback"])
	case "gh_review":
		return stringValue(signal.Payload["body"])
	default:
		return ""
	}
}

func triageAbortExplanation(decision string, signal QueuedSignal, run RunReplayData, wf *Workflow) string {
	switch decision {
	case triageAbortNoRun:
		return fmt.Sprintf("Glimmung received PR feedback on %s#%s but could not find an agent-tracked run for it. No action taken.", signal.TargetRepo, signal.TargetID)
	case triageAbortBudgetCost:
		capTotal := run.Budget.Total
		if capTotal <= 0 && wf != nil {
			capTotal = wf.Budget.Total
		}
		return fmt.Sprintf("Glimmung cannot dispatch a recycle: cumulative cost $%.2f is at or over the budget cap $%.2f.", run.CumulativeCostUSD, capTotal)
	case triageAbortBudgetAttempts:
		capText := "the configured cap"
		if wf != nil && wf.PR.RecyclePolicy != nil {
			capText = fmt.Sprintf("max_attempts=%d", wf.PR.RecyclePolicy.MaxAttempts)
		}
		return "Glimmung cannot dispatch a recycle: attempts on the recycle target have reached " + capText + "."
	default:
		return "Triage aborted: " + decision
	}
}

func dispatchTriage(ctx context.Context, store SignalDrainStore, nativeLauncher NativeLauncher, signal QueuedSignal, holderID string, decision triageDecisionResult) error {
	run := decision.Run
	target := decision.Target
	if target == nil || decision.Workflow == nil {
		return errors.New("triage dispatch missing workflow target")
	}
	if nativeLauncher == nil {
		return errors.New("no native launcher configured")
	}
	if err := store.ClaimIssueLock(ctx, run.Project, run.IssueNumber, holderID, defaultIssueLockTTLSeconds); err != nil {
		return fmt.Errorf("claim triage issue lock: %w", err)
	}

	phaseKind := workflowPhaseKind(target.Kind)
	if err := validateNativeWorkflowKind(phaseKind); err != nil {
		store.ReleaseIssueLock(ctx, run.Project, run.IssueNumber, holderID)
		return err
	}
	workflowFilename := target.WorkflowFilename
	if workflowFilename == "" {
		workflowFilename = fmt.Sprintf("%s:%s", phaseKind, target.Name)
	}
	reopened, attemptIndex, err := store.ReopenRunForTriage(ctx, TriageReopenRequest{
		Project:           run.Project,
		RunID:             run.ID,
		PhaseName:         target.Name,
		PhaseKind:         phaseKind,
		WorkflowFilename:  workflowFilename,
		IssueLockHolderID: holderID,
		PRLockHolderID:    holderID,
	})
	if err != nil {
		store.ReleaseIssueLock(ctx, run.Project, run.IssueNumber, holderID)
		return fmt.Errorf("reopen run for triage: %w", err)
	}

	wfName := decision.Workflow.Name
	metadata := map[string]any{
		"run_id":               reopened.ID,
		"run_ref":              runRefFromData(reopened),
		"phase_name":           target.Name,
		"attempt_index":        strconv.Itoa(attemptIndex),
		"issue_number":         strconv.Itoa(reopened.IssueNumber),
		"issue_lock_holder_id": holderID,
		"triage_signal_id":     signal.ID,
		"feedback":             decision.Feedback,
		"native_k8s":           true,
	}
	if reopened.CallbackToken != nil && *reopened.CallbackToken != "" {
		metadata["run_callback_token"] = *reopened.CallbackToken
	}
	requirements := target.Requirements
	if len(requirements) == 0 {
		requirements = decision.Workflow.DefaultRequirements
	}
	lease, err := acquireLeaseInstrumented(ctx, LeasePurposeSignalDrain, LeaseAcquireRequest{
		Project:      reopened.Project,
		Workflow:     &wfName,
		Requirements: requirements,
		Metadata:     metadata,
	}, store.AcquireLease)
	if err != nil {
		_, _ = store.AbortRunByID(ctx, reopened.Project, reopened.ID, "triage_lease_acquire_failed: "+err.Error())
		return fmt.Errorf("acquire triage lease: %w", err)
	}
	if lease.State != "claimed" {
		_, _ = store.AbortRunByID(ctx, reopened.Project, reopened.ID, "triage_dispatch_failed: native lease was not claimed")
		return errors.New("native lease was not claimed")
	}
	if _, err := nativeLauncher.LaunchNativePhase(ctx, NativeLaunchRequest{
		Lease:    lease,
		Workflow: *decision.Workflow,
		Phase:    *target,
		Run:      runWithAttempt(reopened, attemptIndex, target.Name),
	}); err != nil {
		_, _ = store.AbortRunByID(ctx, reopened.Project, reopened.ID, "triage_dispatch_failed: "+err.Error())
		return fmt.Errorf("native dispatch: %w", err)
	}
	return nil
}

func StartSignalDrainLoop(ctx context.Context, store ReadStore, nativeLauncher NativeLauncher, interval time.Duration, logf func(string, ...any)) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := DrainSignals(ctx, store, nativeLauncher, defaultSignalDrainBatchSize)
			if err != nil {
				if logf != nil {
					logf("signal drain failed: %v", err)
				}
				continue
			}
			if logf != nil && (result.Processed > 0 || result.Failed > 0) {
				logf("signal drain processed=%d failed=%d skipped=%d", result.Processed, result.Failed, result.Skipped)
			}
		}
	}
}

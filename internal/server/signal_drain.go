package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type SignalDrainStore interface {
	RunDispatchStore
	ListPendingSignals(ctx context.Context, limit int) ([]QueuedSignal, error)
	MarkSignalProcessing(ctx context.Context, signal QueuedSignal) (QueuedSignal, bool, error)
	MarkSignalProcessed(ctx context.Context, signal QueuedSignal, decision string) (QueuedSignal, error)
	MarkSignalFailed(ctx context.Context, signal QueuedSignal, reason string) error
	ClaimLock(ctx context.Context, scope, key, holderID string, ttlSeconds int, metadata map[string]any) error
	ReleaseLock(ctx context.Context, scope, key, holderID string) bool
	FindRunForPR(ctx context.Context, repo string, prNumber int) (RunReplayData, error)
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

var signalDrainWake atomic.Value // stores func()

const (
	triageDispatch              = "dispatch_triage"
	triageReleaseGate           = "release_touchpoint_gate"
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
			if err := dispatchTriage(ctx, drainStore, nativeLauncher, signal, decision); err != nil {
				_ = drainStore.MarkSignalFailed(ctx, signal, err.Error())
				drainStore.ReleaseLock(ctx, scope, key, holderID)
				result.Failed++
				continue
			}
		}
		if decision.Decision == triageReleaseGate {
			if err := releaseTouchpointGate(ctx, drainStore, nativeLauncher, decision); err != nil {
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

func StartSignalDrainReconciler(ctx context.Context, store ReadStore, nativeLauncher NativeLauncher, logf func(string, ...any)) {
	if _, ok := store.(SignalDrainStore); !ok || store == nil || nativeLauncher == nil {
		return
	}
	wakeCh := make(chan struct{}, 128)
	signalDrainWake.Store(func() {
		select {
		case wakeCh <- struct{}{}:
		default:
		}
	})
	wakeSignalDrain()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-wakeCh:
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
	}()
}

func wakeSignalDrain() {
	fn, ok := signalDrainWake.Load().(func())
	if ok && fn != nil {
		fn()
	}
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
	if wf == nil {
		return triageDecisionResult{Decision: triageIgnore}, nil
	}

	// approve: release the review gate by launching its pr_merge job.
	// The run must be parked at the gate (review_required) and the workflow
	// must declare a purpose=review_gate phase for the approve to be actionable.
	// Approve is idempotent — a second approve while the merge is in flight
	// or already complete is a safe no-op surfaced as "ignore."
	if signalIsApprove(signal) {
		gate := phaseSpecByName(wf.Phases, "")
		for _, phase := range wf.Phases {
			if phasePurpose(phase) == PhasePurposeReviewGate {
				p := phase
				gate = &p
				break
			}
		}
		if gate == nil {
			return triageDecisionResult{Decision: triageIgnore, Detail: stringPtr("workflow has no review_gate phase; approve has nothing to release")}, nil
		}
		return triageDecisionResult{
			Decision: triageReleaseGate,
			Run:      run,
			Workflow: wf,
			Target:   gate,
		}, nil
	}

	if wf.PR.RecyclePolicy == nil {
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
		Run:      run,
		Workflow: wf,
		Target:   target,
		Feedback: triageFeedbackText(signal),
	}, nil
}

func triageActionable(signal QueuedSignal) bool {
	switch signal.Source {
	case "glimmung_ui":
		kind := stringValue(signal.Payload["kind"])
		return kind == "reject" || kind == "approve"
	case "gh_review":
		return stringValue(signal.Payload["state"]) == "changes_requested" && strings.TrimSpace(stringValue(signal.Payload["body"])) != ""
	default:
		return false
	}
}

func signalIsApprove(signal QueuedSignal) bool {
	return signal.Source == "glimmung_ui" && stringValue(signal.Payload["kind"]) == "approve"
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

// releaseTouchpointGate launches the pr_merge job for a parked review gate
// phase in response to an approve signal. The run state transitions from
// review_required → in_progress before the launch so the dispatch path
// sees a normal in-progress run. If the run is not in fact parked at
// review_required this is a safe no-op (the approve arrived too late or
// twice; idempotent).
func releaseTouchpointGate(ctx context.Context, store SignalDrainStore, nativeLauncher NativeLauncher, decision triageDecisionResult) error {
	if decision.Run.State != "review_required" {
		// Not parked at the gate (already advanced, already merged, or
		// recycled by a competing reject). Treat approve as a benign no-op.
		return nil
	}
	completionStore, ok := any(store).(RunCompletionStore)
	if !ok || completionStore == nil {
		return fmt.Errorf("approve drain requires the RunCompletionStore surface")
	}
	if decision.Target == nil {
		return fmt.Errorf("approve drain missing target gate phase")
	}
	if decision.Workflow == nil {
		return fmt.Errorf("approve drain missing workflow")
	}
	if err := launchTouchpointGateMerge(ctx, completionStore, nativeLauncher, decision.Run, decision.Workflow, *decision.Target); err != nil {
		return fmt.Errorf("launch touchpoint gate: %w", err)
	}
	return nil
}

// launchTouchpointGateMerge releases the existing parked gate attempt and
// launches its pr_merge job. The merge job calls back into the standard
// completion endpoint, so the workflow advances to cleanup automatically once
// GitHub returns.
func launchTouchpointGateMerge(
	ctx context.Context,
	store RunCompletionStore,
	nativeLauncher NativeLauncher,
	run RunReplayData,
	wf *Workflow,
	gate PhaseSpec,
) error {
	if nativeLauncher == nil {
		return fmt.Errorf("no native launcher configured")
	}
	phaseKind := workflowPhaseKind(gate.Kind)
	if phaseKind != workflowKindNativeK8sJob {
		return fmt.Errorf("expected review gate executor kind %q, got %q", workflowKindNativeK8sJob, phaseKind)
	}
	if phasePurpose(gate) != PhasePurposeReviewGate {
		return fmt.Errorf("expected review_gate phase, got purpose=%q", phasePurpose(gate))
	}
	attemptIdx, ok := latestAttemptIndexForPhase(run, gate.Name)
	if !ok {
		return fmt.Errorf("run is review_required but has no parked attempt for phase %q", gate.Name)
	}
	if err := store.ReleaseReviewGate(ctx, run.Project, run.ID, gate.Name, attemptIdx); err != nil {
		return fmt.Errorf("release gate attempt: %w", err)
	}
	lease, err := leaseForRunPhase(ctx, store, run, gate.Name, attemptIdx, nil)
	if err != nil {
		return fmt.Errorf("read lease for gate: %w", err)
	}
	if lease.State != "claimed" {
		return nativeLeaseNotClaimedError(lease)
	}
	canonical := CanonicalNativePhase(gate)
	started := runWithLatestAttempt(run, attemptIdx, gate.Name)
	launched, err := launchCommittedNativePhase(ctx, nativeLauncher, NativeLaunchRequest{
		Lease:    lease,
		Workflow: *wf,
		Phase:    canonical,
		Run:      started,
	})
	if err != nil {
		return fmt.Errorf("native dispatch: %w", err)
	}
	_ = recordLaunchedNativeJobs(ctx, store, started, canonical, launched)
	return nil
}

func latestAttemptIndexForPhase(run RunReplayData, phase string) (int, bool) {
	for i := len(run.Attempts) - 1; i >= 0; i-- {
		attempt := run.Attempts[i]
		if attempt.Phase == phase && !attempt.Completed && attempt.Decision == "" {
			return attempt.AttemptIndex, true
		}
	}
	return 0, false
}

func dispatchTriage(ctx context.Context, store SignalDrainStore, nativeLauncher NativeLauncher, signal QueuedSignal, decision triageDecisionResult) error {
	run := decision.Run
	if decision.Workflow == nil {
		return errors.New("triage dispatch missing workflow")
	}
	if nativeLauncher == nil {
		return errors.New("no native launcher configured")
	}
	triggerSource := map[string]any{
		"kind":             "pr_feedback",
		"triage_signal_id": signal.ID,
		"feedback":         decision.Feedback,
		"previous_run_id":  run.ID,
		"source":           signal.Source,
	}
	result, problem := dispatchRun(ctx, store, nativeLauncher, DispatchRunRequest{
		Project:       run.Project,
		IssueNumber:   run.IssueNumber,
		WorkflowName:  decision.Workflow.Name,
		TriggerSource: triggerSource,
	})
	if problem != nil {
		return errors.New(problem.message)
	}
	switch result.State {
	case "dispatched", "queued":
		return nil
	default:
		detail := ""
		if result.Detail != nil {
			detail = ": " + *result.Detail
		}
		return fmt.Errorf("triage dispatch returned %s%s", result.State, detail)
	}
}

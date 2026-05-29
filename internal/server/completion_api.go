package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/decision"
	"github.com/nelsong6/glimmung/internal/domain/phaserefs"
	"github.com/nelsong6/glimmung/internal/domain/publicids"
	"github.com/nelsong6/glimmung/internal/metrics"
)

// CompletionPayload carries the completion data to stamp on a run attempt.
type CompletionPayload struct {
	JobID               *string
	Conclusion          string
	VerificationStatus  string
	VerificationReasons []string
	EvidenceRefs        []string
	Evidence            []EvidenceArtifact
	CostUSD             float64
	SummaryMarkdown     *string
	ScreenshotsMarkdown *string
	PhaseOutputs        map[string]string
	AttemptIndex        *int

	// TerminalReason is an optional closed-enum reason the caller has
	// already derived (e.g. the reconciler maps k8s Failed condition
	// reason="DeadlineExceeded" to TerminalReason="deadline_exceeded").
	// When set it overrides the generic conclusion-to-reason mapping at
	// the store layer so RunJobExecution.Reason carries the precise
	// failure mode the operator needs. Empty means the store derives
	// the reason from Conclusion as before.
	//
	// Values are bounded to the JobTerminalReason* enum below so the
	// metric label cardinality stays small.
	TerminalReason string

	// LogArchiveURL is the durable pointer to where this attempt's
	// logs can be reviewed. Today this is a Grafana Explore deep-link
	// to the cluster Loki datasource scoped to the failing pod's
	// namespace + time window. A future Stage 3 of the inner-Job
	// observation contract may upgrade this to an artifact-store URL
	// surviving Loki retention. Empty means the caller has no link to
	// offer; the dashboard renders the archive surface as unavailable.
	LogArchiveURL string
}

// JobTerminalReason* is the closed enum for CompletionPayload.TerminalReason
// and the matching glimmung_run_phase_job_terminal_total{reason} metric
// label. Cardinality is bounded by construction; any other string
// arriving from a caller collapses to JobTerminalReasonUnknown.
const (
	// Pod hit activeDeadlineSeconds and was killed by kubelet.
	JobTerminalReasonDeadlineExceeded = "deadline_exceeded"
	// Job controller exceeded its backoff limit (typically pod failed
	// fast with backoffLimit=0).
	JobTerminalReasonBackoffExceeded = "backoff_exceeded"
	// The Job no longer exists in k8s — TTL'd or externally deleted —
	// before the runner could deliver a callback.
	JobTerminalReasonPodGone = "pod_gone"
	// Job reached Complete=True in k8s but the runner never delivered
	// the /completed callback. Surfaces as failed because evidence is
	// missing.
	JobTerminalReasonCallbackLost = "callback_lost"
	// Catch-all for runner-reported failures with no specific reason.
	JobTerminalReasonJobFailed = "job_failed"
	// Verification evidence said pass — used for runner-reported
	// success completions.
	JobTerminalReasonSucceeded = ""
	// Runner-reported timeout (not k8s-driven).
	JobTerminalReasonTimeout = "timeout"
	// Runner-reported cancellation.
	JobTerminalReasonCancelled = "cancelled"
	// Verification step explicitly failed.
	JobTerminalReasonVerificationFailed = "verification_failed"
	// Verification step errored.
	JobTerminalReasonVerificationError = "verification_error"
	// Catch-all when no reason can be derived.
	JobTerminalReasonUnknown = "unknown"
)

// IsKnownJobTerminalReason reports whether reason is in the closed
// JobTerminalReason* enum. Empty string maps to true (the success
// sentinel).
func IsKnownJobTerminalReason(reason string) bool {
	switch reason {
	case JobTerminalReasonSucceeded,
		JobTerminalReasonDeadlineExceeded,
		JobTerminalReasonBackoffExceeded,
		JobTerminalReasonPodGone,
		JobTerminalReasonCallbackLost,
		JobTerminalReasonJobFailed,
		JobTerminalReasonTimeout,
		JobTerminalReasonCancelled,
		JobTerminalReasonVerificationFailed,
		JobTerminalReasonVerificationError,
		JobTerminalReasonUnknown:
		return true
	}
	return false
}

// NormalizeJobTerminalReason collapses an unknown reason string to
// JobTerminalReasonUnknown so the metric label cardinality stays
// bounded.
func NormalizeJobTerminalReason(reason string) string {
	if IsKnownJobTerminalReason(reason) {
		return reason
	}
	return JobTerminalReasonUnknown
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
	GetWorkflowBySchemaRef(ctx context.Context, project, schemaRef string) (*Workflow, error)
	StampRunCompletion(ctx context.Context, project, runID string, p CompletionPayload) (RunReplayData, error)
	StampRunDecision(ctx context.Context, project, runID, decision string) error
	SetRunTerminalState(ctx context.Context, project, runID, state string, abortReason *string) (AbortRunResult, error)
	SetRunReviewRequired(ctx context.Context, project, runID string) error
	SetRunInProgress(ctx context.Context, project, runID string) error
	StampLatestAttemptSkipped(ctx context.Context, project, runID string) error
	CreateRecycleCycle(ctx context.Context, req CreateRecycleCycleRequest) (CreatedRun, error)
	AppendRunAttempt(ctx context.Context, project, runID, phase, phaseKind, workflowFilename string) (int, error)
	StartRunCycle(ctx context.Context, req StartRunCycleRequest) (int, error)
	ReadLeaseByRef(ctx context.Context, project, ref string) (Lease, error)
	CancelLeaseByRef(ctx context.Context, project, ref string) (CancelLeaseResult, error)
}

type NativeJobCompletionStore interface {
	RecordNativeJobCompletion(ctx context.Context, project, runID string, p CompletionPayload) (NativeJobCompletionResult, error)
}

// NativeRunCompletedRequest is the body for POST /run-callbacks/{token}/native/completed.
type NativeRunCompletedRequest struct {
	JobID               *string            `json:"job_id"`
	Conclusion          string             `json:"conclusion"`
	AttemptIndex        *int               `json:"attempt_index,omitempty"`
	CostUSD             float64            `json:"cost_usd,omitempty"`
	Verification        map[string]any     `json:"verification"`
	Evidence            []EvidenceArtifact `json:"evidence,omitempty"`
	ScreenshotsMarkdown *string            `json:"screenshots_markdown"`
	SummaryMarkdown     *string            `json:"summary_markdown"`
	Outputs             map[string]string  `json:"outputs"`
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
			writeInternalError(w, r, err, "read run by callback token failed")
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
			writeInternalError(w, r, err, "record native job completion failed")
			return
		}
		// Bounded-cardinality terminal counter. Runner-driven
		// completions rarely set TerminalReason explicitly; in that
		// case we fall through to the conclusion-derived reason so
		// the metric is still populated.
		metrics.RecordRunPhaseJobTerminal(payload.Conclusion, NormalizeJobTerminalReason(deriveCallbackReason(payload)))
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

		result := processRunCompletion(r.Context(), w, r, completionStore, nativeLauncher, project, runID, jobResult.PhasePayload)
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
		AttemptIndex:        req.AttemptIndex,
		CostUSD:             req.CostUSD,
		SummaryMarkdown:     req.SummaryMarkdown,
		ScreenshotsMarkdown: req.ScreenshotsMarkdown,
		PhaseOutputs:        req.Outputs,
	}
	extractVerification(req.Verification, &p)
	p.Evidence = append(p.Evidence, req.Evidence...)
	p.EvidenceRefs = appendMissingStrings(p.EvidenceRefs, EvidenceRefsFromArtifacts(req.Evidence)...)
	return p
}

// deriveCallbackReason picks the JobTerminalReason* enum value for a
// runner-driven completion. Runner-driven callbacks do not currently
// set TerminalReason explicitly; we infer from Conclusion and
// VerificationStatus so the metric is populated without changing the
// runner contract. Synthesized completions from the reconciler set
// TerminalReason directly and bypass this helper.
func deriveCallbackReason(p CompletionPayload) string {
	if p.TerminalReason != "" {
		return p.TerminalReason
	}
	switch p.VerificationStatus {
	case "fail":
		return JobTerminalReasonVerificationFailed
	case "error":
		return JobTerminalReasonVerificationError
	}
	switch p.Conclusion {
	case "success":
		return JobTerminalReasonSucceeded
	case "timed_out":
		return JobTerminalReasonTimeout
	case "cancelled":
		return JobTerminalReasonCancelled
	}
	return JobTerminalReasonJobFailed
}

func extractVerification(raw map[string]any, p *CompletionPayload) {
	if raw == nil {
		return
	}
	if s, ok := raw["status"].(string); ok {
		p.VerificationStatus = s
	}
	if c, ok := raw["cost_usd"].(float64); ok && c > 0 {
		p.CostUSD = c
	}
	if reasons, ok := raw["reasons"].([]any); ok {
		for _, r := range reasons {
			if s, ok := r.(string); ok {
				p.VerificationReasons = append(p.VerificationReasons, s)
			}
		}
	}
	p.EvidenceRefs = stringSliceFromVerification(raw["evidence_refs"])
	p.Evidence = EvidenceArtifactsFromVerificationPayload(raw)
	p.EvidenceRefs = appendMissingStrings(p.EvidenceRefs, EvidenceRefsFromArtifacts(p.Evidence)...)
}

func stringSliceFromVerification(raw any) []string {
	out := []string{}
	switch values := raw.(type) {
	case []string:
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value != "" {
				out = append(out, value)
			}
		}
	case []any:
		for _, value := range values {
			s, ok := value.(string)
			if !ok {
				continue
			}
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// processRunCompletion is the shared decision-engine path for native completions.
// Returns the RunCallbackResult to write, or nil if it already wrote an error response.
func processRunCompletion(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
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
		writeInternalError(w, r, err, "record completion failed")
		return nil
	}

	// 2. Build the run ref for the response.
	runRef := runRefFromData(run)

	// 3. Read the immutable workflow schema this cycle was created with.
	wf, err := workflowForRun(ctx, store, run)
	if err != nil {
		writeInternalError(w, r, err, "read workflow failed")
		return nil
	}
	if wf == nil {
		reason := "workflow_schema_missing"
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
	decisionAttempts := decisionAttemptsForRun(run)
	decisionRun := decision.Run{
		Attempts:          decisionAttempts,
		CumulativeCostUSD: run.CumulativeCostUSD,
		Budget:            budget.Config{Total: wf.Budget.Total},
	}
	verdict, err := decision.Decide(decisionRun, decisionWorkflow)
	if err != nil {
		writeInternalError(w, r, err, fmt.Sprintf("decision engine: %s", err))
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
			return abortRunWithWorkflowCleanup(ctx, w, r, store, nativeLauncher, run, wf, runRef, decision.AbortMalformed, abortReason)
		}
		return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}

	case decision.Advance:
		targets := allReadyDispatchTargets(wf, run, verdict)
		if len(targets) > 0 {
			for _, target := range targets {
				if err := dispatchForwardPhase(ctx, store, nativeLauncher, run, wf, target); err != nil {
					abortReason := fmt.Sprintf("forward_dispatch_failed: %s", err)
					return abortRunWithWorkflowCleanup(ctx, w, r, store, nativeLauncher, run, wf, runRef, decision.AbortMalformed, abortReason)
				}
			}
			verdictStr = "advance_phase"
			return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}
		}
		if hasInFlightAttempts(run) {
			return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}
		}
		// Always-run cleanup can finish successfully after a primary phase abort;
		// it must not convert that earlier abort into a reviewable success.
		if abortDecision, ok := latestNonAlwaysAbortDecision(wf.Phases, run); ok {
			explanation, _ := decision.AbortExplanation(decisionRun, decisionWorkflow, abortDecision)
			var abortReason *string
			if explanation != "" {
				abortReason = &explanation
			}
			result, err := store.SetRunTerminalState(ctx, project, runID, "aborted", abortReason)
			if err != nil {
				writeInternalError(w, r, err, "mark run aborted failed")
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
		// Every workflow ends at the touchpoint_gate. Reaching this code
		// path means the gate was approved, the pr_merge primitive ran,
		// and cleanup_final completed. Terminal state is always "passed";
		// the issue is closed downstream by closeIssueOnGatedTerminal.
		// Reaching this with no linked PR is malformed: the pr_touchpoint
		// primitive in the touchpoint phase should have set run.PRNumber.
		if run.PRNumber == nil || *run.PRNumber < 1 {
			abortReason := "PR primitive: touchpoint job completed without linking a PR"
			return markRunAborted(ctx, w, r, store, nativeLauncher, run, runRef, decision.AbortMalformed, abortReason)
		}
		state := "passed"
		result, err := store.SetRunTerminalState(ctx, project, runID, state, nil)
		if err != nil {
			writeInternalError(w, r, err, "mark run terminal failed")
			return nil
		}
		advancePlaybooksForTerminalRun(ctx, store, nativeLauncher, project, runID)
		if state == "passed" {
			// A gated run reached terminal "passed" by going through the
			// touchpoint_gate (the only way a gated workflow advances is
			// through approve → pr_merge). Close the issue so the
			// review-surfaces contract invariant "Merged Touchpoints close
			// their Issue in the normal isolated-PR case" holds.
			closeIssueOnGatedTerminal(ctx, store, wf, run)
		}
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
					return markRunAborted(ctx, w, r, store, nativeLauncher, run, runRef, decision.AbortMalformed, abortReason)
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
			writeInternalError(w, r, err, "mark run aborted failed")
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

type workflowReadStore interface {
	GetWorkflowByName(ctx context.Context, project, name string) (*Workflow, error)
	GetWorkflowBySchemaRef(ctx context.Context, project, schemaRef string) (*Workflow, error)
}

func workflowForRun(ctx context.Context, store workflowReadStore, run RunReplayData) (*Workflow, error) {
	var wf *Workflow
	var err error
	if run.WorkflowSchemaRef != "" {
		wf, err = store.GetWorkflowBySchemaRef(ctx, run.Project, run.WorkflowSchemaRef)
	} else {
		wf, err = store.GetWorkflowByName(ctx, run.Project, run.WorkflowName)
	}
	if err != nil || wf == nil {
		return wf, err
	}
	canonical := CanonicalWorkflow(*wf)
	return &canonical, nil
}

func abortRunWithWorkflowCleanup(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
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
					return markRunAborted(ctx, w, r, store, nativeLauncher, run, runRef, decision.AbortMalformed, abortReason+"; cleanup_dispatch_failed: "+err.Error())
				}
			}
			decisionStr := "advance_phase"
			return &RunCallbackResult{RunRef: runRef, Decision: &decisionStr}
		}
		if hasInFlightAttempts(run) {
			return &RunCallbackResult{RunRef: runRef, Decision: &verdictStr}
		}
	}
	return markRunAborted(ctx, w, r, store, nativeLauncher, run, runRef, verdict, abortReason)
}

func markRunAborted(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
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
		writeInternalError(w, r, err, "mark run aborted failed")
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

// closeIssueOnGatedTerminal flips the issue to state=closed when a workflow
// that includes a touchpoint_gate phase reaches terminal "passed." That
// state is only reachable by going through the gate (approve → pr_merge →
// cleanup), so the merge has happened and the issue should reflect it.
// Non-gated workflows leave their issue state alone — those workflows don't
// own the merge decision and may have other reviewers in the loop.
//
// Best-effort: a failure to close the issue is logged but doesn't roll back
// the run's terminal state. The admin endpoint or a follow-up PATCH can
// reconcile by hand.
func closeIssueOnGatedTerminal(ctx context.Context, store RunCompletionStore, wf *Workflow, run RunReplayData) {
	if wf == nil || run.IssueNumber <= 0 {
		return
	}
	hasGate := false
	for _, phase := range wf.Phases {
		if workflowPhaseKind(phase.Kind) == workflowKindTouchpointGate {
			hasGate = true
			break
		}
	}
	if !hasGate {
		return
	}
	issueStore, ok := any(store).(IssueStore)
	if !ok || issueStore == nil {
		return
	}
	closed := "closed"
	_, _ = issueStore.PatchIssueByNumber(ctx, IssuePatch{
		Project: run.Project,
		Number:  run.IssueNumber,
		State:   &closed,
	})
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
	for i, phase := range phases {
		if attempted[phase.Name] {
			continue
		}
		if phase.Always && !includeAlways {
			continue
		}
		if i == 0 {
			continue
		}
		if completed[phases[i-1].Name] {
			return []PhaseSpec{phase}
		}
	}
	return nil
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
		if attempt.Decision == "" && attempt.Completed && decision.IsAdvanceConclusion(attempt.Conclusion) {
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

func runHasNonAlwaysAbort(phases []PhaseSpec, run RunReplayData) bool {
	_, ok := latestNonAlwaysAbortDecision(phases, run)
	return ok
}

func latestNonAlwaysAbortDecision(phases []PhaseSpec, run RunReplayData) (decision.RunDecision, bool) {
	for i := len(run.Attempts) - 1; i >= 0; i-- {
		attempt := run.Attempts[i]
		phase := phaseSpecByName(phases, attempt.Phase)
		if phase != nil && phase.Always {
			continue
		}
		if isAbortDecision(attempt.Decision) {
			return decision.RunDecision(attempt.Decision), true
		}
	}
	return "", false
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
	// Touchpoint gate: the workflow has reached a human-decision boundary.
	// Do NOT append an attempt and do NOT launch jobs. Set the run to the
	// non-terminal review_required sub-state and return. The gate is
	// released by an approve signal in the signal drain, which calls back
	// into dispatchTouchpointGateApprove to append the attempt and launch
	// the pr_merge primitive. review_required does NOT release issue/PR
	// locks — the run is still in flight, just parked at the human gate.
	if phaseKind == workflowKindTouchpointGate {
		if err := store.SetRunReviewRequired(ctx, run.Project, run.ID); err != nil {
			return fmt.Errorf("set review_required: %w", err)
		}
		return nil
	}
	// Skip-when-preserve: an always-run phase (typically cleanup_early)
	// flagged with SkipWhenPreserveTestEnv is appended as a synthesized
	// "skipped" attempt instead of launching its jobs, whenever the run's
	// preserve_test_env snapshot is true. The attempt is durable so run
	// history shows the deliberate skip. After stamping the skip we
	// recursively dispatch the next ready phase — control flows forward
	// through the workflow within this single handler invocation, so the
	// touchpoint phase can run, then touchpoint_gate parks at review.
	if targetPhase.SkipWhenPreserveTestEnv && run.PreserveTestEnv {
		if _, err := store.AppendRunAttempt(ctx, run.Project, run.ID, targetPhase.Name, phaseKind, workflowFilename); err != nil {
			return fmt.Errorf("append skipped attempt: %w", err)
		}
		if err := store.StampLatestAttemptSkipped(ctx, run.Project, run.ID); err != nil {
			return fmt.Errorf("stamp skipped attempt: %w", err)
		}
		updated, err := store.ReadRunForReplay(ctx, run.Project, run.ID)
		if err != nil {
			return fmt.Errorf("re-read run after skip: %w", err)
		}
		for _, next := range allReadyDispatchTargets(wf, updated, decision.Advance) {
			if err := dispatchForwardPhase(ctx, store, nativeLauncher, updated, wf, next); err != nil {
				return err
			}
		}
		return nil
	}
	newAttemptIdx, err := store.AppendRunAttempt(ctx, run.Project, run.ID, targetPhase.Name, phaseKind, workflowFilename)
	if err != nil {
		return fmt.Errorf("append forward attempt: %w", err)
	}
	lease, err := leaseForRunPhase(ctx, store, run, targetPhase.Name, newAttemptIdx, substituted)
	if err != nil {
		return fmt.Errorf("read lease for forward phase: %w", err)
	}
	if lease.State != "claimed" {
		return fmt.Errorf("native lease was not claimed")
	}
	started := runWithAttempt(run, newAttemptIdx, targetPhase.Name)
	launched, err := launchCommittedNativePhase(ctx, nativeLauncher, NativeLaunchRequest{
		Lease:    lease,
		Workflow: *wf,
		Phase:    targetPhase,
		Run:      started,
	})
	if err != nil {
		return fmt.Errorf("native dispatch: %w", err)
	}
	_ = recordLaunchedNativeJobs(ctx, store, started, targetPhase, launched)
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

func leaseForRunPhase(ctx context.Context, store RunCompletionStore, run RunReplayData, phaseName string, attemptIndex int, phaseInputs map[string]string) (Lease, error) {
	if run.SlotLeaseRef == nil || *run.SlotLeaseRef == "" {
		return Lease{}, fmt.Errorf("run has no slot lease")
	}
	lease, err := store.ReadLeaseByRef(ctx, run.Project, *run.SlotLeaseRef)
	if err != nil {
		return Lease{}, err
	}
	metadata := mapOrEmpty(lease.Metadata)
	metadata["run_id"] = run.ID
	metadata["run_ref"] = runRefFromData(run)
	metadata["phase_name"] = phaseName
	metadata["attempt_index"] = strconv.Itoa(attemptIndex)
	metadata["phase_inputs"] = phaseInputs
	metadata["issue_ref"] = publicids.IssueRef(run.Project, positiveIssueNumber(run.IssueNumber))
	metadata["issue_number"] = strconv.Itoa(run.IssueNumber)
	display := "unknown"
	if run.RunDisplayNumber != nil && *run.RunDisplayNumber != "" {
		display = *run.RunDisplayNumber
	}
	metadata["work_context_branch"] = fmt.Sprintf("issue-%d-run-%s", run.IssueNumber, display)
	metadata["native_k8s"] = true
	if run.CallbackToken != nil && *run.CallbackToken != "" {
		metadata["run_callback_token"] = *run.CallbackToken
	}
	if run.IssueLockHolderID != nil && *run.IssueLockHolderID != "" {
		metadata["issue_lock_holder_id"] = *run.IssueLockHolderID
	}
	lease.Metadata = metadata
	return lease, nil
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
					if entry, ok := workflowEntryPhase(wf.Phases); ok {
						target = entry.Name
					} else {
						target = failingPhase
					}
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
	carryForwardAttempts := retryCarryForwardAttempts(run, wf, targetPhase.Name)
	phaseInputs, err := substituteCompletionPhaseInputs(*targetPhase, runWithAttempts(run, carryForwardAttempts))
	if err != nil {
		return fmt.Errorf("substitute retry phase inputs: %w", err)
	}
	recycle, err := store.CreateRecycleCycle(ctx, CreateRecycleCycleRequest{
		Parent:               run,
		WorkflowSchemaRef:    wf.SchemaRef,
		TargetPhaseName:      targetPhase.Name,
		CarryForwardAttempts: carryForwardAttempts,
		TriggerSource:        map[string]any{"kind": "recycle_policy", "recycled_from_run_id": run.ID, "failing_phase": failingPhase},
		EvidenceRequirements: run.EvidenceRequirements,
	})
	if err != nil {
		return fmt.Errorf("create recycle cycle: %w", err)
	}
	leaseRef := ""
	if run.SlotLeaseRef != nil {
		leaseRef = *run.SlotLeaseRef
	}
	newAttemptIdx, err := store.StartRunCycle(ctx, StartRunCycleRequest{
		Project:          run.Project,
		RunID:            recycle.ID,
		PhaseName:        targetPhase.Name,
		PhaseKind:        phaseKind,
		WorkflowFilename: workflowFilename,
		SlotLeaseRef:     leaseRef,
	})
	if err != nil {
		return fmt.Errorf("start recycle cycle: %w", err)
	}
	recycleRun := RunReplayData{
		ID:                   recycle.ID,
		Project:              run.Project,
		WorkflowName:         run.WorkflowName,
		WorkflowSchemaRef:    wf.SchemaRef,
		CumulativeCostUSD:    run.CumulativeCostUSD,
		Budget:               run.Budget,
		IssueNumber:          run.IssueNumber,
		RunNumber:            &recycle.RunNumber,
		CycleNumber:          &recycle.CycleNumber,
		RunCycleNumber:       &recycle.RunCycle,
		RunDisplayNumber:     &recycle.RunDisplay,
		IssueRepo:            run.IssueRepo,
		CallbackToken:        &recycle.CallbackToken,
		IssueLockHolderID:    run.IssueLockHolderID,
		SlotLeaseRef:         &leaseRef,
		EntrypointPhase:      &targetPhase.Name,
		EvidenceRequirements: run.EvidenceRequirements,
		Attempts: append(append([]RunAttemptData{}, recycle.CarryForwardAttempts...), RunAttemptData{
			AttemptIndex: newAttemptIdx,
			Phase:        targetPhase.Name,
		}),
	}
	lease, err := leaseForRunPhase(ctx, store, recycleRun, targetPhase.Name, newAttemptIdx, phaseInputs)
	if err != nil {
		return fmt.Errorf("read lease for retry: %w", err)
	}
	if lease.State != "claimed" {
		return fmt.Errorf("native lease was not claimed")
	}
	launched, err := launchCommittedNativePhase(ctx, nativeLauncher, NativeLaunchRequest{
		Lease:    lease,
		Workflow: *wf,
		Phase:    *targetPhase,
		Run:      recycleRun,
	})
	if err != nil {
		return fmt.Errorf("native dispatch: %w", err)
	}
	_ = recordLaunchedNativeJobs(ctx, store, recycleRun, *targetPhase, launched)
	return nil
}

func retryCarryForwardAttempts(parent RunReplayData, wf *Workflow, targetPhase string) []RunAttemptData {
	if wf == nil || strings.TrimSpace(targetPhase) == "" {
		return nil
	}
	latestByPhase := map[string]RunAttemptData{}
	for _, attempt := range parent.Attempts {
		if !attempt.Completed && attempt.Decision == "" {
			continue
		}
		latestByPhase[attempt.Phase] = attempt
	}
	carry := make([]RunAttemptData, 0)
	for _, phase := range wf.Phases {
		if phase.Name == targetPhase {
			break
		}
		prior, ok := latestByPhase[phase.Name]
		if !ok {
			continue
		}
		carry = append(carry, RunAttemptData{
			AttemptIndex: len(carry),
			Phase:        phase.Name,
			Conclusion:   "success",
			Decision:     string(decision.Advance),
			Completed:    true,
			CarryForward: true,
			PhaseOutputs: cloneStringMap(prior.PhaseOutputs),
		})
	}
	return carry
}

func runWithAttempts(run RunReplayData, attempts []RunAttemptData) RunReplayData {
	out := run
	out.Attempts = append(append([]RunAttemptData{}, run.Attempts...), attempts...)
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
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

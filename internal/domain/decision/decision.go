package decision

import (
	"fmt"
	"strings"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/metrics"
)

type RunDecision string

const (
	Retry               RunDecision = "retry"
	Advance             RunDecision = "advance"
	AbortBudgetAttempts RunDecision = "abort_budget_attempts"
	AbortBudgetCost     RunDecision = "abort_budget_cost"
	AbortMalformed      RunDecision = "abort_malformed"
	// AbortRequested is a phase-requested ("fail-closed") abort. A phase
	// script emits a non-empty `abort_reason` phase output to declare the
	// run cannot proceed (e.g. spirelens env-prep finding the warm host
	// asleep, or an unexpected mod on disk). It is distinct from the
	// budget/malformed aborts: it is not driven by verification status or
	// retry/cost ceilings, so it overrides the verify-loop routing
	// entirely and short-circuits straight to teardown-then-abort. The
	// operator-facing reason is the phase's own `abort_reason` string, not
	// a generic decision-engine explanation.
	AbortRequested RunDecision = "abort_requested"
)

// AbortReasonOutputKey is the phase-output key a phase script sets to
// request a fail-closed run abort. The native runner stops the remaining
// steps in the phase when it sees this key set to a non-empty value and
// reports the completion with conclusion=ConclusionAborted; the decision
// engine then routes the run to AbortRequested.
const AbortReasonOutputKey = "abort_reason"

// ConclusionAborted is the completion conclusion the native runner reports
// for a phase-requested abort. It is recorded on the attempt for
// observability; the load-bearing routing signal is the per-attempt
// AbortReason (sourced from the AbortReasonOutputKey phase output).
const ConclusionAborted = "aborted"

type VerificationStatus string

const (
	VerificationPass  VerificationStatus = "pass"
	VerificationFail  VerificationStatus = "fail"
	VerificationError VerificationStatus = "error"
)

type Verification struct {
	Status  VerificationStatus
	Reasons []string
}

type Attempt struct {
	Phase        string
	Conclusion   string
	Verification *Verification
	// AbortReason carries the phase's `abort_reason` output (empty when
	// the phase did not request an abort). A non-empty value on the
	// deciding attempt forces the AbortRequested verdict ahead of any
	// verify/budget routing.
	AbortReason string
}

type RecyclePolicy struct {
	MaxAttempts int
	On          []string
}

type PhaseSpec struct {
	Name                     string
	Verify                   bool
	EvidenceVerificationGate bool
	Purpose                  string
	RecyclePolicy            *RecyclePolicy
}

type Workflow struct {
	Phases []PhaseSpec
}

type Run struct {
	Attempts          []Attempt
	CumulativeCostUSD float64
	Budget            budget.Config
}

// Decide returns the next verify-loop action for a run attempt. Every
// well-formed decision is recorded to the glimmung_decisions_total counter
// at the moment it is determined — Decide is the canonical site, so any
// caller that re-uses the same decision (e.g. replay handlers) must not
// re-record.
func Decide(run Run, workflow Workflow, attemptIndex ...int) (decision RunDecision, err error) {
	defer func() {
		if err == nil && decision != "" {
			metrics.RecordDecision(string(decision))
		}
	}()

	if len(run.Attempts) == 0 {
		return "", fmt.Errorf("decide called on run with no attempts")
	}

	index := len(run.Attempts) - 1
	if len(attemptIndex) > 0 {
		index = attemptIndex[0]
		if index < 0 || index >= len(run.Attempts) {
			return "", fmt.Errorf("decide attempt_index=%d out of range (0..%d)", index, len(run.Attempts)-1)
		}
	}

	last := run.Attempts[index]

	// A phase-requested abort overrides every other routing rule. The
	// phase emitted a non-empty `abort_reason`, so there is nothing to
	// verify, retry, or advance — the run short-circuits to
	// teardown-then-abort regardless of phase shape or budget. Honor it
	// before the workflow phase lookup so a phase-requested abort still
	// aborts cleanly even under workflow drift.
	if strings.TrimSpace(last.AbortReason) != "" {
		return AbortRequested, nil
	}

	phaseSpec, ok := phaseByName(workflow, last.Phase)
	if !ok {
		return "", fmt.Errorf("attempt phase %q not found in workflow.phases", last.Phase)
	}

	if phaseSpec.EvidenceVerificationGate {
		if IsAdvanceConclusion(last.Conclusion) {
			return Advance, nil
		}
		if run.CumulativeCostUSD >= run.Budget.Total {
			return AbortBudgetCost, nil
		}
		rp := phaseSpec.RecyclePolicy
		if rp == nil || !contains(rp.On, "verify_fail") {
			return AbortBudgetAttempts, nil
		}
		if attemptsInPhase(run.Attempts, last.Phase) >= rp.MaxAttempts {
			return AbortBudgetAttempts, nil
		}
		return Retry, nil
	}

	if !phaseSpec.Verify {
		if IsAdvanceConclusion(last.Conclusion) {
			return Advance, nil
		}
		return AbortMalformed, nil
	}

	if next, ok := nextPhaseAfter(workflow, phaseSpec.Name); ok && next.EvidenceVerificationGate {
		if IsAdvanceConclusion(last.Conclusion) {
			return Advance, nil
		}
		return AbortMalformed, nil
	}

	if last.Verification != nil && last.Verification.Status == VerificationPass {
		return Advance, nil
	}

	if run.CumulativeCostUSD >= run.Budget.Total {
		return AbortBudgetCost, nil
	}

	trigger := triggerForAttempt(last)
	rp := phaseSpec.RecyclePolicy
	if rp == nil || !contains(rp.On, trigger) {
		if trigger == "verify_malformed" {
			return AbortMalformed, nil
		}
		return AbortBudgetAttempts, nil
	}

	if attemptsInPhase(run.Attempts, last.Phase) >= rp.MaxAttempts {
		return AbortBudgetAttempts, nil
	}

	return Retry, nil
}

// AbortExplanation returns the human-readable abort comment body for a terminal decision.
func AbortExplanation(run Run, workflow Workflow, decision RunDecision) (string, error) {
	last := primaryAttemptForExplanation(run, workflow)
	reasons := []string{}
	if last != nil && last.Verification != nil {
		reasons = last.Verification.Reasons
	}

	detail := ""
	if len(reasons) > 0 {
		lines := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			lines = append(lines, "- "+reason)
		}
		detail = "\n\nMost recent verification reasons:\n" + strings.Join(lines, "\n")
	}

	switch decision {
	case AbortRequested:
		reason := ""
		phaseName := "?"
		if last != nil {
			reason = strings.TrimSpace(last.AbortReason)
			if last.Phase != "" {
				phaseName = last.Phase
			}
		}
		if reason == "" {
			return fmt.Sprintf("Aborting run: phase %q requested a fail-closed abort.%s", phaseName, detail), nil
		}
		return fmt.Sprintf("Aborting run: phase %q requested a fail-closed abort (%s).%s", phaseName, reason, detail), nil
	case AbortBudgetAttempts:
		if last == nil {
			return "Aborting verify-loop on phase '?': no retry path available for the latest verification result." + detail, nil
		}
		phaseSpec, ok := phaseByName(workflow, last.Phase)
		attempts := attemptsInPhase(run.Attempts, last.Phase)
		if ok && phaseSpec.RecyclePolicy != nil {
			return fmt.Sprintf(
				"Aborting verify-loop after %d attempt(s) on phase %q; reached max_attempts=%d.%s",
				attempts,
				last.Phase,
				phaseSpec.RecyclePolicy.MaxAttempts,
				detail,
			), nil
		}
		return fmt.Sprintf(
			"Aborting verify-loop on phase %q: no retry path available for the latest verification result.%s",
			last.Phase,
			detail,
		), nil
	case AbortBudgetCost:
		return fmt.Sprintf(
			"Aborting verify-loop after cumulative cost $%.2f >= budget $%.2f.%s",
			run.CumulativeCostUSD,
			run.Budget.Total,
			detail,
		), nil
	case AbortMalformed:
		return "Aborting verify-loop: the latest workflow run did not produce a well-formed `verification.json` artifact, or the failure mode is not in this phase's recycle policy. The decision engine cannot retry against a missing or invalid producer contract.", nil
	default:
		return "", fmt.Errorf("abort explanation called with non-abort decision %q", decision)
	}
}

func primaryAttemptForExplanation(run Run, workflow Workflow) *Attempt {
	for i := len(run.Attempts) - 1; i >= 0; i-- {
		attempt := &run.Attempts[i]
		phase, ok := phaseByName(workflow, attempt.Phase)
		if !ok || phaseIsPrimary(phase) {
			return attempt
		}
	}
	if len(run.Attempts) > 0 {
		return &run.Attempts[len(run.Attempts)-1]
	}
	return nil
}

func phaseIsPrimary(phase PhaseSpec) bool {
	switch phase.Purpose {
	case "teardown", "review_touchpoint", "review_gate":
		return false
	default:
		return true
	}
}

func phaseByName(workflow Workflow, name string) (PhaseSpec, bool) {
	for _, phase := range workflow.Phases {
		if phase.Name == name {
			return phase, true
		}
	}
	return PhaseSpec{}, false
}

func nextPhaseAfter(workflow Workflow, name string) (PhaseSpec, bool) {
	for i, phase := range workflow.Phases {
		if phase.Name == name {
			if i+1 < len(workflow.Phases) {
				return workflow.Phases[i+1], true
			}
			return PhaseSpec{}, false
		}
	}
	return PhaseSpec{}, false
}

func triggerForAttempt(attempt Attempt) string {
	if attempt.Verification == nil {
		return "verify_malformed"
	}
	return "verify_fail"
}

func attemptsInPhase(attempts []Attempt, phase string) int {
	count := 0
	for _, attempt := range attempts {
		if attempt.Phase == phase {
			count++
		}
	}
	return count
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// IsAdvanceConclusion reports whether a phase conclusion should be treated
// as a forward-advance signal by the decision engine. "success" is the
// canonical advance; "skipped" advances exactly like success and is emitted
// by phases that were scheduled but deliberately did not execute (for
// example, the early cleanup phase when the issue's preserve_test_env flag
// is set so the validation environment stays alive through review).
func IsAdvanceConclusion(conclusion string) bool {
	switch conclusion {
	case "success", "skipped":
		return true
	default:
		return false
	}
}

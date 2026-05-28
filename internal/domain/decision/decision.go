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
)

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
}

type RecyclePolicy struct {
	MaxAttempts int
	On          []string
}

type PhaseSpec struct {
	Name                     string
	Verify                   bool
	EvidenceVerificationGate bool
	Always                   bool
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
		if !ok || !phase.Always {
			return attempt
		}
	}
	if len(run.Attempts) > 0 {
		return &run.Attempts[len(run.Attempts)-1]
	}
	return nil
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

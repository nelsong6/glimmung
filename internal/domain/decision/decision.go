package decision

import (
	"fmt"

	"github.com/nelsong6/glimmung/internal/domain/budget"
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
	Status VerificationStatus
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

// Decide returns the next verify-loop action for a run attempt.
func Decide(run Run, workflow Workflow, attemptIndex ...int) (RunDecision, error) {
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
		if last.Conclusion == "success" {
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
		if last.Conclusion == "success" {
			return Advance, nil
		}
		return AbortMalformed, nil
	}

	if next, ok := nextPhaseAfter(workflow, phaseSpec.Name); ok && next.EvidenceVerificationGate {
		if last.Conclusion == "success" {
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

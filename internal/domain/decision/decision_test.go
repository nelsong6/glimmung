package decision

import (
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/domain/budget"
)

func workflow(opts ...func(*PhaseSpec)) Workflow {
	phase := PhaseSpec{
		Name:   "agent",
		Verify: true,
		RecyclePolicy: &RecyclePolicy{
			MaxAttempts: 3,
			On:          []string{"verify_fail"},
		},
	}
	for _, opt := range opts {
		opt(&phase)
	}
	return Workflow{Phases: []PhaseSpec{phase}}
}

func withVerify(verify bool) func(*PhaseSpec) {
	return func(phase *PhaseSpec) {
		phase.Verify = verify
		if !verify {
			phase.RecyclePolicy = nil
		}
	}
}

func withMaxAttempts(maxAttempts int) func(*PhaseSpec) {
	return func(phase *PhaseSpec) {
		phase.RecyclePolicy.MaxAttempts = maxAttempts
	}
}

func withRecycleOn(on ...string) func(*PhaseSpec) {
	return func(phase *PhaseSpec) {
		phase.RecyclePolicy.On = on
	}
}

func run(attempts []Attempt, cumulativeCost float64, total float64) Run {
	return Run{
		Attempts:          attempts,
		CumulativeCostUSD: cumulativeCost,
		Budget:            budget.Config{Total: total},
	}
}

func attempt(phase string, status *VerificationStatus, conclusion string) Attempt {
	var verification *Verification
	if status != nil {
		verification = &Verification{Status: *status}
	}
	return Attempt{
		Phase:        phase,
		Conclusion:   conclusion,
		Verification: verification,
	}
}

func attemptWithReasons(phase string, status VerificationStatus, conclusion string, reasons ...string) Attempt {
	return Attempt{
		Phase:      phase,
		Conclusion: conclusion,
		Verification: &Verification{
			Status:  status,
			Reasons: reasons,
		},
	}
}

func status(value VerificationStatus) *VerificationStatus {
	return &value
}

func mustDecide(t *testing.T, run Run, workflow Workflow, want RunDecision, attemptIndex ...int) {
	t.Helper()
	got, err := Decide(run, workflow, attemptIndex...)
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPassOnFirstAttemptAdvances(t *testing.T) {
	mustDecide(t,
		run([]Attempt{attempt("agent", status(VerificationPass), "success")}, 2.5, 25.0),
		workflow(),
		Advance,
	)
}

func TestFailOnFirstWithinBudgetRetries(t *testing.T) {
	mustDecide(t,
		run([]Attempt{attempt("agent", status(VerificationFail), "failure")}, 3.0, 25.0),
		workflow(),
		Retry,
	)
}

func TestPassOnRetryAdvances(t *testing.T) {
	mustDecide(t,
		run([]Attempt{
			attempt("agent", status(VerificationFail), "failure"),
			attempt("agent", status(VerificationPass), "success"),
		}, 7.0, 25.0),
		workflow(),
		Advance,
	)
}

func TestAbortOnAttemptsBudget(t *testing.T) {
	mustDecide(t,
		run([]Attempt{
			attempt("agent", status(VerificationFail), "failure"),
			attempt("agent", status(VerificationFail), "failure"),
			attempt("agent", status(VerificationFail), "failure"),
		}, 6.0, 25.0),
		workflow(withMaxAttempts(3)),
		AbortBudgetAttempts,
	)
}

func TestAbortOnCostBudget(t *testing.T) {
	mustDecide(t,
		run([]Attempt{attempt("agent", status(VerificationFail), "failure")}, 30.0, 25.0),
		workflow(withMaxAttempts(5)),
		AbortBudgetCost,
	)
}

func TestMissingVerificationArtifactIsTerminalWhenNotInRecycleOn(t *testing.T) {
	mustDecide(t,
		run([]Attempt{attempt("agent", nil, "success")}, 0.0, 25.0),
		workflow(withRecycleOn("verify_fail")),
		AbortMalformed,
	)
}

func TestMalformedWithRecycleOnRetries(t *testing.T) {
	mustDecide(t,
		run([]Attempt{attempt("agent", nil, "success")}, 0.0, 25.0),
		workflow(withRecycleOn("verify_fail", "verify_malformed")),
		Retry,
	)
}

func TestErrorStatusTreatedAsVerifyFail(t *testing.T) {
	mustDecide(t,
		run([]Attempt{attempt("agent", status(VerificationError), "failure")}, 1.0, 25.0),
		workflow(),
		Retry,
	)
}

func TestCostGateWinsOverAttemptsGate(t *testing.T) {
	mustDecide(t,
		run([]Attempt{
			attempt("agent", status(VerificationFail), "failure"),
			attempt("agent", status(VerificationFail), "failure"),
			attempt("agent", status(VerificationFail), "failure"),
		}, 37.0, 25.0),
		workflow(withMaxAttempts(3)),
		AbortBudgetCost,
	)
}

func TestPassWinsOverBudgetBreach(t *testing.T) {
	mustDecide(t,
		run([]Attempt{
			attempt("agent", status(VerificationFail), "failure"),
			attempt("agent", status(VerificationPass), "success"),
		}, 30.0, 25.0),
		workflow(withMaxAttempts(2)),
		Advance,
	)
}

func TestDecideRaisesOnEmptyAttempts(t *testing.T) {
	_, err := Decide(run(nil, 0, 25.0), workflow())
	if err == nil || !strings.Contains(err.Error(), "no attempts") {
		t.Fatalf("got error %v, want no attempts", err)
	}
}

func TestNonVerifyPhaseConclusionRoutes(t *testing.T) {
	mustDecide(t,
		run([]Attempt{attempt("agent", nil, "failure")}, 0, 25.0),
		workflow(withVerify(false)),
		AbortMalformed,
	)
	mustDecide(t,
		run([]Attempt{attempt("agent", nil, "success")}, 0, 25.0),
		workflow(withVerify(false)),
		Advance,
	)
}

func TestExplicitAttemptIndex(t *testing.T) {
	r := run([]Attempt{
		attempt("agent", status(VerificationFail), "failure"),
		attempt("agent", status(VerificationPass), "success"),
	}, 3.0, 25.0)
	mustDecide(t, r, workflow(), Retry, 0)
	mustDecide(t, r, workflow(), Advance, 1)
}

func TestEvidenceVerificationGateRoutes(t *testing.T) {
	gate := Workflow{Phases: []PhaseSpec{
		{
			Name:                     "evidence",
			EvidenceVerificationGate: true,
			RecyclePolicy:            &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}},
		},
	}}
	mustDecide(t,
		run([]Attempt{attempt("evidence", nil, "success")}, 0, 25.0),
		gate,
		Advance,
	)
	mustDecide(t,
		run([]Attempt{attempt("evidence", nil, "failure")}, 1, 25.0),
		gate,
		Retry,
	)
}

func TestVerifyPhaseBeforeEvidenceGateAdvancesOnSuccessConclusion(t *testing.T) {
	wf := Workflow{Phases: []PhaseSpec{
		{Name: "agent", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}}},
		{Name: "evidence", EvidenceVerificationGate: true},
	}}
	mustDecide(t,
		run([]Attempt{attempt("agent", status(VerificationFail), "success")}, 0, 25.0),
		wf,
		Advance,
	)
	mustDecide(t,
		run([]Attempt{attempt("agent", status(VerificationFail), "failure")}, 0, 25.0),
		wf,
		AbortMalformed,
	)
}

func TestAbortExplanationAttemptsIncludesCountAndReasons(t *testing.T) {
	text, err := AbortExplanation(
		run([]Attempt{
			attemptWithReasons("agent", VerificationFail, "failure", "selector .foo not found"),
			attempt("agent", status(VerificationFail), "failure"),
			attemptWithReasons("agent", VerificationFail, "failure", "expected status 200, got 500"),
		}, 3.0, 25.0),
		workflow(withMaxAttempts(3)),
		AbortBudgetAttempts,
	)
	if err != nil {
		t.Fatalf("AbortExplanation returned error: %v", err)
	}
	if !strings.Contains(text, "max_attempts=3") || !strings.Contains(text, "expected status 200") {
		t.Fatalf("unexpected explanation: %s", text)
	}
}

func TestAbortExplanationCostIncludesAmounts(t *testing.T) {
	text, err := AbortExplanation(
		run([]Attempt{attempt("agent", status(VerificationFail), "failure")}, 30.0, 25.0),
		workflow(),
		AbortBudgetCost,
	)
	if err != nil {
		t.Fatalf("AbortExplanation returned error: %v", err)
	}
	if !strings.Contains(text, "$30.00") || !strings.Contains(text, "$25.00") {
		t.Fatalf("unexpected explanation: %s", text)
	}
}

func TestAbortExplanationMalformedSelfExplanatory(t *testing.T) {
	text, err := AbortExplanation(
		run([]Attempt{attempt("agent", nil, "failure")}, 0, 25.0),
		workflow(),
		AbortMalformed,
	)
	if err != nil {
		t.Fatalf("AbortExplanation returned error: %v", err)
	}
	if !strings.Contains(text, "verification.json") {
		t.Fatalf("unexpected explanation: %s", text)
	}
}

func TestAbortExplanationSkipsAlwaysRunAttempts(t *testing.T) {
	wf := Workflow{Phases: []PhaseSpec{
		{
			Name:   "agent",
			Verify: true,
			RecyclePolicy: &RecyclePolicy{
				MaxAttempts: 3,
				On:          []string{"verify_fail"},
			},
		},
		{Name: "cleanup", Always: true},
	}}
	text, err := AbortExplanation(
		run([]Attempt{
			attemptWithReasons("agent", VerificationFail, "failure", "agent failed"),
			attemptWithReasons("cleanup", VerificationFail, "failure", "cleanup failed"),
		}, 2.0, 25.0),
		wf,
		AbortBudgetAttempts,
	)
	if err != nil {
		t.Fatalf("AbortExplanation returned error: %v", err)
	}
	if !strings.Contains(text, "agent failed") || strings.Contains(text, "cleanup failed") {
		t.Fatalf("unexpected explanation: %s", text)
	}
}

func TestAbortExplanationRejectsNonAbortDecision(t *testing.T) {
	_, err := AbortExplanation(run([]Attempt{attempt("agent", nil, "success")}, 0, 25.0), workflow(), Advance)
	if err == nil || !strings.Contains(err.Error(), "non-abort") {
		t.Fatalf("got error %v, want non-abort error", err)
	}
}

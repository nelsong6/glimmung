package server

import (
	"errors"
	"testing"
	"time"
)

// fixedTime is the deterministic timestamp every transition test uses so
// the assertion is on which fields got set, not on what value they got.
var fixedTime = time.Date(2026, 5, 17, 8, 0, 0, 0, time.UTC)

func TestNewUnseededSlot(t *testing.T) {
	s := NewUnseededSlot("tank-operator", 5, "tank-operator-slot-5", fixedTime)
	if s.Project != "tank-operator" {
		t.Fatalf("Project=%q, want tank-operator", s.Project)
	}
	if s.SlotIndex != 5 {
		t.Fatalf("SlotIndex=%d, want 5", s.SlotIndex)
	}
	if s.SlotName != "tank-operator-slot-5" {
		t.Fatalf("SlotName=%q", s.SlotName)
	}
	if s.State != SlotStateUnseeded {
		t.Fatalf("State=%q, want %q", s.State, SlotStateUnseeded)
	}
	if !s.UpdatedAt.Equal(fixedTime) {
		t.Fatalf("UpdatedAt=%v", s.UpdatedAt)
	}
	if s.DocID() != "tank-operator:5" {
		t.Fatalf("DocID=%q, want tank-operator:5", s.DocID())
	}
}

func TestSlotDocID(t *testing.T) {
	if got := SlotDocID("tank-operator", 5); got != "tank-operator:5" {
		t.Fatalf("SlotDocID=%q, want tank-operator:5", got)
	}
}

func TestSlotWithETag(t *testing.T) {
	s := NewUnseededSlot("tank-operator", 5, "tank-operator-slot-5", fixedTime)
	if s.ETag() != "" {
		t.Fatalf("fresh slot ETag=%q, want empty", s.ETag())
	}
	s2 := s.WithETag(`"abc123"`)
	if s2.ETag() != `"abc123"` {
		t.Fatalf("ETag after WithETag=%q", s2.ETag())
	}
	if s.ETag() != "" {
		t.Fatal("WithETag must not mutate the receiver")
	}
}

// allValidTransitions lists every (from, to) pair we claim is valid. The
// happy-path tests below iterate over this set and assert
// CanTransitionTo returns nil. The invalid-transition test does the
// inverse: every pair NOT in this list must be rejected.
//
// Same-state transitions are added separately because CanTransitionTo
// special-cases them and they're not in validSlotTransitions.
var allValidTransitions = []struct{ from, to string }{
	{SlotStateUnseeded, SlotStateProvisioning},
	{SlotStateProvisioning, SlotStateProvisioned},
	{SlotStateProvisioning, SlotStateError},
	{SlotStateProvisioned, SlotStateActivating},
	{SlotStateActivating, SlotStateRunning},
	{SlotStateActivating, SlotStateCleaning},
	{SlotStateActivating, SlotStateError},
	{SlotStateRunning, SlotStateCleaning},
	{SlotStateRunning, SlotStateError},
	{SlotStateCleaning, SlotStateProvisioned},
	{SlotStateCleaning, SlotStateError},
}

func TestCanTransitionToAcceptsValidTransitions(t *testing.T) {
	for _, tr := range allValidTransitions {
		s := Slot{State: tr.from}
		if err := s.CanTransitionTo(tr.to); err != nil {
			t.Errorf("CanTransitionTo(%q -> %q) returned err=%v, want nil", tr.from, tr.to, err)
		}
	}
}

func TestCanTransitionToAcceptsSameState(t *testing.T) {
	for _, state := range SlotStates {
		s := Slot{State: state}
		if err := s.CanTransitionTo(state); err != nil {
			t.Errorf("same-state %q rejected: %v", state, err)
		}
	}
}

func TestCanTransitionToRejectsInvalidTransitions(t *testing.T) {
	valid := map[string]map[string]bool{}
	for _, tr := range allValidTransitions {
		if valid[tr.from] == nil {
			valid[tr.from] = map[string]bool{}
		}
		valid[tr.from][tr.to] = true
	}
	for _, from := range SlotStates {
		for _, to := range SlotStates {
			if from == to {
				continue
			}
			if valid[from][to] {
				continue
			}
			s := Slot{State: from}
			err := s.CanTransitionTo(to)
			if err == nil {
				t.Errorf("CanTransitionTo(%q -> %q) returned nil, want err", from, to)
				continue
			}
			if !errors.Is(err, ErrInvalidSlotTransition) {
				t.Errorf("CanTransitionTo(%q -> %q) returned %v, want wrapped ErrInvalidSlotTransition", from, to, err)
			}
		}
	}
}

func TestCanTransitionToRejectsUnknownFromState(t *testing.T) {
	s := Slot{State: "junk"}
	err := s.CanTransitionTo(SlotStateProvisioned)
	if !errors.Is(err, ErrInvalidSlotTransition) {
		t.Fatalf("err=%v, want wrapped ErrInvalidSlotTransition", err)
	}
}

func TestErrorTerminal(t *testing.T) {
	// Any forward transition from error is rejected — recovery is operator-
	// driven via decrease-then-increase count, not a state machine path.
	s := Slot{State: SlotStateError}
	for _, to := range SlotStates {
		if to == SlotStateError {
			continue
		}
		if err := s.CanTransitionTo(to); err == nil {
			t.Errorf("transition error -> %q must be rejected", to)
		}
	}
}

func TestMarkProvisioning(t *testing.T) {
	s := NewUnseededSlot("p", 1, "p-slot-1", fixedTime.Add(-time.Hour))
	s.Detail = strPtr("residue")
	got, err := s.MarkProvisioning(fixedTime)
	if err != nil {
		t.Fatalf("MarkProvisioning err=%v", err)
	}
	if got.State != SlotStateProvisioning {
		t.Fatalf("State=%q", got.State)
	}
	if !got.UpdatedAt.Equal(fixedTime) {
		t.Fatalf("UpdatedAt=%v", got.UpdatedAt)
	}
	if got.Detail != nil {
		t.Fatalf("Detail=%v, want nil after transition", got.Detail)
	}
}

func TestMarkProvisioningRejectsInvalidFrom(t *testing.T) {
	s := Slot{State: SlotStateRunning}
	_, err := s.MarkProvisioning(fixedTime)
	if !errors.Is(err, ErrInvalidSlotTransition) {
		t.Fatalf("err=%v, want wrapped ErrInvalidSlotTransition", err)
	}
}

func TestMarkProvisionedClearsActivationFields(t *testing.T) {
	s := Slot{State: SlotStateCleaning}
	attempt := 3
	jobName := "old-job"
	errMsg := "old error"
	leaseRef := "old/leases/99"
	startedAt := fixedTime.Add(-time.Hour)
	completedAt := fixedTime.Add(-30 * time.Minute)
	s.ActivationAttempt = &attempt
	s.ActivationStartedAt = &startedAt
	s.ActivationCompletedAt = &completedAt
	s.ActivationJobName = &jobName
	s.ActivationError = &errMsg
	s.ActiveLeaseRef = &leaseRef

	got, err := s.MarkCleaned(fixedTime)
	if err != nil {
		t.Fatalf("MarkCleaned err=%v", err)
	}
	if got.State != SlotStateProvisioned {
		t.Fatalf("State=%q, want provisioned", got.State)
	}
	if got.ActivationAttempt != nil {
		t.Errorf("ActivationAttempt=%v, want nil", got.ActivationAttempt)
	}
	if got.ActivationStartedAt != nil {
		t.Errorf("ActivationStartedAt=%v, want nil", got.ActivationStartedAt)
	}
	if got.ActivationJobName != nil {
		t.Errorf("ActivationJobName=%v, want nil", got.ActivationJobName)
	}
	if got.ActivationError != nil {
		t.Errorf("ActivationError=%v, want nil", got.ActivationError)
	}
	if got.ActiveLeaseRef != nil {
		t.Errorf("ActiveLeaseRef=%v, want nil", got.ActiveLeaseRef)
	}
	if got.CleanupCompletedAt == nil || !got.CleanupCompletedAt.Equal(fixedTime) {
		t.Errorf("CleanupCompletedAt=%v, want %v", got.CleanupCompletedAt, fixedTime)
	}
}

func TestMarkProvisionedFromProvisioningStampsProvisionedAt(t *testing.T) {
	s := Slot{State: SlotStateProvisioning, Project: "p", SlotIndex: 1}
	got, err := s.MarkProvisioned(fixedTime)
	if err != nil {
		t.Fatalf("MarkProvisioned err=%v", err)
	}
	if got.ProvisionedAt == nil || !got.ProvisionedAt.Equal(fixedTime) {
		t.Fatalf("ProvisionedAt=%v, want %v", got.ProvisionedAt, fixedTime)
	}
}

func TestMarkActivatingSetsLeaseAndAttempt(t *testing.T) {
	s := Slot{State: SlotStateProvisioned, Project: "p", SlotIndex: 1}
	got, err := s.MarkActivating(fixedTime, "p/leases/42", 2, "p-slot-1-installer")
	if err != nil {
		t.Fatalf("MarkActivating err=%v", err)
	}
	if got.State != SlotStateActivating {
		t.Fatalf("State=%q", got.State)
	}
	if got.ActiveLeaseRef == nil || *got.ActiveLeaseRef != "p/leases/42" {
		t.Errorf("ActiveLeaseRef=%v", got.ActiveLeaseRef)
	}
	if got.ActivationAttempt == nil || *got.ActivationAttempt != 2 {
		t.Errorf("ActivationAttempt=%v", got.ActivationAttempt)
	}
	if got.ActivationJobName == nil || *got.ActivationJobName != "p-slot-1-installer" {
		t.Errorf("ActivationJobName=%v", got.ActivationJobName)
	}
	if got.ActivationStartedAt == nil || !got.ActivationStartedAt.Equal(fixedTime) {
		t.Errorf("ActivationStartedAt=%v", got.ActivationStartedAt)
	}
}

func TestMarkRunningStampsActivationCompletedAt(t *testing.T) {
	startedAt := fixedTime.Add(-time.Minute)
	s := Slot{
		State:               SlotStateActivating,
		ActivationStartedAt: &startedAt,
	}
	got, err := s.MarkRunning(fixedTime)
	if err != nil {
		t.Fatalf("MarkRunning err=%v", err)
	}
	if got.State != SlotStateRunning {
		t.Fatalf("State=%q", got.State)
	}
	if got.ActivationCompletedAt == nil || !got.ActivationCompletedAt.Equal(fixedTime) {
		t.Errorf("ActivationCompletedAt=%v", got.ActivationCompletedAt)
	}
}

func TestMarkCleaningRetainsActiveLeaseRef(t *testing.T) {
	leaseRef := "p/leases/42"
	s := Slot{State: SlotStateRunning, ActiveLeaseRef: &leaseRef}
	got, err := s.MarkCleaning(fixedTime)
	if err != nil {
		t.Fatalf("MarkCleaning err=%v", err)
	}
	if got.State != SlotStateCleaning {
		t.Fatalf("State=%q", got.State)
	}
	if got.ActiveLeaseRef == nil || *got.ActiveLeaseRef != "p/leases/42" {
		t.Errorf("ActiveLeaseRef=%v, want retained for allocator gating", got.ActiveLeaseRef)
	}
	if got.CleanupStartedAt == nil || !got.CleanupStartedAt.Equal(fixedTime) {
		t.Errorf("CleanupStartedAt=%v", got.CleanupStartedAt)
	}
}

func TestMarkErrorRetainsDiagnostics(t *testing.T) {
	startedAt := fixedTime.Add(-time.Minute)
	jobName := "p-slot-1-installer"
	s := Slot{
		State:               SlotStateActivating,
		ActivationStartedAt: &startedAt,
		ActivationJobName:   &jobName,
	}
	got, err := s.MarkError(fixedTime, "helm install timed out")
	if err != nil {
		t.Fatalf("MarkError err=%v", err)
	}
	if got.State != SlotStateError {
		t.Fatalf("State=%q", got.State)
	}
	if got.Detail == nil || *got.Detail != "helm install timed out" {
		t.Errorf("Detail=%v", got.Detail)
	}
	if got.ActivationStartedAt == nil {
		t.Error("ActivationStartedAt cleared; should be retained on error for diagnostics")
	}
	if got.ActivationJobName == nil {
		t.Error("ActivationJobName cleared; should be retained on error for diagnostics")
	}
}

func TestMarkErrorFromUnseededRejected(t *testing.T) {
	s := Slot{State: SlotStateUnseeded}
	_, err := s.MarkError(fixedTime, "won't happen")
	if !errors.Is(err, ErrInvalidSlotTransition) {
		t.Fatalf("err=%v, want wrapped ErrInvalidSlotTransition", err)
	}
}

// strPtr lives in completion_api.go and is shared across tests in this package.

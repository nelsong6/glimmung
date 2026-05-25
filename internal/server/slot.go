package server

import (
	"errors"
	"fmt"
	"time"
)

// Slot lifecycle states. A slot is one durable document in the `slots`
// collection; its state describes what is true right now, not history (see
// the "Slot Status Field Contract" section of docs/test-slot-lifecycle.md).
//
// These names are part of the durable contract: they appear verbatim in
// Cosmos documents and in the public state API. Renaming requires a
// migration of every persisted doc. The retired names `warming`, `ready`,
// `active` map to `provisioning`, `provisioned`, `running` respectively
// per docs/test-slot-storage-rework.md.
const (
	SlotStateUnseeded     = "unseeded"     // count requires this slot; no preliminary infra yet
	SlotStateProvisioning = "provisioning" // warmup goroutine is creating namespaces / RBAC / etc.
	SlotStateProvisioned  = "provisioned"  // preliminary infra exists; slot is leasable
	SlotStateActivating   = "activating"   // a lease has been claimed; runtime is being installed
	SlotStateRunning      = "running"      // claimed + runtime is up and serving
	SlotStateCleaning     = "cleaning"     // lease released; runtime is being torn down
	SlotStateError        = "error"        // terminal failure for the last lifecycle operation; repair/cleanup retry may re-enter work
)

// SlotStates is the canonical ordered list of valid slot states. Used by
// validation and by the symbol-inventory test that keeps retired names
// from re-entering the codebase.
var SlotStates = []string{
	SlotStateUnseeded,
	SlotStateProvisioning,
	SlotStateProvisioned,
	SlotStateActivating,
	SlotStateRunning,
	SlotStateCleaning,
	SlotStateError,
}

// Slot is the durable record for one capacity unit of one project. One
// Cosmos document per slot, partition key = project, document id =
// "<project>:<slot_index>".
//
// The lease record (if any) referenced by ActiveLeaseRef lives in the
// separate `leases` collection. Slot history (returns, lease churn) lives
// in the `slot_history` collection. The slot doc itself is small and
// per-row writable; cross-slot writes don't contend because each slot is
// its own document.
type Slot struct {
	Project        string     `json:"project"`
	SlotIndex      int        `json:"slot_index"`
	SlotName       string     `json:"slot_name"`
	State          string     `json:"state"`
	Detail         *string    `json:"detail,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ProvisionedAt  *time.Time `json:"provisioned_at,omitempty"`
	ActiveLeaseRef *string    `json:"active_lease_ref,omitempty"`

	// Activation describes the current activation. Populated while a slot is
	// activating or running. Cleared when the slot returns to provisioned.
	// Retained on error so operators have diagnostics.
	ActivationAttempt     *int       `json:"activation_attempt,omitempty"`
	ActivationStartedAt   *time.Time `json:"activation_started_at,omitempty"`
	ActivationCompletedAt *time.Time `json:"activation_completed_at,omitempty"`
	ActivationJobName     *string    `json:"activation_job_name,omitempty"`
	ActivationError       *string    `json:"activation_error,omitempty"`

	// Cleanup describes the current or most recent cleanup of the slot.
	// Not cleared on transition because it describes the slot itself (last
	// cleanup), not a transient lease's footprint.
	CleanupStartedAt   *time.Time `json:"cleanup_started_at,omitempty"`
	CleanupCompletedAt *time.Time `json:"cleanup_completed_at,omitempty"`
	CleanupError       *string    `json:"cleanup_error,omitempty"`

	// etag captures the Cosmos resource etag on this document. Empty when
	// the slot came from a list query that doesn't expose per-row etags.
	// Use SlotStore.UpdateIfMatch to perform CAS writes keyed on this.
	etag string `json:"-"`
}

// ETag returns the Cosmos resource etag captured by the read that produced
// this Slot. Pass to SlotStore.UpdateIfMatch for optimistic-concurrency
// writes.
func (s Slot) ETag() string { return s.etag }

// WithETag returns a copy of s with `tag` as its captured etag. Used by
// store implementations to attach the etag at read time and by tests to
// construct slots with synthetic etags.
func (s Slot) WithETag(tag string) Slot { s.etag = tag; return s }

// DocID returns the Cosmos document id for this slot.
func (s Slot) DocID() string { return SlotDocID(s.Project, s.SlotIndex) }

// SlotDocID is the canonical id format for a slot document. Project and
// slot_index together are globally unique inside the slots collection.
func SlotDocID(project string, slotIndex int) string {
	return fmt.Sprintf("%s:%d", project, slotIndex)
}

// validSlotTransitions defines the slot state machine. A transition is
// valid only if `validSlotTransitions[from][to] == true`. Same-state
// transitions are always valid (recovery re-fires goroutines without
// changing durable state); see CanTransitionTo.
var validSlotTransitions = map[string]map[string]bool{
	SlotStateUnseeded: {
		SlotStateProvisioning: true,
	},
	SlotStateProvisioning: {
		SlotStateProvisioned: true,
		SlotStateError:       true,
	},
	SlotStateProvisioned: {
		SlotStateActivating:   true,
		SlotStateProvisioning: true,
	},
	SlotStateActivating: {
		SlotStateRunning:  true,
		SlotStateCleaning: true, // activation failure proceeds to cleanup before lease release
		SlotStateError:    true,
	},
	SlotStateRunning: {
		SlotStateCleaning: true,
		// Running slots can also crash into `error` directly (helm
		// release dies, Deployment evicted, etc.). Cleanup is the
		// expected followup but isn't a precondition for naming the
		// failure.
		SlotStateError: true,
	},
	SlotStateCleaning: {
		SlotStateProvisioned: true,
		SlotStateError:       true,
	},
	SlotStateError: {
		// Error is a "last attempt failed" tag, not a permanent dead-end.
		// A repeat of the failed operation is the recovery: returnTestSlot
		// (or the release-callback path / TTL timer) re-fires cleanup against
		// an error slot, and RecoverInFlightTestSlots does the same on
		// startup. Helm install/uninstall and the K8s deletes underneath are
		// idempotent, so retrying converges on success — or the slot re-errors
		// with diagnostic context preserved in slot_history for an operator to
		// look at. A genuinely stuck slot (cleanup retry also fails) is still
		// recovered by decreasing then re-increasing count; that path remains
		// the last resort.
		SlotStateCleaning:     true,
		SlotStateProvisioning: true,
	},
}

// CanTransitionTo reports whether the slot can move from its current state
// to `next`. Same-state is always valid. Returns nil on valid, an error
// naming both states on invalid.
func (s Slot) CanTransitionTo(next string) error {
	if s.State == next {
		return nil
	}
	allowed, known := validSlotTransitions[s.State]
	if !known {
		return fmt.Errorf("%w: from unknown state %q to %q", ErrInvalidSlotTransition, s.State, next)
	}
	if !allowed[next] {
		return fmt.Errorf("%w: %q -> %q", ErrInvalidSlotTransition, s.State, next)
	}
	return nil
}

// ErrInvalidSlotTransition signals a programming bug: a caller tried to
// transition a slot to a state that the state machine doesn't allow from
// the current state. Treat as a panic-worthy error in production code;
// tests assert it where invalid transitions are expected.
var ErrInvalidSlotTransition = errors.New("invalid slot state transition")

// NewUnseededSlot constructs a slot in `unseeded` state. Used by the
// PATCH-count handler and the boot recovery sweep to seed slot documents
// for indices that should exist (within `1..count`) but don't yet.
//
// slotName is the operator-visible name (typically "<project>-slot-<n>").
// now is captured into UpdatedAt; the caller provides it so tests can
// inject deterministic timestamps.
func NewUnseededSlot(project string, slotIndex int, slotName string, now time.Time) Slot {
	return Slot{
		Project:   project,
		SlotIndex: slotIndex,
		SlotName:  slotName,
		State:     SlotStateUnseeded,
		UpdatedAt: now.UTC(),
	}
}

// MarkProvisioning transitions a slot to provisioning. Caller
// should invoke this from a SlotStore.UpdateIfMatch mutator so the write
// is etag-conditional. The normal path is `unseeded` -> `provisioning`.
// Admin repair can also move an unleased `provisioned` or preliminary-error
// slot back through `provisioning` to revalidate preliminary resources.
// Returns ErrInvalidSlotTransition if the transition is not allowed.
func (s Slot) MarkProvisioning(now time.Time) (Slot, error) {
	if err := s.CanTransitionTo(SlotStateProvisioning); err != nil {
		return s, err
	}
	s.State = SlotStateProvisioning
	s.UpdatedAt = now.UTC()
	s.Detail = nil
	s.ProvisionedAt = nil
	return s, nil
}

// MarkProvisioned transitions a slot from provisioning to provisioned.
// Stamps ProvisionedAt. Clears any activation/cleanup error fields so the
// slot is a clean slate for the next lease.
func (s Slot) MarkProvisioned(now time.Time) (Slot, error) {
	if err := s.CanTransitionTo(SlotStateProvisioned); err != nil {
		return s, err
	}
	t := now.UTC()
	s.State = SlotStateProvisioned
	s.UpdatedAt = t
	s.ProvisionedAt = &t
	s.Detail = nil
	s.ActiveLeaseRef = nil
	s.ActivationAttempt = nil
	s.ActivationStartedAt = nil
	s.ActivationCompletedAt = nil
	s.ActivationJobName = nil
	s.ActivationError = nil
	return s, nil
}

// MarkReserved records that a provisioned slot is assigned to a lease while
// leaving the slot's lifecycle state as provisioned. The state still describes
// the preliminary resources; ActiveLeaseRef is what removes the slot from the
// available pool until env-prep activates runtime or cleanup releases it.
func (s Slot) MarkReserved(now time.Time, leaseRef string) (Slot, error) {
	if err := s.CanTransitionTo(SlotStateProvisioned); err != nil {
		return s, err
	}
	if s.State != SlotStateProvisioned {
		return s, fmt.Errorf("%w: %q -> reserved", ErrInvalidSlotTransition, s.State)
	}
	if s.ActiveLeaseRef != nil && *s.ActiveLeaseRef != "" && *s.ActiveLeaseRef != leaseRef {
		return s, fmt.Errorf("%w: slot already reserved", ErrInvalidSlotTransition)
	}
	t := now.UTC()
	leaseRefCopy := leaseRef
	s.ActiveLeaseRef = &leaseRefCopy
	s.UpdatedAt = t
	s.Detail = nil
	return s, nil
}

// MarkReservationReleased clears a provisioned-slot reservation that never
// advanced to runtime activation. Runtime cleanup paths should use
// MarkCleaning/MarkCleaned instead.
func (s Slot) MarkReservationReleased(now time.Time, leaseRef string) (Slot, error) {
	if err := s.CanTransitionTo(SlotStateProvisioned); err != nil {
		return s, err
	}
	if s.State != SlotStateProvisioned {
		return s, fmt.Errorf("%w: %q -> reservation release", ErrInvalidSlotTransition, s.State)
	}
	if s.ActiveLeaseRef != nil && *s.ActiveLeaseRef != "" && *s.ActiveLeaseRef != leaseRef {
		return s, fmt.Errorf("%w: slot reserved by another lease", ErrInvalidSlotTransition)
	}
	t := now.UTC()
	s.ActiveLeaseRef = nil
	s.UpdatedAt = t
	return s, nil
}

// MarkActivating transitions a provisioned slot to activating. Attaches
// the lease ref so consumers reading the slot can resolve to its lease
// without a separate cross-collection lookup at read time.
func (s Slot) MarkActivating(now time.Time, leaseRef string, attempt int, jobName string) (Slot, error) {
	if err := s.CanTransitionTo(SlotStateActivating); err != nil {
		return s, err
	}
	t := now.UTC()
	leaseRefCopy := leaseRef
	attemptCopy := attempt
	jobNameCopy := jobName
	s.State = SlotStateActivating
	s.UpdatedAt = t
	s.ActiveLeaseRef = &leaseRefCopy
	s.ActivationAttempt = &attemptCopy
	s.ActivationStartedAt = &t
	s.ActivationCompletedAt = nil
	s.ActivationJobName = &jobNameCopy
	s.ActivationError = nil
	return s, nil
}

// MarkRunning transitions activating to running. Stamps ActivationCompletedAt.
func (s Slot) MarkRunning(now time.Time) (Slot, error) {
	if err := s.CanTransitionTo(SlotStateRunning); err != nil {
		return s, err
	}
	t := now.UTC()
	s.State = SlotStateRunning
	s.UpdatedAt = t
	s.ActivationCompletedAt = &t
	s.ActivationError = nil
	return s, nil
}

// MarkCleaning transitions running or activating to cleaning. Records
// the cleanup start time; existing ActiveLeaseRef is retained so the
// allocator can't hand the slot to a new caller until cleanup is done.
func (s Slot) MarkCleaning(now time.Time) (Slot, error) {
	if err := s.CanTransitionTo(SlotStateCleaning); err != nil {
		return s, err
	}
	t := now.UTC()
	s.State = SlotStateCleaning
	s.UpdatedAt = t
	s.CleanupStartedAt = &t
	s.CleanupCompletedAt = nil
	s.CleanupError = nil
	return s, nil
}

// MarkCleaned transitions cleaning to provisioned. Stamps CleanupCompletedAt
// and clears the active lease ref so the slot is available again.
func (s Slot) MarkCleaned(now time.Time) (Slot, error) {
	if err := s.CanTransitionTo(SlotStateProvisioned); err != nil {
		return s, err
	}
	t := now.UTC()
	s.State = SlotStateProvisioned
	s.UpdatedAt = t
	s.CleanupCompletedAt = &t
	s.CleanupError = nil
	s.ActiveLeaseRef = nil
	s.ActivationAttempt = nil
	s.ActivationStartedAt = nil
	s.ActivationCompletedAt = nil
	s.ActivationJobName = nil
	s.ActivationError = nil
	return s, nil
}

// MarkError transitions any non-terminal state to error. `detail` is the
// operator-visible reason. The slot's activation/cleanup fields are
// preserved so operators have full diagnostics; the slot can only be
// recovered by decreasing then re-increasing count.
//
// MarkError also populates the phase-specific error field based on the
// prior state: failures observed during activation or running write to
// `activation_error`; failures observed during cleaning write to
// `cleanup_error`. Both reflect the same root cause; the duplication
// matches the legacy contract that dashboards/CLI tools rendered against.
func (s Slot) MarkError(now time.Time, detail string) (Slot, error) {
	priorState := s.State
	if err := s.CanTransitionTo(SlotStateError); err != nil {
		return s, err
	}
	t := now.UTC()
	detailCopy := detail
	s.State = SlotStateError
	s.UpdatedAt = t
	s.Detail = &detailCopy
	switch priorState {
	case SlotStateActivating, SlotStateRunning:
		errCopy := detail
		s.ActivationError = &errCopy
	case SlotStateCleaning:
		errCopy := detail
		s.CleanupError = &errCopy
	}
	return s, nil
}

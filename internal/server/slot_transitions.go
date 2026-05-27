package server

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// slotStoreFromReadStore returns the SlotStore type-asserted off a
// ReadStore. Returns nil if the store doesn't implement SlotStore (which
// is a misconfiguration in production but tolerated in unit tests that
// don't exercise slot lifecycle).
func slotStoreFromReadStore(store ReadStore) SlotStore {
	if store == nil {
		return nil
	}
	if s, ok := store.(SlotStore); ok {
		return s
	}
	return nil
}

// slotHistoryStoreFromReadStore is the SlotHistoryStore companion of
// slotStoreFromReadStore.
func slotHistoryStoreFromReadStore(store ReadStore) SlotHistoryStore {
	if store == nil {
		return nil
	}
	if s, ok := store.(SlotHistoryStore); ok {
		return s
	}
	return nil
}

// errSlotStoreNotConfigured surfaces from slot-transition helpers when
// the store isn't wired with SlotStore. This is a real production-time
// failure mode; the helpers log it loudly rather than silently no-op.
var errSlotStoreNotConfigured = errors.New("slot store not configured")

// markSlotProvisioning transitions a slot from unseeded to provisioning.
// Used by the warmup path at the start of its work. Idempotent for the
// recovery-sweep re-fire case: if the slot is already in `provisioning`,
// CanTransitionTo allows same-state and the etag-conditional write just
// refreshes UpdatedAt.
func markSlotProvisioning(ctx context.Context, store ReadStore, project string, slotIndex int, now time.Time) (Slot, error) {
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return Slot{}, errSlotStoreNotConfigured
	}
	return slotStore.UpdateIfMatch(ctx, project, slotIndex, func(s Slot) (Slot, error) {
		return s.MarkProvisioning(now)
	})
}

// markSlotProvisioned transitions a slot from provisioning to provisioned.
// Used by the warmup path on success.
func markSlotProvisioned(ctx context.Context, store ReadStore, project string, slotIndex int, now time.Time) (Slot, error) {
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return Slot{}, errSlotStoreNotConfigured
	}
	return slotStore.UpdateIfMatch(ctx, project, slotIndex, func(s Slot) (Slot, error) {
		return s.MarkProvisioned(now)
	})
}

// markSlotError transitions a slot to error with the given detail. Used
// by the warmup path on failure of `EnsureTestSlotPreliminaries`, and
// by the activation/cleanup paths on terminal failure.
func markSlotError(ctx context.Context, store ReadStore, project string, slotIndex int, now time.Time, cause error) (Slot, error) {
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return Slot{}, errSlotStoreNotConfigured
	}
	detail := cause.Error()
	return slotStore.UpdateIfMatch(ctx, project, slotIndex, func(s Slot) (Slot, error) {
		return s.MarkError(now, detail)
	})
}

// markLeaseSlotActivating transitions the slot identified by the lease's
// metadata to activating, recording the lease ref and activation
// attempt/job-name. Ensures the slot doc exists first (in case the slot
// is being activated before any warmup has had a chance to seed it —
// happens in tests that pre-populate project state and skip warmup, and
// in recovery scenarios where a claimed lease persisted across pods
// that didn't have the slot doc yet).
func markLeaseSlotActivating(ctx context.Context, store ReadStore, lease Lease, jobName string, now time.Time) (Slot, error) {
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return Slot{}, errSlotStoreNotConfigured
	}
	slotIndex, projectKey, err := slotIdentityFromLease(lease)
	if err != nil {
		return Slot{}, err
	}
	if err := ensureSlotForLease(ctx, store, lease, now); err != nil {
		return Slot{}, err
	}
	leaseRef := LeasePublicRefFromLease(lease)
	attempt := 0
	if lease.LeaseNumber != nil {
		attempt = *lease.LeaseNumber
	}
	return slotStore.UpdateIfMatch(ctx, projectKey, slotIndex, func(s Slot) (Slot, error) {
		// Pull the slot through unseeded → provisioning → provisioned
		// on demand if the lease was claimed against a slot that the
		// new storage layer hasn't seen yet. This is a transitional
		// path: production startup runs the migration before any
		// lifecycle runs, but tests may exercise activation against a
		// slot that was only set up in the legacy project metadata.
		if s.State == SlotStateUnseeded || s.State == SlotStateProvisioning {
			next, err := s.MarkProvisioning(now)
			if err == nil {
				s = next
			}
		}
		if s.State == SlotStateProvisioning {
			next, err := s.MarkProvisioned(now)
			if err == nil {
				s = next
			}
		}
		return s.MarkActivating(now, leaseRef, attempt, jobName)
	})
}

// ensureSlotForLease seeds a slot doc for the slot identified by the
// lease's metadata if one doesn't yet exist. The seeded doc is in
// `unseeded` state; the caller's transition then walks it forward. Used
// by lifecycle helpers (markLeaseSlotActivating, markLeaseSlotCleaning,
// markLeaseSlotCleaned, markLeaseSlotError) so they can operate on a
// slot that the migration / PATCH-count / boot-recovery has not yet
// seeded.
func ensureSlotForLease(ctx context.Context, store ReadStore, lease Lease, now time.Time) error {
	slotIndex, projectKey, err := slotIdentityFromLease(lease)
	if err != nil {
		return err
	}
	slotName := ""
	if name := nativeSlotNameFromMetadata(lease.Metadata); name != nil {
		slotName = *name
	}
	if slotName == "" {
		slotName = fmt.Sprintf("%s-slot-%d", projectKey, slotIndex)
	}
	_, err = ensureSlotExists(ctx, store, projectKey, slotIndex, slotName, now)
	return err
}

// markLeaseSlotRunning transitions an activating slot to running.
func markLeaseSlotRunning(ctx context.Context, store ReadStore, lease Lease, now time.Time) (Slot, error) {
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return Slot{}, errSlotStoreNotConfigured
	}
	slotIndex, projectKey, err := slotIdentityFromLease(lease)
	if err != nil {
		return Slot{}, err
	}
	if err := ensureSlotForLease(ctx, store, lease, now); err != nil {
		return Slot{}, err
	}
	return slotStore.UpdateIfMatch(ctx, projectKey, slotIndex, func(s Slot) (Slot, error) {
		return s.MarkRunning(now)
	})
}

// markLeaseSlotCleaning transitions a slot (running or activating) to
// cleaning. The active_lease_ref is retained until cleanup finishes so
// the allocator can't hand the slot to a new caller mid-cleanup.
func markLeaseSlotCleaning(ctx context.Context, store ReadStore, lease Lease, now time.Time) (Slot, error) {
	return updateLeaseSlotCleaning(ctx, store, lease, now, false)
}

func claimLeaseSlotCleaning(ctx context.Context, store ReadStore, lease Lease, now time.Time) (Slot, error) {
	return updateLeaseSlotCleaning(ctx, store, lease, now, true)
}

func updateLeaseSlotCleaning(ctx context.Context, store ReadStore, lease Lease, now time.Time, rejectAlreadyCleaning bool) (Slot, error) {
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return Slot{}, errSlotStoreNotConfigured
	}
	slotIndex, projectKey, err := slotIdentityFromLease(lease)
	if err != nil {
		return Slot{}, err
	}
	if err := ensureSlotForLease(ctx, store, lease, now); err != nil {
		return Slot{}, err
	}
	return slotStore.UpdateIfMatch(ctx, projectKey, slotIndex, func(s Slot) (Slot, error) {
		if rejectAlreadyCleaning && s.State == SlotStateCleaning {
			return s, ErrPreconditionFailed
		}
		// Tolerate a transition from any pre-cleanup state. If the
		// slot was found in unseeded/provisioning/provisioned and
		// cleanup is being fired against it (TTL expiry on a lease
		// whose activation never reached running, or recovery from
		// a stale cleaning state), walk it forward so the cleaning
		// transition is valid.
		switch s.State {
		case SlotStateUnseeded, SlotStateProvisioning, SlotStateProvisioned:
			// Cleanup on a slot that never made it to active is a
			// no-op-ish: the slot just goes back to provisioned. We
			// model this by marking it cleaning then cleaned so the
			// cleanup pathway is unified.
			leaseRef := LeasePublicRefFromLease(lease)
			s.ActiveLeaseRef = &leaseRef
			if s.State == SlotStateUnseeded {
				if next, err := s.MarkProvisioning(now); err == nil {
					s = next
				}
			}
			if s.State == SlotStateProvisioning {
				if next, err := s.MarkProvisioned(now); err == nil {
					s = next
				}
			}
			if next, err := s.MarkActivating(now, leaseRef, 0, ""); err == nil {
				s = next
			}
		}
		return s.MarkCleaning(now)
	})
}

// markLeaseSlotCleaned transitions a slot back to provisioned, clearing
// the active_lease_ref. If the slot is in `running` or `activating`
// (cleanup-finished was called without a prior cleaning transition —
// e.g. a test that exercises the helper directly, or a path that
// short-circuits the cleaning state), walk the slot through cleaning
// first so the transition is well-formed.
func markLeaseSlotCleaned(ctx context.Context, store ReadStore, lease Lease, now time.Time) (Slot, error) {
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return Slot{}, errSlotStoreNotConfigured
	}
	slotIndex, projectKey, err := slotIdentityFromLease(lease)
	if err != nil {
		return Slot{}, err
	}
	if err := ensureSlotForLease(ctx, store, lease, now); err != nil {
		return Slot{}, err
	}
	return slotStore.UpdateIfMatch(ctx, projectKey, slotIndex, func(s Slot) (Slot, error) {
		switch s.State {
		case SlotStateRunning, SlotStateActivating, SlotStateError:
			// SlotStateError is the recovery-retry case: the previous
			// cleanup attempt left the slot in error with cleanup_error
			// set, and the recovery sweep (or a follow-up returnTestSlot
			// that bypassed claimTestSlotCleanup) is asking to converge
			// directly. Walk through cleaning so the transition to
			// provisioned is well-formed.
			next, err := s.MarkCleaning(now)
			if err != nil {
				return s, err
			}
			s = next
		}
		return s.MarkCleaned(now)
	})
}

// markLeaseSlotError transitions the slot identified by the lease to
// error with cause as detail. Activation/cleanup diagnostics are
// preserved by MarkError for operator triage.
func markLeaseSlotError(ctx context.Context, store ReadStore, lease Lease, now time.Time, cause error) (Slot, error) {
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return Slot{}, errSlotStoreNotConfigured
	}
	slotIndex, projectKey, err := slotIdentityFromLease(lease)
	if err != nil {
		return Slot{}, err
	}
	if err := ensureSlotForLease(ctx, store, lease, now); err != nil {
		return Slot{}, err
	}
	detail := cause.Error()
	return slotStore.UpdateIfMatch(ctx, projectKey, slotIndex, func(s Slot) (Slot, error) {
		// Walk the slot through provisioning/provisioned/activating
		// as needed if the failure happened before any prior
		// transition (e.g. activation failed at the first step).
		// Error is a valid transition from provisioning, activating,
		// cleaning — so push the slot to one of those first if
		// necessary.
		if s.State == SlotStateUnseeded {
			if next, err := s.MarkProvisioning(now); err == nil {
				s = next
			}
		}
		return s.MarkError(now, detail)
	})
}

// appendLeaseSlotHistory writes one entry into the slot_history
// collection. Returns nil silently if the store doesn't implement
// SlotHistoryStore (matches the silently-skipped pattern for fakes that
// don't model history).
func appendLeaseSlotHistory(ctx context.Context, store ReadStore, entry SlotHistoryEntry) error {
	history := slotHistoryStoreFromReadStore(store)
	if history == nil {
		return nil
	}
	if _, err := history.AppendSlotHistory(ctx, entry); err != nil {
		return err
	}
	return nil
}

// slotIdentityFromLease extracts (slot_index, project_key) from a test-
// slot lease's metadata. Returns an error if the lease is missing the
// required metadata fields — that's a programming bug (a non-test-slot
// lease shouldn't be flowing through the slot-transition helpers).
func slotIdentityFromLease(lease Lease) (slotIndex int, projectKey string, err error) {
	idxPtr := nativeSlotIndexFromMetadata(lease.Metadata)
	if idxPtr == nil {
		return 0, "", fmt.Errorf("test-slot lease is missing native_slot_index metadata")
	}
	projectKey = firstNonEmpty(lease.Project, "")
	if projectKey == "" {
		return 0, "", fmt.Errorf("test-slot lease is missing project")
	}
	return *idxPtr, projectKey, nil
}

// ensureSlotExists creates an unseeded slot doc if one doesn't yet
// exist. Used by the PATCH-count handler and boot recovery to seed the
// missing slots inside `1..count`. Returns the slot (existing or freshly
// created). Idempotent.
func ensureSlotExists(ctx context.Context, store ReadStore, project string, slotIndex int, slotName string, now time.Time) (Slot, error) {
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return Slot{}, errSlotStoreNotConfigured
	}
	existing, err := slotStore.GetSlot(ctx, project, slotIndex)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Slot{}, err
	}
	return slotStore.CreateSlot(ctx, NewUnseededSlot(project, slotIndex, slotName, now))
}

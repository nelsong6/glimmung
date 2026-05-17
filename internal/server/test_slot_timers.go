package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nelsong6/glimmung/internal/metrics"
)

// testSlotLeaseTimers holds one *time.Timer per claimed test-slot lease that
// has a TTL. The timer fires once at the deadline and kicks off the same
// cleanup pathway that POST /v1/test-slots/return uses. Release/cancel paths
// Stop the timer first so it never fires after the lease is gone.
//
// Storage is in-process and not durable. On glimmung restart the map is
// empty; recovery is the responsibility of RecoverInFlightTestSlots, which
// walks Cosmos once at startup and re-arms a timer for every still-claimed
// test-slot lease (computing remaining TTL from AssignedAt).
//
// This design replaces the 15-second polling reconciler that previously
// detected expired leases. The "event" the cleanup pathway responds to is the
// deadline arriving, not a tick noticing the deadline has already passed.
var testSlotLeaseTimers sync.Map // map[string]*time.Timer keyed by LeasePublicRefFromLease

// armLeaseExpiryTimer schedules cleanup of `lease` to fire at its TTL
// deadline. If a timer is already armed for this lease ref it is replaced
// (handles re-arming on process restart through the recovery sweep). A
// deadline already in the past fires on the next scheduler tick.
//
// Call only for `claimed` test-slot leases with TTLSeconds > 0.
func armLeaseExpiryTimer(store ReadStore, preparer TestSlotPreparer, project Project, lease Lease, logf func(string, ...any)) {
	if preparer == nil {
		return
	}
	ref := LeasePublicRefFromLease(lease)
	if ref == "" {
		return
	}
	if lease.TTLSeconds <= 0 {
		return
	}
	started := lease.RequestedAt
	if lease.AssignedAt != nil {
		started = *lease.AssignedAt
	}
	delay := time.Duration(lease.TTLSeconds)*time.Second - time.Since(started)
	if delay < 0 {
		delay = 0
	}

	timer := time.AfterFunc(delay, func() {
		fireLeaseExpiry(store, preparer, project, lease, logf)
	})

	if prior, loaded := testSlotLeaseTimers.Swap(ref, timer); loaded {
		if t, ok := prior.(*time.Timer); ok {
			t.Stop()
		}
	}
}

// cancelLeaseExpiryTimer stops and forgets the timer for `leaseRef`. Called
// from any path that takes a lease out of the `claimed` state: explicit
// return, callback release, admin cancel, the cleanup pathway itself. Safe
// to call when no timer is armed.
func cancelLeaseExpiryTimer(leaseRef string) {
	if leaseRef == "" {
		return
	}
	if prior, loaded := testSlotLeaseTimers.LoadAndDelete(leaseRef); loaded {
		if t, ok := prior.(*time.Timer); ok {
			t.Stop()
		}
	}
}

// fireLeaseExpiry runs in the timer goroutine. It removes its own map entry,
// re-checks the lease is still claimed, then atomically claims the cleanup
// against the project doc's etag. The atomic claim is what makes the design
// safe across multiple replicas: the same timer is armed in every running
// pod, every pod's timer fires at the same wall-clock instant, but the
// database arbitrates — exactly one pod's etag-conditional write succeeds
// and the others get ErrPreconditionFailed back, which means "another
// replica already won, my work is done."
func fireLeaseExpiry(store ReadStore, preparer TestSlotPreparer, project Project, lease Lease, logf func(string, ...any)) {
	ref := LeasePublicRefFromLease(lease)
	testSlotLeaseTimers.Delete(ref)

	ctx := context.Background()
	if !testSlotLeaseStillClaimed(ctx, store, lease) {
		// Returned out from under us. Nothing to do — the return path is
		// already running or has already finished cleanup.
		return
	}

	if _, err := claimTestSlotCleanup(ctx, store, project, lease, "lease.ttl_expiry"); err != nil {
		if errors.Is(err, ErrPreconditionFailed) {
			// Another replica's timer fired first and won the etag race.
			// Their cleanup is in flight; ours is a no-op. This is the
			// path that makes multi-replica deploys (rolling updates, node
			// drains) correct without leader election.
			return
		}
		if logf != nil {
			logf("test-slot lease expiry claim failed project=%s lease=%s: %v", lease.Project, ref, err)
		}
		return
	}
	// Only one replica wins the etag race and reaches here, so the
	// counter increment is at-most-once per expired lease across the
	// fleet. test_slot_checkout is the only purpose that arms a TTL
	// timer, so the label is unambiguous.
	metrics.RecordLeaseReleased(LeasePurposeTestSlotCheckout, "expired")
	beginTestSlotCleanup(store, preparer, project, lease, true, logf)
}

// claimTestSlotCleanup atomically transitions the slot status to `cleaning`
// using a Cosmos etag-conditional write. Exactly one caller across all
// glimmung replicas wins; the rest get ErrPreconditionFailed back. Returns
// ErrUnsupported when the store doesn't implement the CAS interface (in
// tests with a write-only fake the cleanup will run unconditionally).
//
// `source` is recorded on the return-history entry so dashboards can tell
// timer-expiry from operator return.
func claimTestSlotCleanup(ctx context.Context, store ReadStore, project Project, lease Lease, source string) (Project, error) {
	claimer, hasClaimer := store.(ProjectTestEnvironmentSlotStatusClaimer)
	reader, hasReader := store.(ProjectReader)
	if !hasClaimer || !hasReader {
		// No CAS support — fall back to the unconditional path. This is
		// fine for single-replica deploys; multi-replica safety requires
		// the store to implement both interfaces. Tests using the unconditional
		// fake exercise this branch.
		historyEntry := testSlotReturnHistoryEntry(lease, testSlotReturnAudit{
			Source:         source,
			CleanupStarted: true,
		})
		return setLeaseSlotCleanupStarting(ctx, store, project, lease, historyEntry)
	}

	projectKey := firstNonEmpty(lease.Project, project.ID, project.Name)
	fresh, err := reader.ReadProject(ctx, projectKey)
	if err != nil {
		return Project{}, err
	}
	slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
	if slotIndex == nil {
		return Project{}, errors.New("test-slot lease missing slot index metadata")
	}
	current, hasCurrent := testEnvironmentSlotStatus(fresh, *slotIndex)
	if hasCurrent && current.State == testSlotStateCleaning {
		// Already in the cleaning state in durable storage — another
		// replica's claim landed before our read. Treat as a lost race.
		return Project{}, ErrPreconditionFailed
	}

	now := time.Now().UTC()
	slotName := current.SlotName
	if slotName == "" {
		if name := nativeSlotNameFromMetadata(lease.Metadata); name != nil {
			slotName = *name
		}
	}
	state := testSlotStateCleaning
	status := current
	status.SlotIndex = *slotIndex
	status.SlotName = slotName
	status.State = state
	status.UpdatedAt = now
	status.CleanupState = &state
	status.CleanupStartedAt = &now
	status.CleanupCompletedAt = nil
	status.CleanupError = nil
	status.ReturnHistory = appendBoundedTestSlotReturnHistory(status.ReturnHistory, testSlotReturnHistoryEntry(lease, testSlotReturnAudit{
		Source:         source,
		CleanupStarted: true,
	}))

	return claimer.SetProjectTestEnvironmentSlotStatusIfMatch(ctx, projectKey, status, fresh.ETag())
}

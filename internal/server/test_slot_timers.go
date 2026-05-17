package server

import (
	"context"
	"sync"
	"time"
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
// re-checks the lease is still claimed (the lease may have been returned in
// the small window between the deadline and this goroutine running), and
// then enters the same cleanup pathway return/callback-release use.
func fireLeaseExpiry(store ReadStore, preparer TestSlotPreparer, project Project, lease Lease, logf func(string, ...any)) {
	ref := LeasePublicRefFromLease(lease)
	testSlotLeaseTimers.Delete(ref)

	ctx := context.Background()
	if !testSlotLeaseStillClaimed(ctx, store, lease) {
		// Returned out from under us. Nothing to do — the return path is
		// already running or has already finished cleanup.
		return
	}

	historyEntry := testSlotReturnHistoryEntry(lease, testSlotReturnAudit{
		Source:         "lease.ttl_expiry",
		CleanupStarted: true,
	})
	if _, err := setLeaseSlotCleanupStarting(ctx, store, project, lease, historyEntry); err != nil {
		if logf != nil {
			logf("test-slot lease expiry record failed project=%s lease=%s: %v", lease.Project, ref, err)
		}
		return
	}
	beginTestSlotCleanup(store, preparer, project, lease, true, logf)
}

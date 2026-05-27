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
// walks durable lease state once at startup and re-arms a timer for every
// still-claimed test-slot lease (computing remaining TTL from AssignedAt).
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
	current, ok, err := currentTestSlotLease(ctx, store, lease)
	if err != nil {
		if logf != nil {
			logf("test-slot lease expiry state check failed project=%s lease=%s: %v", lease.Project, ref, err)
		}
	} else if !ok || current.State != "claimed" {
		// Returned out from under us. Nothing to do — the return path is
		// already running or has already finished cleanup.
		return
	} else if !testSlotLeaseDeadlineReached(current, time.Now()) {
		// Another replica or API request extended the durable TTL after this
		// process armed its old timer. Re-arm from the current document so
		// the old deadline cannot clean up the lease early.
		armLeaseExpiryTimer(store, preparer, project, current, logf)
		return
	} else {
		lease = current
	}

	if _, err := claimTestSlotCleanup(ctx, store, project, lease, testSlotReturnAudit{Source: "lease.ttl_expiry"}); err != nil {
		if errors.Is(err, ErrPreconditionFailed) {
			// Another replica's timer fired first and won the etag race.
			// Their cleanup is in flight; ours is a no-op. This is the
			// path that makes multi-replica deploys (rolling updates, node
			// drains) correct without leader election.
			metrics.RecordTestSlotCleanupClaim(activationCancelTTLExpiry, metrics.CleanupClaimOutcomeLostRace)
			return
		}
		metrics.RecordTestSlotCleanupClaim(activationCancelTTLExpiry, metrics.CleanupClaimOutcomeError)
		if logf != nil {
			logf("test-slot lease expiry claim failed project=%s lease=%s: %v", lease.Project, ref, err)
		}
		return
	}
	metrics.RecordTestSlotCleanupClaim(activationCancelTTLExpiry, metrics.CleanupClaimOutcomeGranted)
	// Only one replica wins the etag race and reaches here, so the
	// counter increment is at-most-once per expired lease across the
	// fleet. test_slot_checkout is the only purpose that arms a TTL
	// timer, so the label is unambiguous.
	metrics.RecordLeaseReleased(LeasePurposeTestSlotCheckout, "expired")
	beginTestSlotCleanup(store, preparer, project, lease, true, activationCancelTTLExpiry, logf)
}

// claimTestSlotCleanup atomically transitions the slot to `cleaning` via
// the new SlotStore's per-slot CAS write. Exactly one caller across all
// glimmung replicas wins; the rest get ErrPreconditionFailed back if
// they observe the slot already in `cleaning`.
//
// The slot may be in any pre-cleanup state — provisioned, activating,
// running, or error. error→cleaning is a recovery retry: the prior
// cleanup attempt left the slot in error with cleanup_error set, and a
// new caller (return, callback release, TTL timer, or startup recovery)
// is asking for another attempt. K8s ops underneath are idempotent, so
// retry converges or re-errors with diagnostic context preserved in
// slot_history. The validSlotTransitions map enforces this explicitly.
//
// `audit` is what the caller knows about who triggered the cleanup and
// is appended to the slot_history collection so dashboards can
// distinguish timer-expiry, return-API, callback-release, and startup-
// recovery sources from each other and carry per-call context like
// caller pod IP, session ID, and reason where available.
func claimTestSlotCleanup(ctx context.Context, store ReadStore, _ Project, lease Lease, audit testSlotReturnAudit) (Project, error) {
	now := time.Now().UTC()
	if err := ensureSlotForLease(ctx, store, lease, now); err != nil {
		return Project{}, err
	}
	// Claim the cleaning transition inside the same CAS mutation that writes
	// the slot. A pre-read alone is stale under concurrent return/timer calls:
	// a loser can observe "running", then reach the write after the winner has
	// already moved the slot to "cleaning". error->cleaning remains a valid
	// retry; cleaning->cleaning means another caller already owns this cleanup.
	if _, err := claimLeaseSlotCleaning(ctx, store, lease, now); err != nil {
		return Project{}, err
	}
	// Append the audit entry to the slot_history collection so the slot's
	// audit trail records who triggered this cleanup.
	audit.CleanupStarted = true
	if err := appendLeaseSlotHistory(ctx, store, slotHistoryEntryFromLegacy(testSlotReturnHistoryEntry(lease, audit))); err != nil {
		return Project{}, err
	}
	return Project{}, nil
}

// warmupRetryRng generates the jitter used to spread out concurrent
// claimTestSlotWarmup was retired with the slot-storage rework. The
// transition from unseeded to provisioning is now per-slot via
// SlotStore.UpdateIfMatch (see markSlotProvisioning); cross-slot
// contention vanishes because every slot is its own document. The
// jittered-backoff retry loop, the warmupRetryRng/warmupRetryMu pair,
// and the `recoveryMinAge`-gated re-read logic became unnecessary at
// the same time.

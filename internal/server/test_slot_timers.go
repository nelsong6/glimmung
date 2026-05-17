package server

import (
	"context"
	"errors"
	"math/rand"
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

// warmupRetryRng generates the jitter used to spread out concurrent
// warmup-CAS retries. A separately-seeded Rand avoids contention on the
// global default Source under heavy goroutine pressure.
var warmupRetryRng = rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // jitter only

var warmupRetryMu sync.Mutex

func warmupRetryJitter(base time.Duration) time.Duration {
	warmupRetryMu.Lock()
	defer warmupRetryMu.Unlock()
	if base <= 0 {
		return 0
	}
	// Sleep duration ∈ [base, 2·base) — full-jitter exponential backoff.
	return base + time.Duration(warmupRetryRng.Int63n(int64(base)))
}

// claimTestSlotWarmup writes the initial `warming` status for `slotIndex`
// using an etag-conditional ReplaceItem. The atomic write IS the ownership
// claim: when two replicas both fire warmup for the same slot, the second
// will either observe state=warming on its retry-read and back off, or
// race the first to the write and lose CAS.
//
// Cross-slot etag contention: a project doc's etag is bumped by writes
// to *any* slot, not just this one. When PATCH count fires warmup for a
// project with count=N, all N goroutines write to the same doc; each
// cross-slot write triggers a 412 on every other in-flight goroutine.
//
// The retry loop distinguishes two kinds of 412 explicitly:
//
//  1. **Cross-slot contention** (our slot is still missing or stale-warming
//     on re-read) — keep trying with jittered backoff. The previous
//     5-attempt limit was too tight for count-of-10 warmup storms: one
//     unlucky goroutine could lose 5 races in a row and silently give up,
//     leaving a slot permanently un-warmed (observed on prod after the
//     PR #516 rollout — slot 8 went missing for exactly this reason).
//  2. **Lost claim** (our slot's state moved to `ready`, or to `warming`
//     with a recent `updated_at`) — return ErrPreconditionFailed; another
//     replica owns this slot's warmup and ours is a genuine no-op.
//
// Stores that don't implement the CAS interface fall through to an
// unconditional write — safe for single-replica deploys and for in-process
// test fakes.
func claimTestSlotWarmup(ctx context.Context, store ReadStore, writer ProjectTestEnvironmentSlotStatusWriter, projectKey string, slotIndex int, slotName string, now time.Time) (Project, error) {
	claimer, hasClaimer := store.(ProjectTestEnvironmentSlotStatusClaimer)
	reader, hasReader := store.(ProjectReader)
	if !hasClaimer || !hasReader {
		return writer.SetProjectTestEnvironmentSlotStatus(ctx, projectKey, TestEnvironmentSlotStatus{
			SlotIndex: slotIndex,
			SlotName:  slotName,
			State:     testSlotStateWarming,
			UpdatedAt: now,
		})
	}

	const (
		maxAttempts     = 30
		initialBackoff  = 5 * time.Millisecond
		maxBackoff      = 200 * time.Millisecond
	)
	backoff := initialBackoff
	for attempt := 0; attempt < maxAttempts; attempt++ {
		fresh, err := reader.ReadProject(ctx, projectKey)
		if err != nil {
			return Project{}, err
		}
		status := TestEnvironmentSlotStatus{
			SlotIndex: slotIndex,
			SlotName:  slotName,
			State:     testSlotStateWarming,
			UpdatedAt: now,
		}
		if current, hasCurrent := testEnvironmentSlotStatus(fresh, slotIndex); hasCurrent {
			// Already finished? `ready` means another writer completed the
			// whole cycle while we were spinning up; nothing to do.
			if current.State == testSlotStateReady {
				return Project{}, ErrPreconditionFailed
			}
			// Already in-flight? state=warming with a recent updated_at
			// means another writer's warmup is actively running. Stale
			// warming (older than recoveryMinAge) is the resume case and
			// is allowed to proceed.
			if current.State == testSlotStateWarming && !current.UpdatedAt.IsZero() && time.Since(current.UpdatedAt) < recoveryMinAge {
				return Project{}, ErrPreconditionFailed
			}
			// Preserve metadata (cleanup_state, return history) on the
			// claim write — we only mutate state + updated_at.
			current.SlotIndex = slotIndex
			current.SlotName = slotName
			current.State = testSlotStateWarming
			current.UpdatedAt = now
			status = current
		}
		updated, err := claimer.SetProjectTestEnvironmentSlotStatusIfMatch(ctx, projectKey, status, fresh.ETag())
		if errors.Is(err, ErrPreconditionFailed) {
			// Etag moved. Don't give up yet — back off and re-read. The
			// state-check at the top of the next iteration will exit early
			// if our slot was actually claimed by another writer; otherwise
			// we'll attempt the CAS again with the fresh etag.
			select {
			case <-ctx.Done():
				return Project{}, ctx.Err()
			case <-time.After(warmupRetryJitter(backoff)):
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		return updated, err
	}
	// Retries exhausted under genuine contention. The slot may still be
	// stuck if every retry lost; the next pod restart or PATCH count will
	// retry. Return a distinct error so the caller logs loudly rather than
	// silently treating it as "another replica owns it."
	return Project{}, errors.New("warmup CAS exhausted retries under cross-slot contention")
}

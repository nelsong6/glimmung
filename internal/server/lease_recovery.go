package server

import (
	"context"
	"time"
)

// StaleLeaseExpiryRow is the projection of a lease row that the
// expire-stale sweep needs: durable state, durable deadline, and identity.
// The store-package adapter on *Store derives State from
// `payload->>'state'` and ExpiresAt from the dedicated `expires_at`
// column.
type StaleLeaseExpiryRow struct {
	ID        string
	Project   string
	State     string
	ExpiresAt *time.Time
}

// StaleLeaseStore is the runtime store surface the expire-stale sweep
// uses. The real implementation lives on store.Store and is wired in
// cmd/glimmung-go/main.go. Tests use the in-memory implementation in
// lease_recovery_test.go.
type StaleLeaseStore interface {
	ListLeasesForExpirySweep(ctx context.Context) ([]StaleLeaseExpiryRow, error)
	PatchLeasePayload(ctx context.Context, project, id string, mutate func(payload map[string]any) error) error
}

// ExpireStaleLeases transitions every lease whose durable expires_at
// deadline has passed but whose state is still active or claimed to
// state=expired with expired_at and expiry_reason=stale_at_startup. The
// sweep runs once during glimmung process start and only when
// Settings.ControlPlaneLoopsEnabled is true; slot processes intentionally
// do not run it.
//
// Source of orphans the sweep recovers:
//
//   - native-k8s active leases never released because the run completion
//     callback never arrived (404 token, malformed payload, pod evicted
//     mid-callback). The active state has no in-process timer; only this
//     sweep moves it terminal.
//   - claimed test-slot checkout leases whose AfterFunc deadline fired
//     while glimmung was down and whose lease metadata lacks the
//     test_slot_checkout=true flag that RecoverInFlightTestSlots filters
//     on (older lease shapes). Recover silently skips them so the timer
//     is never re-armed.
//   - Lease rows from pre-test-slot-lifecycle code that no live cleanup
//     path covers.
//
// At-startup execution is sufficient because every fresh active/claimed
// lease arms its own in-process release or AfterFunc timer in the same
// process that created it. Only orphans from a previous process survive
// across a restart, and the sweep catches them on the way back up.
//
// The mutate closure inside PatchLeasePayload re-checks state inside the
// SELECT FOR UPDATE transaction so a concurrent release/cancel/callback
// path that wins the race is not overwritten by the sweep.
func ExpireStaleLeases(ctx context.Context, store StaleLeaseStore, now time.Time, logf func(string, ...any)) (int, error) {
	if store == nil {
		return 0, nil
	}
	rows, err := store.ListLeasesForExpirySweep(ctx)
	if err != nil {
		return 0, err
	}
	expiredAt := now.UTC().Format(time.RFC3339Nano)
	count := 0
	for _, row := range rows {
		if row.ExpiresAt == nil || !row.ExpiresAt.Before(now) {
			continue
		}
		if row.State != "active" && row.State != "claimed" {
			continue
		}
		priorState := row.State
		mutated := false
		patchErr := store.PatchLeasePayload(ctx, row.Project, row.ID, func(payload map[string]any) error {
			liveState, _ := payload["state"].(string)
			if liveState != "active" && liveState != "claimed" {
				// Concurrent release/cancel/callback won the race after
				// ListLeasesForExpirySweep ran. Treat the lease as already
				// terminalized and leave the durable state alone.
				return nil
			}
			payload["state"] = "expired"
			payload["expired_at"] = expiredAt
			payload["expiry_reason"] = "stale_at_startup"
			mutated = true
			return nil
		})
		if patchErr != nil {
			if logf != nil {
				logf("expire stale lease patch failed project=%s id=%s prior_state=%s: %v", row.Project, row.ID, priorState, patchErr)
			}
			continue
		}
		if mutated {
			count++
			if logf != nil {
				logf("expired stale lease project=%s id=%s prior_state=%s expires_at=%s", row.Project, row.ID, priorState, row.ExpiresAt.UTC().Format(time.RFC3339Nano))
			}
		}
	}
	return count, nil
}

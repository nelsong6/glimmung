package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
)

// TestReturnInterruptsActivation is the contract test for the cleanup-vs-
// activation race fix. A return arriving mid-activation must cancel the
// in-flight activation goroutine, await its unwind, and then proceed with
// cleanup. Without the cancel-await the in-process activation keeps
// recreating the lease-scoped Playwright Deployment after cleanup deletes
// it, waitForNoPodsInNamespaces times out, and the slot lands in `error`.
//
// The proof: the fake preparer records p.activateCtxCancelled when its
// ActivateTestSlotRuntime returns because ctx fired (not because
// activateRelease was closed). After return, that flag must be true.
func TestReturnInterruptsActivation(t *testing.T) {
	// Real wall-clock so the TTL timer doesn't fire during the test.
	now := time.Now().UTC()
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "tank-operator",
			Name: "tank-operator",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "tank-slot",
				"record_base": "tank.dev.romaine.life",
				"count":       float64(1),
				"slots": []any{
					map[string]any{"slot_index": float64(1), "slot_name": "tank-slot-1", "state": "ready"},
				},
			}},
		}}},
		lease: Lease{
			Project:     "tank-operator",
			LeaseNumber: intPtr(5),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "tank-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  3600,
		},
	}
	preparer := &fakeTestSlotPreparer{
		// No activateRelease close: the only way for the activation
		// goroutine to exit ActivateTestSlotRuntime is ctx cancellation.
		activateStarted: make(chan struct{}, 1),
		activateRelease: make(chan struct{}),
		activateDone:    make(chan struct{}, 1),
		// returnRelease nil → ReturnTestSlotRuntime doesn't block, so the
		// cleanup goroutine can proceed once the activation has unwound.
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	// 1. Checkout fires the activation goroutine.
	checkoutReq := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator","tank_session_id":"99"}`))
	checkoutReq.Header.Set("Authorization", "Bearer admin")
	checkoutRec := httptest.NewRecorder()
	handler.ServeHTTP(checkoutRec, checkoutReq)
	if checkoutRec.Code != http.StatusAccepted {
		t.Fatalf("checkout status=%d body=%s", checkoutRec.Code, checkoutRec.Body.String())
	}

	// 2. Wait until the activation goroutine is inside ActivateTestSlotRuntime.
	select {
	case <-preparer.activateStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("activation did not start")
	}

	// 3. Return arrives mid-activation. The handler must transition the
	//    slot to `cleaning` and spawn the cleanup goroutine; the cleanup
	//    goroutine must cancel the activation ctx and wait for it.
	returnReq := httptest.NewRequest(http.MethodPost, "/v1/test-slots/return", strings.NewReader(`{"project":"tank-operator","slot_index":1}`))
	returnReq.Header.Set("Authorization", "Bearer admin")
	returnRec := httptest.NewRecorder()
	handler.ServeHTTP(returnRec, returnReq)
	if returnRec.Code != http.StatusAccepted {
		t.Fatalf("return status=%d body=%s", returnRec.Code, returnRec.Body.String())
	}

	// 4. The activation goroutine exits because ctx fired, NOT because
	//    activateRelease was closed (we never close it). activateDone
	//    closes from the defer in the fake.
	select {
	case <-preparer.activateDone:
	case <-time.After(2 * time.Second):
		t.Fatal("activation did not unwind after cancel — cleanup raced the in-flight activation goroutine")
	}
	if !preparer.activateCtxCancelled {
		t.Fatal("activation returned without ctx cancellation — the cancel-await wiring is not interrupting the activation goroutine")
	}

	// 5. activateRelease is still open. If anyone closed it, this test
	//    isn't proving cancellation; it's measuring a race against a
	//    fast-path completion.
	select {
	case <-preparer.activateRelease:
		t.Fatal("activateRelease channel was closed — test cannot distinguish cancel-driven exit from natural completion")
	default:
	}

	// 6. Cleanup converges: slot reaches provisioned (the legacy wire-compat
	//    `ready` state via the bridge).
	waitForSlotStatus(t, store, testSlotStateReady)
}

// TestErrorCleaningRetryViaReturn proves the error→cleaning recovery
// transition: a slot whose previous cleanup attempt landed it in `error`
// (with cleanup_error set) recovers when the operator calls returnTestSlot
// again. The retry is idempotent at the K8s layer (the fake's
// ReturnTestSlotRuntime is invoked twice; the second invocation succeeds).
func TestErrorCleaningRetryViaReturn(t *testing.T) {
	// Use real wall-clock so the TTL timer armed at checkout doesn't
	// fire immediately (a fixed past `now` makes `delay = ttl - large`
	// clamp to zero and fire cleanup before the test gets to drive it).
	now := time.Now().UTC()
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "tank-operator",
			Name: "tank-operator",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "tank-slot",
				"record_base": "tank.dev.romaine.life",
				"count":       float64(1),
				"slots": []any{
					map[string]any{"slot_index": float64(1), "slot_name": "tank-slot-1", "state": "ready"},
				},
			}},
		}}},
		lease: Lease{
			Project:     "tank-operator",
			LeaseNumber: intPtr(6),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "tank-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  3600,
		},
	}
	// Activation that succeeds (we close activateRelease after the
	// goroutine starts), then we fail the first cleanup so the slot
	// lands in `error`.
	preparer := &fakeTestSlotPreparer{
		activateStarted: make(chan struct{}, 1),
		activateRelease: make(chan struct{}),
		activateDone:    make(chan struct{}, 1),
		returnStarted:   make(chan struct{}, 16),
		returnDone:      make(chan struct{}, 16),
		returnErr:       errors.New("first cleanup boom — transient K8s API error"),
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	// 1. Checkout → slot reaches running.
	checkoutReq := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator","tank_session_id":"99"}`))
	checkoutReq.Header.Set("Authorization", "Bearer admin")
	checkoutRec := httptest.NewRecorder()
	handler.ServeHTTP(checkoutRec, checkoutReq)
	if checkoutRec.Code != http.StatusAccepted {
		t.Fatalf("checkout status=%d body=%s", checkoutRec.Code, checkoutRec.Body.String())
	}
	select {
	case <-preparer.activateStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("activation did not start")
	}
	close(preparer.activateRelease)
	select {
	case <-preparer.activateDone:
	case <-time.After(2 * time.Second):
		t.Fatal("activation did not finish")
	}
	waitForSlotStatus(t, store, testSlotStateActive) // wire-compat for `running`

	// 2. First return → cleanup runs, returnErr fires, slot lands in error.
	returnReq := httptest.NewRequest(http.MethodPost, "/v1/test-slots/return", strings.NewReader(`{"project":"tank-operator","slot_index":1}`))
	returnReq.Header.Set("Authorization", "Bearer admin")
	returnRec := httptest.NewRecorder()
	handler.ServeHTTP(returnRec, returnReq)
	if returnRec.Code != http.StatusAccepted {
		t.Fatalf("first return status=%d body=%s", returnRec.Code, returnRec.Body.String())
	}
	select {
	case <-preparer.returnStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first cleanup did not start")
	}
	select {
	case <-preparer.returnDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first cleanup did not finish (with error)")
	}
	waitForSlotStatus(t, store, "error")

	// 3. Second return retries cleanup. After my fix, this must succeed
	//    even though the prior state is `error` — the error→cleaning
	//    transition is now valid.
	preparer.returnErr = nil // simulate the transient K8s error clearing
	retryReq := httptest.NewRequest(http.MethodPost, "/v1/test-slots/return", strings.NewReader(`{"project":"tank-operator","slot_index":1}`))
	retryReq.Header.Set("Authorization", "Bearer admin")
	retryRec := httptest.NewRecorder()
	handler.ServeHTTP(retryRec, retryReq)
	if retryRec.Code != http.StatusAccepted {
		t.Fatalf("retry return status=%d body=%s", retryRec.Code, retryRec.Body.String())
	}
	select {
	case <-preparer.returnStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("retry cleanup did not start")
	}
	select {
	case <-preparer.returnDone:
	case <-time.After(2 * time.Second):
		t.Fatal("retry cleanup did not finish")
	}
	waitForSlotStatus(t, store, testSlotStateReady) // wire-compat for `provisioned`
}

// TestRecoverInFlightErrorSlotWithClaimedLease covers the startup recovery
// branch: a slot left in `error` with cleanup_error set AND a claimed
// lease (the prior process died with cleanup mid-flight or the cleanup
// goroutine logged an error and exited). The fresh process's recovery
// sweep must re-fire cleanup against the slot.
func TestRecoverInFlightErrorSlotWithClaimedLease(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-2 * recoveryMinAge)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "recover-err",
			Name: "recover-err",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "recover-err-slot",
				"record_base": "recover-err.dev.romaine.life",
				"count":       float64(1),
			}},
		}}},
		lease: Lease{
			Project:     "recover-err",
			LeaseNumber: intPtr(7),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "recover-err-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  3600,
		},
	}

	// Pre-populate the slot doc directly in the new collection: state=error
	// with cleanup_error set, updated_at stale enough to bypass the
	// recoveryMinAge gate.
	priorCleanupErr := "prior cleanup boom"
	errSlot := Slot{
		Project:      "recover-err",
		SlotIndex:    1,
		SlotName:     "recover-err-slot-1",
		State:        SlotStateError,
		UpdatedAt:    stale,
		CleanupError: &priorCleanupErr,
	}
	if _, err := store.CreateSlot(context.Background(), errSlot); err != nil {
		t.Fatalf("seed error slot: %v", err)
	}

	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
		returnDone:    make(chan struct{}, 1),
	}

	RecoverInFlightTestSlots(context.Background(), store, preparer, nil, nil)

	// Recovery must spawn a cleanup goroutine for the error slot.
	select {
	case <-preparer.returnStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not re-fire cleanup for error slot with claimed lease")
	}
	select {
	case <-preparer.returnDone:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery cleanup did not finish")
	}
	// Cleanup converges, slot returns to provisioned.
	waitForSlotStatus(t, store, testSlotStateReady)
}

// TestRecoverInFlightOrphanErrorSlot is the slot-collection counterpart:
// an orphan error slot (no claimed lease) with cleanup_error set is
// re-cleaned by the recovery sweep's slot-collection pass. This is the
// only path that recovers post-#518-migration orphan-error slots; the
// legacy project-metadata sweep doesn't see them.
func TestRecoverInFlightOrphanErrorSlot(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-2 * recoveryMinAge)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "orphan-err",
			Name: "orphan-err",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "orphan-err-slot",
				"record_base": "orphan-err.dev.romaine.life",
				"count":       float64(1),
			}},
		}}},
		// No claimed test-slot lease.
	}

	priorCleanupErr := "prior cleanup boom (orphan)"
	errSlot := Slot{
		Project:      "orphan-err",
		SlotIndex:    1,
		SlotName:     "orphan-err-slot-1",
		State:        SlotStateError,
		UpdatedAt:    stale,
		CleanupError: &priorCleanupErr,
	}
	if _, err := store.CreateSlot(context.Background(), errSlot); err != nil {
		t.Fatalf("seed orphan error slot: %v", err)
	}

	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
		returnDone:    make(chan struct{}, 1),
	}

	RecoverInFlightTestSlots(context.Background(), store, preparer, nil, nil)

	select {
	case <-preparer.returnStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not re-fire cleanup for orphan error slot")
	}
	select {
	case <-preparer.returnDone:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery cleanup did not finish")
	}
	waitForSlotStatus(t, store, testSlotStateReady)
}

// TestCancelInflightActivationNoOpOnEmptyKey is a guard that
// cancelInflightActivation is safe to call when no activation goroutine
// is in flight. Cleanup paths shouldn't need to pre-check the map; the
// helper must handle the absent case.
func TestCancelInflightActivationNoOpOnEmptyKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if cancelInflightActivation(ctx, "no-such-key", activationCancelReturn) {
		t.Fatal("cancelInflightActivation reported a cancel for an absent key")
	}
}

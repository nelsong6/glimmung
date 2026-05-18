package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
	"github.com/nelsong6/glimmung/internal/metrics"
)

type fakeTestSlotPreparer struct {
	preliminaries         bool
	activated             bool
	activateCtxCancelled  bool
	returned              bool
	installerCleaned      bool
	cleanedSlots          []string
	project               Project
	deprovisioned         []string
	deprovisionedSessions []string
	preliminariesErr      error
	activateErr           error
	returnErr             error
	activateStarted       chan struct{}
	activateRelease       chan struct{}
	activateDone          chan struct{}
	returnStarted         chan struct{}
	returnRelease         chan struct{}
	returnDone            chan struct{}
}

func (s *fakeLeaseStore) AppendTestSlotHotSwapHistory(_ context.Context, _ string, _ string, entry TestSlotHotSwapHistoryEntry) (Lease, error) {
	if s.err != nil {
		return Lease{}, s.err
	}
	if len(s.leases) > 0 {
		if s.leases[0].Metadata == nil {
			s.leases[0].Metadata = map[string]any{}
		}
		s.leases[0].Metadata["last_hot_swap_status"] = entry.Status
		return s.leases[0], nil
	}
	return s.lease, nil
}

func (p *fakeTestSlotPreparer) EnsureTestSlotPreliminaries(_ context.Context, _ Lease, project Project) error {
	p.preliminaries = true
	p.project = project
	return p.preliminariesErr
}

func (p *fakeTestSlotPreparer) ActivateTestSlotRuntime(ctx context.Context, _ Lease, project Project, _ NativeGitHubTokenMinter) error {
	p.activated = true
	p.project = project
	signalTestChannel(p.activateStarted)
	defer signalTestChannel(p.activateDone)
	// Honor ctx.Done() alongside activateRelease so cancellation tests
	// can verify the activation goroutine actually unwinds on cancel.
	// Existing tests that close activateRelease before any cancel still
	// take the activateRelease branch.
	if p.activateRelease != nil {
		select {
		case <-p.activateRelease:
		case <-ctx.Done():
			p.activateCtxCancelled = true
			return ctx.Err()
		}
	}
	return p.activateErr
}

func (p *fakeTestSlotPreparer) LaunchNativePhase(context.Context, NativeLaunchRequest) ([]string, error) {
	return nil, nil
}

func (p *fakeTestSlotPreparer) ReturnTestSlotRuntime(context.Context, Lease, Project) error {
	p.returned = true
	signalTestChannel(p.returnStarted)
	if p.returnRelease != nil {
		<-p.returnRelease
	}
	defer signalTestChannel(p.returnDone)
	return p.returnErr
}

func (p *fakeTestSlotPreparer) CleanupTestSlotInstaller(_ context.Context, lease Lease, _ Project) error {
	p.installerCleaned = true
	if slotName, _ := stringFromMap(lease.Metadata, "native_slot_name"); strings.TrimSpace(slotName) != "" {
		p.cleanedSlots = append(p.cleanedSlots, strings.TrimSpace(slotName))
	}
	return nil
}

func (p *fakeTestSlotPreparer) DeprovisionTestSlot(_ context.Context, lease Lease, _ Project) error {
	if slotName, _ := stringFromMap(lease.Metadata, "native_slot_name"); strings.TrimSpace(slotName) != "" {
		p.deprovisioned = append(p.deprovisioned, strings.TrimSpace(slotName))
	}
	if namespace, _ := stringFromMap(lease.Metadata, "native_sessions_namespace"); strings.TrimSpace(namespace) != "" {
		p.deprovisionedSessions = append(p.deprovisionedSessions, strings.TrimSpace(namespace))
	}
	return nil
}

func signalTestChannel(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func TestCheckoutTestSlotStartsAsyncActivation(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
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
			LeaseNumber: intPtr(2),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "tank-slot-1",
			},
			RequestedAt: now,
		},
	}
	preparer := &fakeTestSlotPreparer{
		activateStarted: make(chan struct{}, 1),
		activateRelease: make(chan struct{}),
		activateDone:    make(chan struct{}, 1),
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator","tank_session_id":"98"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if preparer.preliminaries {
		t.Fatal("checkout activation should not call the warmup path through the API handler")
	}
	if len(store.slotStatuses) != 1 || store.slotStatuses[0].State != testSlotStateActivating {
		t.Fatalf("slot statuses=%#v, want activating", store.slotStatuses)
	}
	if store.slotStatuses[0].ActivationAttempt == nil || *store.slotStatuses[0].ActivationAttempt != 2 {
		t.Fatalf("activation attempt=%v, want 2", store.slotStatuses[0].ActivationAttempt)
	}
	if store.slotStatuses[0].ActivationState == nil || *store.slotStatuses[0].ActivationState != testSlotStateActivating {
		t.Fatalf("activation state=%v, want activating", store.slotStatuses[0].ActivationState)
	}
	if store.slotStatuses[0].ActivationStartedAt == nil || store.slotStatuses[0].ActivationJobName == nil {
		t.Fatalf("activation metadata missing: %#v", store.slotStatuses[0])
	}
	if len(store.leaseReq.Metadata) != 1 || !boolFromMap(store.leaseReq.Metadata, "test_slot_checkout") {
		t.Fatalf("checkout metadata should not include mode: %#v", store.leaseReq.Metadata)
	}
	if store.leaseReq.TTLSeconds == nil || *store.leaseReq.TTLSeconds != testSlotDefaultTTLSeconds {
		t.Fatalf("default TTL not applied: ttl=%v, want %d", store.leaseReq.TTLSeconds, testSlotDefaultTTLSeconds)
	}
	for _, want := range []string{`"state":"activating"`, `"usable":false`, `"slot_name":"tank-slot-1"`, `"url":"https://tank-slot-1.tank.dev.romaine.life/"`, `"host":"native-k8s"`, `"status_url":"/v1/projects/tank-operator/test-environments/tank-slot-1"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("response missing %s: %s", want, rec.Body.String())
		}
	}
	select {
	case <-preparer.activateStarted:
	case <-time.After(time.Second):
		t.Fatal("background activation did not start")
	}
	close(preparer.activateRelease)
	select {
	case <-preparer.activateDone:
	case <-time.After(time.Second):
		t.Fatal("background activation did not finish")
	}
	waitForSlotStatus(t, store, testSlotStateActive)
	waitForInstallerCleanup(t, preparer)
	finalStatus := store.slotStatuses[len(store.slotStatuses)-1]
	if finalStatus.ActivationState == nil || *finalStatus.ActivationState != testSlotStateActive {
		t.Fatalf("final activation state=%v, want active", finalStatus.ActivationState)
	}
	if finalStatus.ActivationCompletedAt == nil {
		t.Fatalf("activation completion missing: %#v", finalStatus)
	}
}

func TestCheckoutTestSlotExposesPlaywrightWSEndpointWhenEnabled(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
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
			LeaseNumber: intPtr(3),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "tank-slot-1",
			},
			RequestedAt: now,
		},
	}
	preparer := &fakeTestSlotPreparer{
		activateStarted: make(chan struct{}, 1),
		activateRelease: make(chan struct{}, 1),
		activateDone:    make(chan struct{}, 1),
	}
	close(preparer.activateRelease)
	settings := Settings{
		NativeRunnerPlaywrightEnabled: true,
		NativeRunnerPlaywrightPort:    "3000",
		NativeRunnerPlaywrightImage:   "playwright:latest",
	}
	handler := newHandler(settings, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator","tank_session_id":"42"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := `"playwright_ws_endpoint":"ws://slot-playwright.tank-slot-1.svc.cluster.local:3000"`
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("checkout response missing %s: %s", want, rec.Body.String())
	}
}

func TestCheckoutTestSlotOmitsPlaywrightWSEndpointWhenDisabled(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
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
			LeaseNumber: intPtr(4),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "tank-slot-1",
			},
			RequestedAt: now,
		},
	}
	preparer := &fakeTestSlotPreparer{
		activateStarted: make(chan struct{}, 1),
		activateRelease: make(chan struct{}, 1),
		activateDone:    make(chan struct{}, 1),
	}
	close(preparer.activateRelease)
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator","tank_session_id":"42"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"playwright_ws_endpoint"`) {
		t.Fatalf("checkout response should omit playwright_ws_endpoint when disabled: %s", rec.Body.String())
	}
}

func TestCheckoutTestSlotHonorsExplicitTTL(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
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
			LeaseNumber: intPtr(3),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "tank-slot-1",
			},
			RequestedAt: now,
		},
	}
	preparer := &fakeTestSlotPreparer{
		activateStarted: make(chan struct{}, 1),
		activateRelease: make(chan struct{}),
		activateDone:    make(chan struct{}, 1),
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator","tank_session_id":"99","ttl_seconds":120}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.leaseReq.TTLSeconds == nil || *store.leaseReq.TTLSeconds != 120 {
		t.Fatalf("explicit ttl ignored: ttl=%v, want 120", store.leaseReq.TTLSeconds)
	}
	// Let the spawned activation drain so the goroutine doesn't leak across tests.
	select {
	case <-preparer.activateStarted:
	case <-time.After(time.Second):
		t.Fatal("background activation did not start")
	}
	close(preparer.activateRelease)
	select {
	case <-preparer.activateDone:
	case <-time.After(time.Second):
		t.Fatal("background activation did not finish")
	}
}

// The block of tests that follows exercises the event-driven test-slot
// lifecycle: no polling reconciler, per-lease AfterFunc timers for TTL, and
// a one-shot RecoverInFlightTestSlots sweep at process boot.

func TestRecoverInFlightTestSlotsResumesActivation(t *testing.T) {
	// Pod restart finds a claimed lease whose slot is mid-activation. The
	// startup sweep must spawn a fresh activation goroutine. The slot
	// status's updated_at must be older than recoveryMinAge — recent
	// in-flight states are skipped under the assumption another live pod
	// is still working on it (rolling-update overlap case).
	now := time.Now().UTC()
	stale := now.Add(-2 * recoveryMinAge)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "recover",
			Name: "recover",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "recover-slot",
				"record_base": "recover.dev.romaine.life",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "recover-slot-1",
						"state":      testSlotStateActivating,
						"updated_at": stale.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		lease: Lease{
			Project:     "recover",
			LeaseNumber: intPtr(4),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "recover-slot-1",
			},
			RequestedAt: now,
		},
	}
	preparer := &fakeTestSlotPreparer{
		activateStarted: make(chan struct{}, 1),
		activateRelease: make(chan struct{}),
		activateDone:    make(chan struct{}, 1),
	}

	RecoverInFlightTestSlots(context.Background(), store, preparer, nil, nil)
	select {
	case <-preparer.activateStarted:
	case <-time.After(time.Second):
		t.Fatal("recovered activation did not start")
	}
	close(preparer.activateRelease)
	select {
	case <-preparer.activateDone:
	case <-time.After(time.Second):
		t.Fatal("recovered activation did not finish")
	}
	waitForSlotStatus(t, store, testSlotStateActive)
}

func TestRecoverInFlightTestSlotsResumesCleanup(t *testing.T) {
	// Stale `cleaning` (older than recoveryMinAge) — recent in-flight
	// states are skipped to avoid racing a live pod that's still cleaning.
	now := time.Now().UTC()
	stale := now.Add(-2 * recoveryMinAge)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "recover",
			Name: "recover",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "recover-slot",
				"record_base": "recover.dev.romaine.life",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "recover-slot-1",
						"state":      testSlotStateCleaning,
						"updated_at": stale.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		lease: Lease{
			Project:     "recover",
			LeaseNumber: intPtr(4),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "recover-slot-1",
			},
			RequestedAt: now,
		},
	}
	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
		returnRelease: make(chan struct{}),
		returnDone:    make(chan struct{}, 1),
	}

	RecoverInFlightTestSlots(context.Background(), store, preparer, nil, nil)
	select {
	case <-preparer.returnStarted:
	case <-time.After(time.Second):
		t.Fatal("recovered cleanup did not start")
	}
	close(preparer.returnRelease)
	select {
	case <-preparer.returnDone:
	case <-time.After(time.Second):
		t.Fatal("recovered cleanup did not finish")
	}
	waitForSlotStatus(t, store, testSlotStateReady)
	if store.cancelledRef != "recover-slot-1" {
		t.Fatalf("cancelledRef=%q, want recover-slot-1", store.cancelledRef)
	}
}

func TestRecoverInFlightTestSlotsCleansSlotWithoutLease(t *testing.T) {
	// Cleanup was recorded but the lease was already released and the
	// goroutine died before finishing. Startup must drive cleanup to
	// completion with releaseLease=false (no lease left to cancel).
	// updated_at is stale so the recovery sweep doesn't assume another
	// live pod is still working on it.
	now := time.Now().UTC()
	stale := now.Add(-2 * recoveryMinAge)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "recover",
			Name: "recover",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "recover-slot",
				"record_base": "recover.dev.romaine.life",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "recover-slot-1",
						"state":      testSlotStateCleaning,
						"updated_at": stale.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		leases: []Lease{},
	}
	preparer := &fakeTestSlotPreparer{}

	RecoverInFlightTestSlots(context.Background(), store, preparer, nil, nil)
	waitForSlotStatus(t, store, testSlotStateReady)
	if store.cancelledRef != "" {
		t.Fatalf("cancelledRef=%q, want empty", store.cancelledRef)
	}
}

func TestLeaseExpiryTimerFiresCleanup(t *testing.T) {
	// Arm a timer with a 0 TTL so it fires immediately. The cleanup pathway
	// must record the lease-expiry source and start the cleanup goroutine —
	// the same one return / callback-release uses. This is the event-driven
	// replacement for the polling-reconciler "did this lease expire yet"
	// check that used to run every 15 seconds for every claimed lease.
	now := time.Now().UTC().Add(-time.Hour) // assigned in the past
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "expire",
			Name: "expire",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "expire-slot",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "expire-slot-1",
						"state":      testSlotStateActive,
						"updated_at": now.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		lease: Lease{
			Project:     "expire",
			LeaseNumber: intPtr(7),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "expire-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  1, // already exceeded by an hour
		},
	}
	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
		returnRelease: make(chan struct{}),
		returnDone:    make(chan struct{}, 1),
	}

	armLeaseExpiryTimer(store, preparer, store.projects[0], store.lease, nil)

	select {
	case <-preparer.returnStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expiry timer did not fire cleanup")
	}
	close(preparer.returnRelease)
	select {
	case <-preparer.returnDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
	waitForSlotStatus(t, store, testSlotStateReady)
	if store.cancelledRef != "expire-slot-1" {
		t.Fatalf("cancelledRef=%q, want expire-slot-1", store.cancelledRef)
	}
	// The cleanup pathway records the trigger; the lease-ttl-expiry source
	// distinguishes timer-driven expiry from operator return.
	snapshot := store.snapshotSlotStatuses()
	if len(snapshot) == 0 {
		t.Fatal("no slot statuses recorded")
	}
	first := snapshot[0]
	if len(first.ReturnHistory) != 1 || first.ReturnHistory[0].Source != "lease.ttl_expiry" {
		t.Fatalf("return history=%#v, want source lease.ttl_expiry", first.ReturnHistory)
	}
}

func TestLeaseExpiryTimerCancelPreventsFire(t *testing.T) {
	// Arm a 300ms timer, cancel immediately, wait long enough for the
	// original deadline to elapse. The cleanup goroutine must not run.
	now := time.Now().UTC()
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "expire",
			Name: "expire",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "expire-slot",
				"count":       float64(1),
			}},
		}}},
		lease: Lease{
			Project:     "expire",
			LeaseNumber: intPtr(7),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_slot_index":  "1",
				"native_slot_name":   "expire-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  1, // ~1 second deadline
		},
	}
	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
	}

	armLeaseExpiryTimer(store, preparer, store.projects[0], store.lease, nil)
	cancelLeaseExpiryTimer(LeasePublicRefFromLease(store.lease))

	select {
	case <-preparer.returnStarted:
		t.Fatal("expiry fired after cancel")
	case <-time.After(1500 * time.Millisecond):
		// expected: nothing fires
	}
}

func TestClaimTestSlotCleanupDedupsOnEtagConflict(t *testing.T) {
	// Simulates two replicas' TTL timers firing for the same lease at the
	// same wall-clock instant. Both pods read the project doc at the same
	// etag, both compute the cleaning-state mutation, both attempt the
	// etag-conditional write. Exactly one wins; the other gets
	// ErrPreconditionFailed back. The database is the synchronization
	// point — no in-process coordination, no leader election.
	now := time.Now().UTC()
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "race",
			Name: "race",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "race-slot",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "race-slot-1",
						"state":      testSlotStateActive,
						"updated_at": now.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		lease: Lease{
			Project:     "race",
			LeaseNumber: intPtr(42),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_slot_index":  "1",
				"native_slot_name":   "race-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  3600,
		},
	}

	type result struct {
		err error
	}
	results := make(chan result, 2)
	var ready sync.WaitGroup
	var start sync.WaitGroup
	ready.Add(2)
	start.Add(1)
	for i := 0; i < 2; i++ {
		go func() {
			ready.Done()
			start.Wait()
			_, err := claimTestSlotCleanup(context.Background(), store, store.projects[0], store.lease, testSlotReturnAudit{Source: "lease.ttl_expiry"})
			results <- result{err: err}
		}()
	}
	ready.Wait()
	start.Done()

	first := <-results
	second := <-results

	winners := 0
	losers := 0
	for _, r := range []result{first, second} {
		switch {
		case r.err == nil:
			winners++
		case errors.Is(r.err, ErrPreconditionFailed):
			losers++
		default:
			t.Fatalf("unexpected error: %v", r.err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("winners=%d losers=%d, want 1/1", winners, losers)
	}
	// Exactly one cleaning-state status write should be recorded — the loser
	// returned before writing.
	cleaningWrites := 0
	for _, status := range store.snapshotSlotStatuses() {
		if status.State == testSlotStateCleaning {
			cleaningWrites++
		}
	}
	if cleaningWrites != 1 {
		t.Fatalf("cleaning writes=%d, want 1 (loser must not have written)", cleaningWrites)
	}
}

// TestClaimTestSlotWarmupDedupsConcurrentSameSlot was retired with the
// slot-storage rework — the function it tested (claimTestSlotWarmup)
// existed only to CAS the unseeded→provisioning transition against the
// shared project doc. Per-row CAS in SlotStore replaces it; concurrent
// `markSlotProvisioning` calls against the same slot doc still
// produce exactly one winner via Cosmos etag, which is exercised by
// TestLiveCosmosSlotUpdateIfMatchSurfacesPreconditionFailed in the live
// smoke suite.

func TestClaimTestSlotWarmupRetriesAcrossCrossSlotWrites(t *testing.T) {
	// Cross-slot writes bump the project doc's etag without affecting our
	// slot's state. Our claim's CAS will hit 412 the first time, retry,
	// re-check state, and succeed. This is what makes PATCH count for
	// count>1 work — multiple warmup goroutines firing simultaneously
	// against the same project doc.
	//
	// Count of 10 deliberately exceeds the previous (broken) retry
	// budget of 5 attempts so this test would fail before the fix: under
	// the old code at least one slot was statistically likely to lose 5
	// CAS races in a row to its 9 siblings and silently give up. That
	// happened in production right after PR #516 — slot 8 went missing
	// from the tank-operator project doc despite no error path being
	// triggered.
	const count = 10
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "multi",
			Name: "multi",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "multi-slot",
				"count":       float64(count),
			}},
		}}},
	}
	preparer := &fakeTestSlotPreparer{}

	EnsureProjectTestSlotsWarmed(context.Background(), store, preparer, store.projects[0], nil, nil)

	waitForSlotStatusCount(t, store, count*2) // count slots × (warming + ready)
	seen := map[int]string{}
	for _, status := range store.snapshotSlotStatuses() {
		seen[status.SlotIndex] = status.State
	}
	for i := 1; i <= count; i++ {
		if seen[i] != testSlotStateReady {
			t.Fatalf("slot %d final state=%q, want ready (cross-slot CAS contention must not strand a slot under retry exhaustion)", i, seen[i])
		}
	}
}

func TestFireLeaseExpiryNoOpsWhenAnotherReplicaAlreadyClaimed(t *testing.T) {
	// Pre-claim the slot for cleanup (simulating "another replica's timer
	// fired first"). Then fire a stale timer. fireLeaseExpiry must see the
	// CAS conflict and return without spawning a cleanup goroutine.
	now := time.Now().UTC().Add(-time.Hour)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "race",
			Name: "race",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "race-slot",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "race-slot-1",
						"state":      testSlotStateActive,
						"updated_at": now.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		lease: Lease{
			Project:     "race",
			LeaseNumber: intPtr(42),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_slot_index":  "1",
				"native_slot_name":   "race-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  1,
		},
	}

	// First claim wins.
	if _, err := claimTestSlotCleanup(context.Background(), store, store.projects[0], store.lease, testSlotReturnAudit{Source: "lease.ttl_expiry"}); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
	}
	// fireLeaseExpiry must see the claim is already taken and return
	// without spawning preparer.ReturnTestSlotRuntime.
	fireLeaseExpiry(store, preparer, store.projects[0], store.lease, nil)

	select {
	case <-preparer.returnStarted:
		t.Fatal("fireLeaseExpiry should not start cleanup when another replica won the claim")
	case <-time.After(300 * time.Millisecond):
		// expected
	}
	// The single cleaning state write is from the first claim, not from
	// fireLeaseExpiry's lost race.
	cleaningWrites := 0
	for _, status := range store.snapshotSlotStatuses() {
		if status.State == testSlotStateCleaning {
			cleaningWrites++
		}
	}
	if cleaningWrites != 1 {
		t.Fatalf("cleaning writes=%d, want 1 (fireLeaseExpiry must not have written after losing CAS)", cleaningWrites)
	}
}

func TestRecoverInFlightTestSlotsArmsTimerForClaimedLease(t *testing.T) {
	// On boot the in-memory timer map is empty. The startup sweep must
	// re-arm a timer for any still-claimed test-slot lease so TTL
	// enforcement survives process restarts.
	now := time.Now().UTC().Add(-time.Hour)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "expire",
			Name: "expire",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "expire-slot",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "expire-slot-1",
						"state":      testSlotStateActive,
						"updated_at": now.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		lease: Lease{
			Project:     "expire",
			LeaseNumber: intPtr(7),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_slot_index":  "1",
				"native_slot_name":   "expire-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  1, // deadline already passed an hour ago
		},
	}
	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
		returnRelease: make(chan struct{}),
		returnDone:    make(chan struct{}, 1),
	}

	RecoverInFlightTestSlots(context.Background(), store, preparer, nil, nil)

	// The re-armed timer fires immediately because the deadline is in the
	// past. The cleanup pathway is the same one operator returns trigger.
	select {
	case <-preparer.returnStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("re-armed timer did not fire cleanup after recovery")
	}
	close(preparer.returnRelease)
	<-preparer.returnDone
}

func TestRecoverInFlightTestSlotsCleansInstallerForActiveSlot(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "active",
			Name: "active",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "active-slot",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "active-slot-1",
						"state":      testSlotStateActive,
						"updated_at": now.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		lease: Lease{
			Project:     "active",
			LeaseNumber: intPtr(8),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_slot_index":  "1",
				"native_slot_name":   "active-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  3600,
		},
	}
	preparer := &fakeTestSlotPreparer{}

	RecoverInFlightTestSlots(context.Background(), store, preparer, nil, nil)
	// Stop the re-armed timer so it doesn't fire after the test returns
	// (would race with other tests' assertions about cleanup state).
	defer cancelLeaseExpiryTimer(LeasePublicRefFromLease(store.lease))

	if !preparer.installerCleaned {
		t.Fatal("expected installer cleanup for active slot during recovery")
	}
}

func TestRecoverInFlightTestSlotsWarmsMissingSlots(t *testing.T) {
	// Project has count=3 but no slots[*] entries — exactly the state
	// tank-operator landed in when warmup was a synchronous PATCH side
	// effect. The startup sweep must seed all three indices.
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "seed",
			Name: "seed",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "seed-slot",
				"record_base": "seed.dev.romaine.life",
				"count":       float64(3),
			}},
		}}},
		leases: []Lease{},
	}
	preparer := &fakeTestSlotPreparer{}

	RecoverInFlightTestSlots(context.Background(), store, preparer, nil, nil)

	waitForSlotStatusCount(t, store, 6) // 3 slots × (warming + ready)
	seen := map[int]string{}
	for _, status := range store.snapshotSlotStatuses() {
		seen[status.SlotIndex] = status.State
	}
	for i := 1; i <= 3; i++ {
		if seen[i] != testSlotStateReady {
			t.Fatalf("slot %d final state=%q, want ready", i, seen[i])
		}
	}
	if !preparer.preliminaries {
		t.Fatal("expected EnsureTestSlotPreliminaries to run for seeded slots")
	}
}

func TestRecoverInFlightTestSlotsResumesStaleWarming(t *testing.T) {
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "stale",
			Name: "stale",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "stale-slot",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "stale-slot-1",
						"state":      testSlotStateWarming,
						"updated_at": time.Now().UTC().Add(-2 * recoveryMinAge).Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		leases: []Lease{},
	}
	preparer := &fakeTestSlotPreparer{}

	RecoverInFlightTestSlots(context.Background(), store, preparer, nil, nil)
	waitForSlotStatus(t, store, testSlotStateReady)
	if !preparer.preliminaries {
		t.Fatal("expected EnsureTestSlotPreliminaries to run for stale warming slot")
	}
}

func TestRecoverInFlightTestSlotsSkipsClaimedSlot(t *testing.T) {
	// A claimed lease drives its own lifecycle (activation, cleaning, or
	// installer cleanup once active). The recovery sweep must not fire a
	// fresh warmup against a slot that's already busy.
	now := time.Now().UTC()
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "claim",
			Name: "claim",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "claim-slot",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "claim-slot-1",
						"state":      testSlotStateActive,
						"updated_at": now.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		lease: Lease{
			Project:     "claim",
			LeaseNumber: intPtr(11),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_slot_index":  "1",
				"native_slot_name":   "claim-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  3600,
		},
	}
	preparer := &fakeTestSlotPreparer{}

	RecoverInFlightTestSlots(context.Background(), store, preparer, nil, nil)
	defer cancelLeaseExpiryTimer(LeasePublicRefFromLease(store.lease))

	// installer cleanup is allowed (single-shot); warmup is not.
	if preparer.preliminaries {
		t.Fatal("recovery must not run preliminary warmup against a claimed slot")
	}
}

func waitForSlotStatusCount(t *testing.T, store *fakeLeaseStore, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = len(store.snapshotSlotStatuses())
		if got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("slot status writes=%d, want >=%d", got, want)
}

func TestAsyncCheckoutFailureMarksErrorAndReleasesLease(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
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
		},
	}
	preparer := &fakeTestSlotPreparer{
		activateErr:  errors.New("render/apply failed"),
		activateDone: make(chan struct{}, 1),
		returnDone:   make(chan struct{}, 1),
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case <-preparer.activateDone:
	case <-time.After(time.Second):
		t.Fatal("background activation did not finish")
	}
	waitForSlotStatus(t, store, "error")
	select {
	case <-preparer.returnDone:
	case <-time.After(time.Second):
		t.Fatal("failed activation cleanup did not finish")
	}
	finalStatus := store.slotStatuses[len(store.slotStatuses)-1]
	if finalStatus.ActivationState == nil || *finalStatus.ActivationState != "error" {
		t.Fatalf("activation state=%v, want error", finalStatus.ActivationState)
	}
	if finalStatus.ActivationError == nil || !strings.Contains(*finalStatus.ActivationError, "render/apply failed") {
		t.Fatalf("activation error=%v, want render/apply failed", finalStatus.ActivationError)
	}
	waitForFailedActivationCleanup(t, store, preparer)
	if store.cancelledRef != "tank-slot-1" {
		t.Fatalf("cancelledRef=%q, want tank-slot-1", store.cancelledRef)
	}
}

func waitForSlotStatus(t *testing.T, store *fakeLeaseStore, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var snapshot []TestEnvironmentSlotStatus
	for time.Now().Before(deadline) {
		snapshot = store.snapshotSlotStatuses()
		if len(snapshot) > 0 && snapshot[len(snapshot)-1].State == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(snapshot) == 0 {
		t.Fatalf("slot statuses empty, want %s", state)
	}
	t.Fatalf("final slot status=%q, want %s", snapshot[len(snapshot)-1].State, state)
}

func waitForInstallerCleanup(t *testing.T, preparer *fakeTestSlotPreparer) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if preparer.installerCleaned {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("installer cleanup did not run")
}

func waitForFailedActivationCleanup(t *testing.T, store *fakeLeaseStore, preparer *fakeTestSlotPreparer) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if preparer.returned && store.cancelledRef != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("failed activation cleanup did not run")
}

func TestCheckoutTestSlotRejectsModeField(t *testing.T) {
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{ID: "tank-operator", Name: "tank-operator"}}},
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator","mode":"delete"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "mode") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCheckoutTestSlotRejectsSlotIndexField(t *testing.T) {
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{ID: "tank-operator", Name: "tank-operator"}}},
	}
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator","slot_index":1}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if preparer.activated || preparer.preliminaries {
		t.Fatal("slot preparer should not run for caller-selected slots")
	}
	if !strings.Contains(rec.Body.String(), "slot_index") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCheckoutTestSlotRejectsPhaseInputsField(t *testing.T) {
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{ID: "tank-operator", Name: "tank-operator"}}},
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator","phase_inputs":{"validation_slot_index":"1"}}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "phase_inputs") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCheckoutTestSlotMapsUnavailable(t *testing.T) {
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{ID: "tank-operator", Name: "tank-operator"}}},
		err:           ErrUnavailable,
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil)

	// Snapshot the counter before the request so the test is order-
	// independent: any prior test that hit the same handler under
	// saturation would already have moved the counter. The metric is
	// scraped via the /metrics handler to keep the test free of
	// metrics-package internals.
	const labelLine = `glimmung_unavailable_total{reason="test_slot_saturation",route="POST /v1/test-slots/checkout"}`
	before := scrapeMetricSample(t, labelLine)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no ready") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	after := scrapeMetricSample(t, labelLine)
	if after-before != 1 {
		t.Fatalf("%s delta=%v, want 1", labelLine, after-before)
	}
}

// scrapeMetricSample fetches /metrics and returns the value of the
// sample line that starts with prefix (the metric name + label set).
// Returns 0 if the line is absent — Prometheus omits metric families
// with no samples, so an unmoved counter looks the same as a never-
// observed one. The Helper marker makes failure backtraces point at
// the caller.
func scrapeMetricSample(t *testing.T, prefix string) float64 {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d", rec.Code)
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, prefix+" ") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			t.Fatalf("malformed metric line: %q", line)
		}
		v, err := strconv.ParseFloat(parts[len(parts)-1], 64)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		return v
	}
	return 0
}

func TestReturnTestSlotReleasesLease(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{ID: "tank-operator", Name: "tank-operator"}}},
		leases: []Lease{{
			Project:     "tank-operator",
			LeaseNumber: intPtr(2),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_slot_index":  "1",
				"native_slot_name":   "tank-slot-1",
			},
			RequestedAt: now,
		}},
		result: CancelLeaseResult{State: "no_active_run", LeaseRef: "tank-slot-1"},
	}
	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
		returnRelease: make(chan struct{}),
		returnDone:    make(chan struct{}, 1),
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/return", strings.NewReader(`{"project":"tank-operator","slot_name":"tank-slot-1","caller_pod_ip":"10.244.1.166","caller_session_id":"14","source":"mcp-glimmung.return_test_slot","reason":"done"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state":"cleaning"`) || !strings.Contains(rec.Body.String(), `"cleanup_started":true`) {
		t.Fatalf("response=%s", rec.Body.String())
	}
	if len(store.slotStatuses) != 1 || store.slotStatuses[0].State != testSlotStateCleaning {
		t.Fatalf("slot statuses=%#v, want cleaning", store.slotStatuses)
	}
	if len(store.slotStatuses[0].ReturnHistory) != 1 {
		t.Fatalf("return history=%#v, want one entry", store.slotStatuses[0].ReturnHistory)
	}
	history := store.slotStatuses[0].ReturnHistory[0]
	if history.Source != "mcp-glimmung.return_test_slot" || history.CallerPodIP == nil || *history.CallerPodIP != "10.244.1.166" {
		t.Fatalf("return history=%#v", history)
	}
	if history.CallerSessionID == nil || *history.CallerSessionID != "14" || history.Reason == nil || *history.Reason != "done" {
		t.Fatalf("return caller/reason history=%#v", history)
	}
	if history.LeaseNumber == nil || *history.LeaseNumber != 2 || history.LeaseRequester != nil {
		t.Fatalf("return lease history=%#v", history)
	}
	if store.slotStatuses[0].CleanupState == nil || *store.slotStatuses[0].CleanupState != testSlotStateCleaning {
		t.Fatalf("cleanup state=%v, want cleaning", store.slotStatuses[0].CleanupState)
	}
	select {
	case <-preparer.returnStarted:
	case <-time.After(time.Second):
		t.Fatal("background cleanup did not start")
	}
	close(preparer.returnRelease)
	select {
	case <-preparer.returnDone:
	case <-time.After(time.Second):
		t.Fatal("background cleanup did not finish")
	}
	waitForSlotStatus(t, store, testSlotStateReady)
	if store.cancelledRef != "tank-slot-1" {
		t.Fatalf("cancelledRef=%q, want tank-slot-1", store.cancelledRef)
	}
	finalStatus := store.slotStatuses[len(store.slotStatuses)-1]
	if finalStatus.CleanupState == nil || *finalStatus.CleanupState != testSlotStateReady {
		t.Fatalf("cleanup state=%v, want ready", finalStatus.CleanupState)
	}
	if finalStatus.CleanupCompletedAt == nil {
		t.Fatalf("cleanup completion missing: %#v", finalStatus)
	}
}

func TestSetLeaseSlotCleanupFinishedClearsActivationFieldsOnSuccess(t *testing.T) {
	// Successful cleanup returns the slot to the pool. The previous lease's
	// activation_* fields are now historical and must not linger — leaving
	// them populated forces every consumer (dashboard, mcp-glimmung,
	// operators reading the doc) to encode "is this still meaningful?"
	// judgment in the rendering layer. The audit trail lives in
	// ReturnHistory; the activation_* fields describe current state only.
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	attempt := 77
	state := testSlotStateActive
	jobName := "glim-slot-apply-tank-slot-1-77"
	startedAt := now.Add(-time.Hour)
	completedAt := now.Add(-time.Hour + 19*time.Second)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "tank",
			Name: "tank",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "tank-slot",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index":              float64(1),
						"slot_name":               "tank-slot-1",
						"state":                   testSlotStateActive,
						"updated_at":              now.Format(time.RFC3339Nano),
						"activation_attempt":      float64(attempt),
						"activation_state":        state,
						"activation_started_at":   startedAt.Format(time.RFC3339Nano),
						"activation_completed_at": completedAt.Format(time.RFC3339Nano),
						"activation_job_name":     jobName,
					},
				},
			}},
		}}},
	}
	lease := Lease{
		Project:     "tank",
		LeaseNumber: intPtr(77),
		State:       "claimed",
		Metadata: map[string]any{
			"test_slot_checkout": true,
			"native_slot_index":  "1",
			"native_slot_name":   "tank-slot-1",
		},
		RequestedAt: now,
	}

	if _, err := setLeaseSlotCleanupFinished(context.Background(), store, store.projects[0], lease, testSlotStateReady, nil); err != nil {
		t.Fatalf("cleanup finished: %v", err)
	}
	snap := store.snapshotSlotStatuses()
	if len(snap) == 0 {
		t.Fatal("no slot status written")
	}
	final := snap[len(snap)-1]
	if final.State != testSlotStateReady {
		t.Fatalf("state=%q, want ready", final.State)
	}
	if final.ActivationAttempt != nil {
		t.Errorf("ActivationAttempt=%v, want nil after clean return", *final.ActivationAttempt)
	}
	if final.ActivationState != nil {
		t.Errorf("ActivationState=%q, want nil after clean return", *final.ActivationState)
	}
	if final.ActivationStartedAt != nil {
		t.Errorf("ActivationStartedAt=%v, want nil after clean return", final.ActivationStartedAt)
	}
	if final.ActivationCompletedAt != nil {
		t.Errorf("ActivationCompletedAt=%v, want nil after clean return", final.ActivationCompletedAt)
	}
	if final.ActivationJobName != nil {
		t.Errorf("ActivationJobName=%q, want nil after clean return", *final.ActivationJobName)
	}
	if final.ActivationError != nil {
		t.Errorf("ActivationError=%q, want nil after clean return", *final.ActivationError)
	}
	if final.CleanupState == nil || *final.CleanupState != testSlotStateReady {
		t.Errorf("CleanupState=%v, want ready", final.CleanupState)
	}
}

func TestSetLeaseSlotCleanupFinishedPreservesActivationFieldsOnError(t *testing.T) {
	// Cleanup failed; slot ends in `error`. The activation_* fields stay
	// visible as diagnostic context for the operator who has to repair —
	// they're the "what was this slot doing when it broke" trail.
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "tank",
			Name: "tank",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "tank-slot",
				"count":       float64(1),
				"slots": []any{
					map[string]any{
						"slot_index":          float64(1),
						"slot_name":           "tank-slot-1",
						"state":               testSlotStateActive,
						"updated_at":          now.Format(time.RFC3339Nano),
						"activation_attempt":  float64(77),
						"activation_state":    testSlotStateActive,
						"activation_job_name": "glim-slot-apply-tank-slot-1-77",
					},
				},
			}},
		}}},
	}
	lease := Lease{
		Project:     "tank",
		LeaseNumber: intPtr(77),
		State:       "claimed",
		Metadata: map[string]any{
			"test_slot_checkout": true,
			"native_slot_index":  "1",
			"native_slot_name":   "tank-slot-1",
		},
		RequestedAt: now,
	}

	if _, err := setLeaseSlotCleanupFinished(context.Background(), store, store.projects[0], lease, "error", errors.New("helm uninstall failed: timeout")); err != nil {
		t.Fatalf("cleanup finished: %v", err)
	}
	snap := store.snapshotSlotStatuses()
	final := snap[len(snap)-1]
	if final.State != "error" {
		t.Fatalf("state=%q, want error", final.State)
	}
	if final.ActivationAttempt == nil || *final.ActivationAttempt != 77 {
		t.Errorf("ActivationAttempt=%v, want preserved as 77 on error path", final.ActivationAttempt)
	}
	if final.ActivationState == nil || *final.ActivationState != testSlotStateActive {
		t.Errorf("ActivationState=%v, want preserved on error path", final.ActivationState)
	}
	if final.ActivationJobName == nil {
		t.Error("ActivationJobName=nil, want preserved on error path")
	}
}

func TestAppendTestSlotHotSwapHistoryResolvesSlotLease(t *testing.T) {
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{ID: "tank-operator", Name: "tank-operator"}}},
		leases: []Lease{{
			Project:     "tank-operator",
			LeaseNumber: intPtr(2),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_slot_index":  "1",
				"native_slot_name":   "tank-slot-1",
			},
		}},
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/hot-swap-history", strings.NewReader(`{"project":"tank-operator","slot_name":"tank-slot-1","entry":{"operation":"backend","status":"ok"}}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"lease":"tank-slot-1"`) || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

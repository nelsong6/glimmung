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

type fakeTestSlotPreparer struct {
	preliminaries         bool
	activated             bool
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

func (p *fakeTestSlotPreparer) ActivateTestSlotRuntime(_ context.Context, _ Lease, project Project, _ NativeGitHubTokenMinter) error {
	p.activated = true
	p.project = project
	signalTestChannel(p.activateStarted)
	if p.activateRelease != nil {
		<-p.activateRelease
	}
	defer signalTestChannel(p.activateDone)
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

func TestReconcileActivatingTestSlotsRestartsOldActivation(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-2 * time.Minute)
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

	if got := reconcileTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 1 {
		t.Fatalf("reconciled=%d, want 1", got)
	}
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

func TestReconcileCleaningTestSlotsRestartsOldCleanup(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-2 * time.Minute)
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

	if got := reconcileTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 1 {
		t.Fatalf("reconciled=%d, want 1", got)
	}
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

func TestReconcileCleaningTestSlotWithoutLeaseMarksReady(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-2 * time.Minute)
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

	if got := reconcileTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 1 {
		t.Fatalf("reconciled=%d, want 1", got)
	}
	waitForSlotStatus(t, store, testSlotStateReady)
	if store.cancelledRef != "" {
		t.Fatalf("cancelledRef=%q, want empty", store.cancelledRef)
	}
}

func TestReconcileExpiredTestSlotLeaseStartsCleanup(t *testing.T) {
	now := time.Now().UTC().Add(-30 * time.Minute)
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
			TTLSeconds:  60,
		},
	}
	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
		returnRelease: make(chan struct{}),
		returnDone:    make(chan struct{}, 1),
	}

	if got := reconcileTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 1 {
		t.Fatalf("reconciled=%d, want 1", got)
	}
	if len(store.slotStatuses) == 0 || store.slotStatuses[0].State != testSlotStateCleaning {
		t.Fatalf("slot statuses=%#v, want cleaning", store.slotStatuses)
	}
	if len(store.slotStatuses[0].ReturnHistory) != 1 || store.slotStatuses[0].ReturnHistory[0].Source != "reconciler.test_slot_ttl" {
		t.Fatalf("return history=%#v, want ttl source", store.slotStatuses[0].ReturnHistory)
	}
	select {
	case <-preparer.returnStarted:
	case <-time.After(time.Second):
		t.Fatal("expired cleanup did not start")
	}
	close(preparer.returnRelease)
	select {
	case <-preparer.returnDone:
	case <-time.After(time.Second):
		t.Fatal("expired cleanup did not finish")
	}
	waitForSlotStatus(t, store, testSlotStateReady)
	if store.cancelledRef != "expire-slot-1" {
		t.Fatalf("cancelledRef=%q, want expire-slot-1", store.cancelledRef)
	}
}

func TestReconcileActiveTestSlotCleansInstaller(t *testing.T) {
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
				"native_k8s":         true,
				"native_slot_index":  "1",
				"native_slot_name":   "active-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  900,
		},
	}
	preparer := &fakeTestSlotPreparer{}

	if got := reconcileTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 0 {
		t.Fatalf("reconciled=%d, want 0", got)
	}
	if !preparer.installerCleaned {
		t.Fatal("expected installer cleanup for active slot")
	}
}

func TestReconcileSeedsMissingTestSlots(t *testing.T) {
	// Project has count=3 but no slots[*] entries — exactly the state
	// tank-operator landed in when warmup was a one-shot PATCH side effect.
	// The reconciler must seed all three indices and bring them to ready.
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

	if got := reconcileTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 3 {
		t.Fatalf("reconciled=%d, want 3", got)
	}
	waitForSlotStatusCount(t, store, 6) // 3 slots × (warming + ready)
	seen := map[int]string{}
	for _, status := range store.slotStatuses {
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
	// Slots warm in parallel goroutines so cleanup order is non-deterministic;
	// assert set membership instead of order.
	gotCleaned := map[string]bool{}
	for _, name := range preparer.cleanedSlots {
		gotCleaned[name] = true
	}
	for _, want := range []string{"seed-slot-1", "seed-slot-2", "seed-slot-3"} {
		if !gotCleaned[want] {
			t.Fatalf("installer cleanup missing %s: got %#v", want, preparer.cleanedSlots)
		}
	}
}

func TestReconcileResumesStaleWarming(t *testing.T) {
	// A slot whose preliminary reconciliation crashed mid-flight leaves a
	// stale `warming` record. The reconciler must pick it up and bring it to
	// ready — without this, the slot is permanently stuck unless an operator
	// re-PATCHes count, which is the bug we're removing.
	now := time.Now().UTC()
	stale := now.Add(-2 * time.Minute)
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
						"updated_at": stale.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		leases: []Lease{},
	}
	preparer := &fakeTestSlotPreparer{}

	if got := reconcileTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 1 {
		t.Fatalf("reconciled=%d, want 1", got)
	}
	waitForSlotStatus(t, store, testSlotStateReady)
	if !preparer.preliminaries {
		t.Fatal("expected EnsureTestSlotPreliminaries to run for stale warming slot")
	}
}

func TestReconcileSkipsRecentWarmingAndClaimedSlots(t *testing.T) {
	// Recent warming (within minAge) belongs to a still-running warmup; the
	// reconciler must not double-fire. A claimed lease drives its own
	// lifecycle and must not be re-warmed.
	now := time.Now().UTC()
	fresh := now.Add(-5 * time.Second)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "skip",
			Name: "skip",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "skip-slot",
				"count":       float64(2),
				"slots": []any{
					map[string]any{
						"slot_index": float64(1),
						"slot_name":  "skip-slot-1",
						"state":      testSlotStateWarming,
						"updated_at": fresh.Format(time.RFC3339Nano),
					},
					map[string]any{
						"slot_index": float64(2),
						"slot_name":  "skip-slot-2",
						"state":      testSlotStateReady,
						"updated_at": fresh.Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		lease: Lease{
			Project:     "skip",
			LeaseNumber: intPtr(11),
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_k8s":         true,
				"native_slot_index":  "2",
				"native_slot_name":   "skip-slot-2",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  900,
		},
	}
	preparer := &fakeTestSlotPreparer{}

	// Slot 2 is active+claimed so the reconciler will run installer cleanup
	// for it (counted as 0 reconciliation starts). Slot 1 is fresh warming,
	// must be skipped. Net: zero warmup starts.
	if got := reconcileTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 0 {
		t.Fatalf("reconciled=%d, want 0 (fresh warming + claimed slot must be skipped)", got)
	}
	if preparer.preliminaries {
		t.Fatal("expected no preliminary work for fresh-warming or claimed slots")
	}
}

func waitForSlotStatusCount(t *testing.T, store *fakeLeaseStore, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(store.slotStatuses) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("slot status writes=%d, want >=%d", len(store.slotStatuses), want)
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
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(store.slotStatuses) > 0 && store.slotStatuses[len(store.slotStatuses)-1].State == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(store.slotStatuses) == 0 {
		t.Fatalf("slot statuses empty, want %s", state)
	}
	t.Fatalf("final slot status=%q, want %s", store.slotStatuses[len(store.slotStatuses)-1].State, state)
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

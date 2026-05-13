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
	preliminaries    bool
	activated        bool
	returned         bool
	installerCleaned bool
	project          Project
	deprovisioned    []string
	preliminariesErr error
	activateErr      error
	returnErr        error
	activateStarted  chan struct{}
	activateRelease  chan struct{}
	activateDone     chan struct{}
	returnStarted    chan struct{}
	returnRelease    chan struct{}
	returnDone       chan struct{}
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

func (p *fakeTestSlotPreparer) CleanupTestSlotInstaller(context.Context, Lease, Project) error {
	p.installerCleaned = true
	return nil
}

func (p *fakeTestSlotPreparer) DeprovisionTestSlot(_ context.Context, lease Lease, _ Project) error {
	if slotName, _ := stringFromMap(lease.Metadata, "native_slot_name"); strings.TrimSpace(slotName) != "" {
		p.deprovisioned = append(p.deprovisioned, strings.TrimSpace(slotName))
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
		host: &Host{Name: "native-k8s"},
	}
	preparer := &fakeTestSlotPreparer{
		activateStarted: make(chan struct{}, 1),
		activateRelease: make(chan struct{}),
		activateDone:    make(chan struct{}, 1),
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil, preparer)

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

func TestRecoverActivatingTestSlotsRestartsOldActivation(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
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
						"updated_at": now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
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
		host: &Host{Name: "native-k8s"},
	}
	preparer := &fakeTestSlotPreparer{
		activateStarted: make(chan struct{}, 1),
		activateRelease: make(chan struct{}),
		activateDone:    make(chan struct{}, 1),
	}

	if got := recoverActivatingTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 1 {
		t.Fatalf("recoveries=%d, want 1", got)
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

func TestRecoverCleaningTestSlotsRestartsOldCleanup(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
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
						"updated_at": now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
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
		host: &Host{Name: "native-k8s"},
	}
	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
		returnRelease: make(chan struct{}),
		returnDone:    make(chan struct{}, 1),
	}

	if got := recoverActivatingTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 1 {
		t.Fatalf("recoveries=%d, want 1", got)
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

func TestRecoverCleaningTestSlotWithoutLeaseMarksReady(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
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
						"updated_at": now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
					},
				},
			}},
		}}},
		leases: []Lease{},
	}
	preparer := &fakeTestSlotPreparer{}

	if got := recoverActivatingTestSlots(context.Background(), store, preparer, nil, 30*time.Second, nil); got != 1 {
		t.Fatalf("recoveries=%d, want 1", got)
	}
	waitForSlotStatus(t, store, testSlotStateReady)
	if store.cancelledRef != "" {
		t.Fatalf("cancelledRef=%q, want empty", store.cancelledRef)
	}
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
		host: &Host{Name: "native-k8s"},
	}
	preparer := &fakeTestSlotPreparer{
		activateErr:  errors.New("render/apply failed"),
		activateDone: make(chan struct{}, 1),
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil, preparer)

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
	finalStatus := store.slotStatuses[len(store.slotStatuses)-1]
	if finalStatus.ActivationState == nil || *finalStatus.ActivationState != "error" {
		t.Fatalf("activation state=%v, want error", finalStatus.ActivationState)
	}
	if finalStatus.ActivationError == nil || !strings.Contains(*finalStatus.ActivationError, "render/apply failed") {
		t.Fatalf("activation error=%v, want render/apply failed", finalStatus.ActivationError)
	}
	if !preparer.returned {
		t.Fatal("expected failed activation cleanup")
	}
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

func TestCheckoutTestSlotRejectsModeField(t *testing.T) {
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{ID: "tank-operator", Name: "tank-operator"}}},
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil, nil)

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
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil, preparer)

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
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil, nil)

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
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil, nil)

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
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil, preparer)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/return", strings.NewReader(`{"project":"tank-operator","slot_name":"tank-slot-1"}`))
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
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil, nil)

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

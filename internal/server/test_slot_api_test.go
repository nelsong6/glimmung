package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakeTestSlotPreparer struct {
	ensured  bool
	returned bool
	project  Project
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

func (p *fakeTestSlotPreparer) EnsureTestSlot(_ context.Context, _ Lease, project Project, _ NativeGitHubTokenMinter) error {
	p.ensured = true
	p.project = project
	return nil
}

func (p *fakeTestSlotPreparer) LaunchNativePhase(context.Context, NativeLaunchRequest) ([]string, error) {
	return nil, nil
}

func (p *fakeTestSlotPreparer) ReturnTestSlot(context.Context, Lease) error {
	p.returned = true
	return nil
}

func TestCheckoutTestSlotClaimsNativeLease(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:       "tank-operator",
			Name:     "tank-operator",
			Metadata: map[string]any{"native_standby_dns": map[string]any{"slot_prefix": "tank-slot", "record_base": "tank.dev.romaine.life"}},
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
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil, preparer)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/checkout", strings.NewReader(`{"project":"tank-operator","tank_session_id":"98","mode":"provision"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if preparer.ensured {
		t.Fatal("checkout should lease an already-ready slot without preparing it")
	}
	for _, want := range []string{`"state":"claimed"`, `"slot_name":"tank-slot-1"`, `"url":"https://tank-slot-1.tank.dev.romaine.life/"`, `"host":"native-k8s"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("response missing %s: %s", want, rec.Body.String())
		}
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
	if preparer.ensured {
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
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil, preparer)

	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/return", strings.NewReader(`{"project":"tank-operator","slot_name":"tank-slot-1"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !preparer.returned {
		t.Fatal("expected test slot cleanup")
	}
	if !strings.Contains(rec.Body.String(), `"lease":"tank-slot-1"`) {
		t.Fatalf("response=%s", rec.Body.String())
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

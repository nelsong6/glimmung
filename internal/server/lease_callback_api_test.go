package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeLeaseCallbackStore struct {
	fakeReadStore
	lease        Lease
	leases       []Lease
	slotStatuses []TestEnvironmentSlotStatus
	cancelledRef string
	err          error
	heartbeatErr error
	releaseErr   error
	token        string
	heartbeats   []string
	releases     []string
}

func (s fakeLeaseCallbackStore) ReadLeaseByCallbackToken(_ context.Context, token string) (Lease, error) {
	if s.err != nil {
		return Lease{}, s.err
	}
	if token != s.token {
		return Lease{}, ErrNotFound
	}
	return s.lease, nil
}

func (s *fakeLeaseCallbackStore) ReleaseLeaseByCallbackToken(_ context.Context, token string) (Lease, error) {
	s.releases = append(s.releases, token)
	if s.releaseErr != nil {
		return Lease{}, s.releaseErr
	}
	if s.err != nil {
		return Lease{}, s.err
	}
	if token != s.token {
		return Lease{}, ErrNotFound
	}
	lease := s.lease
	lease.State = "released"
	releasedAt := lease.RequestedAt.Add(time.Minute)
	lease.ReleasedAt = &releasedAt
	return lease, nil
}

func (s *fakeLeaseCallbackStore) HeartbeatLeaseByCallbackToken(_ context.Context, token string) (Lease, error) {
	s.heartbeats = append(s.heartbeats, token)
	if s.heartbeatErr != nil {
		return Lease{}, s.heartbeatErr
	}
	if s.err != nil {
		return Lease{}, s.err
	}
	if token != s.token {
		return Lease{}, ErrNotFound
	}
	if s.lease.State != "claimed" {
		return Lease{}, ErrInactive
	}
	return s.lease, nil
}

func (s *fakeLeaseCallbackStore) ListHosts(context.Context) ([]Host, error) {
	return nil, s.err
}

func (s *fakeLeaseCallbackStore) ListLeases(context.Context) ([]Lease, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.leases != nil {
		return s.leases, nil
	}
	return []Lease{s.lease}, nil
}

func (s *fakeLeaseCallbackStore) CancelLeaseByRef(_ context.Context, _, ref string) (CancelLeaseResult, error) {
	s.cancelledRef = ref
	if s.err != nil {
		return CancelLeaseResult{}, s.err
	}
	return CancelLeaseResult{State: "no_active_run", LeaseRef: ref}, nil
}

func (s *fakeLeaseCallbackStore) SetProjectTestEnvironmentSlotStatus(_ context.Context, project string, status TestEnvironmentSlotStatus) (Project, error) {
	s.slotStatuses = append(s.slotStatuses, status)
	for i := range s.projects {
		if s.projects[i].Name != project && s.projects[i].ID != project {
			continue
		}
		if s.projects[i].Metadata == nil {
			s.projects[i].Metadata = map[string]any{}
		}
		standby, _ := s.projects[i].Metadata["native_standby_dns"].(map[string]any)
		if standby == nil {
			standby = map[string]any{}
		}
		slots, _ := standby["slots"].([]any)
		replaced := false
		for j, raw := range slots {
			slot, _ := raw.(map[string]any)
			if slot == nil {
				continue
			}
			if index, ok := positiveIntFromMap(slot, "slot_index"); ok && index == status.SlotIndex {
				slots[j] = testSlotStatusMap(status)
				replaced = true
			}
		}
		if !replaced {
			slots = append(slots, testSlotStatusMap(status))
		}
		standby["slots"] = slots
		s.projects[i].Metadata["native_standby_dns"] = standby
		return s.projects[i], nil
	}
	return Project{}, ErrNotFound
}

func TestReadLeaseByCallbackTokenReturnsPublicLease(t *testing.T) {
	now := time.Date(2026, 5, 11, 4, 30, 0, 0, time.UTC)
	store := fakeLeaseCallbackStore{
		token: "callback-token",
		lease: Lease{
			ID:           "01JLEASEBACKING",
			LeaseNumber:  intPtr(42),
			Project:      "glimmung",
			Workflow:     stringPtr("agent-run"),
			Host:         stringPtr("native-k8s"),
			State:        "claimed",
			Requirements: map[string]any{"native_k8s": true},
			Metadata: map[string]any{
				"lease_callback_token": "callback-token",
				"native_slot_name":     "glimmung-slot-2",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  14400,
		},
	}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lease-callbacks/callback-token", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"ref":"glimmung-slot-2"`,
		`"lease_number":42`,
		`"project":"glimmung"`,
		`"workflow":"agent-run"`,
		`"state":"claimed"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body=%s missing %s", body, want)
		}
	}
	if strings.Contains(body, "01JLEASEBACKING") {
		t.Fatalf("body leaks backing lease id: %s", body)
	}
}

func TestHeartbeatLeaseByCallbackTokenReturnsPublicLease(t *testing.T) {
	now := time.Date(2026, 5, 11, 4, 45, 0, 0, time.UTC)
	store := &fakeLeaseCallbackStore{
		token: "callback-token",
		lease: Lease{
			ID:           "01JLEASEBACKING",
			LeaseNumber:  intPtr(7),
			Project:      "glimmung",
			Workflow:     stringPtr("native-run"),
			Host:         stringPtr("native-k8s"),
			State:        "claimed",
			Requirements: map[string]any{},
			Metadata:     map[string]any{"lease_callback_token": "callback-token"},
			RequestedAt:  now,
			AssignedAt:   &now,
			TTLSeconds:   900,
		},
	}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/callback-token/heartbeat", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.heartbeats) != 1 || store.heartbeats[0] != "callback-token" {
		t.Fatalf("heartbeats=%#v", store.heartbeats)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ref":"glimmung/leases/7"`) || strings.Contains(body, "01JLEASEBACKING") {
		t.Fatalf("body=%s", body)
	}
}

func TestHeartbeatLeaseByCallbackTokenRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/missing/heartbeat", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHeartbeatLeaseByCallbackTokenMapsErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "not found", err: ErrNotFound, want: http.StatusNotFound},
		{name: "conflict", err: ErrConflict, want: http.StatusConflict},
		{name: "inactive", err: ErrInactive, want: http.StatusConflict},
		{name: "generic", err: errors.New("boom"), want: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewWithStore(Settings{}, &fakeLeaseCallbackStore{heartbeatErr: tc.err})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/token/heartbeat", nil))
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestReleaseLeaseByCallbackTokenReturnsPublicLease(t *testing.T) {
	now := time.Date(2026, 5, 11, 5, 0, 0, 0, time.UTC)
	store := &fakeLeaseCallbackStore{
		token: "callback-token",
		lease: Lease{
			ID:           "01JLEASEBACKING",
			LeaseNumber:  intPtr(9),
			Project:      "glimmung",
			Workflow:     stringPtr("native-run"),
			Host:         stringPtr("native-k8s"),
			State:        "claimed",
			Requirements: map[string]any{},
			Metadata:     map[string]any{"lease_callback_token": "callback-token"},
			RequestedAt:  now,
			AssignedAt:   &now,
			TTLSeconds:   900,
		},
	}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/callback-token/release", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.releases) != 1 || store.releases[0] != "callback-token" {
		t.Fatalf("releases=%#v", store.releases)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"state":"released"`) || !strings.Contains(body, `"ref":"glimmung/leases/9"`) {
		t.Fatalf("body=%s", body)
	}
	if strings.Contains(body, "01JLEASEBACKING") {
		t.Fatalf("body leaks backing lease id: %s", body)
	}
}

func TestReleaseLeaseByCallbackTokenStartsTestSlotCleanup(t *testing.T) {
	now := time.Date(2026, 5, 11, 5, 15, 0, 0, time.UTC)
	store := &fakeLeaseCallbackStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "tank",
			Name: "tank",
			Metadata: map[string]any{"native_standby_dns": map[string]any{
				"slot_prefix": "tank-slot",
				"count":       float64(1),
				"slots": []any{
					map[string]any{"slot_index": float64(1), "slot_name": "tank-slot-1", "state": testSlotStateActive},
				},
			}},
		}}},
		token: "callback-token",
		lease: Lease{
			ID:           "01JLEASEBACKING",
			LeaseNumber:  intPtr(10),
			Project:      "tank",
			Workflow:     stringPtr("test-slot-checkout"),
			Host:         stringPtr("native-k8s"),
			State:        "claimed",
			Requirements: map[string]any{},
			Metadata: map[string]any{
				"lease_callback_token": "callback-token",
				"test_slot_checkout":   true,
				"native_slot_index":    "1",
				"native_slot_name":     "tank-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  900,
		},
	}
	store.leases = []Lease{store.lease}
	preparer := &fakeTestSlotPreparer{
		returnStarted: make(chan struct{}, 1),
		returnRelease: make(chan struct{}),
		returnDone:    make(chan struct{}, 1),
	}
	handler := newHandler(Settings{}, store, nil, nil, nil, preparer)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/callback-token/release", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.slotStatuses) != 1 || store.slotStatuses[0].State != testSlotStateCleaning {
		t.Fatalf("slot statuses=%#v, want cleaning", store.slotStatuses)
	}
	select {
	case <-preparer.returnStarted:
	case <-time.After(time.Second):
		t.Fatal("callback cleanup did not start")
	}
	close(preparer.returnRelease)
	select {
	case <-preparer.returnDone:
	case <-time.After(time.Second):
		t.Fatal("callback cleanup did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && store.cancelledRef == "" {
		time.Sleep(10 * time.Millisecond)
	}
	if store.cancelledRef != "tank-slot-1" {
		t.Fatalf("cancelledRef=%q, want tank-slot-1", store.cancelledRef)
	}
}

func TestReleaseLeaseByCallbackTokenRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/missing/release", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReleaseLeaseByCallbackTokenMapsErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "not found", err: ErrNotFound, want: http.StatusNotFound},
		{name: "conflict", err: ErrConflict, want: http.StatusConflict},
		{name: "unsupported", err: ErrUnsupported, want: http.StatusServiceUnavailable},
		{name: "generic", err: errors.New("boom"), want: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 5, 11, 5, 30, 0, 0, time.UTC)
			handler := NewWithStore(Settings{}, &fakeLeaseCallbackStore{
				token:      "token",
				releaseErr: tc.err,
				lease: Lease{
					Project:     "glimmung",
					LeaseNumber: intPtr(9),
					State:       "claimed",
					Metadata:    map[string]any{"lease_callback_token": "token"},
					RequestedAt: now,
					AssignedAt:  &now,
				},
			})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/token/release", nil))
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestReadLeaseByCallbackTokenRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lease-callbacks/missing", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadLeaseByCallbackTokenMapsNotFoundAndConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "not found", err: ErrNotFound, want: http.StatusNotFound},
		{name: "conflict", err: ErrConflict, want: http.StatusConflict},
		{name: "generic", err: errors.New("boom"), want: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewWithStore(Settings{}, fakeLeaseCallbackStore{err: tc.err})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lease-callbacks/token", nil))
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

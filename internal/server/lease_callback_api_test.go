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
			handler := NewWithStore(Settings{}, &fakeLeaseCallbackStore{releaseErr: tc.err})
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

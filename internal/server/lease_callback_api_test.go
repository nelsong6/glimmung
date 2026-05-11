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
	lease Lease
	err   error
	token string
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

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

type fakeLeaseStore struct {
	fakeReadStore
	lease  Lease
	leases []Lease
	host   *Host
	result CancelLeaseResult
	err    error
}

func (s *fakeLeaseStore) AcquireLease(_ context.Context, _ LeaseAcquireRequest) (Lease, *Host, error) {
	if s.err != nil {
		return Lease{}, nil, s.err
	}
	return s.lease, s.host, nil
}

func (s *fakeLeaseStore) CancelLeaseByRef(_ context.Context, _, _ string) (CancelLeaseResult, error) {
	if s.err != nil {
		return CancelLeaseResult{}, s.err
	}
	return s.result, nil
}

func (s *fakeLeaseStore) ListHosts(context.Context) ([]Host, error) {
	return nil, s.err
}

func (s *fakeLeaseStore) ListLeases(context.Context) ([]Lease, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.leases != nil {
		return s.leases, nil
	}
	return []Lease{s.lease}, nil
}

func TestCreateLease(t *testing.T) {
	now := time.Date(2026, 5, 11, 8, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{
		lease: Lease{
			ID:           "01JLEASE1234",
			LeaseNumber:  intPtr(3),
			Project:      "myproject",
			Workflow:     stringPtr("agent-run"),
			Host:         stringPtr("native-k8s"),
			State:        "claimed",
			Requirements: map[string]any{"native_k8s": true},
			Metadata: map[string]any{
				"native_slot_name": "myproject-slot-1",
			},
			RequestedAt: now,
			AssignedAt:  &now,
			TTLSeconds:  900,
		},
	}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"project":"myproject","workflow":"agent-run","requirements":{"native_k8s":true},"requester":{"consumer":"glimmung","kind":"run","ref":"myproject#1/runs/1"},"metadata":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/lease", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	for _, want := range []string{`"ref":"myproject-slot-1"`, `"state":"claimed"`, `"project":"myproject"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("body missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "01JLEASE1234") {
		t.Fatalf("body leaks backing lease id: %s", got)
	}
}

func TestCreateLeasePendingNoHost(t *testing.T) {
	now := time.Date(2026, 5, 11, 8, 0, 0, 0, time.UTC)
	store := &fakeLeaseStore{
		lease: Lease{
			ID:           "01JLEASE5678",
			LeaseNumber:  intPtr(4),
			Project:      "myproject",
			State:        "pending",
			Requirements: map[string]any{},
			Metadata:     map[string]any{},
			RequestedAt:  now,
			TTLSeconds:   900,
		},
		host: nil,
	}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"project":"myproject","requester":{"consumer":"glimmung","kind":"run","ref":"myproject#1/runs/1"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/lease", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if !strings.Contains(got, `"state":"pending"`) {
		t.Fatalf("body missing pending state: %s", got)
	}
	if strings.Contains(got, `"host":"`) {
		t.Fatalf("body should not contain host for pending lease: %s", got)
	}
}

func TestCreateLeaseValidates(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeLeaseStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	cases := []struct {
		body string
		desc string
	}{
		{`{"requester":{"consumer":"g","kind":"run","ref":"r"}}`, "missing project"},
		{`{"project":"p","requester":{"kind":"run","ref":"r"}}`, "missing requester.consumer"},
		{`{"project":"p","requester":{"consumer":"g","ref":"r"}}`, "missing requester.kind"},
		{`{"project":"p","requester":{"consumer":"g","kind":"run"}}`, "missing requester.ref"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/lease", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", tc.desc, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateLeaseMapsUnavailable(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeLeaseStore{err: ErrUnavailable}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"project":"myproject","requester":{"consumer":"glimmung","kind":"run","ref":"myproject#1/runs/1"},"requirements":{"native_k8s":true}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/lease", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateLeaseRequiresAdmin(t *testing.T) {
	handler := NewWithStore(Settings{}, &fakeLeaseStore{})
	body := `{"project":"p","requester":{"consumer":"g","kind":"run","ref":"r"}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/lease", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateLeaseRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/lease", strings.NewReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCancelLeaseByRef(t *testing.T) {
	store := &fakeLeaseStore{
		result: CancelLeaseResult{
			State:    "cancelled",
			LeaseRef: "myproject/leases/3",
		},
	}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"project":"myproject","lease_ref":"myproject/leases/3"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/leases/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state":"cancelled"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCancelLeaseByRefNotFound(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeLeaseStore{err: ErrNotFound}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"project":"myproject","lease_ref":"myproject/leases/99"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/leases/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCancelLeaseByRefRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/leases/cancel", strings.NewReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

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

type fakeSignalStore struct {
	fakeReadStore
	result PublicSignal
	err    error
}

func (s *fakeSignalStore) EnqueueSignal(_ context.Context, _ SignalEnqueue) (PublicSignal, error) {
	if s.err != nil {
		return PublicSignal{}, s.err
	}
	return s.result, nil
}

func TestCreateSignal(t *testing.T) {
	store := &fakeSignalStore{result: PublicSignal{
		Ref:        "signal:pr:owner/repo:main:2026-01-01T00:00:00Z",
		TargetType: "pr",
		TargetRepo: "owner/repo",
		TargetRef:  "main",
		Source:     "glimmung_ui",
		State:      "pending",
		EnqueuedAt: time.Now(),
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	body := `{"target_type":"pr","target_repo":"owner/repo","target_ref":"main","source":"glimmung_ui"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/signals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"target_type":"pr"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCreateSignalValidates(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeSignalStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	cases := []struct {
		body   string
		status int
		desc   string
	}{
		{`{"target_repo":"owner/repo","target_ref":"main"}`, http.StatusBadRequest, "missing target_type"},
		{`{"target_type":"pr","target_ref":"main"}`, http.StatusBadRequest, "missing target_repo"},
		{`{"target_type":"pr","target_repo":"owner/repo"}`, http.StatusBadRequest, "missing target_ref"},
		{`{"target_type":"pr","target_repo":"owner/repo","target_ref":"main","source":"bad_source"}`, http.StatusBadRequest, "invalid source"},
		{`{"target_type":"bad","target_repo":"owner/repo","target_ref":"main"}`, http.StatusBadRequest, "invalid target_type"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/signals", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer admin")
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.desc, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateSignalNotFound(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeSignalStore{err: ErrNotFound}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"target_type":"issue","target_repo":"myproject","target_ref":"myproject#999"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/signals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateSignalRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/signals", strings.NewReader(`{}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

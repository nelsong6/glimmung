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

type fakeHostStore struct {
	fakeReadStore
	host Host
	err  error
	seen HostRegistration
}

func (s *fakeHostStore) UpsertHost(_ context.Context, input HostRegistration) (Host, error) {
	s.seen = input
	if s.err != nil {
		return Host{}, s.err
	}
	return s.host, nil
}

func TestRegisterHostRequiresAdmin(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/hosts", strings.NewReader(`{"name":"runner-1"}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestRegisterHostUpsertsHost(t *testing.T) {
	created := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := &fakeHostStore{host: Host{
		ID:           "runner-1",
		Name:         "runner-1",
		Capabilities: map[string]any{"gpu": "none"},
		CreatedAt:    created,
	}}
	handler := NewWithDependencies(
		Settings{},
		store,
		fakeAdminAuthenticator{},
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/hosts", strings.NewReader(`{"name":" runner-1 ","capabilities":{"gpu":"none"},"drained":true}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.seen.Name != "runner-1" {
		t.Fatalf("seen=%#v", store.seen)
	}
	if store.seen.Capabilities["gpu"] != "none" {
		t.Fatalf("capabilities=%#v", store.seen.Capabilities)
	}
	if store.seen.Drained == nil || *store.seen.Drained != true {
		t.Fatalf("drained=%v", store.seen.Drained)
	}
	if !strings.Contains(rec.Body.String(), `"name":"runner-1"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRegisterHostValidatesName(t *testing.T) {
	store := &fakeHostStore{}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/hosts", strings.NewReader(`{"capabilities":{}}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestRegisterHostStoreErrorsReturn500(t *testing.T) {
	store := &fakeHostStore{err: errors.New("boom")}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/hosts", strings.NewReader(`{"name":"runner-1"}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

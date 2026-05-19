package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

func TestUpdateGlobalTestLeaseDefaultTTL(t *testing.T) {
	store := &fakeLeaseStore{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPatch, "/v1/test-slots/default-ttl", strings.NewReader(`{"ttl_seconds":7200}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.defaults.GlobalTTLSeconds != 7200 {
		t.Fatalf("global default=%d, want 7200", store.defaults.GlobalTTLSeconds)
	}
	if !strings.Contains(rec.Body.String(), `"global_ttl_seconds":7200`) {
		t.Fatalf("response=%s, want global_ttl_seconds", rec.Body.String())
	}
}

func TestUpdateProjectTestLeaseDefaultTTL(t *testing.T) {
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:       "tank-operator",
			Name:     "tank-operator",
			Metadata: map[string]any{},
		}}},
		defaults: TestLeaseDefaults{GlobalTTLSeconds: 3600},
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPatch, "/v1/test-slots/default-ttl", strings.NewReader(`{"project":"tank-operator","ttl_seconds":14400}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, ok := store.projects[0].Metadata[testLeaseProjectDefaultTTLSecondsKey].(int)
	if !ok || got != 14400 {
		t.Fatalf("project default=%#v, want 14400", store.projects[0].Metadata[testLeaseProjectDefaultTTLSecondsKey])
	}
	if !strings.Contains(rec.Body.String(), `"test_lease_default_ttl_seconds":14400`) {
		t.Fatalf("response=%s, want project metadata", rec.Body.String())
	}
}

func TestResetProjectTestLeaseDefaultTTL(t *testing.T) {
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "tank-operator",
			Name: "tank-operator",
			Metadata: map[string]any{
				testLeaseProjectDefaultTTLSecondsKey: 14400,
			},
		}}},
		defaults: TestLeaseDefaults{GlobalTTLSeconds: 3600},
	}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPatch, "/v1/test-slots/default-ttl", strings.NewReader(`{"project":"tank-operator","reset":true}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.projects[0].Metadata[testLeaseProjectDefaultTTLSecondsKey]; ok {
		t.Fatalf("project default was not cleared: %#v", store.projects[0].Metadata)
	}
}

func TestUpdateTestLeaseDefaultTTLValidatesTTL(t *testing.T) {
	store := &fakeLeaseStore{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPatch, "/v1/test-slots/default-ttl", strings.NewReader(`{"ttl_seconds":0}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

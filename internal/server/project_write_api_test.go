package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakeProjectStore struct {
	fakeReadStore
	project Project
	req     ProjectRegister
	err     error
}

func (s *fakeProjectStore) UpsertProject(_ context.Context, req ProjectRegister) (Project, error) {
	s.req = req
	if s.err != nil {
		return Project{}, s.err
	}
	return s.project, nil
}

func TestRegisterProjectRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeProjectStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader(`{"name":"ambience","github_repo":"nelsong6/ambience"}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestRegisterProjectUpsertsProject(t *testing.T) {
	created := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := &fakeProjectStore{project: Project{
		ID:         "ambience",
		Name:       "ambience",
		GitHubRepo: "nelsong6/ambience",
		Metadata:   map[string]any{"tier": "app"},
		CreatedAt:  created,
	}}
	handler := NewWithDependencies(
		Settings{},
		store,
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	var project Project
	postJSON(t, handler, "/v1/projects", `{"name":"ambience","github_repo":"nelsong6/ambience","argocd_app":"ignored","metadata":{"tier":"app"}}`, &project)

	if project.Name != "ambience" || project.GitHubRepo != "nelsong6/ambience" {
		t.Fatalf("project=%#v", project)
	}
	if store.req.Name != "ambience" || store.req.GitHubRepo != "nelsong6/ambience" {
		t.Fatalf("req=%#v", store.req)
	}
	if store.req.Metadata["tier"] != "app" {
		t.Fatalf("metadata=%#v", store.req.Metadata)
	}
}

func TestRegisterProjectValidatesRequiredFields(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeProjectStore{},
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader(`{"name":"ambience"}`)))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", rec.Code)
	}
}

func TestRegisterProjectStoreErrorsReturn500(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeProjectStore{err: errors.New("boom")},
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader(`{"name":"ambience","github_repo":"nelsong6/ambience"}`)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func postJSON(t *testing.T, handler http.Handler, path string, body string, target any) {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

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

type fakeProjectScalerStore struct {
	fakeReadStore
	project Project
	name    string
	count   int
	err     error
}

func (s *fakeProjectScalerStore) SetProjectTestEnvironmentCount(_ context.Context, project string, count int) (Project, error) {
	s.name = project
	s.count = count
	if s.err != nil {
		return Project{}, s.err
	}
	return s.project, nil
}

func TestScaleProjectTestEnvironmentsRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeProjectScalerStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/projects/ambience/test-environments/count", strings.NewReader(`{"count":2}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestScaleProjectTestEnvironmentsUpdatesCount(t *testing.T) {
	created := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := &fakeProjectScalerStore{project: Project{
		ID:         "ambience",
		Name:       "ambience",
		GitHubRepo: "nelsong6/ambience",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{"count": float64(3)},
		},
		CreatedAt: created,
	}}
	handler := NewWithDependencies(
		Settings{},
		store,
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	var project Project
	patchJSON(t, handler, "/v1/projects/ambience/test-environments/count", `{"count":3}`, &project)

	if store.name != "ambience" || store.count != 3 {
		t.Fatalf("name=%q count=%d", store.name, store.count)
	}
	if project.Metadata["native_standby_dns"] == nil {
		t.Fatalf("metadata=%#v", project.Metadata)
	}
}

func TestScaleProjectTestEnvironmentsValidatesCount(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeProjectScalerStore{},
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/projects/ambience/test-environments/count", strings.NewReader(`{"count":51}`)))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", rec.Code)
	}
}

func TestScaleProjectTestEnvironmentsMapsNotFound(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeProjectScalerStore{err: ErrNotFound},
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/projects/missing/test-environments/count", strings.NewReader(`{"count":1}`)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestScaleProjectTestEnvironmentsStoreErrorsReturn500(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeProjectScalerStore{err: errors.New("boom")},
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/projects/ambience/test-environments/count", strings.NewReader(`{"count":1}`)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func patchJSON(t *testing.T, handler http.Handler, path string, body string, target any) {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

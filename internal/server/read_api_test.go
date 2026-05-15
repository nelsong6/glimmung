package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/budget"
)

type fakeReadStore struct {
	projects  []Project
	workflows []Workflow
	err       error
}

func (s fakeReadStore) ListProjects(context.Context) ([]Project, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.projects, nil
}

func (s fakeReadStore) ListWorkflows(context.Context) ([]Workflow, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.workflows, nil
}

func TestListProjectsFiltersAndLimits(t *testing.T) {
	created := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := fakeReadStore{projects: []Project{
		{
			ID:         "ambience",
			Name:       "ambience",
			GitHubRepo: "nelsong6/ambience",
			ArgoCDApp:  "ambience",
			Metadata:   map[string]any{"tier": "app"},
			CreatedAt:  created,
		},
		{
			ID:         "glimmung",
			Name:       "glimmung",
			GitHubRepo: "nelsong6/glimmung",
			CreatedAt:  created,
		},
	}}
	handler := NewWithStore(Settings{}, store)

	var rows []Project
	getJSON(t, handler, "/v1/projects?name=amb&github_repo=AMBIENCE&limit=1", &rows)

	if len(rows) != 1 || rows[0].Name != "ambience" {
		t.Fatalf("rows=%#v, want ambience only", rows)
	}
	if rows[0].Metadata["tier"] != "app" {
		t.Fatalf("metadata=%#v", rows[0].Metadata)
	}
}

func TestListWorkflowsFiltersAndLimits(t *testing.T) {
	created := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := fakeReadStore{workflows: []Workflow{
		{
			ID:      "issue-agent",
			Project: "ambience",
			Name:    "issue-agent",
			Budget:  budget.Config{Total: 40},
			Phases: []PhaseSpec{
				{
					Name:             "agent",
					Kind:             "k8s_job",
					WorkflowFilename: "k8s_job:agent",
					WorkflowRef:      "main",
					Verify:           true,
					RecyclePolicy:    &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}, LandsAt: "self"},
				},
			},
			PR:        PrPrimitive{},
			Metadata:  map[string]any{"kind": "primary"},
			CreatedAt: created,
		},
		{
			ID:        "other",
			Project:   "glimmung",
			Name:      "other",
			Budget:    budget.Config{Total: 25},
			CreatedAt: created,
		},
	}}
	handler := NewWithStore(Settings{}, store)

	var rows []Workflow
	getJSON(t, handler, "/v1/workflows?project=ambience&name=ISSUE&limit=1", &rows)

	if len(rows) != 1 || rows[0].Name != "issue-agent" {
		t.Fatalf("rows=%#v, want issue-agent only", rows)
	}
	if rows[0].Budget.Total != 40 {
		t.Fatalf("budget=%#v", rows[0].Budget)
	}
	if rows[0].Phases[0].RecyclePolicy.MaxAttempts != 3 {
		t.Fatalf("phase=%#v", rows[0].Phases[0])
	}
}

func TestReadEndpointsFailClosedWithoutStore(t *testing.T) {
	handler := New(Settings{})
	for _, path := range []string{"/v1/projects", "/v1/workflows"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status=%d, want 503", path, rec.Code)
		}
	}
}

func TestReadEndpointsValidateLimit(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	for _, path := range []string{"/v1/projects?limit=0", "/v1/workflows?limit=501"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d, want 400", path, rec.Code)
		}
	}
}

func TestReadEndpointStoreErrorsReturn500(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{err: errors.New("boom")})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func getJSON(t *testing.T, handler http.Handler, path string, target any) {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

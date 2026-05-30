package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

// fakeProjectSyncClient satisfies both WorkflowSyncClient (so it can be
// passed through NewWithSyncClient) and ProjectSyncClient (the surface under
// test). Only FetchProjectFile is exercised here.
type fakeProjectSyncClient struct {
	content    []byte
	statusCode int
	err        error
}

func (f *fakeProjectSyncClient) FetchWorkflowFile(_ context.Context, _, _, _ string) ([]byte, int, error) {
	return nil, 404, ErrNotFound
}

func (f *fakeProjectSyncClient) FetchProjectFile(_ context.Context, _, _ string) ([]byte, int, error) {
	return f.content, f.statusCode, f.err
}

type fakeProjectSyncStore struct {
	fakeReadStore
	projects []Project
	upserted *ProjectRegister
	result   Project
	err      error
}

func (s *fakeProjectSyncStore) ListProjects(_ context.Context) ([]Project, error) {
	return s.projects, nil
}

func (s *fakeProjectSyncStore) UpsertProject(_ context.Context, req ProjectRegister) (Project, error) {
	if s.err != nil {
		return Project{}, s.err
	}
	s.upserted = &req
	s.result = Project{
		Name:       req.Name,
		GitHubRepo: req.GitHubRepo,
		Metadata:   req.Metadata,
	}
	return s.result, nil
}

var exampleProjectYAML = []byte(`
github_repo: nelsong6/myproject
metadata:
  native_webapp: true
  test_slot_hot_swap:
    artifacts:
      - kind: static
        build: frontend/dist
        dest: /var/run/glimmung-static-override
`)

func newHandlerWithProjectSync(store *fakeProjectSyncStore, client WorkflowSyncClient) http.Handler {
	return NewWithSyncClient(Settings{}, store, nil, client)
}

func newHandlerWithProjectSyncAdmin(store *fakeProjectSyncStore, client WorkflowSyncClient) http.Handler {
	return NewWithSyncClient(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, client)
}

func TestGetProjectUpstream(t *testing.T) {
	store := &fakeProjectSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject"}},
	}
	client := &fakeProjectSyncClient{content: exampleProjectYAML, statusCode: 200}
	handler := newHandlerWithProjectSync(store, client)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/myproject/upstream", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"repo":"nelsong6/myproject"`) {
		t.Fatalf("body=%s", body)
	}
	// Project starts with empty metadata; file carries config → drift.
	if !strings.Contains(body, `"in_sync":false`) {
		t.Fatalf("expected drift, body=%s", body)
	}
}

func TestGetProjectUpstreamNoGHClient(t *testing.T) {
	store := &fakeProjectSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject"}},
	}
	handler := newHandlerWithProjectSync(store, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/myproject/upstream", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fetch_error") {
		t.Fatalf("body missing fetch_error: %s", rec.Body.String())
	}
}

func TestGetProjectUpstreamProjectNotFound(t *testing.T) {
	store := &fakeProjectSyncStore{projects: []Project{}}
	client := &fakeProjectSyncClient{content: exampleProjectYAML, statusCode: 200}
	handler := newHandlerWithProjectSync(store, client)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/nonexistent/upstream", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetProjectUpstreamFileNotFound(t *testing.T) {
	store := &fakeProjectSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject"}},
	}
	client := &fakeProjectSyncClient{statusCode: 404, err: ErrNotFound}
	handler := newHandlerWithProjectSync(store, client)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/myproject/upstream", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSyncProject(t *testing.T) {
	store := &fakeProjectSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject"}},
	}
	client := &fakeProjectSyncClient{content: exampleProjectYAML, statusCode: 200}
	handler := newHandlerWithProjectSyncAdmin(store, client)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/sync", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"in_sync":true`) {
		t.Fatalf("body missing in_sync: %s", rec.Body.String())
	}
	if store.upserted == nil {
		t.Fatalf("expected project to be upserted")
	}
	if store.upserted.Name != "myproject" {
		t.Fatalf("upserted name=%q, want myproject", store.upserted.Name)
	}
	if _, ok := store.upserted.Metadata["test_slot_hot_swap"]; !ok {
		t.Fatalf("authored config dropped test_slot_hot_swap: %#v", store.upserted.Metadata)
	}
}

func TestSyncProjectStripsServerManagedStatus(t *testing.T) {
	yamlWithStatus := []byte(`
github_repo: nelsong6/myproject
metadata:
  native_webapp: true
  managed_auth_origin_status:
    state: ok
`)
	store := &fakeProjectSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject"}},
	}
	client := &fakeProjectSyncClient{content: yamlWithStatus, statusCode: 200}
	handler := newHandlerWithProjectSyncAdmin(store, client)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/sync", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.upserted == nil {
		t.Fatalf("expected upsert")
	}
	if _, ok := store.upserted.Metadata["managed_auth_origin_status"]; ok {
		t.Fatalf("server-managed status leaked into authored config: %#v", store.upserted.Metadata)
	}
}

func TestSyncProjectAlreadyInSync(t *testing.T) {
	current := Project{
		Name:       "myproject",
		GitHubRepo: "nelsong6/myproject",
		Metadata: map[string]any{
			"native_webapp": true,
			"test_slot_hot_swap": map[string]any{
				"artifacts": []any{
					map[string]any{
						"kind":  "static",
						"build": "frontend/dist",
						"dest":  "/var/run/glimmung-static-override",
					},
				},
			},
			// A reconciler-owned status key the read path merged in; it must
			// not register as drift against a file that omits it.
			"managed_auth_origin_status": map[string]any{"state": "ok"},
		},
	}
	store := &fakeProjectSyncStore{projects: []Project{current}}
	client := &fakeProjectSyncClient{content: exampleProjectYAML, statusCode: 200}
	handler := newHandlerWithProjectSyncAdmin(store, client)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/sync", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"in_sync":true`) {
		t.Fatalf("expected in_sync, body=%s", rec.Body.String())
	}
	if store.upserted != nil {
		t.Fatalf("expected no upsert when already in sync")
	}
}

func TestSyncProjectRejectsRetiredHelmImageTag(t *testing.T) {
	badYAML := []byte(`
github_repo: nelsong6/myproject
metadata:
  test_slot_helm:
    values:
      image.tag: pinned-stale
`)
	store := &fakeProjectSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject"}},
	}
	client := &fakeProjectSyncClient{content: badYAML, statusCode: 200}
	handler := newHandlerWithProjectSyncAdmin(store, client)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/sync", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.upserted != nil {
		t.Fatalf("project should not be upserted")
	}
}

func TestParseProjectYAML(t *testing.T) {
	reg, err := parseProjectYAML(exampleProjectYAML, "testproject")
	if err != nil {
		t.Fatalf("parseProjectYAML error: %v", err)
	}
	if reg.Name != "testproject" {
		t.Fatalf("name not overridden from path: %q", reg.Name)
	}
	if reg.GitHubRepo != "nelsong6/myproject" {
		t.Fatalf("github_repo=%q", reg.GitHubRepo)
	}
	if _, ok := reg.Metadata["test_slot_hot_swap"]; !ok {
		t.Fatalf("metadata missing test_slot_hot_swap: %#v", reg.Metadata)
	}
}

func TestParseProjectYAMLRequiresGitHubRepo(t *testing.T) {
	if _, err := parseProjectYAML([]byte("metadata:\n  foo: bar\n"), "p"); err == nil {
		t.Fatalf("expected error when github_repo is missing")
	}
}

func TestParseProjectYAMLAcceptsCamelCaseRepo(t *testing.T) {
	reg, err := parseProjectYAML([]byte("githubRepo: nelsong6/p\n"), "p")
	if err != nil {
		t.Fatalf("parseProjectYAML error: %v", err)
	}
	if reg.GitHubRepo != "nelsong6/p" {
		t.Fatalf("github_repo=%q", reg.GitHubRepo)
	}
}

func TestProjectsInSyncIgnoresStatusKeys(t *testing.T) {
	upstream := ProjectRegister{
		Name:       "p",
		GitHubRepo: "nelsong6/p",
		Metadata:   map[string]any{"a": float64(1)},
	}
	current := &Project{
		Name:       "p",
		GitHubRepo: "nelsong6/p",
		Metadata: map[string]any{
			"a":                          float64(1),
			"managed_auth_origin_status": map[string]any{"state": "ok"},
		},
	}
	if !projectsInSync(upstream, current) {
		t.Fatalf("expected in sync ignoring status keys")
	}
}

func TestProjectsInSyncDetectsAuthoredDrift(t *testing.T) {
	upstream := ProjectRegister{
		Name:       "p",
		GitHubRepo: "nelsong6/p",
		Metadata:   map[string]any{"a": float64(2)},
	}
	current := &Project{
		Name:       "p",
		GitHubRepo: "nelsong6/p",
		Metadata:   map[string]any{"a": float64(1)},
	}
	if projectsInSync(upstream, current) {
		t.Fatalf("expected drift on differing authored config")
	}
}

// TestCommittedProjectYAMLParsesAndCarriesHotSwap guards the real, checked-in
// `.glimmung/project.yaml` for the glimmung project. The Stage-3 reconcile (CI
// POST .../v1/projects/glimmung/sync) replaces authored config wholesale from
// this file, so it must parse through the exact register-path validators and
// must carry the test_slot_hot_swap block that a partial register previously
// dropped. This keeps the committed source of truth honest at CI time.
func TestCommittedProjectYAMLParsesAndCarriesHotSwap(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test filename")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	data, err := os.ReadFile(filepath.Join(repoRoot, ".glimmung", "project.yaml"))
	if err != nil {
		t.Fatalf("read .glimmung/project.yaml: %v", err)
	}

	reg, err := parseProjectYAML(data, "glimmung")
	if err != nil {
		t.Fatalf("parseProjectYAML(committed file): %v", err)
	}
	if reg.GitHubRepo != "nelsong6/glimmung" {
		t.Fatalf("github_repo=%q, want nelsong6/glimmung", reg.GitHubRepo)
	}

	// The complete authored config must be present (sync replaces wholesale).
	for _, key := range []string{
		"native_standby_dns",
		"native_standby_workload_identity",
		"test_slot_helm",
		"test_slot_hot_swap",
	} {
		if _, ok := reg.Metadata[key]; !ok {
			t.Fatalf("committed project.yaml missing authored key %q", key)
		}
	}

	// No reconciler-owned status key may be authored in the file.
	for _, key := range serverManagedProjectStatusKeys {
		if _, ok := reg.Metadata[key]; ok {
			t.Fatalf("committed project.yaml must not author status key %q", key)
		}
	}

	// The restored hot-swap block must describe the supervisor backend path.
	hs, ok := reg.Metadata["test_slot_hot_swap"].(map[string]any)
	if !ok {
		t.Fatalf("test_slot_hot_swap is %T, want map", reg.Metadata["test_slot_hot_swap"])
	}
	backend, ok := hs["backend"].(map[string]any)
	if !ok {
		t.Fatalf("test_slot_hot_swap.backend is %T, want map", hs["backend"])
	}
	if backend["strategy"] != "supervisor" {
		t.Fatalf("backend.strategy=%v, want supervisor", backend["strategy"])
	}
	if backend["health_path"] != "/healthz" {
		t.Fatalf("backend.health_path=%v, want /healthz", backend["health_path"])
	}
}

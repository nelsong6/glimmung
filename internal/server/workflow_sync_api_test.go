package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
	"github.com/nelsong6/glimmung/internal/domain/budget"
)

type fakeWorkflowSyncClient struct {
	content    []byte
	statusCode int
	err        error
}

func (f *fakeWorkflowSyncClient) FetchWorkflowFile(_ context.Context, _, _, _ string) ([]byte, int, error) {
	return f.content, f.statusCode, f.err
}

type fakeWorkflowSyncStore struct {
	fakeReadStore
	projects  []Project
	workflows []Workflow
	upserted  *Workflow
	err       error
}

func (s *fakeWorkflowSyncStore) ListProjects(_ context.Context) ([]Project, error) {
	return s.projects, nil
}

func (s *fakeWorkflowSyncStore) ListWorkflows(_ context.Context) ([]Workflow, error) {
	return s.workflows, nil
}

func (s *fakeWorkflowSyncStore) GetWorkflowByName(_ context.Context, project, name string) (*Workflow, error) {
	if s.err != nil {
		return nil, s.err
	}
	for _, w := range s.workflows {
		if w.Project == project && w.Name == name {
			return &w, nil
		}
	}
	return nil, nil
}

func (s *fakeWorkflowSyncStore) UpsertWorkflowFromRegister(_ context.Context, reg WorkflowRegister) (Workflow, error) {
	if s.err != nil {
		return Workflow{}, s.err
	}
	w := Workflow{
		Project:  reg.Project,
		Name:     reg.Name,
		Phases:   reg.Phases,
		PR:       reg.PR,
		Budget:   reg.Budget,
		Metadata: reg.Metadata,
	}
	s.upserted = &w
	return w, nil
}

var exampleWorkflowYAML = []byte(`
phases:
  - name: entry
    kind: k8s_job
    workflow_filename: run.yaml
    depends_on: []
    jobs:
      - id: entry
  - name: test
    kind: k8s_job
    workflow_filename: verify.yaml
    verify: true
    depends_on: [entry]
    jobs:
      - id: test
  - name: cleanup_early
    kind: k8s_job
    workflow_filename: cleanup_early.yaml
    run_on: always
    purpose: teardown
    skip_when_preserve_test_env: true
    depends_on: [test]
    jobs:
      - id: cleanup-early
  - name: touchpoint
    kind: k8s_job
    workflow_filename: touchpoint.yaml
    run_on: success
    purpose: review_touchpoint
    depends_on: [cleanup_early]
    jobs:
      - id: pr-touchpoint
        primitive: pr_touchpoint
  - name: touchpoint_gate
    kind: touchpoint_gate
    depends_on: [touchpoint]
    jobs:
      - id: pr-merge
        primitive: pr_merge
  - name: cleanup_final
    kind: k8s_job
    workflow_filename: cleanup_final.yaml
    run_on: always
    purpose: teardown
    depends_on: [touchpoint_gate]
    jobs:
      - id: cleanup-final
`)

var nativeWorkflowYAMLOmittedKind = []byte(`
phases:
  - name: entry
    workflow_filename: run.yaml
    depends_on: []
    jobs:
      - id: entry
  - name: test
    workflow_filename: verify.yaml
    verify: true
    depends_on: [entry]
    jobs:
      - id: test
  - name: cleanup_early
    workflow_filename: cleanup_early.yaml
    run_on: always
    purpose: teardown
    skip_when_preserve_test_env: true
    depends_on: [test]
    jobs:
      - id: cleanup-early
  - name: touchpoint
    workflow_filename: touchpoint.yaml
    run_on: success
    purpose: review_touchpoint
    depends_on: [cleanup_early]
    jobs:
      - id: pr-touchpoint
        primitive: pr_touchpoint
  - name: touchpoint_gate
    kind: touchpoint_gate
    depends_on: [touchpoint]
    jobs:
      - id: pr-merge
        primitive: pr_merge
  - name: cleanup_final
    workflow_filename: cleanup_final.yaml
    run_on: always
    purpose: teardown
    depends_on: [touchpoint_gate]
    jobs:
      - id: cleanup-final
`)

func TestGetWorkflowUpstream(t *testing.T) {
	store := &fakeWorkflowSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject"}},
	}
	client := &fakeWorkflowSyncClient{content: exampleWorkflowYAML, statusCode: 200}
	handler := newHandlerWithSyncClient(store, client)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/myproject/workflows/agent-run/upstream", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"repo":"nelsong6/myproject"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestGetWorkflowUpstreamNoGHClient(t *testing.T) {
	store := &fakeWorkflowSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject"}},
	}
	handler := newHandlerWithSyncClient(store, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/myproject/workflows/agent-run/upstream", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fetch_error") {
		t.Fatalf("body missing fetch_error: %s", rec.Body.String())
	}
}

func TestGetWorkflowUpstreamProjectNotFound(t *testing.T) {
	store := &fakeWorkflowSyncStore{projects: []Project{}}
	client := &fakeWorkflowSyncClient{content: exampleWorkflowYAML}
	handler := newHandlerWithSyncClient(store, client)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/nonexistent/workflows/foo/upstream", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSyncWorkflow(t *testing.T) {
	store := &fakeWorkflowSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject", Metadata: map[string]any{"native_webapp": true}}},
	}
	client := &fakeWorkflowSyncClient{content: exampleWorkflowYAML, statusCode: 200}
	handler := newHandlerWithSyncClientAdmin(store, client)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/workflows/agent-run/sync", nil)
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
		t.Fatalf("expected workflow to be upserted")
	}
}

func TestSyncWorkflowDefaultsOmittedKindsToK8sJob(t *testing.T) {
	store := &fakeWorkflowSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject", Metadata: map[string]any{"app_type": "native_web_app"}}},
	}
	client := &fakeWorkflowSyncClient{content: nativeWorkflowYAMLOmittedKind, statusCode: 200}
	handler := newHandlerWithSyncClientAdmin(store, client)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/workflows/agent-run/sync", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.upserted == nil {
		t.Fatalf("expected workflow to be upserted")
	}
	for _, phase := range store.upserted.Phases {
		// touchpoint_gate is its own valid kind (introduced by the
		// gated-workflow migration); the omitted-kind default only
		// applies to non-gate phases.
		if phase.Kind != "k8s_job" && phase.Kind != "touchpoint_gate" {
			t.Fatalf("phase %q kind=%q, want k8s_job or touchpoint_gate", phase.Name, phase.Kind)
		}
	}
}

func TestSyncWorkflowRejectsNonNativeKind(t *testing.T) {
	store := &fakeWorkflowSyncStore{
		projects: []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject", Metadata: map[string]any{"app_type": "native_web_app"}}},
	}
	client := &fakeWorkflowSyncClient{content: []byte(`
phases:
  - name: entry
    kind: container
  - name: test
    kind: k8s_job
    verify: true
    depends_on: [entry]
  - name: cleanup
    kind: k8s_job
    run_on: always
    purpose: teardown
    depends_on: [test]
`), statusCode: 200}
	handler := newHandlerWithSyncClientAdmin(store, client)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/workflows/agent-run/sync", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.upserted != nil {
		t.Fatalf("workflow should not be upserted")
	}
}

func TestSyncWorkflowAlreadyInSync(t *testing.T) {
	parsed, err := parseWorkflowYAML(exampleWorkflowYAML, "myproject", "agent-run", "k8s_job")
	if err != nil {
		t.Fatalf("parseWorkflowYAML setup: %v", err)
	}
	normalizeWorkflowRegister(&parsed)
	phases := parsed.Phases
	existing := Workflow{
		Project:             "myproject",
		Name:                "agent-run",
		Phases:              phases,
		Budget:              budget.DefaultConfig(),
		DefaultRequirements: map[string]any{},
		Metadata:            map[string]any{},
	}
	store := &fakeWorkflowSyncStore{
		projects:  []Project{{Name: "myproject", GitHubRepo: "nelsong6/myproject"}},
		workflows: []Workflow{existing},
	}
	client := &fakeWorkflowSyncClient{content: exampleWorkflowYAML, statusCode: 200}
	handler := newHandlerWithSyncClientAdmin(store, client)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/workflows/agent-run/sync", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.upserted != nil {
		t.Fatalf("expected no upsert when already in sync")
	}
}

func TestParseWorkflowYAML(t *testing.T) {
	reg, err := parseWorkflowYAML(exampleWorkflowYAML, "testproject", "my-workflow", "k8s_job")
	if err != nil {
		t.Fatalf("parseWorkflowYAML error: %v", err)
	}
	if reg.Project != "testproject" || reg.Name != "my-workflow" {
		t.Fatalf("project/name not filled in: %+v", reg)
	}
	if len(reg.Phases) != 6 {
		t.Fatalf("expected 6 phases, got %d", len(reg.Phases))
	}
}

func TestParseWorkflowYAMLUsesProvidedDefaultPhaseKind(t *testing.T) {
	reg, err := parseWorkflowYAML(nativeWorkflowYAMLOmittedKind, "testproject", "my-workflow", "k8s_job")
	if err != nil {
		t.Fatalf("parseWorkflowYAML error: %v", err)
	}
	if reg.Phases[0].Kind != "k8s_job" || reg.Phases[1].Kind != "k8s_job" || reg.Phases[2].Kind != "k8s_job" {
		t.Fatalf("phase kinds=%#v", reg.Phases)
	}
}

func newHandlerWithSyncClient(store *fakeWorkflowSyncStore, client WorkflowSyncClient) http.Handler {
	return NewWithSyncClient(Settings{}, store, nil, client)
}

func newHandlerWithSyncClientAdmin(store *fakeWorkflowSyncStore, client WorkflowSyncClient) http.Handler {
	return NewWithSyncClient(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, client)
}

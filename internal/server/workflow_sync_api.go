package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkflowSyncClient fetches raw file bytes from a project's GitHub repo.
// Returns ErrNotFound when the file doesn't exist in the repo.
type WorkflowSyncClient interface {
	FetchWorkflowFile(ctx context.Context, repo, name, ref string) ([]byte, int, error)
}

// WorkflowSyncStore extends the ReadStore with the ability to read/write workflows
// for the purposes of upstream sync.
type WorkflowSyncStore interface {
	GetWorkflowByName(ctx context.Context, project, name string) (*Workflow, error)
	UpsertWorkflowFromRegister(ctx context.Context, reg WorkflowRegister) (Workflow, error)
}

// WorkflowUpstreamResult is returned by the upstream check and sync endpoints.
type WorkflowUpstreamResult struct {
	Project    string            `json:"project"`
	Workflow   string            `json:"workflow"`
	Ref        string            `json:"ref"`
	Repo       string            `json:"repo"`
	Upstream   *WorkflowRegister `json:"upstream"`
	Current    *Workflow         `json:"current"`
	InSync     bool              `json:"in_sync"`
	FetchError *string           `json:"fetch_error"`
}

func getWorkflowUpstream(store ReadStore, ghClient WorkflowSyncClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		name := r.PathValue("name")
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			ref = "main"
		}
		result, err := fetchUpstreamResult(r.Context(), store, ghClient, project, name, ref)
		if err != nil {
			writeProblem(w, err.(*upstreamError).status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func syncWorkflow(store ReadStore, ghClient WorkflowSyncClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		name := r.PathValue("name")
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			ref = "main"
		}
		result, err := fetchUpstreamResult(r.Context(), store, ghClient, project, name, ref)
		if err != nil {
			writeProblem(w, err.(*upstreamError).status, err.Error())
			return
		}
		if result.InSync {
			writeJSON(w, http.StatusOK, result)
			return
		}
		if result.Upstream == nil {
			writeJSON(w, http.StatusOK, result)
			return
		}
		syncWriter, ok := store.(WorkflowSyncStore)
		if !ok || syncWriter == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow sync store not configured")
			return
		}
		reg := *result.Upstream
		projectDoc := findProjectForSync(r.Context(), store, project)
		if projectDoc == nil {
			writeProblem(w, http.StatusNotFound, fmt.Sprintf("project %q does not exist", project))
			return
		}
		if err := ValidateWorkflowRegister(reg); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		updated, err := syncWriter.UpsertWorkflowFromRegister(r.Context(), reg)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "sync workflow failed")
			return
		}
		result.Current = &updated
		result.InSync = true
		writeJSON(w, http.StatusOK, result)
	}
}

// upstreamError carries an HTTP status alongside the error message.
type upstreamError struct {
	status  int
	message string
}

func (e *upstreamError) Error() string { return e.message }

func fetchUpstreamResult(
	ctx context.Context,
	store ReadStore,
	ghClient WorkflowSyncClient,
	project, name, ref string,
) (*WorkflowUpstreamResult, error) {
	proj := findProjectForSync(ctx, store, project)
	if proj == nil {
		return nil, &upstreamError{http.StatusNotFound, fmt.Sprintf("project %q does not exist", project)}
	}
	if proj.GitHubRepo == "" {
		return nil, &upstreamError{http.StatusBadRequest, fmt.Sprintf("project %q has no github_repo set; cannot fetch workflow upstream", project)}
	}
	if ghClient == nil {
		errMsg := "GitHub App token minter is not configured; cannot fetch workflow upstream"
		cur := readCurrentWorkflow(ctx, store, project, name)
		return &WorkflowUpstreamResult{
			Project:    project,
			Workflow:   name,
			Ref:        ref,
			Repo:       proj.GitHubRepo,
			Upstream:   nil,
			Current:    cur,
			InSync:     false,
			FetchError: &errMsg,
		}, nil
	}

	raw, status, err := ghClient.FetchWorkflowFile(ctx, proj.GitHubRepo, name, ref)
	cur := readCurrentWorkflow(ctx, store, project, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) || status == 404 {
			errMsg := fmt.Sprintf(".glimmung/workflows/%s.yaml not found in %s@%s", name, proj.GitHubRepo, ref)
			return nil, &upstreamError{http.StatusNotFound, errMsg}
		}
		errMsg := fmt.Sprintf("fetch workflow upstream failed: %s", err)
		return &WorkflowUpstreamResult{
			Project:    project,
			Workflow:   name,
			Ref:        ref,
			Repo:       proj.GitHubRepo,
			Upstream:   nil,
			Current:    cur,
			InSync:     false,
			FetchError: &errMsg,
		}, nil
	}

	upstream, err := parseWorkflowYAML(raw, project, name, workflowKindNativeK8sJob)
	if err != nil {
		return nil, &upstreamError{http.StatusUnprocessableEntity, fmt.Sprintf("parse workflow upstream: %s", err)}
	}

	inSync := workflowsInSync(upstream, cur)
	return &WorkflowUpstreamResult{
		Project:  project,
		Workflow: name,
		Ref:      ref,
		Repo:     proj.GitHubRepo,
		Upstream: &upstream,
		Current:  cur,
		InSync:   inSync,
	}, nil
}

func findProjectForSync(ctx context.Context, store ReadStore, project string) *Project {
	if store == nil {
		return nil
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return nil
	}
	for _, p := range projects {
		if firstNonEmpty(p.Name, p.ID) == project {
			return &p
		}
	}
	return nil
}

func readCurrentWorkflow(ctx context.Context, store ReadStore, project, name string) *Workflow {
	syncStore, ok := store.(WorkflowSyncStore)
	if !ok || syncStore == nil {
		return nil
	}
	w, err := syncStore.GetWorkflowByName(ctx, project, name)
	if err != nil {
		return nil
	}
	return w
}

// parseWorkflowYAML parses YAML bytes as a WorkflowRegister, filling in project and name.
func parseWorkflowYAML(data []byte, project, name, defaultPhaseKind string) (WorkflowRegister, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return WorkflowRegister{}, fmt.Errorf("invalid YAML: %w", err)
	}
	if raw == nil {
		return WorkflowRegister{}, fmt.Errorf("workflow file is empty")
	}
	raw["project"] = project
	raw["name"] = name
	b, err := json.Marshal(raw)
	if err != nil {
		return WorkflowRegister{}, fmt.Errorf("re-encode YAML as JSON: %w", err)
	}
	var reg WorkflowRegister
	if err := json.Unmarshal(b, &reg); err != nil {
		return WorkflowRegister{}, fmt.Errorf("unmarshal workflow register: %w", err)
	}
	normalizeWorkflowRegisterWithDefaultKind(&reg, defaultPhaseKind)
	if err := ValidateWorkflowRegister(reg); err != nil {
		return WorkflowRegister{}, err
	}
	return reg, nil
}

// workflowsInSync returns true when the upstream register matches the current workflow,
// ignoring server-set fields.
func workflowsInSync(upstream WorkflowRegister, current *Workflow) bool {
	if current == nil {
		return false
	}
	// Compare by re-serializing both to JSON without server-only fields.
	aBytes, err := json.Marshal(upstream)
	if err != nil {
		return false
	}
	var aMap map[string]any
	if err := json.Unmarshal(aBytes, &aMap); err != nil {
		return false
	}
	bBytes, err := json.Marshal(current)
	if err != nil {
		return false
	}
	var bMap map[string]any
	if err := json.Unmarshal(bBytes, &bMap); err != nil {
		return false
	}
	for _, key := range []string{"id", "created_at", "metadata"} {
		delete(aMap, key)
		delete(bMap, key)
	}
	aCanon, _ := json.Marshal(aMap)
	bCanon, _ := json.Marshal(bMap)
	return strings.EqualFold(string(aCanon), string(bCanon))
}

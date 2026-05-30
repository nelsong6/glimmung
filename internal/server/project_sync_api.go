package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/nelsong6/glimmung/internal/domain/hotswap"
	"gopkg.in/yaml.v3"
)

// ProjectSyncClient fetches the declarative project-config file
// (`.glimmung/project.yaml`) from a project's own GitHub repo. Returns
// ErrNotFound / status 404 when the file does not exist in the repo.
//
// This mirrors WorkflowSyncClient: the same concrete GitHub adapter
// satisfies both interfaces, so the router passes a single ghClient value
// and the project handlers type-assert it to ProjectSyncClient.
type ProjectSyncClient interface {
	FetchProjectFile(ctx context.Context, repo, ref string) ([]byte, int, error)
}

// serverManagedProjectStatusKeys are reconciler-owned status keys that live
// in the durable `projects.status` column, never in authored config. Reads
// merge them back under Metadata for API/Frontend compatibility, so they
// must be stripped before comparing authored config or writing a sync.
//
// This list MUST stay in sync with the identically-named list in
// internal/store/pg/projects.go (the durable write path strips the same
// keys). Both derive from docs/durable-project-config.md → "Config vs status
// split". Server cannot import the pg package (layering), hence the mirror.
var serverManagedProjectStatusKeys = []string{
	"managed_auth_origin_status",
	"native_standby_workload_identity_status",
}

func isServerManagedProjectStatusKey(key string) bool {
	for _, k := range serverManagedProjectStatusKeys {
		if k == key {
			return true
		}
	}
	return false
}

// ProjectUpstreamResult is returned by the project upstream + sync endpoints.
type ProjectUpstreamResult struct {
	Project    string           `json:"project"`
	Ref        string           `json:"ref"`
	Repo       string           `json:"repo"`
	Upstream   *ProjectRegister `json:"upstream"`
	Current    *Project         `json:"current"`
	InSync     bool             `json:"in_sync"`
	FetchError *string          `json:"fetch_error"`
}

func getProjectUpstream(store ReadStore, ghClient WorkflowSyncClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			ref = "main"
		}
		result, err := fetchProjectUpstreamResult(r.Context(), store, projectSyncClientFrom(ghClient), project, ref)
		if err != nil {
			writeProblem(w, err.(*upstreamError).status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func syncProject(store ReadStore, ghClient WorkflowSyncClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			ref = "main"
		}
		result, err := fetchProjectUpstreamResult(r.Context(), store, projectSyncClientFrom(ghClient), project, ref)
		if err != nil {
			writeProblem(w, err.(*upstreamError).status, err.Error())
			return
		}
		if result.InSync || result.Upstream == nil {
			writeJSON(w, http.StatusOK, result)
			return
		}
		writer, ok := store.(ProjectWriter)
		if !ok || writer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "project writer not configured")
			return
		}
		// UpsertProject is the Stage-1 durable write path: it strips
		// server-managed status, mints an immutable config version, and
		// moves the pointer transactionally — it never touches the status
		// column. Replacing authored config from the file is therefore
		// safe and cannot clobber reconciled status.
		updated, err := writer.UpsertProject(r.Context(), *result.Upstream)
		if err != nil {
			writeInternalError(w, r, err, "sync project failed")
			return
		}
		result.Current = &updated
		result.InSync = true
		writeJSON(w, http.StatusOK, result)
	}
}

func fetchProjectUpstreamResult(
	ctx context.Context,
	store ReadStore,
	client ProjectSyncClient,
	project, ref string,
) (*ProjectUpstreamResult, error) {
	proj := findProjectForSync(ctx, store, project)
	if proj == nil {
		return nil, &upstreamError{http.StatusNotFound, fmt.Sprintf("project %q does not exist", project)}
	}
	if proj.GitHubRepo == "" {
		return nil, &upstreamError{http.StatusBadRequest, fmt.Sprintf("project %q has no github_repo set; cannot fetch project upstream", project)}
	}
	if client == nil {
		errMsg := "GitHub App token minter is not configured; cannot fetch project upstream"
		return &ProjectUpstreamResult{
			Project:    project,
			Ref:        ref,
			Repo:       proj.GitHubRepo,
			Upstream:   nil,
			Current:    proj,
			InSync:     false,
			FetchError: &errMsg,
		}, nil
	}

	raw, status, err := client.FetchProjectFile(ctx, proj.GitHubRepo, ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) || status == 404 {
			errMsg := fmt.Sprintf(".glimmung/project.yaml not found in %s@%s", proj.GitHubRepo, ref)
			return nil, &upstreamError{http.StatusNotFound, errMsg}
		}
		errMsg := fmt.Sprintf("fetch project upstream failed: %s", err)
		return &ProjectUpstreamResult{
			Project:    project,
			Ref:        ref,
			Repo:       proj.GitHubRepo,
			Upstream:   nil,
			Current:    proj,
			InSync:     false,
			FetchError: &errMsg,
		}, nil
	}

	upstream, err := parseProjectYAML(raw, project)
	if err != nil {
		return nil, &upstreamError{http.StatusUnprocessableEntity, fmt.Sprintf("parse project upstream: %s", err)}
	}

	inSync := projectsInSync(upstream, proj)
	return &ProjectUpstreamResult{
		Project:  project,
		Ref:      ref,
		Repo:     proj.GitHubRepo,
		Upstream: &upstream,
		Current:  proj,
		InSync:   inSync,
	}, nil
}

func projectSyncClientFrom(ghClient WorkflowSyncClient) ProjectSyncClient {
	if c, ok := ghClient.(ProjectSyncClient); ok {
		return c
	}
	return nil
}

// parseProjectYAML parses the `.glimmung/project.yaml` document as a
// ProjectRegister. The project name from the request path is authoritative
// (the file's `name`, if any, is overridden). The document is the complete
// authored-config source: {name, github_repo, metadata}. Any server-managed
// status key that leaked into the file is stripped — status is reconciler
// owned and never authored.
func parseProjectYAML(data []byte, project string) (ProjectRegister, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return ProjectRegister{}, fmt.Errorf("invalid YAML: %w", err)
	}
	if raw == nil {
		return ProjectRegister{}, fmt.Errorf("project file is empty")
	}
	// Tolerate the camelCase githubRepo spelling used in prose/docs.
	if _, ok := raw["github_repo"]; !ok {
		if v, ok := raw["githubRepo"]; ok {
			raw["github_repo"] = v
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return ProjectRegister{}, fmt.Errorf("re-encode YAML as JSON: %w", err)
	}
	var reg ProjectRegister
	if err := json.Unmarshal(b, &reg); err != nil {
		return ProjectRegister{}, fmt.Errorf("unmarshal project register: %w", err)
	}
	reg.Name = project
	reg.Metadata = stripServerManagedProjectStatus(mapOrEmpty(reg.Metadata))
	if reg.GitHubRepo == "" {
		return ProjectRegister{}, fmt.Errorf("project file must set github_repo")
	}
	// Same authored-config validators the imperative register path runs.
	if _, _, err := hotswap.FromMetadata(reg.Metadata); err != nil {
		return ProjectRegister{}, err
	}
	if err := validateTestSlotHelmMetadata(reg.Metadata); err != nil {
		return ProjectRegister{}, err
	}
	return reg, nil
}

// projectsInSync reports whether the upstream authored-config document
// matches the durable project's authored config. Server-managed status keys
// (which reads merge under Metadata) are stripped from both sides so a
// reconciler status write never registers as authored drift.
func projectsInSync(upstream ProjectRegister, current *Project) bool {
	if current == nil {
		return false
	}
	return canonicalAuthoredProject(upstream.Name, upstream.GitHubRepo, upstream.Metadata) ==
		canonicalAuthoredProject(firstNonEmpty(current.Name, current.ID), current.GitHubRepo, current.Metadata)
}

// canonicalAuthoredProject renders {name, github_repo, metadata-without-status}
// as deterministic JSON (encoding/json sorts map keys recursively). This is
// the same authored-config shape the durable content hash is computed over in
// internal/store/pg/projects.go; here it drives drift detection.
func canonicalAuthoredProject(name, repo string, metadata map[string]any) string {
	doc := map[string]any{
		"name":        name,
		"github_repo": repo,
		"metadata":    stripServerManagedProjectStatus(metadata),
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return ""
	}
	return string(b)
}

func stripServerManagedProjectStatus(metadata map[string]any) map[string]any {
	out := make(map[string]any, len(metadata))
	for k, v := range metadata {
		if isServerManagedProjectStatusKey(k) {
			continue
		}
		out[k] = v
	}
	return out
}

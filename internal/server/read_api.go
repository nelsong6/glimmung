package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/budget"
)

type ReadStore interface {
	ListProjects(ctx context.Context) ([]Project, error)
	ListWorkflows(ctx context.Context) ([]Workflow, error)
}

type Project struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	GitHubRepo string         `json:"github_repo"`
	ArgoCDApp  string         `json:"argocd_app"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  time.Time      `json:"created_at"`
}

type Workflow struct {
	ID                  string         `json:"id"`
	Project             string         `json:"project"`
	Name                string         `json:"name"`
	Phases              []PhaseSpec    `json:"phases"`
	PR                  PrPrimitive    `json:"pr"`
	Budget              budget.Config  `json:"budget"`
	DefaultRequirements map[string]any `json:"default_requirements"`
	Metadata            map[string]any `json:"metadata"`
	CreatedAt           time.Time      `json:"created_at"`
}

type PhaseSpec struct {
	Name                     string            `json:"name"`
	Kind                     string            `json:"kind"`
	WorkflowFilename         string            `json:"workflow_filename"`
	WorkflowRef              string            `json:"workflow_ref"`
	Inputs                   map[string]string `json:"inputs"`
	Outputs                  []string          `json:"outputs"`
	Requirements             map[string]any    `json:"requirements"`
	Verify                   bool              `json:"verify"`
	RecyclePolicy            *RecyclePolicy    `json:"recycle_policy"`
	Always                   bool              `json:"always"`
	EvidenceVerificationGate bool              `json:"evidence_verification_gate"`
	DependsOn                []string          `json:"depends_on"`
	Jobs                     []NativeJobSpec   `json:"jobs"`
}

type RecyclePolicy struct {
	MaxAttempts int      `json:"max_attempts"`
	On          []string `json:"on"`
	LandsAt     string   `json:"lands_at"`
}

type NativeJobSpec struct {
	ID             string            `json:"id"`
	Name           *string           `json:"name"`
	Image          string            `json:"image"`
	Command        []string          `json:"command"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	Steps          []NativeStepSpec  `json:"steps"`
	TimeoutSeconds *int              `json:"timeout_seconds"`
}

type NativeStepSpec struct {
	Slug  string  `json:"slug"`
	Title *string `json:"title"`
}

type PrPrimitive struct {
	Enabled       bool           `json:"enabled"`
	RecyclePolicy *RecyclePolicy `json:"recycle_policy"`
}

func listProjects(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeProblem(w, http.StatusServiceUnavailable, "read store not configured")
			return
		}
		limit, ok := parseLimit(w, r)
		if !ok {
			return
		}
		rows, err := store.ListProjects(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "list projects failed")
			return
		}

		nameNeedle := strings.ToLower(r.URL.Query().Get("name"))
		repoNeedle := strings.ToLower(r.URL.Query().Get("github_repo"))
		filtered := make([]Project, 0, len(rows))
		for _, row := range rows {
			if nameNeedle != "" && !strings.Contains(strings.ToLower(row.Name), nameNeedle) {
				continue
			}
			if repoNeedle != "" && !strings.Contains(strings.ToLower(row.GitHubRepo), repoNeedle) {
				continue
			}
			filtered = append(filtered, row)
			if limit != nil && len(filtered) >= *limit {
				break
			}
		}
		writeJSON(w, http.StatusOK, filtered)
	}
}

func listWorkflows(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeProblem(w, http.StatusServiceUnavailable, "read store not configured")
			return
		}
		limit, ok := parseLimit(w, r)
		if !ok {
			return
		}
		rows, err := store.ListWorkflows(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "list workflows failed")
			return
		}

		project := r.URL.Query().Get("project")
		nameNeedle := strings.ToLower(r.URL.Query().Get("name"))
		filtered := make([]Workflow, 0, len(rows))
		for _, row := range rows {
			if project != "" && row.Project != project {
				continue
			}
			if nameNeedle != "" && !strings.Contains(strings.ToLower(row.Name), nameNeedle) {
				continue
			}
			filtered = append(filtered, row)
			if limit != nil && len(filtered) >= *limit {
				break
			}
		}
		writeJSON(w, http.StatusOK, filtered)
	}
}

func parseLimit(w http.ResponseWriter, r *http.Request) (*int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return nil, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 500 {
		writeProblem(w, http.StatusBadRequest, "limit must be between 1 and 500")
		return nil, false
	}
	return &limit, true
}

func writeProblem(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"detail": message})
}

func stringPtr(value string) *string {
	return &value
}

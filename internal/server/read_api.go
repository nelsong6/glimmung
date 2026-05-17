package server

import (
	"context"
	"log/slog"
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
	// etag carries the Cosmos resource etag for callers that need to do
	// optimistic-concurrency writes (etag-conditional ReplaceItem). Populated
	// by point reads (ReadProject); zero from list queries that don't expose
	// per-row etags. Not serialized — it's an implementation artifact, not
	// part of the project's public shape.
	etag string `json:"-"`
}

// ETag exposes the resource etag captured by the store on the read that
// produced this Project. Use it to perform CAS writes via
// ProjectTestEnvironmentSlotStatusClaimer. Empty when the project came from
// a list query that doesn't carry per-row etags.
func (p Project) ETag() string { return p.etag }

// WithETag returns a copy of the project with `tag` as its captured etag.
// Used by store implementations and tests to attach the etag at read time.
func (p Project) WithETag(tag string) Project { p.etag = tag; return p }

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
			writeInternalError(w, r, err, "list projects failed")
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
			writeInternalError(w, r, err, "list workflows failed")
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

// writeProblem writes a 4xx JSON problem response. Callers MUST NOT use
// it for 5xx — every 5xx in this package goes through writeInternalError
// so the underlying error survives in logs. The migration guard at
// scripts/check-handler-5xx-migration.mjs fails CI on any callsite that
// passes a 5xx status here. Removing the swallow path is the whole point
// of glimmung#514: without the err, the only signal a 500 leaves is the
// abstract `detail` body the SPA prints, and the actual cause (Cosmos
// timeout, schema mismatch, IO error, etc.) is unrecoverable. See
// docs/quality-timeframes.md for why observability is gating scope, not a
// follow-up.
func writeProblem(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"detail": message})
}

// writeInternalError writes a 500 JSON problem response and emits a
// structured slog.Error capturing the request method, the registered
// route pattern (r.Pattern, Go 1.22+ ServeMux), the public summary, and
// the underlying error. The body remains the abstract `summary` so the
// public API surface is unchanged; the err is preserved in logs only.
//
// Callers must supply an err that explains the 5xx. If a 5xx has no
// underlying error to log, the right shape is usually a different status
// (404, 409, 422) — investigate before defaulting to writeInternalError
// with a synthesized error.
func writeInternalError(w http.ResponseWriter, r *http.Request, err error, summary string) {
	route := r.Pattern
	if route == "" {
		route = "(unmatched)"
	}
	slog.Error("handler returned 5xx",
		"method", r.Method,
		"route", route,
		"summary", summary,
		"err", err,
	)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": summary})
}

func stringPtr(value string) *string {
	return &value
}

package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/nelsong6/glimmung/internal/domain/hotswap"
)

type ProjectWriter interface {
	UpsertProject(ctx context.Context, req ProjectRegister) (Project, error)
}

type ProjectRegister struct {
	Name       string         `json:"name"`
	GitHubRepo string         `json:"github_repo"`
	ArgoCDApp  string         `json:"argocd_app"`
	Metadata   map[string]any `json:"metadata"`
}

type projectRegisterRequest struct {
	Name       *string        `json:"name"`
	GitHubRepo *string        `json:"github_repo"`
	ArgoCDApp  string         `json:"argocd_app"`
	Metadata   map[string]any `json:"metadata"`
}

func registerProject(store ReadStore, managedOrigins ManagedOriginReconciler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(ProjectWriter)
		if !ok || writer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "project writer not configured")
			return
		}

		var req projectRegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.Name == nil {
			writeProblem(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		if req.GitHubRepo == nil {
			writeProblem(w, http.StatusUnprocessableEntity, "github_repo is required")
			return
		}
		if _, _, err := hotswap.FromMetadata(req.Metadata); err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}

		project, err := writer.UpsertProject(r.Context(), ProjectRegister{
			Name:       *req.Name,
			GitHubRepo: *req.GitHubRepo,
			ArgoCDApp:  req.ArgoCDApp,
			Metadata:   mapOrEmpty(req.Metadata),
		})
		if err != nil {
			writeInternalError(w, r, err, "register project failed")
			return
		}
		// Reconcile glimmung-owned auth.romaine.life slot origins for this
		// project. Skipped when the project doesn't opt in via
		// managed_auth_origins.enabled; failed status persists on the
		// project row so dashboards surface the broken state. See
		// nelsong6/glimmung#142 stage 2.
		if updated, ok := reconcileManagedAuthOrigins(r.Context(), store, managedOrigins, project); ok {
			project = updated
		}
		writeJSON(w, http.StatusOK, project)
	}
}

// reconcileManagedAuthOrigins runs the managed-origin reconciler and
// persists its status on the project row. Returns the refreshed project
// when status was written, or the original when the reconciler was
// absent or the status writer is unsupported. Errors are intentionally
// swallowed at this layer (status carries the failure) so the
// outer handler can still return the upserted project.
func reconcileManagedAuthOrigins(ctx context.Context, store ReadStore, reconciler ManagedOriginReconciler, project Project) (Project, bool) {
	if reconciler == nil {
		return project, false
	}
	status, _ := reconciler.ReconcileManagedOrigins(ctx, project)
	if status.State == "" {
		return project, false
	}
	writer, ok := store.(ProjectManagedAuthOriginStatusWriter)
	if !ok || writer == nil {
		return project, false
	}
	updated, err := writer.SetProjectManagedAuthOriginStatus(ctx, firstNonEmpty(project.Name, project.ID), status)
	if err != nil {
		return project, false
	}
	return updated, true
}

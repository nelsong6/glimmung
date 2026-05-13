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

func registerProject(store ReadStore) http.HandlerFunc {
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
		if hasLegacyNativeAuthRedirectMetadata(req.Metadata) {
			writeProblem(w, http.StatusUnprocessableEntity, "native_standby_entra_redirects is no longer supported; use native_auth_redirects")
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
			writeProblem(w, http.StatusInternalServerError, "register project failed")
			return
		}
		writeJSON(w, http.StatusOK, project)
	}
}

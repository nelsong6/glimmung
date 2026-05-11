package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

type ProjectTestEnvironmentScaler interface {
	SetProjectTestEnvironmentCount(ctx context.Context, project string, count int) (Project, error)
}

type TestEnvironmentScaleRequest struct {
	Count *int `json:"count"`
}

func scaleProjectTestEnvironments(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scaler, ok := store.(ProjectTestEnvironmentScaler)
		if !ok || scaler == nil {
			writeProblem(w, http.StatusServiceUnavailable, "project scaler not configured")
			return
		}
		project := r.PathValue("project")
		if project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}

		var req TestEnvironmentScaleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.Count == nil || *req.Count < 0 || *req.Count > 50 {
			writeProblem(w, http.StatusUnprocessableEntity, "count must be between 0 and 50")
			return
		}

		updated, err := scaler.SetProjectTestEnvironmentCount(r.Context(), project, *req.Count)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "project not found")
				return
			}
			writeProblem(w, http.StatusInternalServerError, "scale project test environments failed")
			return
		}
		writeJSON(w, http.StatusOK, updated)
	}
}

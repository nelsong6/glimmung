package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

type WorkflowWriteStore interface {
	DeleteWorkflow(ctx context.Context, project string, name string) (Workflow, error)
}

type WorkflowPatchStore interface {
	PatchWorkflow(ctx context.Context, project string, name string, req WorkflowPatchRequest) (Workflow, error)
}

type WorkflowPatchRequest struct {
	PREnabled   *bool    `json:"pr_enabled"`
	BudgetTotal *float64 `json:"budget_total"`
}

func patchWorkflow(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		patcher, ok := store.(WorkflowPatchStore)
		if !ok || patcher == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow patcher not configured")
			return
		}
		var req WorkflowPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		project := r.PathValue("project")
		name := r.PathValue("name")
		workflow, err := patcher.PatchWorkflow(r.Context(), project, name, req)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "workflow "+project+"."+name+" not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "patch workflow failed")
			return
		}
		writeJSON(w, http.StatusOK, workflow)
	}
}

func deleteWorkflow(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(WorkflowWriteStore)
		if !ok || writer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow writer not configured")
			return
		}
		project := r.PathValue("project")
		name := r.PathValue("name")
		workflow, err := writer.DeleteWorkflow(r.Context(), project, name)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "workflow "+project+"."+name+" not found")
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "delete workflow failed")
			return
		}
		writeJSON(w, http.StatusOK, workflow)
	}
}

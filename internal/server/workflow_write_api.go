package server

import (
	"context"
	"errors"
	"net/http"
)

type WorkflowWriteStore interface {
	DeleteWorkflow(ctx context.Context, project string, name string) (Workflow, error)
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

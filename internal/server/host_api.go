package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type HostWriteStore interface {
	UpsertHost(ctx context.Context, input HostRegistration) (Host, error)
}

type HostRegistration struct {
	Name         string         `json:"name"`
	Capabilities map[string]any `json:"capabilities"`
	Drained      *bool          `json:"drained"`
}

func registerHost(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostStore, ok := store.(HostWriteStore)
		if !ok || hostStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "host store not configured")
			return
		}

		var input HostRegistration
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		input.Name = strings.TrimSpace(input.Name)
		if input.Name == "" {
			writeProblem(w, http.StatusBadRequest, "host.name required")
			return
		}

		host, err := hostStore.UpsertHost(r.Context(), input)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "register host failed")
			return
		}
		writeJSON(w, http.StatusOK, host)
	}
}

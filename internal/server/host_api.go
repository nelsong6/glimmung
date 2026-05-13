package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type HostWriteStore interface {
	UpsertHost(ctx context.Context, input HostRegistration) (Host, error)
	DeleteHost(ctx context.Context, name string) error
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
		if err := validateHostRegistration(r.Context(), store, input); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
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

func deleteHost(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostStore, ok := store.(HostWriteStore)
		if !ok || hostStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "host store not configured")
			return
		}
		name := strings.TrimSpace(r.PathValue("name"))
		if name == "" {
			writeProblem(w, http.StatusBadRequest, "host name required")
			return
		}
		if err := hostStore.DeleteHost(r.Context(), name); err != nil {
			if err == ErrConflict {
				writeProblem(w, http.StatusConflict, "host has an active lease")
				return
			}
			if err == ErrNotFound {
				writeProblem(w, http.StatusNotFound, "host not found")
				return
			}
			writeProblem(w, http.StatusInternalServerError, "delete host failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func validateHostRegistration(ctx context.Context, store ReadStore, input HostRegistration) error {
	projectName, _ := stringFromMap(input.Capabilities, "project")
	if strings.TrimSpace(projectName) == "" {
		return nil
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list projects failed")
	}
	for _, project := range projects {
		if project.Name != projectName && project.ID != projectName {
			continue
		}
		standby, ok := mapFromMap(project.Metadata, "native_standby_dns")
		if !ok {
			return nil
		}
		prefix, _ := stringFromMap(standby, "slot_prefix")
		if strings.TrimSpace(prefix) == "" {
			prefix, _ = stringFromMap(standby, "slotPrefix")
		}
		prefix = strings.Trim(strings.TrimSpace(prefix), ".")
		if prefix == "" || !strings.HasPrefix(input.Name, prefix+"-") {
			return nil
		}
		index, err := strconv.Atoi(strings.TrimPrefix(input.Name, prefix+"-"))
		if err != nil || index < 1 {
			return fmt.Errorf("host %q does not match native slot name pattern %s-N", input.Name, prefix)
		}
		if count, ok := positiveIntFromMap(standby, "count"); ok && index > count {
			return fmt.Errorf("native slot host %q exceeds configured count %d for project %s", input.Name, count, projectName)
		}
		return fmt.Errorf("native slot host %q is managed by native_standby_dns.count for project %s", input.Name, projectName)
	}
	return nil
}

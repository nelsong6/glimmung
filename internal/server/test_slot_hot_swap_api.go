package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type TestSlotHotSwapHistoryStore interface {
	AppendTestSlotHotSwapHistory(ctx context.Context, project, leaseRef string, entry TestSlotHotSwapHistoryEntry) (Lease, error)
}

type TestSlotHotSwapHistoryRequest struct {
	Project   string                      `json:"project"`
	LeaseRef  string                      `json:"lease_ref"`
	SlotIndex *int                        `json:"slot_index"`
	SlotName  *string                     `json:"slot_name"`
	Entry     TestSlotHotSwapHistoryEntry `json:"entry"`
}

type TestSlotHotSwapHistoryEntry struct {
	Operation   string            `json:"operation"`
	Status      string            `json:"status"`
	Summary     string            `json:"summary,omitempty"`
	Diagnostics map[string]any    `json:"diagnostics,omitempty"`
	Timings     map[string]string `json:"timings,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

type TestSlotHotSwapHistoryResult struct {
	Lease string                      `json:"lease"`
	Entry TestSlotHotSwapHistoryEntry `json:"entry"`
}

func appendTestSlotHotSwapHistory(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(TestSlotHotSwapHistoryStore)
		stateStore, hasState := store.(StateStore)
		if !ok || writer == nil || !hasState || stateStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "test-slot hot-swap history store not configured")
			return
		}
		var req TestSlotHotSwapHistoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Project = strings.TrimSpace(req.Project)
		if req.Project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		leaseRef := strings.TrimSpace(req.LeaseRef)
		if leaseRef == "" {
			lease, err := resolveTestSlotLease(r, stateStore, TestSlotReturnRequest{Project: req.Project, SlotIndex: req.SlotIndex, SlotName: req.SlotName})
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					writeProblem(w, http.StatusNotFound, "test slot lease not found")
					return
				}
				writeProblem(w, http.StatusBadRequest, err.Error())
				return
			}
			leaseRef = LeasePublicRefFromLease(lease)
		}
		entry := req.Entry
		if entry.CreatedAt.IsZero() {
			entry.CreatedAt = time.Now().UTC()
		}
		if strings.TrimSpace(entry.Operation) == "" {
			entry.Operation = "hot_swap"
		}
		if strings.TrimSpace(entry.Status) == "" {
			entry.Status = "unknown"
		}
		lease, err := writer.AppendTestSlotHotSwapHistory(r.Context(), req.Project, leaseRef, entry)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "lease not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "append test-slot hot-swap history failed")
			return
		}
		writeJSON(w, http.StatusOK, TestSlotHotSwapHistoryResult{Lease: LeasePublicRefFromLease(lease), Entry: entry})
	}
}

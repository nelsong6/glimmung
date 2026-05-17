package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type LeaseStore interface {
	AcquireLease(ctx context.Context, req LeaseAcquireRequest) (Lease, error)
	CancelLeaseByRef(ctx context.Context, project, ref string) (CancelLeaseResult, error)
}

type LeaseCanceller interface {
	CancelLeaseByRef(ctx context.Context, project, ref string) (CancelLeaseResult, error)
}

type LeaseAcquireRequest struct {
	Project      string
	Workflow     *string
	Requirements map[string]any
	Requester    LeaseRequesterInput
	Metadata     map[string]any
	TTLSeconds   *int
}

type LeaseRequesterInput struct {
	Consumer string            `json:"consumer"`
	Kind     string            `json:"kind"`
	Ref      string            `json:"ref"`
	Label    *string           `json:"label"`
	URL      *string           `json:"url"`
	Metadata map[string]string `json:"metadata"`
}

type CancelLeaseRequest struct {
	Project  string `json:"project"`
	LeaseRef string `json:"lease_ref"`
}

type CancelLeaseResult struct {
	State             string  `json:"state"`
	LeaseRef          string  `json:"lease_ref"`
	RunRef            *string `json:"run_ref"`
	IssueLockReleased *bool   `json:"issue_lock_released"`
	PRLockReleased    *bool   `json:"pr_lock_released"`
}

func cancelLeaseByRef(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ls, ok := store.(LeaseStore)
		if !ok || ls == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease store not configured")
			return
		}
		var body CancelLeaseRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if strings.TrimSpace(body.Project) == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		if strings.TrimSpace(body.LeaseRef) == "" {
			writeProblem(w, http.StatusBadRequest, "lease_ref required")
			return
		}
		result, err := ls.CancelLeaseByRef(r.Context(), body.Project, body.LeaseRef)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "lease not found")
			return
		case err != nil:
			writeInternalError(w, r, err, "cancel lease failed")
			return
		}
		// Admin cancel may target a claimed test-slot lease whose TTL timer
		// is armed in-process. Stop it so it doesn't fire cleanup after the
		// lease has already been released. Safe no-op for any other lease.
		cancelLeaseExpiryTimer(body.LeaseRef)
		writeJSON(w, http.StatusOK, result)
	}
}

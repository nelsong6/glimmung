package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type LeaseStore interface {
	AcquireLease(ctx context.Context, req LeaseAcquireRequest) (Lease, *Host, error)
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

type leaseRequest struct {
	Project      string              `json:"project"`
	Workflow     *string             `json:"workflow"`
	Requirements map[string]any      `json:"requirements"`
	Requester    LeaseRequesterInput `json:"requester"`
	Metadata     map[string]any      `json:"metadata"`
	TTLSeconds   *int                `json:"ttl_seconds"`
}

type LeaseResponse struct {
	Lease LeasePublic `json:"lease"`
	Host  *HostPublic `json:"host"`
}

type CancelLeaseRequest struct {
	Project  string `json:"project"`
	LeaseRef string `json:"lease_ref"`
}

type CancelLeaseResult struct {
	State             string  `json:"state"`
	LeaseRef          string  `json:"lease_ref"`
	RunRef            *string `json:"run_ref"`
	GHRunCancelled    *bool   `json:"gh_run_cancelled"`
	IssueLockReleased *bool   `json:"issue_lock_released"`
	PRLockReleased    *bool   `json:"pr_lock_released"`
}

func createLease(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ls, ok := store.(LeaseStore)
		if !ok || ls == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease store not configured")
			return
		}
		var body leaseRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if strings.TrimSpace(body.Project) == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		if strings.TrimSpace(body.Requester.Consumer) == "" || strings.TrimSpace(body.Requester.Kind) == "" || strings.TrimSpace(body.Requester.Ref) == "" {
			writeProblem(w, http.StatusBadRequest, "requester.consumer, requester.kind, and requester.ref are required")
			return
		}
		req := LeaseAcquireRequest{
			Project:      body.Project,
			Workflow:     body.Workflow,
			Requirements: mapOrEmpty(body.Requirements),
			Requester:    body.Requester,
			Metadata:     mapOrEmpty(body.Metadata),
			TTLSeconds:   body.TTLSeconds,
		}
		lease, host, err := ls.AcquireLease(r.Context(), req)
		if err != nil {
			var validationErr ValidationError
			if errors.As(err, &validationErr) {
				writeProblem(w, http.StatusBadRequest, validationErr.Message)
				return
			}
			if errors.Is(err, ErrUnavailable) {
				writeProblem(w, http.StatusServiceUnavailable, "lease unavailable")
				return
			}
			writeProblem(w, http.StatusInternalServerError, "acquire lease failed")
			return
		}
		resp := LeaseResponse{Lease: leaseToPublic(lease)}
		if host != nil {
			pub := hostToPublic(*host, map[string]string{lease.ID: resp.Lease.Ref})
			resp.Host = &pub
		}
		writeJSON(w, http.StatusOK, resp)
	}
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
			writeProblem(w, http.StatusInternalServerError, "cancel lease failed")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

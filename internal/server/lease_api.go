package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nelsong6/glimmung/internal/metrics"
)

// Lease purposes used in metric labels. Bounded, closed set — any new
// caller must extend this list, which is the point: cardinality stays
// controlled at the source.
const (
	LeasePurposeDispatch         = "dispatch"
	LeasePurposeAdvance          = "advance"
	LeasePurposeRetry            = "retry"
	LeasePurposeResume           = "resume"
	LeasePurposeSignalDrain      = "signal_drain"
	LeasePurposeTestSlotCheckout = "test_slot_checkout"
)

// acquireLeaseInstrumented wraps a LeaseAcquire call with metric recording.
// Use this everywhere AcquireLease is called from glimmung-internal code,
// so glimmung_leases_acquired_total / glimmung_lease_acquire_wait_seconds
// stay consistent. The purpose must be one of the LeasePurpose* constants.
func acquireLeaseInstrumented(
	ctx context.Context,
	purpose string,
	req LeaseAcquireRequest,
	acquire func(context.Context, LeaseAcquireRequest) (Lease, error),
) (Lease, error) {
	start := time.Now()
	lease, err := acquire(ctx, req)
	outcome := classifyLeaseAcquire(lease, err)
	metrics.RecordLeaseAcquire(purpose, outcome, time.Since(start))
	return lease, err
}

func classifyLeaseAcquire(lease Lease, err error) string {
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return "conflict"
		}
		return "error"
	}
	if lease.State == "claimed" {
		return "granted"
	}
	return "conflict"
}

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
			writeProblem(w, http.StatusInternalServerError, "cancel lease failed")
			return
		}
		// Admin cancel may target a claimed test-slot lease whose TTL timer
		// is armed in-process. Stop it so it doesn't fire cleanup after the
		// lease has already been released. Safe no-op for any other lease.
		cancelLeaseExpiryTimer(body.LeaseRef)
		metrics.RecordLeaseReleased(leasePurposeFromCancelResult(result), "cancelled")
		writeJSON(w, http.StatusOK, result)
	}
}

// leasePurposeFromCancelResult maps a cancelled lease back to its caller
// purpose using the cancel result. State alone is not authoritative;
// glimmung's release flows label the purpose at acquire time, but admin
// cancel has no purpose context. We record "admin_cancel" so the released
// counter still moves and the held gauge stays balanced — without claiming
// to know which acquire site originally took the lease.
func leasePurposeFromCancelResult(_ CancelLeaseResult) string {
	return "admin_cancel"
}

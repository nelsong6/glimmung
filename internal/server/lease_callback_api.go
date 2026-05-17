package server

import (
	"context"
	"errors"
	"net/http"
)

type LeaseCallbackReadStore interface {
	ReadLeaseByCallbackToken(ctx context.Context, token string) (Lease, error)
}

type LeaseCallbackHeartbeatStore interface {
	HeartbeatLeaseByCallbackToken(ctx context.Context, token string) (Lease, error)
}

type LeaseCallbackReleaseStore interface {
	ReleaseLeaseByCallbackToken(ctx context.Context, token string) (Lease, error)
}

func readLeaseByCallbackToken(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callbackStore, ok := store.(LeaseCallbackReadStore)
		if !ok || callbackStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease callback store not configured")
			return
		}
		token := r.PathValue("callback_token")
		lease, err := callbackStore.ReadLeaseByCallbackToken(r.Context(), token)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "lease callback token not found")
			return
		case errors.Is(err, ErrConflict):
			writeProblem(w, http.StatusConflict, "lease callback token is ambiguous")
			return
		case err != nil:
			writeInternalError(w, r, err, "read lease callback failed")
			return
		}
		writeJSON(w, http.StatusOK, leaseToPublic(lease))
	}
}

func heartbeatLeaseByCallbackToken(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callbackStore, ok := store.(LeaseCallbackHeartbeatStore)
		if !ok || callbackStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease callback store not configured")
			return
		}
		lease, err := callbackStore.HeartbeatLeaseByCallbackToken(r.Context(), r.PathValue("callback_token"))
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "lease callback token not found")
			return
		case errors.Is(err, ErrConflict):
			writeProblem(w, http.StatusConflict, "lease callback token is ambiguous")
			return
		case errors.Is(err, ErrInactive):
			writeProblem(w, http.StatusConflict, "lease is not active")
			return
		case err != nil:
			writeInternalError(w, r, err, "heartbeat lease callback failed")
			return
		}
		writeJSON(w, http.StatusOK, leaseToPublic(lease))
	}
}

func releaseLeaseByCallbackToken(store ReadStore, preparer TestSlotPreparer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callbackStore, ok := store.(LeaseCallbackReleaseStore)
		if !ok || callbackStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease callback store not configured")
			return
		}
		if readStore, ok := store.(LeaseCallbackReadStore); ok && readStore != nil {
			lease, err := readStore.ReadLeaseByCallbackToken(r.Context(), r.PathValue("callback_token"))
			switch {
			case errors.Is(err, ErrNotFound):
				writeProblem(w, http.StatusNotFound, "lease callback token not found")
				return
			case errors.Is(err, ErrConflict):
				writeProblem(w, http.StatusConflict, "lease callback token is ambiguous")
				return
			case err != nil:
				writeInternalError(w, r, err, "read lease callback failed")
				return
			}
			if boolFromMap(lease.Metadata, "test_slot_checkout") {
				releaseTestSlotLeaseByCallback(w, r, store, preparer, lease)
				return
			}
		}
		lease, err := callbackStore.ReleaseLeaseByCallbackToken(r.Context(), r.PathValue("callback_token"))
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "lease callback token not found")
			return
		case errors.Is(err, ErrConflict):
			writeProblem(w, http.StatusConflict, "lease callback token is ambiguous")
			return
		case errors.Is(err, ErrUnsupported):
			writeProblem(w, http.StatusServiceUnavailable, "test slot cleanup is not configured")
			return
		case err != nil:
			writeInternalError(w, r, err, "release lease callback failed")
			return
		}
		writeJSON(w, http.StatusOK, leaseToPublic(lease))
	}
}

func releaseTestSlotLeaseByCallback(w http.ResponseWriter, r *http.Request, store ReadStore, preparer TestSlotPreparer, lease Lease) {
	if lease.State == "released" || lease.State == "expired" {
		writeJSON(w, http.StatusOK, leaseToPublic(lease))
		return
	}
	if preparer == nil {
		writeProblem(w, http.StatusServiceUnavailable, "test slot cleanup is not configured")
		return
	}
	project, ok, err := findProjectByKey(r.Context(), store, lease.Project)
	if err != nil {
		writeInternalError(w, r, err, "list projects failed")
		return
	}
	if !ok {
		writeProblem(w, http.StatusBadRequest, "project not registered")
		return
	}
	if _, err := setLeaseSlotCleanupStarting(r.Context(), store, project, lease, testSlotReturnHistoryEntry(lease, testSlotReturnAudit{
		Source:         "lease_callback.release",
		CleanupStarted: true,
	})); err != nil {
		writeInternalError(w, r, err, "record test-slot cleanup state failed")
		return
	}
	beginTestSlotCleanup(store, preparer, project, lease, true, nil)
	writeJSON(w, http.StatusAccepted, testSlotReturnResponse(project, lease.Project, lease, testSlotStateCleaning, true))
}

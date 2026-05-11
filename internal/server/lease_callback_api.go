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
			writeProblem(w, http.StatusInternalServerError, "read lease callback failed")
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
			writeProblem(w, http.StatusInternalServerError, "heartbeat lease callback failed")
			return
		}
		writeJSON(w, http.StatusOK, leaseToPublic(lease))
	}
}

func releaseLeaseByCallbackToken(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callbackStore, ok := store.(LeaseCallbackReleaseStore)
		if !ok || callbackStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease callback store not configured")
			return
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
			writeProblem(w, http.StatusInternalServerError, "release lease callback failed")
			return
		}
		writeJSON(w, http.StatusOK, leaseToPublic(lease))
	}
}

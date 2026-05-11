package server

import (
	"context"
	"errors"
	"net/http"
)

type LeaseCallbackReadStore interface {
	ReadLeaseByCallbackToken(ctx context.Context, token string) (Lease, error)
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

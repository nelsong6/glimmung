package server

import (
	"context"
	"net/http"

	"github.com/nelsong6/glimmung/internal/auth"
)

type AdminAuthenticator interface {
	RequireAdmin(ctx context.Context, authorization string) (auth.User, error)
}

func requireAdmin(authenticator AdminAuthenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticator == nil {
			writeProblem(w, http.StatusServiceUnavailable, "admin auth not configured")
			return
		}
		if _, err := authenticator.RequireAdmin(r.Context(), r.Header.Get("Authorization")); err != nil {
			if authErr, ok := err.(auth.AuthError); ok {
				writeProblem(w, authErr.Status, authErr.Message)
				return
			}
			writeProblem(w, http.StatusUnauthorized, err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}

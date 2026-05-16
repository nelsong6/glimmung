package server

import (
	"context"
	"net/http"

	"github.com/nelsong6/glimmung/internal/auth"
)

type adminUserContextKey struct{}

type AdminAuthenticator interface {
	RequireAdmin(ctx context.Context, r *http.Request) (auth.User, error)
}

func requireAdmin(authenticator AdminAuthenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticator == nil {
			writeProblem(w, http.StatusServiceUnavailable, "admin auth not configured")
			return
		}
		user, err := authenticator.RequireAdmin(r.Context(), r)
		if err != nil {
			if authErr, ok := err.(auth.AuthError); ok {
				writeProblem(w, authErr.Status, authErr.Message)
				return
			}
			writeProblem(w, http.StatusUnauthorized, err.Error())
			return
		}
		ctx := context.WithValue(r.Context(), adminUserContextKey{}, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func adminUser(ctx context.Context) (auth.User, bool) {
	user, ok := ctx.Value(adminUserContextKey{}).(auth.User)
	return user, ok
}

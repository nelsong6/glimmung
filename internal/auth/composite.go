package auth

import (
	"context"
	"net/http"
	"strings"
)

// CompositeAuthenticator routes incoming bearer tokens to the right verifier
// based on token shape. K8s ServiceAccount tokens (recognized by their JWT
// structure) go to the K8s authenticator; everything else is treated as an
// auth.romaine.life RS256 JWT.
type CompositeAuthenticator struct {
	RomaineLife *RomaineLifeAuthenticator
	K8s         *K8sAuthenticator
}

func (a CompositeAuthenticator) ResolveCaller(ctx context.Context, authorization string) (User, bool, bool) {
	if !strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		return User{}, false, false
	}
	token := strings.TrimSpace(authorization[7:])
	if token == "" {
		return User{}, false, false
	}

	if LooksLikeK8sSAToken(token) {
		if a.K8s == nil {
			return User{}, false, false
		}
		user, isAdmin, err := a.K8s.Resolve(ctx, token)
		if err != nil {
			return User{}, false, false
		}
		return user, isAdmin, true
	}

	if a.RomaineLife == nil {
		return User{}, false, false
	}
	user, isAdmin, err := a.RomaineLife.Resolve(ctx, token)
	if err != nil {
		return User{}, false, false
	}
	return user, isAdmin, true
}

func (a CompositeAuthenticator) RequireAdmin(ctx context.Context, authorization string) (User, error) {
	if !strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "missing bearer token"}
	}
	token := strings.TrimSpace(authorization[7:])
	if token == "" {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "missing bearer token"}
	}

	if LooksLikeK8sSAToken(token) {
		if a.K8s == nil {
			return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "k8s auth not configured"}
		}
		return a.K8s.RequireAdmin(ctx, token)
	}

	if a.RomaineLife == nil {
		return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "auth.romaine.life auth not configured"}
	}
	return a.RomaineLife.RequireAdmin(ctx, token)
}

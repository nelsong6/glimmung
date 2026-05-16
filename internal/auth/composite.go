package auth

import (
	"context"
	"net/http"
	"strings"
)

// CompositeAuthenticator routes incoming requests to the right verifier:
//   - Authorization: Bearer <K8s SA token>  →  K8sAuthenticator (in-cluster
//     callers like tank-operator's MCP attestation)
//   - .romaine.life session cookie           →  CookieDelegate (browser
//     callers — forwards cookie to auth.romaine.life/api/auth/get-session
//     and gates on the role claim)
//
// Browser callers don't (and shouldn't) hold a Bearer token: the auth
// service owns the session via a cookie scoped to the parent domain, and
// glimmung consults it per-request (cached 60s).
type CompositeAuthenticator struct {
	Cookie *CookieDelegate
	K8s    *K8sAuthenticator
}

// ResolveCaller returns (user, isAdmin, ok). ok=false means no
// recognizable credential was attached; isAdmin=true means the resolved
// user has the admin role.
func (a CompositeAuthenticator) ResolveCaller(ctx context.Context, r *http.Request) (User, bool, bool) {
	if r == nil {
		return User{}, false, false
	}

	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
		token := strings.TrimSpace(authz[7:])
		if token != "" && LooksLikeK8sSAToken(token) {
			if a.K8s == nil {
				return User{}, false, false
			}
			user, isAdmin, err := a.K8s.Resolve(ctx, token)
			if err != nil {
				return User{}, false, false
			}
			return user, isAdmin, true
		}
	}

	if a.Cookie == nil {
		return User{}, false, false
	}
	user, isAdmin, err := a.Cookie.Resolve(ctx, r.Header.Get("Cookie"))
	if err != nil {
		return User{}, false, false
	}
	return user, isAdmin, true
}

// RequireAdmin is the strict variant for admin-only routes. Bearer K8s SA
// tokens still go through K8s.RequireAdmin (which has its own allowlist);
// browser cookies go through CookieDelegate.RequireAdmin (which 403s
// anything other than role=admin).
func (a CompositeAuthenticator) RequireAdmin(ctx context.Context, r *http.Request) (User, error) {
	if r == nil {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "no request"}
	}

	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
		token := strings.TrimSpace(authz[7:])
		if token != "" && LooksLikeK8sSAToken(token) {
			if a.K8s == nil {
				return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "k8s auth not configured"}
			}
			return a.K8s.RequireAdmin(ctx, token)
		}
	}

	if a.Cookie == nil {
		return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "auth.romaine.life delegate not configured"}
	}
	return a.Cookie.RequireAdmin(ctx, r.Header.Get("Cookie"))
}

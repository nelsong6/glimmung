package auth

import (
	"context"
	"net/http"
	"strings"
)

// CompositeAuthenticator routes incoming requests to the right verifier:
//   - Authorization: Bearer <auth.romaine.life JWT>  →  RomaineLifeJWT
//     (the standard bearer path: humans hitting the API directly with
//     a token, MCP servers, in-cluster service principals via the
//     token-exchange flow)
//   - Authorization: Bearer <K8s SA token>            →  K8sAuthenticator
//     (legacy in-cluster callers that haven't migrated to the JWT
//     exchange yet — retired in Stage C of the auth.romaine.life
//     cutover; see docs/test-slot-lifecycle.md for the parallel slot
//     rework pattern)
//   - .romaine.life session cookie                    →  CookieDelegate
//     (browser callers — forwards cookie to
//     auth.romaine.life/api/auth/get-session and gates on the role
//     claim; same trust root as the JWT path, different presentation
//     format)
//
// All three paths terminate at auth.romaine.life as the identity
// provider. Glimmung is a relying party with no user database of its
// own.
type CompositeAuthenticator struct {
	Cookie  *CookieDelegate
	Romaine *RomaineLifeJWTVerifier
	K8s     *K8sAuthenticator
}

// ResolveCaller returns (user, isAdmin, ok). ok=false means no
// recognizable credential was attached; isAdmin=true means the resolved
// user has the admin role (or is a service principal acting on behalf
// of an admin-equivalent identity).
func (a CompositeAuthenticator) ResolveCaller(ctx context.Context, r *http.Request) (User, bool, bool) {
	if r == nil {
		return User{}, false, false
	}

	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
		token := strings.TrimSpace(authz[7:])
		if token != "" {
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
			if a.Romaine != nil {
				user, isAdmin, err := a.Romaine.Resolve(ctx, token)
				if err != nil {
					return User{}, false, false
				}
				return user, isAdmin, true
			}
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

// RequireAdmin is the strict variant for admin-only routes. Each path
// applies its own admin policy: K8s.RequireAdmin checks the SA
// allowlist, RomaineLifeJWT.RequireAdmin accepts role ∈ {admin,
// service}, CookieDelegate.RequireAdmin accepts role=admin.
func (a CompositeAuthenticator) RequireAdmin(ctx context.Context, r *http.Request) (User, error) {
	if r == nil {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "no request"}
	}

	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
		token := strings.TrimSpace(authz[7:])
		if token != "" {
			if LooksLikeK8sSAToken(token) {
				if a.K8s == nil {
					return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "k8s auth not configured"}
				}
				return a.K8s.RequireAdmin(ctx, token)
			}
			if a.Romaine == nil {
				return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "auth.romaine.life JWT verifier not configured"}
			}
			return a.Romaine.RequireAdmin(ctx, token)
		}
	}

	if a.Cookie == nil {
		return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "auth.romaine.life delegate not configured"}
	}
	return a.Cookie.RequireAdmin(ctx, r.Header.Get("Cookie"))
}

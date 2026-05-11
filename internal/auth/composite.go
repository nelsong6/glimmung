package auth

import (
	"context"
	"strings"
)

type CompositeAuthenticator struct {
	Entra *EntraAuthenticator
	K8s   *K8sAuthenticator
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

	if a.Entra == nil {
		return User{}, false, false
	}
	user, isAdmin, err := a.Entra.Resolve(ctx, token)
	if err != nil {
		return User{}, false, false
	}
	return user, isAdmin, true
}

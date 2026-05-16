package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCompositeResolverSoftensInvalidTokens(t *testing.T) {
	resolver := CompositeAuthenticator{}
	_, _, ok := resolver.ResolveCaller(context.Background(), "Bearer invalid")
	if ok {
		t.Fatal("invalid token should not resolve")
	}
}

func TestCompositeResolverResolvesRomaineLifeUser(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newRomaineJWKSServer(t, key)
	defer jwks.Close()
	romaineLife := newTestRomaineLifeAuthenticator(jwks.URL)
	resolver := CompositeAuthenticator{RomaineLife: romaineLife}

	token := signRomaineToken(t, key, map[string]any{
		"iss":   authRomaineLifeIssuer,
		"sub":   "subject",
		"email": "user@example.com",
		"role":  "user",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	user, isAdmin, ok := resolver.ResolveCaller(context.Background(), "Bearer "+token)
	if !ok || isAdmin || user.Email != "user@example.com" {
		t.Fatalf("user=%#v isAdmin=%v ok=%v", user, isAdmin, ok)
	}
}

func TestCompositeResolverResolvesRomaineLifeAdmin(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newRomaineJWKSServer(t, key)
	defer jwks.Close()
	romaineLife := newTestRomaineLifeAuthenticator(jwks.URL)
	resolver := CompositeAuthenticator{RomaineLife: romaineLife}

	token := signRomaineToken(t, key, map[string]any{
		"iss":   authRomaineLifeIssuer,
		"sub":   "subject",
		"email": "admin@example.com",
		"role":  "admin",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	user, isAdmin, ok := resolver.ResolveCaller(context.Background(), "Bearer "+token)
	if !ok || !isAdmin || user.Email != "admin@example.com" {
		t.Fatalf("user=%#v isAdmin=%v ok=%v", user, isAdmin, ok)
	}
}

func TestCompositeResolverResolvesK8sAdminState(t *testing.T) {
	tokenReview := newTokenReviewServer(t, http.StatusOK, tokenReviewResponse{
		Status: tokenReviewStatus{
			Authenticated: true,
			User:          tokenReviewUser{Username: "system:serviceaccount:ns:sa"},
		},
	})
	defer tokenReview.Close()
	k8s := newTestAuthenticator(t, tokenReview.URL, "ns/sa")
	resolver := CompositeAuthenticator{K8s: k8s}

	user, isAdmin, ok := resolver.ResolveCaller(context.Background(), "Bearer "+jwtWithClaims(t, map[string]any{
		"kubernetes.io": map[string]any{"namespace": "ns"},
	}))
	if !ok || !isAdmin || user.Email != "system:serviceaccount:ns:sa" {
		t.Fatalf("user=%#v isAdmin=%v ok=%v", user, isAdmin, ok)
	}
}

func TestCompositeResolverTreatsRejectedK8sTokenAsUnsigned(t *testing.T) {
	tokenReview := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":{"authenticated":false}}`))
	}))
	defer tokenReview.Close()
	k8s := newTestAuthenticator(t, tokenReview.URL, "ns/sa")
	resolver := CompositeAuthenticator{K8s: k8s}

	_, _, ok := resolver.ResolveCaller(context.Background(), "Bearer "+jwtWithClaims(t, map[string]any{
		"kubernetes.io": map[string]any{"namespace": "ns"},
	}))
	if ok {
		t.Fatal("rejected token should not resolve")
	}
}

func TestCompositeRequireAdminRoutesMissingAndUnconfigured(t *testing.T) {
	resolver := CompositeAuthenticator{}

	_, err := resolver.RequireAdmin(context.Background(), "")
	assertAuthStatus(t, err, http.StatusUnauthorized, "missing bearer")

	_, err = resolver.RequireAdmin(context.Background(), "Bearer plain-token")
	assertAuthStatus(t, err, http.StatusServiceUnavailable, "auth.romaine.life auth not configured")

	_, err = resolver.RequireAdmin(context.Background(), "Bearer "+jwtWithClaims(t, map[string]any{
		"kubernetes.io": map[string]any{"namespace": "ns"},
	}))
	assertAuthStatus(t, err, http.StatusServiceUnavailable, "k8s auth not configured")
}

func TestCompositeRequireAdminRoutesRomaineLifeAndK8s(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newRomaineJWKSServer(t, key)
	defer jwks.Close()
	romaineLife := newTestRomaineLifeAuthenticator(jwks.URL)
	romaineToken := signRomaineToken(t, key, map[string]any{
		"iss":   authRomaineLifeIssuer,
		"sub":   "subject",
		"email": "admin@example.com",
		"role":  "admin",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	tokenReview := newTokenReviewServer(t, http.StatusOK, tokenReviewResponse{
		Status: tokenReviewStatus{
			Authenticated: true,
			User:          tokenReviewUser{Username: "system:serviceaccount:ns:sa"},
		},
	})
	defer tokenReview.Close()
	k8s := newTestAuthenticator(t, tokenReview.URL, "ns/sa")

	resolver := CompositeAuthenticator{RomaineLife: romaineLife, K8s: k8s}
	user, err := resolver.RequireAdmin(context.Background(), "Bearer "+romaineToken)
	if err != nil || user.Email != "admin@example.com" {
		t.Fatalf("romaine.life user=%#v err=%v", user, err)
	}

	user, err = resolver.RequireAdmin(context.Background(), "Bearer "+jwtWithClaims(t, map[string]any{
		"kubernetes.io": map[string]any{"namespace": "ns"},
	}))
	if err != nil || user.Email != "system:serviceaccount:ns:sa" {
		t.Fatalf("k8s user=%#v err=%v", user, err)
	}
}

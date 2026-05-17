package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompositeResolverSoftensInvalidTokens(t *testing.T) {
	resolver := CompositeAuthenticator{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer invalid")
	_, _, ok := resolver.ResolveCaller(context.Background(), req)
	if ok {
		t.Fatal("invalid token should not resolve")
	}
}

func TestCompositeResolverResolvesCookieUser(t *testing.T) {
	srv := newFakeAuthServer(t, map[string]string{
		"good-cookie": `{"user":{"id":"sub-user","email":"user@example.com","name":"User","role":"user"}}`,
	})
	defer srv.Close()
	resolver := CompositeAuthenticator{Cookie: newTestCookieDelegate(srv.URL)}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", "good-cookie")
	user, isAdmin, ok := resolver.ResolveCaller(context.Background(), req)
	if !ok || isAdmin || user.Email != "user@example.com" {
		t.Fatalf("user=%#v isAdmin=%v ok=%v", user, isAdmin, ok)
	}
}

func TestCompositeResolverResolvesCookieAdmin(t *testing.T) {
	srv := newFakeAuthServer(t, map[string]string{
		"admin-cookie": `{"user":{"id":"sub-admin","email":"admin@example.com","name":"Admin","role":"admin"}}`,
	})
	defer srv.Close()
	resolver := CompositeAuthenticator{Cookie: newTestCookieDelegate(srv.URL)}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", "admin-cookie")
	user, isAdmin, ok := resolver.ResolveCaller(context.Background(), req)
	if !ok || !isAdmin || user.Email != "admin@example.com" {
		t.Fatalf("user=%#v isAdmin=%v ok=%v", user, isAdmin, ok)
	}
}

func TestCompositeResolverRejectsPendingCookie(t *testing.T) {
	srv := newFakeAuthServer(t, map[string]string{
		"pending-cookie": `{"user":{"id":"sub-pending","email":"pending@example.com","role":"pending"}}`,
	})
	defer srv.Close()
	resolver := CompositeAuthenticator{Cookie: newTestCookieDelegate(srv.URL)}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", "pending-cookie")
	_, _, ok := resolver.ResolveCaller(context.Background(), req)
	if ok {
		t.Fatal("pending role should not resolve")
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

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+jwtWithClaims(t, map[string]any{
		"kubernetes.io": map[string]any{"namespace": "ns"},
	}))
	user, isAdmin, ok := resolver.ResolveCaller(context.Background(), req)
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

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+jwtWithClaims(t, map[string]any{
		"kubernetes.io": map[string]any{"namespace": "ns"},
	}))
	_, _, ok := resolver.ResolveCaller(context.Background(), req)
	if ok {
		t.Fatal("rejected token should not resolve")
	}
}

func TestCompositeRequireAdminRoutesMissingAndUnconfigured(t *testing.T) {
	resolver := CompositeAuthenticator{}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := resolver.RequireAdmin(context.Background(), req)
	assertAuthStatus(t, err, http.StatusServiceUnavailable, "auth.romaine.life delegate not configured")

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+jwtWithClaims(t, map[string]any{
		"kubernetes.io": map[string]any{"namespace": "ns"},
	}))
	_, err = resolver.RequireAdmin(context.Background(), req)
	assertAuthStatus(t, err, http.StatusServiceUnavailable, "k8s auth not configured")
}

func TestCompositeRequireAdminRoutesCookieAndK8s(t *testing.T) {
	authSrv := newFakeAuthServer(t, map[string]string{
		"admin-cookie": `{"user":{"id":"sub-admin","email":"admin@example.com","name":"Admin","role":"admin"}}`,
	})
	defer authSrv.Close()
	delegate := newTestCookieDelegate(authSrv.URL)

	tokenReview := newTokenReviewServer(t, http.StatusOK, tokenReviewResponse{
		Status: tokenReviewStatus{
			Authenticated: true,
			User:          tokenReviewUser{Username: "system:serviceaccount:ns:sa"},
		},
	})
	defer tokenReview.Close()
	k8s := newTestAuthenticator(t, tokenReview.URL, "ns/sa")

	resolver := CompositeAuthenticator{Cookie: delegate, K8s: k8s}

	cookieReq := httptest.NewRequest(http.MethodGet, "/", nil)
	cookieReq.Header.Set("Cookie", "admin-cookie")
	user, err := resolver.RequireAdmin(context.Background(), cookieReq)
	if err != nil || user.Email != "admin@example.com" {
		t.Fatalf("cookie user=%#v err=%v", user, err)
	}

	k8sReq := httptest.NewRequest(http.MethodGet, "/", nil)
	k8sReq.Header.Set("Authorization", "Bearer "+jwtWithClaims(t, map[string]any{
		"kubernetes.io": map[string]any{"namespace": "ns"},
	}))
	user, err = resolver.RequireAdmin(context.Background(), k8sReq)
	if err != nil || user.Email != "system:serviceaccount:ns:sa" {
		t.Fatalf("k8s user=%#v err=%v", user, err)
	}
}

// newFakeAuthServer returns the canned body keyed by Cookie header for
// known cookies, or `null` for unknown ones (matching Better Auth's
// actual behavior on missing/invalid sessions).
func newFakeAuthServer(t *testing.T, responses map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if body, ok := responses[r.Header.Get("Cookie")]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte("null"))
	}))
}

func newTestCookieDelegate(endpoint string) *CookieDelegate {
	d := NewCookieDelegate()
	d.endpoint = endpoint
	return d
}

func TestCompositeResolverResolvesRomaineJWT(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	resolver := CompositeAuthenticator{Romaine: v}

	token := f.sign(adminClaims(f.issuer()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	user, isAdmin, ok := resolver.ResolveCaller(context.Background(), req)
	if !ok || !isAdmin || user.Email != "admin@example.com" {
		t.Fatalf("user=%#v isAdmin=%v ok=%v", user, isAdmin, ok)
	}
}

func TestCompositeRequireAdminAcceptsRomaineJWT(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	resolver := CompositeAuthenticator{Romaine: v}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+f.sign(adminClaims(f.issuer())))
	user, err := resolver.RequireAdmin(context.Background(), req)
	if err != nil || user.Email != "admin@example.com" || user.Role != RomaineRoleAdmin {
		t.Fatalf("user=%#v err=%v", user, err)
	}
}

func TestCompositeRequireAdminAcceptsServiceJWT(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	resolver := CompositeAuthenticator{Romaine: v}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+f.sign(serviceClaims(f.issuer())))
	user, err := resolver.RequireAdmin(context.Background(), req)
	if err != nil || user.Role != RomaineRoleService || user.ActorEmail != "operator@example.com" {
		t.Fatalf("user=%#v err=%v", user, err)
	}
}

func TestCompositeRequireAdminBearerRequiresRomaineVerifier(t *testing.T) {
	// Bearer JWT that's NOT a K8s SA token, but Romaine isn't wired —
	// should surface as a configuration error rather than silently
	// falling through to cookie auth (cookies don't accept Bearer).
	resolver := CompositeAuthenticator{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+jwtWithClaims(t, map[string]any{
		"iss":  "https://auth.romaine.life",
		"role": "admin",
	}))
	_, err := resolver.RequireAdmin(context.Background(), req)
	assertAuthStatus(t, err, http.StatusServiceUnavailable, "auth.romaine.life JWT verifier not configured")
}

package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestRomaineLifeRequireAdminAcceptsAdminRole(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newRomaineJWKSServer(t, key)
	defer jwks.Close()

	authenticator := newTestRomaineLifeAuthenticator(jwks.URL)
	token := signRomaineToken(t, key, map[string]any{
		"iss":   authRomaineLifeIssuer,
		"sub":   "subject",
		"email": "person@example.com",
		"name":  "Person",
		"role":  "admin",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	user, err := authenticator.RequireAdmin(context.Background(), token)
	if err != nil {
		t.Fatalf("RequireAdmin returned error: %v", err)
	}
	if user.Email != "person@example.com" || user.Sub != "subject" || user.Name != "Person" {
		t.Fatalf("user=%#v", user)
	}
}

func TestRomaineLifeRequireAdminRejectsUserRole(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newRomaineJWKSServer(t, key)
	defer jwks.Close()

	authenticator := newTestRomaineLifeAuthenticator(jwks.URL)
	token := signRomaineToken(t, key, map[string]any{
		"iss":   authRomaineLifeIssuer,
		"sub":   "subject",
		"email": "person@example.com",
		"role":  "user",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	_, err := authenticator.RequireAdmin(context.Background(), token)
	assertAuthStatus(t, err, http.StatusForbidden, "admin role required")
}

func TestRomaineLifeRequireAdminRejectsPendingRole(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newRomaineJWKSServer(t, key)
	defer jwks.Close()

	authenticator := newTestRomaineLifeAuthenticator(jwks.URL)
	token := signRomaineToken(t, key, map[string]any{
		"iss":   authRomaineLifeIssuer,
		"sub":   "subject",
		"email": "person@example.com",
		"role":  "pending",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	_, err := authenticator.RequireAdmin(context.Background(), token)
	assertAuthStatus(t, err, http.StatusForbidden, "role not approved")
}

func TestRomaineLifeResolveSurfacesIsAdmin(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newRomaineJWKSServer(t, key)
	defer jwks.Close()

	authenticator := newTestRomaineLifeAuthenticator(jwks.URL)

	// admin role -> isAdmin true
	adminToken := signRomaineToken(t, key, map[string]any{
		"iss":   authRomaineLifeIssuer,
		"sub":   "admin-sub",
		"email": "admin@example.com",
		"role":  "admin",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	user, isAdmin, err := authenticator.Resolve(context.Background(), adminToken)
	if err != nil {
		t.Fatalf("Resolve(admin) returned error: %v", err)
	}
	if !isAdmin || user.Email != "admin@example.com" {
		t.Fatalf("admin: user=%#v isAdmin=%v", user, isAdmin)
	}

	// user role -> isAdmin false but resolves
	userToken := signRomaineToken(t, key, map[string]any{
		"iss":   authRomaineLifeIssuer,
		"sub":   "user-sub",
		"email": "user@example.com",
		"role":  "user",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	user, isAdmin, err = authenticator.Resolve(context.Background(), userToken)
	if err != nil {
		t.Fatalf("Resolve(user) returned error: %v", err)
	}
	if isAdmin || user.Email != "user@example.com" {
		t.Fatalf("user: user=%#v isAdmin=%v", user, isAdmin)
	}

	// pending role -> rejected by Resolve too (so /v1/auth/me returns 403 not 200+is_admin=false)
	pendingToken := signRomaineToken(t, key, map[string]any{
		"iss":   authRomaineLifeIssuer,
		"sub":   "pending-sub",
		"email": "pending@example.com",
		"role":  "pending",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	_, _, err = authenticator.Resolve(context.Background(), pendingToken)
	assertAuthStatus(t, err, http.StatusForbidden, "role not approved")
}

func TestRomaineLifeRejectsBadIssuer(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newRomaineJWKSServer(t, key)
	defer jwks.Close()

	authenticator := newTestRomaineLifeAuthenticator(jwks.URL)
	token := signRomaineToken(t, key, map[string]any{
		"iss":   "https://login.microsoftonline.com/tenant/v2.0",
		"sub":   "subject",
		"email": "person@example.com",
		"role":  "admin",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	_, err := authenticator.RequireAdmin(context.Background(), token)
	assertAuthStatus(t, err, http.StatusUnauthorized, "unexpected issuer")
}

func TestRomaineLifeRejectsMissingEmail(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newRomaineJWKSServer(t, key)
	defer jwks.Close()

	authenticator := newTestRomaineLifeAuthenticator(jwks.URL)
	token := signRomaineToken(t, key, map[string]any{
		"iss":  authRomaineLifeIssuer,
		"sub":  "subject",
		"role": "admin",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})

	_, err := authenticator.RequireAdmin(context.Background(), token)
	assertAuthStatus(t, err, http.StatusUnauthorized, "missing email")
}

func TestRomaineLifeRejectsUnknownSigningKey(t *testing.T) {
	key := mustRSAKey(t)
	otherKey := mustRSAKey(t)
	jwks := newRomaineJWKSServer(t, key)
	defer jwks.Close()

	authenticator := newTestRomaineLifeAuthenticator(jwks.URL)
	token := signRomaineTokenWithKid(t, otherKey, "other-key", map[string]any{
		"iss":   authRomaineLifeIssuer,
		"sub":   "subject",
		"email": "person@example.com",
		"role":  "admin",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	_, err := authenticator.RequireAdmin(context.Background(), token)
	assertAuthStatus(t, err, http.StatusUnauthorized, "unknown kid")
}

// Test helpers — extracted from the deleted entra_test.go since composite_test
// shares them.

func newTestRomaineLifeAuthenticator(jwksURL string) *RomaineLifeAuthenticator {
	a := NewRomaineLifeAuthenticator()
	a.jwksURL = jwksURL
	return a
}

func newRomaineJWKSServer(t *testing.T, key *rsa.PrivateKey) *httptest.Server {
	t.Helper()
	keyID := "test-key"
	body := romaineJWKSResponse{Keys: []romaineJWK{{
		Kid: keyID,
		Kty: "RSA",
		N:   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func signRomaineToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	return signRomaineTokenWithKid(t, key, "test-key", claims)
}

func signRomaineTokenWithKid(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(claims))
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

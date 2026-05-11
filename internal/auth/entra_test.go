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
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAllowedEmails(t *testing.T) {
	allowed := AllowedEmails(" A@example.com, b@example.com ,, ")
	if _, ok := allowed["a@example.com"]; !ok {
		t.Fatalf("normalized email missing: %#v", allowed)
	}
	if _, ok := allowed["b@example.com"]; !ok {
		t.Fatalf("email missing: %#v", allowed)
	}
}

func TestEntraRequireAdminAcceptsAllowedEmail(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newJWKSServer(t, key)
	defer jwks.Close()

	authenticator := newTestEntraAuthenticator(t, jwks.URL, "client-id", "person@example.com")
	token := signEntraToken(t, key, map[string]any{
		"aud":   "client-id",
		"iss":   "https://login.microsoftonline.com/tenant/v2.0",
		"sub":   "subject",
		"email": "person@example.com",
		"name":  "Person",
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

func TestEntraRequireAdminUsesPreferredUsername(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newJWKSServer(t, key)
	defer jwks.Close()

	authenticator := newTestEntraAuthenticator(t, jwks.URL, "client-id", "person@example.com")
	token := signEntraToken(t, key, map[string]any{
		"aud":                "client-id",
		"iss":                "https://login.microsoftonline.com/tenant/v2.0",
		"sub":                "subject",
		"preferred_username": "person@example.com",
		"exp":                time.Now().Add(time.Hour).Unix(),
	})

	user, err := authenticator.RequireAdmin(context.Background(), token)
	if err != nil {
		t.Fatalf("RequireAdmin returned error: %v", err)
	}
	if user.Email != "person@example.com" {
		t.Fatalf("user=%#v", user)
	}
}

func TestEntraRequireAdminRejectsInvalidClaims(t *testing.T) {
	key := mustRSAKey(t)
	jwks := newJWKSServer(t, key)
	defer jwks.Close()
	authenticator := newTestEntraAuthenticator(t, jwks.URL, "client-id", "person@example.com")

	tests := []struct {
		name     string
		claims   map[string]any
		wantCode int
		wantText string
	}{
		{
			name: "wrong audience",
			claims: map[string]any{
				"aud": "other", "iss": "https://login.microsoftonline.com/tenant/v2.0",
				"email": "person@example.com", "exp": time.Now().Add(time.Hour).Unix(),
			},
			wantCode: http.StatusUnauthorized,
			wantText: "aud",
		},
		{
			name: "wrong issuer",
			claims: map[string]any{
				"aud": "client-id", "iss": "https://issuer.example.com",
				"email": "person@example.com", "exp": time.Now().Add(time.Hour).Unix(),
			},
			wantCode: http.StatusUnauthorized,
			wantText: "unexpected issuer",
		},
		{
			name: "missing email",
			claims: map[string]any{
				"aud": "client-id", "iss": "https://login.microsoftonline.com/tenant/v2.0",
				"exp": time.Now().Add(time.Hour).Unix(),
			},
			wantCode: http.StatusUnauthorized,
			wantText: "no email",
		},
		{
			name: "disallowed email",
			claims: map[string]any{
				"aud": "client-id", "iss": "https://login.microsoftonline.com/tenant/v2.0",
				"email": "other@example.com", "exp": time.Now().Add(time.Hour).Unix(),
			},
			wantCode: http.StatusForbidden,
			wantText: "email not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := authenticator.RequireAdmin(context.Background(), signEntraToken(t, key, tt.claims))
			assertAuthStatus(t, err, tt.wantCode, tt.wantText)
		})
	}
}

func TestNewEntraAuthenticatorRequiresConfig(t *testing.T) {
	_, err := NewEntraAuthenticator(EntraConfig{AllowedEmails: "person@example.com"})
	assertAuthStatus(t, err, http.StatusServiceUnavailable, "ENTRA_CLIENT_ID")

	authenticator, err := NewEntraAuthenticator(EntraConfig{Audiences: []string{"client-id"}})
	if err != nil {
		t.Fatalf("empty allowlist should be allowed for /auth/me resolver: %v", err)
	}
	key := mustRSAKey(t)
	jwks := newJWKSServer(t, key)
	defer jwks.Close()
	authenticator.jwksURL = jwks.URL
	_, err = authenticator.RequireAdmin(context.Background(), signEntraToken(t, key, map[string]any{
		"aud":   "client-id",
		"iss":   "https://login.microsoftonline.com/tenant/v2.0",
		"email": "person@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}))
	assertAuthStatus(t, err, http.StatusServiceUnavailable, "ALLOWED_EMAILS")
}

func TestEntraRejectsUnknownSigningKey(t *testing.T) {
	key := mustRSAKey(t)
	otherKey := mustRSAKey(t)
	jwks := newJWKSServer(t, key)
	defer jwks.Close()

	authenticator := newTestEntraAuthenticator(t, jwks.URL, "client-id", "person@example.com")
	token := signEntraTokenWithKid(t, otherKey, "other-key", map[string]any{
		"aud":   "client-id",
		"iss":   "https://login.microsoftonline.com/tenant/v2.0",
		"email": "person@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	_, err := authenticator.RequireAdmin(context.Background(), token)
	assertAuthStatus(t, err, http.StatusUnauthorized, "unknown signing key")
}

func newTestEntraAuthenticator(t *testing.T, jwksURL string, audience string, allowedEmails string) *EntraAuthenticator {
	t.Helper()
	authenticator, err := NewEntraAuthenticator(EntraConfig{
		JWKSURL:       jwksURL,
		Audiences:     []string{audience},
		AllowedEmails: allowedEmails,
	})
	if err != nil {
		t.Fatalf("NewEntraAuthenticator returned error: %v", err)
	}
	return authenticator
}

func newJWKSServer(t *testing.T, key *rsa.PrivateKey) *httptest.Server {
	t.Helper()
	keyID := "test-key"
	body := jwksResponse{Keys: []jwk{{
		Kid: keyID,
		Kty: "RSA",
		N:   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func signEntraToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	return signEntraTokenWithKid(t, key, "test-key", claims)
}

func signEntraTokenWithKid(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
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

func TestErrStringFallback(t *testing.T) {
	if got := errString(nil); !strings.Contains(got, "invalid") {
		t.Fatalf("errString(nil)=%q", got)
	}
}

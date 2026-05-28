package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// romaineJWKSFixture spins up a fake JWKS server and gives the test a
// signing helper so it can mint tokens that the verifier will accept.
// The JWKS endpoint records call counts so cache-behavior tests can
// assert "only fetched once" without time-sensitive flakes.
type romaineJWKSFixture struct {
	t           *testing.T
	privateKey  *rsa.PrivateKey
	kid         string
	server      *httptest.Server
	fetchCount  atomic.Int64
	emptyOnNext atomic.Bool
}

func newRomaineJWKSFixture(t *testing.T) *romaineJWKSFixture {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	f := &romaineJWKSFixture{t: t, privateKey: priv, kid: "test-kid-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/jwks", func(w http.ResponseWriter, r *http.Request) {
		f.fetchCount.Add(1)
		if f.emptyOnNext.Swap(false) {
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
			return
		}
		n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
		// Standard RSA exponent 65537 → big-endian {0x01, 0x00, 0x01}.
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []any{
				map[string]any{"kid": f.kid, "kty": "RSA", "alg": "RS256", "use": "sig", "n": n, "e": e},
			},
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *romaineJWKSFixture) issuer() string  { return f.server.URL }
func (f *romaineJWKSFixture) jwksURL() string { return f.server.URL + "/api/auth/jwks" }

func (f *romaineJWKSFixture) sign(claims jwt.MapClaims) string {
	f.t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = f.kid
	signed, err := token.SignedString(f.privateKey)
	if err != nil {
		f.t.Fatalf("sign token: %v", err)
	}
	return signed
}

func (f *romaineJWKSFixture) signKid(claims jwt.MapClaims, kid string) string {
	f.t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(f.privateKey)
	if err != nil {
		f.t.Fatalf("sign token: %v", err)
	}
	return signed
}

func adminClaims(issuer string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   issuer,
		"sub":   "user-admin-1",
		"email": "ADMIN@example.com",
		"name":  "Admin User",
		"role":  RomaineRoleAdmin,
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
}

func serviceClaims(issuer string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":         issuer,
		"sub":         "tank-operator-mcp",
		"role":        RomaineRoleService,
		"actor_email": "Operator@example.com",
		"exp":         time.Now().Add(time.Hour).Unix(),
	}
}

func TestRomaineLifeJWTVerifierAcceptsAdmin(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	token := f.sign(adminClaims(f.issuer()))

	user, isAdmin, err := v.Resolve(context.Background(), token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !isAdmin {
		t.Fatal("admin role should resolve as admin")
	}
	if user.Email != "admin@example.com" {
		t.Errorf("Email=%q, want lowercased admin@example.com", user.Email)
	}
	if user.Role != RomaineRoleAdmin {
		t.Errorf("Role=%q, want admin", user.Role)
	}
	if !user.IsAdmin() {
		t.Error("IsAdmin() should be true")
	}
	if user.IsService() {
		t.Error("IsService() should be false for admin token")
	}
}

func TestRomaineLifeJWTVerifierAcceptsService(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	token := f.sign(serviceClaims(f.issuer()))

	user, isAdmin, err := v.Resolve(context.Background(), token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !isAdmin {
		t.Fatal("service role should pass the admin gate (acts on behalf of admin-equivalent identity)")
	}
	if user.Role != RomaineRoleService {
		t.Errorf("Role=%q, want service", user.Role)
	}
	if user.ActorEmail != "operator@example.com" {
		t.Errorf("ActorEmail=%q, want lowercased operator@example.com", user.ActorEmail)
	}
	if !user.IsService() {
		t.Error("IsService() should be true")
	}
	if user.IsAdmin() {
		t.Error("IsAdmin() should be false for service token (use IsService instead)")
	}
}

func TestRomaineLifeJWTVerifierServiceWithoutActorEmail401(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	claims := serviceClaims(f.issuer())
	delete(claims, "actor_email")
	token := f.sign(claims)

	_, _, err := v.Resolve(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for service token missing actor_email")
	}
	var authErr AuthError
	if !errors.As(err, &authErr) || authErr.Status != http.StatusUnauthorized {
		t.Fatalf("err=%v, want AuthError 401", err)
	}
}

func TestRomaineLifeJWTVerifierRejectsUnknownRole(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	claims := adminClaims(f.issuer())
	claims["role"] = "pending"
	token := f.sign(claims)

	_, _, err := v.Resolve(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for role=pending")
	}
	var authErr AuthError
	if !errors.As(err, &authErr) || authErr.Status != http.StatusForbidden {
		t.Fatalf("err=%v, want AuthError 403", err)
	}
}

func TestRomaineLifeJWTVerifierRejectsWrongIssuer(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	claims := adminClaims("https://impostor.example")
	token := f.sign(claims)

	_, _, err := v.Resolve(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
	var authErr AuthError
	if !errors.As(err, &authErr) || authErr.Status != http.StatusUnauthorized {
		t.Fatalf("err=%v, want AuthError 401", err)
	}
}

func TestRomaineLifeJWTVerifierHintsAtExchangeWhenK8sSATokenPresented(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	claims := adminClaims("https://westus2.oic.prod-aks.azure.com/2236b5e4-81d2-4d82-bde5-17b1037999ea/5aced6d5-4299-421b-84a9-6638aebbf4f0/")
	claims["kubernetes.io"] = map[string]any{
		"namespace": "tank-operator-sessions",
		"serviceaccount": map[string]any{
			"name": "claude-session",
		},
	}
	token := f.sign(claims)

	_, _, err := v.Resolve(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for k8s SA token")
	}
	var authErr AuthError
	if !errors.As(err, &authErr) || authErr.Status != http.StatusUnauthorized {
		t.Fatalf("err=%v, want AuthError 401", err)
	}
	if !strings.Contains(authErr.Message, "/api/auth/exchange/k8s") {
		t.Fatalf("err=%q, want exchange hint", authErr.Message)
	}
}

func TestRomaineLifeJWTVerifierWrongIssuerWithoutK8sClaimHasNoHint(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	claims := adminClaims("https://impostor.example")
	token := f.sign(claims)

	_, _, err := v.Resolve(context.Background(), token)
	var authErr AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err=%v, want AuthError", err)
	}
	if strings.Contains(authErr.Message, "/api/auth/exchange/k8s") {
		t.Fatalf("err=%q, should not include exchange hint for plain wrong-issuer token", authErr.Message)
	}
}

func TestRomaineLifeJWTVerifierRejectsExpired(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	claims := adminClaims(f.issuer())
	claims["exp"] = time.Now().Add(-10 * time.Minute).Unix()
	token := f.sign(claims)

	_, _, err := v.Resolve(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	var authErr AuthError
	if !errors.As(err, &authErr) || authErr.Status != http.StatusUnauthorized {
		t.Fatalf("err=%v, want AuthError 401", err)
	}
}

func TestRomaineLifeJWTVerifierRejectsUnknownKid(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	token := f.signKid(adminClaims(f.issuer()), "not-a-real-kid")

	_, _, err := v.Resolve(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for unknown kid")
	}
	var authErr AuthError
	if !errors.As(err, &authErr) || authErr.Status != http.StatusUnauthorized {
		t.Fatalf("err=%v, want AuthError 401", err)
	}
}

func TestRomaineLifeJWTVerifierCachesJWKSFetches(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	token := f.sign(adminClaims(f.issuer()))

	for i := 0; i < 5; i++ {
		if _, _, err := v.Resolve(context.Background(), token); err != nil {
			t.Fatalf("Resolve call %d: %v", i, err)
		}
	}
	if got := f.fetchCount.Load(); got != 1 {
		t.Fatalf("JWKS fetches=%d, want 1 (cache should serve subsequent verifies)", got)
	}
}

func TestRomaineLifeJWTVerifierRequireAdminRejectsUser(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	claims := adminClaims(f.issuer())
	claims["role"] = RomaineRoleUser
	token := f.sign(claims)

	_, err := v.RequireAdmin(context.Background(), token)
	if err == nil {
		t.Fatal("expected RequireAdmin to reject role=user")
	}
	var authErr AuthError
	if !errors.As(err, &authErr) || authErr.Status != http.StatusForbidden {
		t.Fatalf("err=%v, want 403", err)
	}
}

func TestRomaineLifeJWTVerifierRejectsTokenSignedByDifferentKey(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())

	// Sign a token with an attacker-controlled key but advertise the
	// fixture's kid in the header. The verifier should fetch the
	// fixture's public key (matching kid), try to verify our signature
	// against it, and fail because the public key doesn't pair with the
	// attacker's private key.
	attacker, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("attacker key: %v", err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, adminClaims(f.issuer()))
	token.Header["kid"] = f.kid
	signed, err := token.SignedString(attacker)
	if err != nil {
		t.Fatalf("sign with attacker key: %v", err)
	}

	_, _, err = v.Resolve(context.Background(), signed)
	if err == nil {
		t.Fatal("expected signature mismatch to be rejected")
	}
	var authErr AuthError
	if !errors.As(err, &authErr) || authErr.Status != http.StatusUnauthorized {
		t.Fatalf("err=%v, want 401", err)
	}
}

func TestRomaineLifeJWTVerifierAudienceGating(t *testing.T) {
	f := newRomaineJWKSFixture(t)
	v := NewRomaineLifeJWTVerifierForTesting(f.issuer(), f.jwksURL(), f.server.Client())
	v.expectedAudience = "glimmung"

	with := f.sign(jwt.MapClaims{
		"iss":   f.issuer(),
		"sub":   "u",
		"email": "a@b",
		"role":  RomaineRoleAdmin,
		"aud":   []any{"glimmung", "other"},
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	if _, _, err := v.Resolve(context.Background(), with); err != nil {
		t.Fatalf("token with matching audience: %v", err)
	}

	without := f.sign(jwt.MapClaims{
		"iss":   f.issuer(),
		"sub":   "u",
		"email": "a@b",
		"role":  RomaineRoleAdmin,
		"aud":   "other",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	if _, _, err := v.Resolve(context.Background(), without); err == nil {
		t.Fatal("token without matching audience should be rejected when expectedAudience is set")
	}
}

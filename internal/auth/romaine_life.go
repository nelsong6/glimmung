package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// auth.romaine.life is the single upstream identity provider. Its Better
// Auth JWT plugin publishes RS256 keys at /api/auth/jwks; the issuer claim
// is the service's baseURL.
const (
	authRomaineLifeJWKSURL = "https://auth.romaine.life/api/auth/jwks"
	authRomaineLifeIssuer  = "https://auth.romaine.life"
	romaineJWKSCacheTTL    = 10 * time.Minute
	romaineJWKSHTTPTimeout = 10 * time.Second
)

// allowedRoles gates access. auth.romaine.life mints `role: pending` for any
// fresh Microsoft sign-in; an admin must promote the user via
// auth.romaine.life/admin before they become useful here. `admin` and `user`
// resolve to a User; anything else is rejected.
//
// isAdmin separates the two within the allowed set: `admin` claims map to
// glimmung-admin (write endpoints), `user` claims map to glimmung-viewer
// (read-only). Same admin/viewer split as the legacy per-app allowlist, but
// the gate itself moves to the platform identity service.
var allowedRoles = map[string]bool{
	"admin": true, // isAdmin = true
	"user":  false,
}

type RomaineLifeAuthenticator struct {
	jwksURL    string
	issuer     string
	httpClient *http.Client
	cache      *romaineJWKSCache
}

func NewRomaineLifeAuthenticator() *RomaineLifeAuthenticator {
	client := &http.Client{Timeout: romaineJWKSHTTPTimeout}
	return &RomaineLifeAuthenticator{
		jwksURL:    authRomaineLifeJWKSURL,
		issuer:     authRomaineLifeIssuer,
		httpClient: client,
		cache:      &romaineJWKSCache{httpClient: client},
	}
}

// Resolve verifies a JWT and returns the user identity plus an isAdmin
// signal. Used by /v1/auth/me and other soft auth checks where a valid
// non-admin signed-in user is still useful (their identity surfaces; admin
// actions render disabled, not absent).
func (a *RomaineLifeAuthenticator) Resolve(ctx context.Context, token string) (User, bool, error) {
	user, role, err := a.verifyAndExtract(ctx, token)
	if err != nil {
		return User{}, false, err
	}
	return user, allowedRoles[role], nil
}

// RequireAdmin verifies a JWT and gates on role == "admin". 403s anything
// else (including a valid `user`-role token).
func (a *RomaineLifeAuthenticator) RequireAdmin(ctx context.Context, token string) (User, error) {
	user, role, err := a.verifyAndExtract(ctx, token)
	if err != nil {
		return User{}, err
	}
	if role != "admin" {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "admin role required"}
	}
	return user, nil
}

func (a *RomaineLifeAuthenticator) verifyAndExtract(ctx context.Context, token string) (User, string, error) {
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != "RS256" {
			return nil, fmt.Errorf("unexpected alg: %s", t.Method.Alg())
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token missing kid")
		}
		return a.cache.getKey(ctx, a.jwksURL, kid)
	}, jwt.WithLeeway(60*time.Second))
	if err != nil || !parsed.Valid {
		if err == nil {
			err = errors.New("invalid token")
		}
		return User{}, "", AuthError{Status: http.StatusUnauthorized, Message: "invalid token: " + err.Error()}
	}

	iss := firstClaimString(claims, "iss")
	if iss != a.issuer {
		return User{}, "", AuthError{Status: http.StatusUnauthorized, Message: "unexpected issuer: " + iss}
	}

	role := firstClaimString(claims, "role")
	if _, ok := allowedRoles[role]; !ok {
		return User{}, "", AuthError{Status: http.StatusForbidden, Message: "role not approved by auth.romaine.life: " + role}
	}

	email := strings.ToLower(strings.TrimSpace(firstClaimString(claims, "email")))
	if email == "" {
		return User{}, "", AuthError{Status: http.StatusUnauthorized, Message: "token missing email claim"}
	}

	return User{
		Sub:   firstClaimString(claims, "sub"),
		Email: email,
		Name:  firstClaimString(claims, "name"),
	}, role, nil
}

// romaineJWKSCache is a tiny TTL cache keyed by `kid`. Mirrors the pattern
// in tank-operator/backend-go/internal/auth/jwks_remote.go.
type romaineJWKSCache struct {
	mu         sync.RWMutex
	keys       map[string]*rsa.PublicKey
	fetchedAt  time.Time
	httpClient *http.Client
}

func (c *romaineJWKSCache) getKey(ctx context.Context, url, kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	if time.Since(c.fetchedAt) < romaineJWKSCacheTTL {
		if key, ok := c.keys[kid]; ok {
			c.mu.RUnlock()
			return key, nil
		}
		c.mu.RUnlock()
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.fetchedAt) < romaineJWKSCacheTTL {
		if key, ok := c.keys[kid]; ok {
			return key, nil
		}
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	if err := c.refresh(ctx, url); err != nil {
		return nil, err
	}
	if key, ok := c.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("unknown kid %q after refresh", kid)
}

func (c *romaineJWKSCache) refresh(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("JWKS request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return AuthError{Status: http.StatusServiceUnavailable, Message: "JWKS unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return AuthError{Status: http.StatusServiceUnavailable, Message: fmt.Sprintf("JWKS error: %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("JWKS read: %w", err)
	}
	var parsed romaineJWKSResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return AuthError{Status: http.StatusServiceUnavailable, Message: "JWKS returned invalid JSON"}
	}
	keys := make(map[string]*rsa.PublicKey, len(parsed.Keys))
	for _, k := range parsed.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := romaineRSAPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	c.keys = keys
	c.fetchedAt = time.Now()
	return nil
}

type romaineJWKSResponse struct {
	Keys []romaineJWK `json:"keys"`
}

type romaineJWK struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func romaineRSAPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	exponent := 0
	for _, b := range eBytes {
		exponent = exponent<<8 + int(b)
	}
	if exponent == 0 {
		return nil, errors.New("invalid RSA exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: exponent}, nil
}

func firstClaimString(claims jwt.MapClaims, names ...string) string {
	for _, name := range names {
		if value, ok := claims[name].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

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

	"github.com/nelsong6/glimmung/internal/metrics"
)

// auth.romaine.life is the single upstream identity provider for the
// .romaine.life ecosystem. Its Better Auth JWT plugin publishes RS256
// keys at /api/auth/jwks; the issuer claim on every token is the
// service's baseURL.
//
// Glimmung is a Relying Party here: it verifies inbound JWTs against
// auth.romaine.life's published JWKS and trusts the role and (for
// service principals) actor_email claims that come with them. No
// per-app user database, no per-app role table — the IdP is the
// source of truth.
const (
	authRomaineLifeIssuer   = "https://auth.romaine.life"
	authRomaineLifeJWKSURL  = "https://auth.romaine.life/api/auth/jwks"
	authRomaineLifeJWKSTTL  = 10 * time.Minute
	authRomaineLifeHTTPTime = 10 * time.Second
)

// RomaineRoleAdmin / RomaineRoleUser / RomaineRoleService is the closed
// set of role values glimmung accepts from auth.romaine.life-issued
// tokens. Anything else (including `pending`, the default for fresh
// Microsoft sign-ins that haven't been promoted) is rejected.
const (
	RomaineRoleAdmin   = "admin"
	RomaineRoleUser    = "user"
	RomaineRoleService = "service"
)

var romaineAllowedRoles = map[string]bool{
	RomaineRoleAdmin:   true,
	RomaineRoleUser:    true,
	RomaineRoleService: true,
}

// RomaineLifeJWTVerifier verifies JWTs minted by auth.romaine.life and
// returns the resolved User + role flags. The verifier holds a small
// in-process JWKS cache so a burst of requests doesn't fan out into a
// network call per token.
//
// This is the bearer-token path for inbound requests. Cookie-bearing
// callers go through CookieDelegate instead, which forwards the cookie
// to auth.romaine.life/api/auth/get-session — same trust root, two
// presentation formats.
type RomaineLifeJWTVerifier struct {
	issuer     string
	jwksURL    string
	jwks       *jwksCache
	httpClient *http.Client
	// expectedAudience, if non-empty, requires the JWT's `aud` claim to
	// contain this value. Empty means "don't gate on aud" (the default
	// while the platform's audience naming hasn't been standardized).
	expectedAudience string
	// leeway absorbs small clock skew between auth.romaine.life and the
	// glimmung pod when verifying exp/nbf. Matches the tank-operator
	// verifier's default.
	leeway time.Duration
}

// NewRomaineLifeJWTVerifier returns a verifier wired to the production
// auth.romaine.life issuer + JWKS URL. Tests construct a verifier with
// NewRomaineLifeJWTVerifierForTesting and inject a stub JWKS server.
func NewRomaineLifeJWTVerifier() *RomaineLifeJWTVerifier {
	return &RomaineLifeJWTVerifier{
		issuer:     authRomaineLifeIssuer,
		jwksURL:    authRomaineLifeJWKSURL,
		jwks:       newJWKSCache(authRomaineLifeJWKSTTL),
		httpClient: &http.Client{Timeout: authRomaineLifeHTTPTime},
		leeway:     60 * time.Second,
	}
}

// NewRomaineLifeJWTVerifierForTesting constructs a verifier pointing at
// a caller-supplied issuer + JWKS URL. The HTTP client is the supplied
// one, or the verifier's own if nil. Test helper only.
func NewRomaineLifeJWTVerifierForTesting(issuer, jwksURL string, client *http.Client) *RomaineLifeJWTVerifier {
	if client == nil {
		client = &http.Client{Timeout: authRomaineLifeHTTPTime}
	}
	return &RomaineLifeJWTVerifier{
		issuer:     issuer,
		jwksURL:    jwksURL,
		jwks:       newJWKSCache(authRomaineLifeJWKSTTL),
		httpClient: client,
		leeway:     60 * time.Second,
	}
}

// Resolve verifies the token and returns (user, isAdmin, error). isAdmin
// is true when the resolved role is `admin` *or* `service` — service
// principals are tokens issued for machine callers acting on behalf of
// admin-equivalent identities, and the on-behalf-of identity is carried
// in User.ActorEmail. Routes that need to distinguish "real admin" from
// "service principal admin" can branch on User.Role / User.IsService().
func (v *RomaineLifeJWTVerifier) Resolve(ctx context.Context, tokenString string) (User, bool, error) {
	user, err := v.Decode(ctx, tokenString)
	if err != nil {
		return User{}, false, err
	}
	return user, user.IsAdmin() || user.IsService(), nil
}

// RequireAdmin is the strict variant: rejects anything other than
// role=admin or role=service. role=user is signed-in but not gated to
// admin operations.
func (v *RomaineLifeJWTVerifier) RequireAdmin(ctx context.Context, tokenString string) (User, error) {
	user, err := v.Decode(ctx, tokenString)
	if err != nil {
		return User{}, err
	}
	if !user.IsAdmin() && !user.IsService() {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "admin or service role required"}
	}
	return user, nil
}

// Decode verifies signature, issuer, expiry, and role claims. Returns a
// fully-populated User including Role and (for service tokens)
// ActorEmail. Used directly by routes that need the role value beyond
// the admin/non-admin gate.
func (v *RomaineLifeJWTVerifier) Decode(ctx context.Context, tokenString string) (User, error) {
	if v == nil {
		metrics.RecordAuthRomaineLifeRequest("unknown", metrics.AuthOutcomeErrorVerifierMisconfig)
		return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "auth.romaine.life verifier not configured"}
	}
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method: %s", t.Method.Alg())
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token missing kid")
		}
		return v.jwks.key(ctx, v.httpClient, v.jwksURL, kid)
	}, jwt.WithLeeway(v.leeway))
	if err != nil || !parsed.Valid {
		if err == nil {
			err = errors.New("invalid token")
		}
		metrics.RecordAuthRomaineLifeRequest("unknown", metrics.AuthOutcomeDeniedToken)
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "invalid auth.romaine.life token: " + err.Error()}
	}

	iss, _ := claims["iss"].(string)
	if iss != v.issuer {
		metrics.RecordAuthRomaineLifeRequest("unknown", metrics.AuthOutcomeDeniedIssuer)
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "unexpected issuer: " + iss}
	}

	if v.expectedAudience != "" {
		if !audienceContains(claims["aud"], v.expectedAudience) {
			metrics.RecordAuthRomaineLifeRequest("unknown", metrics.AuthOutcomeDeniedAudience)
			return User{}, AuthError{Status: http.StatusUnauthorized, Message: "token audience does not include " + v.expectedAudience}
		}
	}

	role := strings.TrimSpace(stringClaim(claims, "role"))
	if !romaineAllowedRoles[role] {
		metrics.RecordAuthRomaineLifeRequest("unknown", metrics.AuthOutcomeDeniedRole)
		return User{}, AuthError{Status: http.StatusForbidden, Message: "role not approved by auth.romaine.life: " + role}
	}

	email := strings.ToLower(strings.TrimSpace(stringClaim(claims, "email")))
	if email == "" && role != RomaineRoleService {
		// Human roles must carry an email claim. Service tokens are
		// allowed to omit it because the meaningful identity is the
		// actor_email — the human on whose behalf the bot is acting.
		metrics.RecordAuthRomaineLifeRequest(role, metrics.AuthOutcomeDeniedToken)
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "token missing email claim"}
	}

	user := User{
		Sub:   stringClaim(claims, "sub"),
		Email: email,
		Name:  stringClaim(claims, "name"),
		Role:  role,
	}

	if role == RomaineRoleService {
		actor := strings.ToLower(strings.TrimSpace(stringClaim(claims, "actor_email")))
		if actor == "" {
			// Upstream is supposed to refuse to mint service tokens
			// without actor_email; seeing one here means tampering or
			// an upstream regression. Fail loud.
			metrics.RecordAuthRomaineLifeRequest(role, metrics.AuthOutcomeDeniedActorMissing)
			return User{}, AuthError{Status: http.StatusUnauthorized, Message: "service token missing actor_email claim"}
		}
		user.ActorEmail = actor
		if user.Email == "" {
			// Backfill Email so middleware that logs Email gets the
			// human identity rather than an empty string.
			user.Email = actor
		}
	}

	metrics.RecordAuthRomaineLifeRequest(role, metrics.AuthOutcomeOK)
	return user, nil
}

// audienceContains accepts both the single-string and string-array
// forms of the `aud` claim and reports whether `want` is present.
func audienceContains(raw any, want string) bool {
	switch v := raw.(type) {
	case string:
		return v == want
	case []any:
		for _, entry := range v {
			if s, ok := entry.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func stringClaim(claims jwt.MapClaims, name string) string {
	value, _ := claims[name].(string)
	return value
}

// jwksCache holds the public keys glimmung uses to verify
// auth.romaine.life-issued JWTs. Keys are fetched once per ttl window,
// indexed by kid. Concurrent verifiers share one fetch via the
// double-checked lock.
type jwksCache struct {
	ttl       time.Duration
	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func newJWKSCache(ttl time.Duration) *jwksCache {
	return &jwksCache{ttl: ttl}
}

func (c *jwksCache) key(ctx context.Context, client *http.Client, url, kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	if time.Since(c.fetchedAt) < c.ttl {
		if key, ok := c.keys[kid]; ok {
			c.mu.RUnlock()
			return key, nil
		}
		c.mu.RUnlock()
		// Cache is fresh but doesn't have this kid. Refresh once in
		// case the IdP rotated keys.
	} else {
		c.mu.RUnlock()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Double-check: another goroutine may have refreshed while we
	// were waiting for the write lock.
	if time.Since(c.fetchedAt) < c.ttl {
		if key, ok := c.keys[kid]; ok {
			return key, nil
		}
	}
	if err := c.refreshLocked(ctx, client, url); err != nil {
		return nil, err
	}
	if key, ok := c.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("unknown kid %q after refresh", kid)
}

func (c *jwksCache) refreshLocked(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("JWKS request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("JWKS fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("JWKS read: %w", err)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("JWKS parse: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := rsaPublicKeyFromJWK(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	c.keys = keys
	c.fetchedAt = time.Now()
	return nil
}

func rsaPublicKeyFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	decode := func(s string) ([]byte, error) {
		// JWK uses base64url with optional padding stripped.
		s = strings.ReplaceAll(s, "-", "+")
		s = strings.ReplaceAll(s, "_", "/")
		switch len(s) % 4 {
		case 2:
			s += "=="
		case 3:
			s += "="
		}
		return base64.StdEncoding.DecodeString(s)
	}
	nb, err := decode(nB64)
	if err != nil {
		return nil, err
	}
	eb, err := decode(eB64)
	if err != nil {
		return nil, err
	}
	eVal := 0
	for _, b := range eb {
		eVal = eVal<<8 | int(b)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: eVal}, nil
}

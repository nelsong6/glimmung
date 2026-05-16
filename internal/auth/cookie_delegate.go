package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CookieDelegate forwards the inbound .romaine.life session cookie to
// auth.romaine.life's get-session endpoint and uses the returned user as
// the identity. No JWT verification locally; auth.romaine.life is the
// single source of truth for sessions. Result is cached in-process for
// 60s per cookie value so a burst of requests from the same logged-in
// user doesn't fan out into a round-trip per call.
//
// Browser callers visit a glimmung.romaine.life URL with the
// `.romaine.life`-scoped cookie attached automatically; the cookie was
// set by auth.romaine.life when the user signed in.

// allowedRoles gates access. auth.romaine.life mints `role: pending` for
// any fresh Microsoft sign-in; an admin must promote the user via
// auth.romaine.life/admin before they become useful here. `admin` and
// `user` resolve to a signed-in caller; anything else is rejected.
// isAdmin = (role == "admin") — `user`-role callers are signed-in but
// gated out of write endpoints.
var allowedRoles = map[string]bool{
	"admin": true,
	"user":  false,
}

const (
	authRomaineLifeSessionURL  = "https://auth.romaine.life/api/auth/get-session"
	romaineSessionCacheTTL     = 60 * time.Second
	romaineSessionCacheMaxSize = 200
	romaineHTTPTimeout         = 10 * time.Second
)

type CookieDelegate struct {
	endpoint   string
	httpClient *http.Client
	cache      *sessionCache
}

func NewCookieDelegate() *CookieDelegate {
	return &CookieDelegate{
		endpoint:   authRomaineLifeSessionURL,
		httpClient: &http.Client{Timeout: romaineHTTPTimeout},
		cache:      newSessionCache(),
	}
}

// Resolve takes the raw Cookie header value from the inbound request
// and returns the corresponding user (or zero User + ok=false). isAdmin
// is true only if the user's role is "admin". `user` role still resolves
// (signed-in but not admin); `pending` and anything else does not.
func (d *CookieDelegate) Resolve(ctx context.Context, cookie string) (User, bool, error) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return User{}, false, AuthError{Status: http.StatusUnauthorized, Message: "no session cookie"}
	}

	cached, ok := d.cache.get(cookie)
	if ok {
		return cached.user, cached.isAdmin, cached.err
	}

	user, isAdmin, err := d.fetchSession(ctx, cookie)
	d.cache.put(cookie, sessionResult{user: user, isAdmin: isAdmin, err: err})
	return user, isAdmin, err
}

// RequireAdmin is the strict variant — only role=admin passes; user / pending /
// missing all 403.
func (d *CookieDelegate) RequireAdmin(ctx context.Context, cookie string) (User, error) {
	user, isAdmin, err := d.Resolve(ctx, cookie)
	if err != nil {
		return User{}, err
	}
	if !isAdmin {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "admin role required"}
	}
	return user, nil
}

func (d *CookieDelegate) fetchSession(ctx context.Context, cookie string) (User, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.endpoint, nil)
	if err != nil {
		return User{}, false, AuthError{Status: http.StatusInternalServerError, Message: "build session request: " + err.Error()}
	}
	req.Header.Set("Cookie", cookie)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return User{}, false, AuthError{Status: http.StatusServiceUnavailable, Message: "auth.romaine.life unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return User{}, false, AuthError{Status: http.StatusServiceUnavailable, Message: fmt.Sprintf("auth.romaine.life returned %d", resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return User{}, false, AuthError{Status: http.StatusServiceUnavailable, Message: "session read: " + err.Error()}
	}

	// Better Auth returns `null` for unauthenticated requests; treat as not signed in.
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed == "null" {
		return User{}, false, AuthError{Status: http.StatusUnauthorized, Message: "not signed in"}
	}

	var parsed struct {
		User *struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
			Role  string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return User{}, false, AuthError{Status: http.StatusServiceUnavailable, Message: "session parse: " + err.Error()}
	}
	if parsed.User == nil {
		return User{}, false, AuthError{Status: http.StatusUnauthorized, Message: "not signed in"}
	}

	role := parsed.User.Role
	if _, ok := allowedRoles[role]; !ok {
		return User{}, false, AuthError{Status: http.StatusForbidden, Message: "role not approved by auth.romaine.life: " + role}
	}

	return User{
		Sub:   parsed.User.ID,
		Email: strings.ToLower(strings.TrimSpace(parsed.User.Email)),
		Name:  parsed.User.Name,
	}, allowedRoles[role], nil
}

// sessionCache is a tiny TTL'd map of cookie value -> last-known session
// result. We cache the error too (so a 401 doesn't get retried on every
// request), but only for the TTL window.
type sessionCache struct {
	mu      sync.Mutex
	entries map[string]sessionCacheEntry
}

type sessionCacheEntry struct {
	expiry time.Time
	sessionResult
}

type sessionResult struct {
	user    User
	isAdmin bool
	err     error
}

func newSessionCache() *sessionCache {
	return &sessionCache{entries: make(map[string]sessionCacheEntry)}
}

func (c *sessionCache) get(key string) (sessionResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return sessionResult{}, false
	}
	if time.Now().After(entry.expiry) {
		delete(c.entries, key)
		return sessionResult{}, false
	}
	return entry.sessionResult, true
}

func (c *sessionCache) put(key string, value sessionResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = sessionCacheEntry{
		expiry:        time.Now().Add(romaineSessionCacheTTL),
		sessionResult: value,
	}
	if len(c.entries) > romaineSessionCacheMaxSize {
		now := time.Now()
		for k, e := range c.entries {
			if now.After(e.expiry) {
				delete(c.entries, k)
			}
		}
	}
}

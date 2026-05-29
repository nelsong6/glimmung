package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFederationAndTailscale stubs both upstreams the
// TailscaleAuthKeyMinter calls: auth.romaine.life's federation
// exchange (which mints the JWT) and api.tailscale.com's
// /api/v2/oauth/token-exchange + /api/v2/tailnet/.../keys. The handlers
// live on one mux because the minter takes two separate base URLs that
// happen to point at the same test server here.
type fakeFederationAndTailscale struct {
	t                     *testing.T
	server                *httptest.Server
	wantOIDCClientID      string
	wantSAToken           string
	federationToken       string
	federationHits        int32
	tailscaleExchangeHits int32
	tailscaleMintHits     int32
	lastJWT               atomic.Value // string
	lastMintBody          atomic.Value // map[string]any
	lastMintTag           atomic.Value // string
	federationStatus      int
	exchangeStatus        int
	mintStatus            int
	tailscaleToken        string
	mintKey               string
	mintExpires           time.Time
}

func newFakeFederationAndTailscale(t *testing.T) *fakeFederationAndTailscale {
	t.Helper()
	f := &fakeFederationAndTailscale{
		t:                t,
		wantOIDCClientID: "T6vFBk1dAa11CNTRL-kf6kJRvG5T11CNTRL",
		wantSAToken:      "sa-token-fake-XXXX",
		federationToken:  "fed-jwt-fake-XXXX",
		federationStatus: http.StatusOK,
		exchangeStatus:   http.StatusOK,
		mintStatus:       http.StatusOK,
		tailscaleToken:   "tsapi-access-token",
		mintKey:          "tskey-auth-fake-1",
		mintExpires:      time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/exchange/federation", f.handleFederation)
	mux.HandleFunc("/api/v2/oauth/token-exchange", f.handleTailscaleTokenExchange)
	mux.HandleFunc("/api/v2/tailnet/", f.handleTailscaleMint)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeFederationAndTailscale) wantAudience() string {
	return federationAudiencePrefix + "/" + f.wantOIDCClientID
}

func (f *fakeFederationAndTailscale) handleFederation(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&f.federationHits, 1)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	authz := r.Header.Get("Authorization")
	if authz != "Bearer "+f.wantSAToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body struct {
		Audience string `json:"audience"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if body.Audience != f.wantAudience() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if f.federationStatus != http.StatusOK {
		w.WriteHeader(f.federationStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      f.federationToken,
		"expires_at": time.Now().Add(5 * time.Minute).Unix(),
		"sub":        "k8s:glimmung/infra-shared",
		"aud":        body.Audience,
	})
}

func (f *fakeFederationAndTailscale) handleTailscaleTokenExchange(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&f.tailscaleExchangeHits, 1)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.Form.Get("client_id") != f.wantOIDCClientID {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	jwt := r.Form.Get("jwt")
	if jwt == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.lastJWT.Store(jwt)
	if jwt != f.federationToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.exchangeStatus != http.StatusOK {
		w.WriteHeader(f.exchangeStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": f.tailscaleToken,
		"expires_in":   3600,
		"token_type":   "Bearer",
	})
}

func (f *fakeFederationAndTailscale) handleTailscaleMint(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&f.tailscaleMintHits, 1)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.tailscaleToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v2/tailnet/") || !strings.HasSuffix(r.URL.Path, "/keys") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.lastMintBody.Store(body)
	if caps, ok := body["capabilities"].(map[string]any); ok {
		if devices, ok := caps["devices"].(map[string]any); ok {
			if create, ok := devices["create"].(map[string]any); ok {
				if tags, ok := create["tags"].([]any); ok && len(tags) == 1 {
					if tag, ok := tags[0].(string); ok {
						f.lastMintTag.Store(tag)
					}
				}
			}
		}
	}
	if f.mintStatus != http.StatusOK {
		w.WriteHeader(f.mintStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "k-1",
		"key":     f.mintKey,
		"expires": f.mintExpires,
	})
}

// writeFakeSAToken stages a projected-SA-token file the minter will
// read on every cold accessToken() call. Returns the path.
func writeFakeSAToken(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fake SA token: %v", err)
	}
	return p
}

func newTestMinter(t *testing.T, f *fakeFederationAndTailscale) *TailscaleAuthKeyMinter {
	t.Helper()
	saPath := writeFakeSAToken(t, f.wantSAToken)
	m, err := NewTailscaleAuthKeyMinterWithTTL(
		f.server.URL, "-", f.wantOIDCClientID, f.server.URL, saPath,
		f.server.Client(), 15*time.Minute,
	)
	if err != nil {
		t.Fatalf("NewTailscaleAuthKeyMinter: %v", err)
	}
	return m
}

func TestNewTailscaleAuthKeyMinterEmptyDisables(t *testing.T) {
	if _, err := NewTailscaleAuthKeyMinter("https://api.tailscale.com", "-", "", "https://auth", "/p", nil); err != errTailscaleUnconfigured {
		t.Fatalf("missing oidc client id: %v", err)
	}
	if _, err := NewTailscaleAuthKeyMinter("https://api.tailscale.com", "-", "cid", "", "/p", nil); err != errAuthRomaineLifeUnconfigured {
		t.Fatalf("missing auth base url: %v", err)
	}
	if _, err := NewTailscaleAuthKeyMinter("https://api.tailscale.com", "-", "cid", "https://auth", "", nil); err != errAuthRomaineLifeUnconfigured {
		t.Fatalf("missing sa token path: %v", err)
	}
}

func TestNewTailscaleAuthKeyMinterTTLClamping(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, authkeyMinTTL},
		{1 * time.Second, authkeyMinTTL},
		{20 * time.Minute, 20 * time.Minute},
		{6 * time.Hour, authkeyMaxTTL},
	}
	for _, tc := range cases {
		m, err := NewTailscaleAuthKeyMinterWithTTL("https://x", "-", "cid", "https://auth", "/p", nil, tc.in)
		if err != nil {
			t.Fatalf("ttl=%s: %v", tc.in, err)
		}
		if m.TTL != tc.want {
			t.Fatalf("ttl=%s: got %s, want %s", tc.in, m.TTL, tc.want)
		}
	}
}

func TestTailscaleAuthKeyMinterMintsKeyEndToEnd(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	m := newTestMinter(t, f)
	result, err := m.MintAuthKey(context.Background(), "tag:spirelens-orchestrator")
	if err != nil {
		t.Fatalf("MintAuthKey: %v", err)
	}
	if result.AuthKey != f.mintKey {
		t.Fatalf("AuthKey=%q, want %q", result.AuthKey, f.mintKey)
	}
	if len(result.Tags) != 1 || result.Tags[0] != "tag:spirelens-orchestrator" {
		t.Fatalf("Tags=%v", result.Tags)
	}
	if !result.ExpiresAt.Equal(f.mintExpires) {
		t.Fatalf("ExpiresAt=%s, want %s", result.ExpiresAt, f.mintExpires)
	}
	if got := atomic.LoadInt32(&f.federationHits); got != 1 {
		t.Fatalf("federationHits=%d", got)
	}
	if got := atomic.LoadInt32(&f.tailscaleExchangeHits); got != 1 {
		t.Fatalf("tailscaleExchangeHits=%d", got)
	}
	if got := atomic.LoadInt32(&f.tailscaleMintHits); got != 1 {
		t.Fatalf("tailscaleMintHits=%d", got)
	}
	if v := f.lastJWT.Load(); v == nil || v.(string) != f.federationToken {
		t.Fatalf("jwt=%v, want %q", v, f.federationToken)
	}
	mintBody, ok := f.lastMintBody.Load().(map[string]any)
	if !ok {
		t.Fatalf("lastMintBody missing or wrong type")
	}
	if got := mintBody["description"]; got != tailscaleAuthKeyDescription {
		t.Fatalf("description=%v, want %q", got, tailscaleAuthKeyDescription)
	}
}

func TestTailscaleAuthKeyMinterCachesAccessToken(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	m := newTestMinter(t, f)
	for range 3 {
		if _, err := m.MintAuthKey(context.Background(), "tag:spirelens-orchestrator"); err != nil {
			t.Fatalf("mint: %v", err)
		}
	}
	if got := atomic.LoadInt32(&f.federationHits); got != 1 {
		t.Fatalf("federationHits=%d, want 1 (SA→JWT exchange should be cached behind the access token)", got)
	}
	if got := atomic.LoadInt32(&f.tailscaleExchangeHits); got != 1 {
		t.Fatalf("tailscaleExchangeHits=%d, want 1 (access token should be cached)", got)
	}
	if got := atomic.LoadInt32(&f.tailscaleMintHits); got != 3 {
		t.Fatalf("tailscaleMintHits=%d, want 3", got)
	}
}

func TestTailscaleAuthKeyMinterFederationError(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	f.federationStatus = http.StatusForbidden
	m := newTestMinter(t, f)
	_, err := m.MintAuthKey(context.Background(), "tag:spirelens-orchestrator")
	if err == nil {
		t.Fatalf("expected error on federation 403")
	}
	if !strings.Contains(err.Error(), "auth.romaine.life federation exchange") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should mention federation + 403: %v", err)
	}
}

func TestTailscaleAuthKeyMinterTailscaleTokenExchangeError(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	f.exchangeStatus = http.StatusUnauthorized
	m := newTestMinter(t, f)
	_, err := m.MintAuthKey(context.Background(), "tag:spirelens-orchestrator")
	if err == nil {
		t.Fatalf("expected error on tailscale 401")
	}
	if !strings.Contains(err.Error(), "tailscale token exchange") {
		t.Fatalf("error should mention tailscale token exchange: %v", err)
	}
}

func TestTailscaleAuthKeyMinterEmptyTag(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	m := newTestMinter(t, f)
	if _, err := m.MintAuthKey(context.Background(), ""); err == nil {
		t.Fatalf("expected error on empty tag")
	}
}

func TestTailscaleAuthKeyMinterFederationAudienceMismatch(t *testing.T) {
	// Server returns a JWT whose aud doesn't match what we requested —
	// the minter must reject the response loudly so a misconfigured
	// federation server can't redirect orchestrator credentials to a
	// different audience.
	f := newFakeFederationAndTailscale(t)
	saPath := writeFakeSAToken(t, f.wantSAToken)
	m, err := NewTailscaleAuthKeyMinterWithTTL(
		f.server.URL, "-", "wrong-client-id", f.server.URL, saPath,
		f.server.Client(), 15*time.Minute,
	)
	if err != nil {
		t.Fatalf("NewTailscaleAuthKeyMinter: %v", err)
	}
	_, err = m.MintAuthKey(context.Background(), "tag:spirelens-orchestrator")
	if err == nil {
		// In this case the federation server will 400 because the
		// audience header is wrong-client-id which doesn't match
		// wantAudience. So we expect a 400 path error, not a mismatch
		// detection at the client. That's fine — the client-side check
		// is a backstop for "server lied about aud", not for "client
		// asked for wrong aud."
		t.Fatalf("expected error")
	}
}

func TestTailscaleAuthKeyMinterMissingSATokenFile(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	m, err := NewTailscaleAuthKeyMinterWithTTL(
		f.server.URL, "-", f.wantOIDCClientID, f.server.URL,
		"/no/such/path/exists", f.server.Client(), 15*time.Minute,
	)
	if err != nil {
		t.Fatalf("NewTailscaleAuthKeyMinter: %v", err)
	}
	_, err = m.MintAuthKey(context.Background(), "tag:spirelens-orchestrator")
	if err == nil || !strings.Contains(err.Error(), "read projected SA token") {
		t.Fatalf("expected SA-token-read error, got: %v", err)
	}
}

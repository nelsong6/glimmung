package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTailscale is a stub of the Tailscale OAuth + keys API used to
// exercise TailscaleAuthKeyMinter end-to-end without touching the real
// service.
type fakeTailscale struct {
	t              *testing.T
	server         *httptest.Server
	wantClientID   string
	wantClientSec  string
	wantTailnet    string
	issuedToken    string
	tokenExpiresIn int
	oauthHits      int32
	mintHits       int32
	lastMintBody   atomic.Value // map[string]any
	lastMintTag    atomic.Value // string
	mintStatus     int
	mintKey        string
	mintExpires    time.Time
}

func newFakeTailscale(t *testing.T) *fakeTailscale {
	t.Helper()
	f := &fakeTailscale{
		t:              t,
		wantClientID:   "client-id",
		wantClientSec:  "client-secret",
		wantTailnet:    "example.ts.net",
		issuedToken:    "tsapi-access-token",
		tokenExpiresIn: 3600,
		mintStatus:     http.StatusOK,
		mintKey:        "tskey-auth-fake-1",
		mintExpires:    time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/oauth/token", f.handleOAuth)
	mux.HandleFunc("/api/v2/tailnet/", f.handleMint)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeTailscale) handleOAuth(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&f.oauthHits, 1)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, sec, ok := r.BasicAuth()
	if !ok || id != f.wantClientID || sec != f.wantClientSec {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.Form.Get("grant_type") != "client_credentials" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": f.issuedToken,
		"expires_in":   f.tokenExpiresIn,
		"token_type":   "Bearer",
	})
}

func (f *fakeTailscale) handleMint(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&f.mintHits, 1)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.issuedToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v2/tailnet/") || !strings.HasSuffix(r.URL.Path, "/keys") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	pathTailnet := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/tailnet/"), "/keys")
	if pathTailnet != f.wantTailnet {
		w.WriteHeader(http.StatusBadRequest)
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

func newTestMinter(t *testing.T, f *fakeTailscale) *TailscaleAuthKeyMinter {
	t.Helper()
	m, err := NewTailscaleAuthKeyMinterWithTTL(f.server.URL, f.wantTailnet, f.wantClientID, f.wantClientSec, f.server.Client(), 15*time.Minute)
	if err != nil {
		t.Fatalf("NewTailscaleAuthKeyMinter: %v", err)
	}
	return m
}

func TestNewTailscaleAuthKeyMinterEmptyDisables(t *testing.T) {
	if _, err := NewTailscaleAuthKeyMinter("https://api.tailscale.com", "-", "", "secret", nil); err != errTailscaleUnconfigured {
		t.Fatalf("missing id: %v", err)
	}
	if _, err := NewTailscaleAuthKeyMinter("https://api.tailscale.com", "-", "id", "", nil); err != errTailscaleUnconfigured {
		t.Fatalf("missing secret: %v", err)
	}
}

func TestNewTailscaleAuthKeyMinterTTLClamping(t *testing.T) {
	cases := []struct {
		in        time.Duration
		want      time.Duration
		wantClamp bool
	}{
		{0, authkeyMinTTL, true},
		{1 * time.Second, authkeyMinTTL, true},
		{20 * time.Minute, 20 * time.Minute, false},
		{6 * time.Hour, authkeyMaxTTL, true},
	}
	for _, tc := range cases {
		m, err := NewTailscaleAuthKeyMinterWithTTL("https://x", "-", "id", "sec", nil, tc.in)
		if err != nil {
			t.Fatalf("ttl=%s: %v", tc.in, err)
		}
		if m.TTL != tc.want {
			t.Fatalf("ttl=%s: got %s, want %s", tc.in, m.TTL, tc.want)
		}
	}
}

func TestTailscaleAuthKeyMinterMintsKey(t *testing.T) {
	f := newFakeTailscale(t)
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
	if tag := f.lastMintTag.Load(); tag == nil || tag.(string) != "tag:spirelens-orchestrator" {
		t.Fatalf("server saw tag=%v", tag)
	}
	body := f.lastMintBody.Load().(map[string]any)
	create := body["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)
	for _, key := range []string{"ephemeral", "preauthorized"} {
		if v, ok := create[key].(bool); !ok || !v {
			t.Fatalf("create.%s=%v", key, create[key])
		}
	}
	if reusable, ok := create["reusable"].(bool); !ok || reusable {
		t.Fatalf("create.reusable=%v", create["reusable"])
	}
	if expiry, ok := body["expirySeconds"].(float64); !ok || int(expiry) != int((15*time.Minute).Seconds()) {
		t.Fatalf("expirySeconds=%v", body["expirySeconds"])
	}
}

func TestTailscaleAuthKeyMinterCachesAccessToken(t *testing.T) {
	f := newFakeTailscale(t)
	m := newTestMinter(t, f)
	for range 3 {
		if _, err := m.MintAuthKey(context.Background(), "tag:spirelens-orchestrator"); err != nil {
			t.Fatalf("mint: %v", err)
		}
	}
	if got := atomic.LoadInt32(&f.oauthHits); got != 1 {
		t.Fatalf("oauthHits=%d, want 1 (token should be cached)", got)
	}
	if got := atomic.LoadInt32(&f.mintHits); got != 3 {
		t.Fatalf("mintHits=%d, want 3", got)
	}
}

func TestTailscaleAuthKeyMinterPropagatesAPIError(t *testing.T) {
	f := newFakeTailscale(t)
	f.mintStatus = http.StatusForbidden
	m := newTestMinter(t, f)
	if _, err := m.MintAuthKey(context.Background(), "tag:spirelens-orchestrator"); err == nil {
		t.Fatalf("expected error on 403")
	} else if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should mention 403: %v", err)
	}
}

func TestTailscaleAuthKeyMinterEmptyTag(t *testing.T) {
	f := newFakeTailscale(t)
	m := newTestMinter(t, f)
	if _, err := m.MintAuthKey(context.Background(), ""); err == nil {
		t.Fatalf("expected error on empty tag")
	}
}

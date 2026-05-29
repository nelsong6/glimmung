package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// errTailscaleUnconfigured is returned when no Tailscale OAuth client
// credentials were supplied through KV. Callers translate it into an
// HTTP 503 so the endpoint fails closed.
var errTailscaleUnconfigured = errors.New("tailscale oauth credentials not configured")

// authkeyDefaultTTL bounds the validity of the minted ephemeral auth
// key. Long enough for an orchestrator pod to fully boot and register;
// short enough that an unconsumed key dies quickly. The Tailscale node
// itself is `ephemeral`, so it disappears shortly after it disconnects.
const authkeyDefaultTTL = 15 * time.Minute

// authkeyMinTTL and authkeyMaxTTL bound any operator-supplied TTL
// override.
const (
	authkeyMinTTL = 5 * time.Minute
	authkeyMaxTTL = time.Hour
)

// tailscaleOAuthEarlyExpirySkew shrinks the cached access token's
// effective lifetime so glimmung re-mints before Tailscale starts
// rejecting it.
const tailscaleOAuthEarlyExpirySkew = 60 * time.Second

// TailscaleAuthKeyMinter mints ephemeral, pre-authorized auth keys via
// Tailscale's OAuth client-credentials flow. One minter per glimmung
// process; per-request callers receive a single-use key scoped to a
// server-controlled tag.
type TailscaleAuthKeyMinter struct {
	BaseURL      string
	Tailnet      string
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	TTL          time.Duration

	mu          sync.Mutex
	cachedToken string
	tokenExp    time.Time
}

// TailscaleAuthKeyResponse is the JSON shape returned to callers of
// the lease-callback endpoint that mints a tailnet auth key.
type TailscaleAuthKeyResponse struct {
	AuthKey   string    `json:"authkey"`
	Tags      []string  `json:"tags"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewTailscaleAuthKeyMinter constructs a minter. Empty client ID or
// secret returns errTailscaleUnconfigured so the endpoint can disable
// cleanly. Empty tailnet defaults to "-", Tailscale's convention for
// "the OAuth client's default tailnet."
func NewTailscaleAuthKeyMinter(baseURL, tailnet, clientID, clientSecret string, httpClient *http.Client) (*TailscaleAuthKeyMinter, error) {
	return NewTailscaleAuthKeyMinterWithTTL(baseURL, tailnet, clientID, clientSecret, httpClient, authkeyDefaultTTL)
}

// NewTailscaleAuthKeyMinterWithTTL is the testable form of
// NewTailscaleAuthKeyMinter. ttl is clamped to [authkeyMinTTL, authkeyMaxTTL].
func NewTailscaleAuthKeyMinterWithTTL(baseURL, tailnet, clientID, clientSecret string, httpClient *http.Client, ttl time.Duration) (*TailscaleAuthKeyMinter, error) {
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	if clientID == "" || clientSecret == "" {
		return nil, errTailscaleUnconfigured
	}
	if strings.TrimSpace(tailnet) == "" {
		tailnet = "-"
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.tailscale.com"
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	if ttl < authkeyMinTTL {
		ttl = authkeyMinTTL
	}
	if ttl > authkeyMaxTTL {
		ttl = authkeyMaxTTL
	}
	return &TailscaleAuthKeyMinter{
		BaseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Tailnet:      tailnet,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		HTTP:         httpClient,
		TTL:          ttl,
	}, nil
}

func (m *TailscaleAuthKeyMinter) accessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.cachedToken != "" && time.Now().Before(m.tokenExp) {
		token := m.cachedToken
		m.mu.Unlock()
		return token, nil
	}
	m.mu.Unlock()

	form := url.Values{}
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/api/v2/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(m.ClientID, m.ClientSecret)
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("tailscale oauth request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("tailscale oauth: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&payload); err != nil {
		return "", fmt.Errorf("tailscale oauth decode: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", errors.New("tailscale oauth: empty access token")
	}
	m.mu.Lock()
	m.cachedToken = payload.AccessToken
	switch {
	case payload.ExpiresIn > int(tailscaleOAuthEarlyExpirySkew.Seconds()):
		m.tokenExp = time.Now().Add(time.Duration(payload.ExpiresIn)*time.Second - tailscaleOAuthEarlyExpirySkew)
	default:
		// No expiry hint or implausibly small one — assume short and
		// re-mint on next call.
		m.tokenExp = time.Now().Add(5 * time.Minute)
	}
	m.mu.Unlock()
	return payload.AccessToken, nil
}

// MintAuthKey returns a single-use, ephemeral, pre-authorized auth key
// tagged with the supplied tag. The tag must exist in the tenant's ACL
// and the OAuth client must own it.
func (m *TailscaleAuthKeyMinter) MintAuthKey(ctx context.Context, tag string) (TailscaleAuthKeyResponse, error) {
	if m == nil {
		return TailscaleAuthKeyResponse{}, errTailscaleUnconfigured
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return TailscaleAuthKeyResponse{}, errors.New("tag required")
	}
	token, err := m.accessToken(ctx)
	if err != nil {
		return TailscaleAuthKeyResponse{}, err
	}
	ttl := m.TTL
	if ttl <= 0 {
		ttl = authkeyDefaultTTL
	}
	body := map[string]any{
		"capabilities": map[string]any{
			"devices": map[string]any{
				"create": map[string]any{
					"reusable":      false,
					"ephemeral":     true,
					"preauthorized": true,
					"tags":          []string{tag},
				},
			},
		},
		"expirySeconds": int(ttl.Seconds()),
		"description":   fmt.Sprintf("glimmung remote-host orchestrator (%s)", tag),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return TailscaleAuthKeyResponse{}, err
	}
	keyURL := fmt.Sprintf("%s/api/v2/tailnet/%s/keys", m.BaseURL, url.PathEscape(m.Tailnet))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, keyURL, bytes.NewReader(raw))
	if err != nil {
		return TailscaleAuthKeyResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return TailscaleAuthKeyResponse{}, fmt.Errorf("tailscale mint request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return TailscaleAuthKeyResponse{}, fmt.Errorf("tailscale mint: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		ID      string    `json:"id"`
		Key     string    `json:"key"`
		Expires time.Time `json:"expires"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
		return TailscaleAuthKeyResponse{}, fmt.Errorf("tailscale mint decode: %w", err)
	}
	if strings.TrimSpace(out.Key) == "" {
		return TailscaleAuthKeyResponse{}, errors.New("tailscale mint: empty key")
	}
	return TailscaleAuthKeyResponse{
		AuthKey:   out.Key,
		Tags:      []string{tag},
		ExpiresAt: out.Expires,
	}, nil
}

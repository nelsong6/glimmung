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
	"os"
	"strings"
	"sync"
	"time"
)

// errTailscaleUnconfigured is returned when the OIDC trust-credential ID
// is unset. Callers translate it into an HTTP 503 so the endpoint fails
// closed (a missing client identifier must never silently bypass the
// federation flow).
var errTailscaleUnconfigured = errors.New("tailscale oidc trust credential not configured")

// errAuthRomaineLifeUnconfigured is returned when the auth.romaine.life
// base URL or projected SA-token path is unset. Same fail-closed posture
// as errTailscaleUnconfigured.
var errAuthRomaineLifeUnconfigured = errors.New("auth.romaine.life federation base URL or sa token path not configured")

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

// tailscaleAccessTokenSkew shrinks the cached Tailscale access token's
// effective lifetime so glimmung re-mints before Tailscale starts
// rejecting it.
const tailscaleAccessTokenSkew = 60 * time.Second

// federationAudiencePrefix is the prefix Tailscale binds in its OIDC
// trust credential. The full `aud` claim is constructed as
// `<prefix>/<oidc_client_id>` — Tailscale uses the path segment as the
// trust-credential lookup key.
const federationAudiencePrefix = "api.tailscale.com"

// federationExchangePath is the auth.romaine.life endpoint that mints
// custom-audience JWTs (added in nelsong6/auth#63). It accepts an
// inbound projected k8s SA token (audience = https://auth.romaine.life)
// and returns an auth.romaine.life-signed JWT with the requested `aud`.
const federationExchangePath = "/api/auth/exchange/federation"

// tailscaleAuthKeyDescription must stay inside Tailscale's restricted
// description charset. Keep the scoped tag in capabilities.tags instead.
const tailscaleAuthKeyDescription = "glimmung remote host orchestrator"

// TailscaleAuthKeyMinter mints ephemeral, pre-authorized auth keys via
// an OIDC workload-identity federation flow:
//
//  1. Read this pod's projected SA token (audience = the auth.romaine.life
//     base URL — same mount the managed-origin reconciler uses).
//  2. POST the SA token to auth.romaine.life's federation exchange
//     endpoint with the desired Tailscale audience; receive an
//     auth.romaine.life-signed JWT (iss = https://auth.romaine.life,
//     aud = api.tailscale.com/<oidc_client_id>).
//  3. POST that JWT to api.tailscale.com /api/v2/oauth/token-exchange
//     with the trust credential's client_id; receive a Tailscale API
//     access token.
//  4. Use the access token to call Tailscale's auth-key mint endpoint.
//
// No long-lived client secret is stored anywhere. The Tailscale OIDC
// trust credential is identified by its client_id, which is not secret
// on its own — possessing it lets you ask for tokens, but the actual
// credentialing happens against the JWT signature.
type TailscaleAuthKeyMinter struct {
	BaseURL                string
	Tailnet                string
	OIDCClientID           string
	AuthRomaineLifeBaseURL string
	SATokenPath            string
	HTTP                   *http.Client
	TTL                    time.Duration

	mu          sync.Mutex
	cachedToken string
	tokenExp    time.Time
}

// TailscaleAuthKeyResponse is the JSON shape returned to callers of
// the lease-callback endpoint that mints a tailnet auth key. Unchanged
// from the previous OAuth-client-secret flow — the response surface is
// the same; only the upstream credential acquisition changed.
type TailscaleAuthKeyResponse struct {
	AuthKey   string    `json:"authkey"`
	Tags      []string  `json:"tags"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewTailscaleAuthKeyMinter constructs a minter that drives the OIDC
// federation flow. Empty `oidcClientID`, `authRomaineLifeBaseURL`, or
// `saTokenPath` returns a sentinel error so the endpoint can disable
// cleanly (HTTP 503). Empty `tailnet` defaults to "-", Tailscale's
// convention for "the credential's default tailnet."
func NewTailscaleAuthKeyMinter(baseURL, tailnet, oidcClientID, authRomaineLifeBaseURL, saTokenPath string, httpClient *http.Client) (*TailscaleAuthKeyMinter, error) {
	return NewTailscaleAuthKeyMinterWithTTL(baseURL, tailnet, oidcClientID, authRomaineLifeBaseURL, saTokenPath, httpClient, authkeyDefaultTTL)
}

// NewTailscaleAuthKeyMinterWithTTL is the testable form of
// NewTailscaleAuthKeyMinter. `ttl` is clamped to
// [authkeyMinTTL, authkeyMaxTTL] for the minted *tailnet* auth key (the
// upstream Tailscale API access token's lifetime is set by Tailscale).
func NewTailscaleAuthKeyMinterWithTTL(baseURL, tailnet, oidcClientID, authRomaineLifeBaseURL, saTokenPath string, httpClient *http.Client, ttl time.Duration) (*TailscaleAuthKeyMinter, error) {
	oidcClientID = strings.TrimSpace(oidcClientID)
	if oidcClientID == "" {
		return nil, errTailscaleUnconfigured
	}
	authRomaineLifeBaseURL = strings.TrimSpace(authRomaineLifeBaseURL)
	saTokenPath = strings.TrimSpace(saTokenPath)
	if authRomaineLifeBaseURL == "" || saTokenPath == "" {
		return nil, errAuthRomaineLifeUnconfigured
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
		BaseURL:                strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Tailnet:                tailnet,
		OIDCClientID:           oidcClientID,
		AuthRomaineLifeBaseURL: strings.TrimRight(authRomaineLifeBaseURL, "/"),
		SATokenPath:            saTokenPath,
		HTTP:                   httpClient,
		TTL:                    ttl,
	}, nil
}

// federationAudience returns the `aud` claim auth.romaine.life must
// stamp on the minted JWT — Tailscale's trust credential pins this
// exactly, so glimmung asks for it by name.
func (m *TailscaleAuthKeyMinter) federationAudience() string {
	return federationAudiencePrefix + "/" + m.OIDCClientID
}

func (m *TailscaleAuthKeyMinter) accessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.cachedToken != "" && time.Now().Before(m.tokenExp) {
		token := m.cachedToken
		m.mu.Unlock()
		return token, nil
	}
	m.mu.Unlock()

	saToken, err := os.ReadFile(m.SATokenPath)
	if err != nil {
		return "", fmt.Errorf("read projected SA token: %w", err)
	}
	saTokenStr := strings.TrimSpace(string(saToken))
	if saTokenStr == "" {
		return "", errors.New("projected SA token file is empty")
	}

	// Step 1: exchange the SA token for an auth.romaine.life JWT bound
	// to the Tailscale trust credential's audience.
	federationJWT, err := m.exchangeForFederationJWT(ctx, saTokenStr)
	if err != nil {
		return "", fmt.Errorf("auth.romaine.life federation exchange: %w", err)
	}

	// Step 2: exchange that JWT for a Tailscale API access token via
	// Tailscale's workload identity token-exchange endpoint.
	tsAccessToken, expiresIn, err := m.exchangeForTailscaleAccessToken(ctx, federationJWT)
	if err != nil {
		return "", fmt.Errorf("tailscale token exchange: %w", err)
	}

	m.mu.Lock()
	m.cachedToken = tsAccessToken
	switch {
	case expiresIn > int(tailscaleAccessTokenSkew.Seconds()):
		m.tokenExp = time.Now().Add(time.Duration(expiresIn)*time.Second - tailscaleAccessTokenSkew)
	default:
		// No expiry hint or implausibly small one — assume short and
		// re-mint on next call.
		m.tokenExp = time.Now().Add(5 * time.Minute)
	}
	m.mu.Unlock()
	return tsAccessToken, nil
}

// exchangeForFederationJWT POSTs the projected SA token to
// auth.romaine.life's federation exchange and returns the issued JWT.
func (m *TailscaleAuthKeyMinter) exchangeForFederationJWT(ctx context.Context, saToken string) (string, error) {
	body, err := json.Marshal(map[string]any{"audience": m.federationAudience()})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.AuthRomaineLifeBaseURL+federationExchangePath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+saToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var payload struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		Sub       string `json:"sub"`
		Aud       string `json:"aud"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if strings.TrimSpace(payload.Token) == "" {
		return "", errors.New("empty token in federation response")
	}
	if payload.Aud != m.federationAudience() {
		// Defence in depth: refuse a JWT whose aud was rewritten by a
		// misconfigured server. Tailscale would reject it anyway, but
		// failing here gives a more diagnosable error.
		return "", fmt.Errorf("federation response audience mismatch: got %q want %q", payload.Aud, m.federationAudience())
	}
	return payload.Token, nil
}

// exchangeForTailscaleAccessToken POSTs the auth.romaine.life-signed
// JWT to Tailscale's workload identity token-exchange endpoint and
// returns the resulting Tailscale API access token.
func (m *TailscaleAuthKeyMinter) exchangeForTailscaleAccessToken(ctx context.Context, federationJWT string) (string, int, error) {
	form := url.Values{}
	form.Set("client_id", m.OIDCClientID)
	form.Set("jwt", federationJWT)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/api/v2/oauth/token-exchange", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&payload); err != nil {
		return "", 0, fmt.Errorf("decode: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", 0, errors.New("empty access_token in tailscale response")
	}
	return payload.AccessToken, payload.ExpiresIn, nil
}

// MintAuthKey returns a single-use, ephemeral, pre-authorized auth key
// tagged with the supplied tag. The tag must exist in the tenant's ACL
// and the Tailscale trust credential's allowed-tag set.
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
		"description":   tailscaleAuthKeyDescription,
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

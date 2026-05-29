package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// errSSHCertGatewayUnconfigured is returned when the auth.romaine.life
// base URL or the projected SA-token path is unset. Callers translate it
// into an HTTP 503 so the endpoint fails closed — a missing upstream
// configuration must never silently bypass the cert mint. Same
// fail-closed posture as errTailscaleUnconfigured.
var errSSHCertGatewayUnconfigured = errors.New("ssh cert gateway: auth.romaine.life base URL or sa token path not configured")

// retiredSSHCAEnv names the environment variable that used to carry the
// in-process SSH CA private key (the local Go signer that this gateway
// replaced). auth.romaine.life is now the sole SSH CA issuer; glimmung
// holds no CA private material. The startup migration guard
// (GuardRetiredSSHCAEnv) refuses to boot if this variable is set again so
// a stale chart/KV wiring can never resurrect a second signing path.
const retiredSSHCAEnv = "GLIMMUNG_SSH_CA_PRIVATE_KEY"

// sshCertExchangePath is the auth.romaine.life endpoint that signs a
// short-TTL OpenSSH user certificate over a caller-supplied public key.
// It accepts an inbound projected k8s SA token (audience =
// https://auth.romaine.life) as the bearer credential and returns the
// signed certificate plus its validity window.
const sshCertExchangePath = "/api/auth/exchange/ssh-cert"

// sshCertGatewayTTLSeconds is the certificate validity window glimmung
// asks auth to stamp. auth bounds this to [60, 3600] and rejects (does
// not clamp) anything outside that range; 600s covers orchestrator setup
// plus the rest of the phase. See nelsong6/auth src/ssh-cert-helpers.ts.
const sshCertGatewayTTLSeconds = 600

// sshCertPermitPTY is the single extension glimmung requests. The
// orchestrator runs `ssh host "<command>"` style invocations and benefits
// from a PTY; every other extension (port/agent/X11 forwarding, user-rc)
// is excluded. auth enforces the allowed-extension set server-side.
const sshCertPermitPTY = "permit-pty"

// SSHCertExchanger is the gateway to auth.romaine.life's SSH user-cert
// signing endpoint. glimmung no longer holds CA private material: it
// proves its identity with a projected SA token (audience-pinned to
// auth.romaine.life) and asks auth to sign a cert over the orchestrator's
// freshly generated public key. One exchanger per glimmung process.
//
// This mirrors TailscaleAuthKeyMinter's federation pattern — read the SA
// token from disk, POST it as a Bearer credential to auth.romaine.life —
// except the SSH cert response is consumed directly rather than chained
// into a downstream exchange.
type SSHCertExchanger struct {
	AuthBaseURL string
	SATokenPath string
	HTTP        *http.Client
}

// sshCertUpstreamError carries auth.romaine.life's status code and body
// back to the handler so it can faithfully propagate the upstream
// decision (a 400 from auth — bad principal/extension/ttl — must surface
// as a 400 to the orchestrator, not get masked as a 502). The handler
// maps the status; the body is preserved for diagnosis.
type sshCertUpstreamError struct {
	status int
	body   string
}

func (e *sshCertUpstreamError) Error() string {
	return fmt.Sprintf("auth.romaine.life ssh-cert exchange: status %d: %s", e.status, e.body)
}

// SSHCertExchangeResult is the parsed subset of auth.romaine.life's
// ssh-cert response that glimmung surfaces to the orchestrator. auth's
// full response also carries `sub`; glimmung does not relay it.
type SSHCertExchangeResult struct {
	Certificate string
	Principals  []string
	KeyID       string
	ValidBefore time.Time
}

// NewSSHCertExchanger constructs an exchanger that calls
// auth.romaine.life's ssh-cert signing endpoint. Empty
// `authRomaineLifeBaseURL` or `saTokenPath` returns
// errSSHCertGatewayUnconfigured so the endpoint can disable cleanly
// (HTTP 503).
func NewSSHCertExchanger(authRomaineLifeBaseURL, saTokenPath string, httpClient *http.Client) (*SSHCertExchanger, error) {
	authRomaineLifeBaseURL = strings.TrimSpace(authRomaineLifeBaseURL)
	saTokenPath = strings.TrimSpace(saTokenPath)
	if authRomaineLifeBaseURL == "" || saTokenPath == "" {
		return nil, errSSHCertGatewayUnconfigured
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &SSHCertExchanger{
		AuthBaseURL: strings.TrimRight(authRomaineLifeBaseURL, "/"),
		SATokenPath: saTokenPath,
		HTTP:        httpClient,
	}, nil
}

// Exchange asks auth.romaine.life to sign a short-TTL OpenSSH user
// certificate over publicKey, stamped with keyID and the supplied
// principals. It reads the projected SA token from disk per call, POSTs
// the cert request with the token as a Bearer credential, and parses the
// signed certificate out of the response. A non-200 from auth returns a
// *sshCertUpstreamError carrying the upstream status so the handler can
// propagate it faithfully.
func (x *SSHCertExchanger) Exchange(ctx context.Context, publicKey, keyID string, principals []string) (SSHCertExchangeResult, error) {
	if x == nil {
		return SSHCertExchangeResult{}, errSSHCertGatewayUnconfigured
	}
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return SSHCertExchangeResult{}, errors.New("public_key required")
	}
	if strings.TrimSpace(keyID) == "" {
		return SSHCertExchangeResult{}, errors.New("key_id required")
	}
	if len(principals) == 0 {
		return SSHCertExchangeResult{}, errors.New("at least one principal required")
	}

	saToken, err := os.ReadFile(x.SATokenPath)
	if err != nil {
		return SSHCertExchangeResult{}, fmt.Errorf("read projected SA token: %w", err)
	}
	saTokenStr := strings.TrimSpace(string(saToken))
	if saTokenStr == "" {
		return SSHCertExchangeResult{}, errors.New("projected SA token file is empty")
	}

	reqBody, err := json.Marshal(map[string]any{
		"public_key":  publicKey,
		"key_id":      keyID,
		"principals":  principals,
		"extensions":  []string{sshCertPermitPTY},
		"ttl_seconds": sshCertGatewayTTLSeconds,
	})
	if err != nil {
		return SSHCertExchangeResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.AuthBaseURL+sshCertExchangePath, bytes.NewReader(reqBody))
	if err != nil {
		return SSHCertExchangeResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+saTokenStr)
	req.Header.Set("Content-Type", "application/json")
	resp, err := x.HTTP.Do(req)
	if err != nil {
		return SSHCertExchangeResult{}, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return SSHCertExchangeResult{}, &sshCertUpstreamError{status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	var payload struct {
		Certificate string   `json:"certificate"`
		ValidBefore int64    `json:"valid_before"`
		Sub         string   `json:"sub"`
		KeyID       string   `json:"key_id"`
		Principals  []string `json:"principals"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&payload); err != nil {
		return SSHCertExchangeResult{}, fmt.Errorf("decode: %w", err)
	}
	if strings.TrimSpace(payload.Certificate) == "" {
		return SSHCertExchangeResult{}, errors.New("empty certificate in ssh-cert response")
	}
	result := SSHCertExchangeResult{
		Certificate: strings.TrimRight(payload.Certificate, "\n"),
		Principals:  payload.Principals,
		KeyID:       payload.KeyID,
	}
	if payload.ValidBefore > 0 {
		result.ValidBefore = time.Unix(payload.ValidBefore, 0).UTC()
	}
	// Defence in depth: glimmung derives the principal and key_id
	// server-side and auth echoes them back. If auth returns an empty
	// echo (older/buggy server), fall back to what glimmung sent so the
	// orchestrator still gets the values it needs.
	if len(result.Principals) == 0 {
		result.Principals = principals
	}
	if strings.TrimSpace(result.KeyID) == "" {
		result.KeyID = keyID
	}
	return result, nil
}

// GuardRetiredSSHCAEnv refuses to boot if the retired in-process SSH CA
// private-key env var is still set. auth.romaine.life is the sole SSH CA
// issuer; glimmung holds no CA private material. A non-empty
// GLIMMUNG_SSH_CA_PRIVATE_KEY means a stale chart/ExternalSecret/KV wiring
// survived the migration and could (if a future code path read it)
// resurrect a second signing authority. Fail fast — loudly, at startup —
// rather than run with ambiguous trust. getenv is injected so the guard
// is unit-testable without mutating process env.
func GuardRetiredSSHCAEnv(getenv func(string) string) error {
	if strings.TrimSpace(getenv(retiredSSHCAEnv)) != "" {
		return fmt.Errorf(
			"%s is set, but glimmung no longer signs SSH certs locally: auth.romaine.life is the sole SSH CA issuer. Remove this env var, its ExternalSecret key, and the glimmung-ssh-ca-private-key KV secret. See docs/remote-host-execution.md.",
			retiredSSHCAEnv,
		)
	}
	return nil
}

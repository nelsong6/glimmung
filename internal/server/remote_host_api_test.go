package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// remoteHostRunCallbackStore is a minimal RunCompletionStore that returns
// a fixed (runID, project) for the configured token and ErrNotFound for
// anything else. The remote-host endpoints don't read the full run
// payload, so we leave ReadRunForReplay unimplemented and rely on the
// shorter token→ids path the handler actually uses.
type remoteHostRunCallbackStore struct {
	fakeReadStore
	token   string
	runID   string
	project string
	err     error
}

func (s remoteHostRunCallbackStore) ReadRunIDForCallbackToken(_ context.Context, token string) (string, string, string, error) {
	if s.err != nil {
		return "", "", "", s.err
	}
	if token != s.token {
		return "", "", "", ErrNotFound
	}
	return s.runID, s.project, "", nil
}

func newRunCallbackStore(token, project string) remoteHostRunCallbackStore {
	return remoteHostRunCallbackStore{token: token, runID: "run_test_01", project: project}
}

func TestSSHCertHandlerHappyPath(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	exchanger := newTestSSHCertExchanger(t, f, "sa-token-abc")
	store := newRunCallbackStore("tok-1", "spirelens")
	handler := mintRunCallbackSSHCert(store, exchanger)

	body := mustEncodeJSON(t, SSHCertRequest{PublicKey: "ssh-ed25519 AAAAUSERKEY"})
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/tok-1/native/ssh-cert", bytes.NewReader(body))
	req.SetPathValue("callback_token", "tok-1")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got SSHCertResponse
	mustDecodeJSON(t, rr.Body.Bytes(), &got)
	if got.Certificate != f.certPEM {
		t.Fatalf("Certificate=%q want %q", got.Certificate, f.certPEM)
	}
	if len(got.Principals) != 1 || got.Principals[0] != "spirelens-agent" {
		t.Fatalf("Principals=%v", got.Principals)
	}
	if got.KeyID != "glimmung-run:spirelens/run_test_01" {
		t.Fatalf("KeyID=%q", got.KeyID)
	}
	if got.ValidBefore.IsZero() {
		t.Fatal("ValidBefore zero")
	}
	// The gateway must have authenticated to auth with the projected SA
	// token and derived the principal/key_id server-side.
	if f.gotAuthHeader != "Bearer sa-token-abc" {
		t.Fatalf("auth Authorization=%q", f.gotAuthHeader)
	}
	if got := f.gotBody["key_id"]; got != "glimmung-run:spirelens/run_test_01" {
		t.Fatalf("auth saw key_id=%v", got)
	}
	principals, ok := f.gotBody["principals"].([]any)
	if !ok || len(principals) != 1 || principals[0] != "spirelens-agent" {
		t.Fatalf("auth saw principals=%v", f.gotBody["principals"])
	}
}

func TestSSHCertHandlerExchangerDisabledReturns503(t *testing.T) {
	store := newRunCallbackStore("tok-1", "spirelens")
	handler := mintRunCallbackSSHCert(store, nil)
	rr := doSSHCertReq(t, handler, "tok-1", `{"public_key":"ssh-ed25519 AAAA"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSSHCertHandlerUnknownTokenReturns404(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	exchanger := newTestSSHCertExchanger(t, f, "sa-token-abc")
	store := newRunCallbackStore("real", "spirelens")
	handler := mintRunCallbackSSHCert(store, exchanger)
	rr := doSSHCertReq(t, handler, "wrong", `{"public_key":"ssh-ed25519 AAAA"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestSSHCertHandlerMissingPubKeyReturns400 covers the local presence /
// shape checks glimmung still owns before it ever calls auth: empty body,
// missing field, empty value, unknown field. The *cryptographic* validity
// of the public key is auth.romaine.life's responsibility now — see
// TestSSHCertHandlerPropagatesAuthBadRequest for the "garbage key" case.
func TestSSHCertHandlerMissingPubKeyReturns400(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	exchanger := newTestSSHCertExchanger(t, f, "sa-token-abc")
	store := newRunCallbackStore("tok-1", "spirelens")
	handler := mintRunCallbackSSHCert(store, exchanger)

	for name, body := range map[string]string{
		"empty body":    ``,
		"missing field": `{}`,
		"empty value":   `{"public_key":""}`,
		"unknown field": `{"public_key":"x","other":"y"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rr := doSSHCertReq(t, handler, "tok-1", body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestSSHCertHandlerPropagatesAuthBadRequest confirms a 400 from auth
// (e.g. an unparsable public key, a disallowed extension, or a ttl out of
// range) surfaces to the orchestrator as a 400 — never masked as a 502.
func TestSSHCertHandlerPropagatesAuthBadRequest(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	f.status = http.StatusBadRequest
	exchanger := newTestSSHCertExchanger(t, f, "sa-token-abc")
	store := newRunCallbackStore("tok-1", "spirelens")
	handler := mintRunCallbackSSHCert(store, exchanger)
	rr := doSSHCertReq(t, handler, "tok-1", `{"public_key":"not a real key"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestSSHCertHandlerPropagatesAuthUnavailable confirms a 503 from auth
// (auth has no CA private key configured) surfaces as a 503.
func TestSSHCertHandlerPropagatesAuthUnavailable(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	f.status = http.StatusServiceUnavailable
	exchanger := newTestSSHCertExchanger(t, f, "sa-token-abc")
	store := newRunCallbackStore("tok-1", "spirelens")
	handler := mintRunCallbackSSHCert(store, exchanger)
	rr := doSSHCertReq(t, handler, "tok-1", `{"public_key":"ssh-ed25519 AAAA"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestSSHCertHandlerPropagatesAuthUpstreamFault confirms an unexpected
// auth status (e.g. 500) surfaces as a 502 bad gateway.
func TestSSHCertHandlerPropagatesAuthUpstreamFault(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	f.status = http.StatusInternalServerError
	exchanger := newTestSSHCertExchanger(t, f, "sa-token-abc")
	store := newRunCallbackStore("tok-1", "spirelens")
	handler := mintRunCallbackSSHCert(store, exchanger)
	rr := doSSHCertReq(t, handler, "tok-1", `{"public_key":"ssh-ed25519 AAAA"}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSSHCertHandlerRejectsRunWithoutProject(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	exchanger := newTestSSHCertExchanger(t, f, "sa-token-abc")
	store := newRunCallbackStore("tok-1", "")
	handler := mintRunCallbackSSHCert(store, exchanger)
	rr := doSSHCertReq(t, handler, "tok-1", `{"public_key":"ssh-ed25519 AAAA"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTailscaleAuthKeyHandlerHappyPath(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	minter := newTestMinter(t, f)
	store := newRunCallbackStore("tok-1", "spirelens")
	handler := mintRunCallbackTailscaleAuthKey(store, minter)

	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/tok-1/native/tailscale-authkey", nil)
	req.SetPathValue("callback_token", "tok-1")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got TailscaleAuthKeyResponse
	mustDecodeJSON(t, rr.Body.Bytes(), &got)
	if got.AuthKey != f.mintKey {
		t.Fatalf("AuthKey=%q", got.AuthKey)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "tag:spirelens-orchestrator" {
		t.Fatalf("Tags=%v", got.Tags)
	}
	if got.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt zero")
	}
	if tag := f.lastMintTag.Load(); tag == nil || tag.(string) != "tag:spirelens-orchestrator" {
		t.Fatalf("server saw tag=%v", tag)
	}
}

func TestTailscaleAuthKeyHandlerMinterDisabledReturns503(t *testing.T) {
	store := newRunCallbackStore("tok-1", "spirelens")
	handler := mintRunCallbackTailscaleAuthKey(store, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/tok-1/native/tailscale-authkey", nil)
	req.SetPathValue("callback_token", "tok-1")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTailscaleAuthKeyHandlerUnknownTokenReturns404(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	minter := newTestMinter(t, f)
	store := newRunCallbackStore("real", "spirelens")
	handler := mintRunCallbackTailscaleAuthKey(store, minter)
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/wrong/native/tailscale-authkey", nil)
	req.SetPathValue("callback_token", "wrong")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTailscaleAuthKeyHandlerPropagatesAPIError(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	f.mintStatus = http.StatusForbidden
	minter := newTestMinter(t, f)
	store := newRunCallbackStore("tok-1", "spirelens")
	handler := mintRunCallbackTailscaleAuthKey(store, minter)
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/tok-1/native/tailscale-authkey", nil)
	req.SetPathValue("callback_token", "tok-1")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRemoteHostPrincipalAndTag(t *testing.T) {
	if got := remoteHostPrincipalForProject("  spirelens  "); got != "spirelens-agent" {
		t.Fatalf("principal=%q", got)
	}
	if got := remoteHostTagForProject("spirelens"); got != "tag:spirelens-orchestrator" {
		t.Fatalf("tag=%q", got)
	}
}

func doSSHCertReq(t *testing.T, handler http.HandlerFunc, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/"+token+"/native/ssh-cert", strings.NewReader(body))
	req.SetPathValue("callback_token", token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func mustEncodeJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustDecodeJSON(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode: %v body=%s", err, string(raw))
	}
}

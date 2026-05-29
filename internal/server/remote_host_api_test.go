package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// remoteHostCallbackStore is a minimal LeaseCallbackReadStore for the
// remote-host endpoint tests. It returns the same lease for any token
// equal to .token; everything else returns ErrNotFound. Keeping it
// local avoids coupling to evolving fakeLeaseCallbackStore in
// lease_callback_api_test.go.
type remoteHostCallbackStore struct {
	fakeReadStore
	token string
	lease Lease
	err   error
}

func (s remoteHostCallbackStore) ReadLeaseByCallbackToken(_ context.Context, token string) (Lease, error) {
	if s.err != nil {
		return Lease{}, s.err
	}
	if token != s.token {
		return Lease{}, ErrNotFound
	}
	return s.lease, nil
}

func newClaimedLease(project string) Lease {
	return Lease{
		ID:      "lse_test_01",
		Kind:    "native",
		Project: project,
		State:   "claimed",
	}
}

func TestSSHCertHandlerHappyPath(t *testing.T) {
	signer := mustNewCertSigner(t)
	store := remoteHostCallbackStore{token: "tok-1", lease: newClaimedLease("spirelens")}
	handler := mintLeaseCallbackSSHCert(store, signer)

	pubKey, _ := generateUserPubKeyForTest(t)
	body := mustEncodeJSON(t, SSHCertRequest{PublicKey: pubKey})
	req := httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/tok-1/ssh-cert", bytes.NewReader(body))
	req.SetPathValue("callback_token", "tok-1")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got SSHCertResponse
	mustDecodeJSON(t, rr.Body.Bytes(), &got)
	if !strings.HasPrefix(got.Certificate, "ssh-ed25519-cert-v01@openssh.com ") {
		t.Fatalf("unexpected certificate prefix: %s", got.Certificate)
	}
	if len(got.Principals) != 1 || got.Principals[0] != "spirelens-agent" {
		t.Fatalf("Principals=%v", got.Principals)
	}
	if got.KeyID != "glimmung-lease:spirelens/lse_test_01" {
		t.Fatalf("KeyID=%q", got.KeyID)
	}
	if got.ValidBefore.Before(got.ValidAfter) || got.ValidBefore.Equal(got.ValidAfter) {
		t.Fatalf("ValidBefore <= ValidAfter")
	}
	// Cert verifies under the same CA signer.
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(got.Certificate))
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	cert, ok := parsed.(*ssh.Certificate)
	if !ok {
		t.Fatalf("not a cert: %T", parsed)
	}
	checker := &ssh.CertChecker{
		IsUserAuthority: func(auth ssh.PublicKey) bool {
			return string(auth.Marshal()) == string(signer.Signer.PublicKey().Marshal())
		},
	}
	if err := checker.CheckCert("spirelens-agent", cert); err != nil {
		t.Fatalf("CertChecker: %v", err)
	}
}

func TestSSHCertHandlerSignerDisabledReturns503(t *testing.T) {
	store := remoteHostCallbackStore{token: "tok-1", lease: newClaimedLease("spirelens")}
	handler := mintLeaseCallbackSSHCert(store, nil)
	rr := doSSHCertReq(t, handler, "tok-1", `{"public_key":"ssh-ed25519 AAAA"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSSHCertHandlerUnknownTokenReturns404(t *testing.T) {
	signer := mustNewCertSigner(t)
	store := remoteHostCallbackStore{token: "real", lease: newClaimedLease("spirelens")}
	handler := mintLeaseCallbackSSHCert(store, signer)
	pub, _ := generateUserPubKeyForTest(t)
	rr := doSSHCertReq(t, handler, "wrong", `{"public_key":"`+pub+`"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSSHCertHandlerLeaseNotClaimedReturns409(t *testing.T) {
	signer := mustNewCertSigner(t)
	lease := newClaimedLease("spirelens")
	lease.State = "released"
	store := remoteHostCallbackStore{token: "tok-1", lease: lease}
	handler := mintLeaseCallbackSSHCert(store, signer)
	pub, _ := generateUserPubKeyForTest(t)
	rr := doSSHCertReq(t, handler, "tok-1", `{"public_key":"`+pub+`"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSSHCertHandlerMissingPubKeyReturns400(t *testing.T) {
	signer := mustNewCertSigner(t)
	store := remoteHostCallbackStore{token: "tok-1", lease: newClaimedLease("spirelens")}
	handler := mintLeaseCallbackSSHCert(store, signer)

	for name, body := range map[string]string{
		"empty body":    ``,
		"missing field": `{}`,
		"empty value":   `{"public_key":""}`,
		"garbage":       `{"public_key":"not a key"}`,
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

func TestSSHCertHandlerRejectsLeaseWithoutProject(t *testing.T) {
	signer := mustNewCertSigner(t)
	lease := newClaimedLease("")
	store := remoteHostCallbackStore{token: "tok-1", lease: lease}
	handler := mintLeaseCallbackSSHCert(store, signer)
	pub, _ := generateUserPubKeyForTest(t)
	rr := doSSHCertReq(t, handler, "tok-1", `{"public_key":"`+pub+`"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTailscaleAuthKeyHandlerHappyPath(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	minter := newTestMinter(t, f)
	store := remoteHostCallbackStore{token: "tok-1", lease: newClaimedLease("spirelens")}
	handler := mintLeaseCallbackTailscaleAuthKey(store, minter)

	req := httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/tok-1/tailscale-authkey", nil)
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
	store := remoteHostCallbackStore{token: "tok-1", lease: newClaimedLease("spirelens")}
	handler := mintLeaseCallbackTailscaleAuthKey(store, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/tok-1/tailscale-authkey", nil)
	req.SetPathValue("callback_token", "tok-1")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTailscaleAuthKeyHandlerLeaseNotClaimedReturns409(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	minter := newTestMinter(t, f)
	lease := newClaimedLease("spirelens")
	lease.State = "released"
	store := remoteHostCallbackStore{token: "tok-1", lease: lease}
	handler := mintLeaseCallbackTailscaleAuthKey(store, minter)
	req := httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/tok-1/tailscale-authkey", nil)
	req.SetPathValue("callback_token", "tok-1")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTailscaleAuthKeyHandlerPropagatesAPIError(t *testing.T) {
	f := newFakeFederationAndTailscale(t)
	f.mintStatus = http.StatusForbidden
	minter := newTestMinter(t, f)
	store := remoteHostCallbackStore{token: "tok-1", lease: newClaimedLease("spirelens")}
	handler := mintLeaseCallbackTailscaleAuthKey(store, minter)
	req := httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/tok-1/tailscale-authkey", nil)
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
	req := httptest.NewRequest(http.MethodPost, "/v1/lease-callbacks/"+token+"/ssh-cert", strings.NewReader(body))
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

// avoid unused imports if the file is edited later
var _ = time.Time{}

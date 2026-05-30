package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeSSHCertAuth stubs auth.romaine.life's ssh-cert signing endpoint so
// the gateway can be exercised without a live auth service or any CA
// private material. It captures the inbound Authorization header and
// request body so tests can assert glimmung sent the right Bearer token,
// key_id, principals, extensions, and ttl.
type fakeSSHCertAuth struct {
	server      *httptest.Server
	certPEM     string
	status      int
	validBefore int64

	gotAuthHeader string
	gotBody       map[string]any
}

func newFakeSSHCertAuth(t *testing.T) *fakeSSHCertAuth {
	t.Helper()
	f := &fakeSSHCertAuth{
		certPEM:     "ssh-ed25519-cert-v01@openssh.com AAAATESTCERT",
		status:      http.StatusOK,
		validBefore: time.Now().Add(10 * time.Minute).Unix(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(sshCertExchangePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.gotAuthHeader = r.Header.Get("Authorization")
		f.gotBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&f.gotBody)
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":"stub rejected the request"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"certificate":  f.certPEM,
			"valid_before": f.validBefore,
			"sub":          "glimmung/infra-shared",
			"key_id":       f.gotBody["key_id"],
			"principals":   f.gotBody["principals"],
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func writeSSHCertSAToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write sa token: %v", err)
	}
	return path
}

func newTestSSHCertExchanger(t *testing.T, f *fakeSSHCertAuth, saToken string) *SSHCertExchanger {
	t.Helper()
	x, err := NewSSHCertExchanger(f.server.URL, writeSSHCertSAToken(t, saToken), f.server.Client())
	if err != nil {
		t.Fatalf("NewSSHCertExchanger: %v", err)
	}
	return x
}

func TestNewSSHCertExchangerUnconfigured(t *testing.T) {
	if _, err := NewSSHCertExchanger("", "/tmp/token", nil); !errors.Is(err, errSSHCertGatewayUnconfigured) {
		t.Fatalf("empty base URL: err=%v, want errSSHCertGatewayUnconfigured", err)
	}
	if _, err := NewSSHCertExchanger("https://auth.romaine.life", "", nil); !errors.Is(err, errSSHCertGatewayUnconfigured) {
		t.Fatalf("empty token path: err=%v, want errSSHCertGatewayUnconfigured", err)
	}
}

func TestSSHCertExchangerHappyPath(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	x := newTestSSHCertExchanger(t, f, "sa-token-abc")

	result, err := x.Exchange(context.Background(), "ssh-ed25519 AAAAUSERKEY", "glimmung-run:spirelens/run_01", []string{"spirelens-agent"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if result.Certificate != f.certPEM {
		t.Fatalf("Certificate=%q want %q", result.Certificate, f.certPEM)
	}
	if len(result.Principals) != 1 || result.Principals[0] != "spirelens-agent" {
		t.Fatalf("Principals=%v", result.Principals)
	}
	if result.KeyID != "glimmung-run:spirelens/run_01" {
		t.Fatalf("KeyID=%q", result.KeyID)
	}
	if result.ValidBefore.IsZero() {
		t.Fatal("ValidBefore zero")
	}

	// glimmung must authenticate with the projected SA token as a Bearer.
	if f.gotAuthHeader != "Bearer sa-token-abc" {
		t.Fatalf("Authorization=%q want %q", f.gotAuthHeader, "Bearer sa-token-abc")
	}
	// The request body must carry exactly the server-derived fields.
	if got := f.gotBody["public_key"]; got != "ssh-ed25519 AAAAUSERKEY" {
		t.Fatalf("public_key=%v", got)
	}
	if got := f.gotBody["key_id"]; got != "glimmung-run:spirelens/run_01" {
		t.Fatalf("key_id=%v", got)
	}
	principals, ok := f.gotBody["principals"].([]any)
	if !ok || len(principals) != 1 || principals[0] != "spirelens-agent" {
		t.Fatalf("principals=%v", f.gotBody["principals"])
	}
	extensions, ok := f.gotBody["extensions"].([]any)
	if !ok || len(extensions) != 1 || extensions[0] != sshCertPermitPTY {
		t.Fatalf("extensions=%v", f.gotBody["extensions"])
	}
	if got := f.gotBody["ttl_seconds"]; got != float64(sshCertGatewayTTLSeconds) {
		t.Fatalf("ttl_seconds=%v want %d", got, sshCertGatewayTTLSeconds)
	}
}

func TestSSHCertExchangerPropagatesUpstreamError(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	f.status = http.StatusBadRequest
	x := newTestSSHCertExchanger(t, f, "sa-token-abc")

	_, err := x.Exchange(context.Background(), "ssh-ed25519 AAAAUSERKEY", "glimmung-run:spirelens/run_01", []string{"spirelens-agent"})
	var upstream *sshCertUpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("err=%v, want *sshCertUpstreamError", err)
	}
	if upstream.status != http.StatusBadRequest {
		t.Fatalf("upstream.status=%d want 400", upstream.status)
	}
}

func TestSSHCertExchangerRejectsEmptyInputs(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	x := newTestSSHCertExchanger(t, f, "sa-token-abc")
	if _, err := x.Exchange(context.Background(), "", "k", []string{"p"}); err == nil {
		t.Fatal("expected error for empty public key")
	}
	if _, err := x.Exchange(context.Background(), "pk", "", []string{"p"}); err == nil {
		t.Fatal("expected error for empty key id")
	}
	if _, err := x.Exchange(context.Background(), "pk", "k", nil); err == nil {
		t.Fatal("expected error for empty principals")
	}
}

func TestSSHCertExchangerEmptySATokenFile(t *testing.T) {
	f := newFakeSSHCertAuth(t)
	x := newTestSSHCertExchanger(t, f, "   ")
	if _, err := x.Exchange(context.Background(), "pk", "k", []string{"p"}); err == nil {
		t.Fatal("expected error for empty SA token file")
	}
}

func TestGuardRetiredSSHCAEnvTrips(t *testing.T) {
	getenv := func(key string) string {
		if key == retiredSSHCAEnv {
			return "-----BEGIN OPENSSH PRIVATE KEY-----\nstale\n-----END OPENSSH PRIVATE KEY-----\n"
		}
		return ""
	}
	if err := GuardRetiredSSHCAEnv(getenv); err == nil {
		t.Fatalf("expected GuardRetiredSSHCAEnv to trip when %s is set", retiredSSHCAEnv)
	}
}

func TestGuardRetiredSSHCAEnvPasses(t *testing.T) {
	if err := GuardRetiredSSHCAEnv(func(string) string { return "" }); err != nil {
		t.Fatalf("GuardRetiredSSHCAEnv unset: %v", err)
	}
	// Whitespace-only is treated as unset (fails closed only on real content).
	if err := GuardRetiredSSHCAEnv(func(key string) string {
		if key == retiredSSHCAEnv {
			return "   \n  "
		}
		return ""
	}); err != nil {
		t.Fatalf("GuardRetiredSSHCAEnv whitespace: %v", err)
	}
}

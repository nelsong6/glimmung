package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// mustGenerateOpenSSHEd25519PEM produces an ed25519 OpenSSH-format
// private key PEM so tests do not depend on shelling out to ssh-keygen.
func mustGenerateOpenSSHEd25519PEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 ca key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "glimmung-ssh-ca-test")
	if err != nil {
		t.Fatalf("marshal openssh private key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

func generateUserPubKeyForTest(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 user key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap ed25519 user key: %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), sshPub
}

func mustNewCertSigner(t *testing.T) *CertSigner {
	t.Helper()
	pem := mustGenerateOpenSSHEd25519PEM(t)
	signer, err := NewCertSignerFromPEM(pem)
	if err != nil {
		t.Fatalf("NewCertSignerFromPEM: %v", err)
	}
	return signer
}

func TestNewCertSignerFromPEMEmptyDisables(t *testing.T) {
	if _, err := NewCertSignerFromPEM(""); err != errSSHCAUnconfigured {
		t.Fatalf("empty PEM: err=%v, want errSSHCAUnconfigured", err)
	}
	if _, err := NewCertSignerFromPEM("   \n  "); err != errSSHCAUnconfigured {
		t.Fatalf("whitespace PEM: err=%v, want errSSHCAUnconfigured", err)
	}
}

func TestNewCertSignerFromPEMInvalidErrors(t *testing.T) {
	if _, err := NewCertSignerFromPEM("-----BEGIN NOT A KEY-----\nabc\n-----END NOT A KEY-----\n"); err == nil {
		t.Fatalf("invalid PEM: expected error, got nil")
	}
}

func TestCertSignerSignsBoundedCert(t *testing.T) {
	signer := mustNewCertSigner(t)
	_, userPub := generateUserPubKeyForTest(t)
	now := time.Now().UTC()
	cert, marshaled, err := signer.SignUserCert(userPub, "glimmung-lease:spirelens/lse_abc", []string{"spirelens-agent"}, now)
	if err != nil {
		t.Fatalf("SignUserCert: %v", err)
	}
	if cert.CertType != ssh.UserCert {
		t.Fatalf("CertType=%d, want UserCert", cert.CertType)
	}
	if cert.KeyId != "glimmung-lease:spirelens/lse_abc" {
		t.Fatalf("KeyId=%q", cert.KeyId)
	}
	if got := cert.ValidPrincipals; len(got) != 1 || got[0] != "spirelens-agent" {
		t.Fatalf("Principals=%v", got)
	}
	if cert.ValidAfter >= cert.ValidBefore {
		t.Fatalf("ValidAfter=%d >= ValidBefore=%d", cert.ValidAfter, cert.ValidBefore)
	}
	if window := cert.ValidBefore - cert.ValidAfter; window < 60 || window > 3700 {
		t.Fatalf("cert validity window=%d seconds, want ~10 min + skew", window)
	}
	if _, ok := cert.Permissions.Extensions["permit-pty"]; !ok {
		t.Fatalf("missing permit-pty extension")
	}
	for _, forbidden := range []string{"permit-port-forwarding", "permit-agent-forwarding", "permit-X11-forwarding", "permit-user-rc"} {
		if _, has := cert.Permissions.Extensions[forbidden]; has {
			t.Fatalf("forbidden extension present: %s", forbidden)
		}
	}
	if !strings.HasPrefix(string(marshaled), "ssh-ed25519-cert-v01@openssh.com ") {
		t.Fatalf("marshaled cert prefix unexpected: %q", strings.SplitN(string(marshaled), " ", 2)[0])
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey(marshaled)
	if err != nil {
		t.Fatalf("re-parse marshaled cert: %v", err)
	}
	parsedCert, ok := parsed.(*ssh.Certificate)
	if !ok {
		t.Fatalf("re-parsed key is not a cert (%T)", parsed)
	}
	checker := &ssh.CertChecker{
		IsUserAuthority: func(auth ssh.PublicKey) bool {
			return string(auth.Marshal()) == string(signer.Signer.PublicKey().Marshal())
		},
	}
	if err := checker.CheckCert("spirelens-agent", parsedCert); err != nil {
		t.Fatalf("CertChecker rejected the freshly signed cert: %v", err)
	}
}

func TestCertSignerRejectsEmptyInputs(t *testing.T) {
	signer := mustNewCertSigner(t)
	_, userPub := generateUserPubKeyForTest(t)
	if _, _, err := signer.SignUserCert(userPub, "", []string{"x"}, time.Now()); err == nil {
		t.Fatal("expected error for empty key id")
	}
	if _, _, err := signer.SignUserCert(userPub, "k", nil, time.Now()); err == nil {
		t.Fatal("expected error for empty principals")
	}
	if _, _, err := signer.SignUserCert(nil, "k", []string{"x"}, time.Now()); err == nil {
		t.Fatal("expected error for nil public key")
	}
}

func TestNewCertSignerFromPEMWithTTLClampsRange(t *testing.T) {
	pem := mustGenerateOpenSSHEd25519PEM(t)
	cases := []struct {
		in           time.Duration
		wantClampLow bool
		wantClampHigh bool
		wantExact    time.Duration
	}{
		{0, true, false, 0},
		{1 * time.Second, true, false, 0},
		{5 * time.Minute, false, false, 5 * time.Minute},
		{2 * time.Hour, false, true, 0},
	}
	for _, tc := range cases {
		signer, err := NewCertSignerFromPEMWithTTL(pem, tc.in)
		if err != nil {
			t.Fatalf("ttl=%s: %v", tc.in, err)
		}
		switch {
		case tc.wantClampLow && signer.TTL != sshCertMinTTL:
			t.Fatalf("ttl=%s: clamp-low expected %s, got %s", tc.in, sshCertMinTTL, signer.TTL)
		case tc.wantClampHigh && signer.TTL != sshCertMaxTTL:
			t.Fatalf("ttl=%s: clamp-high expected %s, got %s", tc.in, sshCertMaxTTL, signer.TTL)
		case !tc.wantClampLow && !tc.wantClampHigh && signer.TTL != tc.wantExact:
			t.Fatalf("ttl=%s: pass-through expected %s, got %s", tc.in, tc.wantExact, signer.TTL)
		}
	}
}

func TestParseUserPublicKeyRejectsCert(t *testing.T) {
	signer := mustNewCertSigner(t)
	_, userPub := generateUserPubKeyForTest(t)
	_, marshaled, err := signer.SignUserCert(userPub, "k", []string{"x"}, time.Now())
	if err != nil {
		t.Fatalf("SignUserCert: %v", err)
	}
	if _, err := ParseUserPublicKey(string(marshaled)); err == nil {
		t.Fatal("ParseUserPublicKey accepted a certificate; want rejection")
	}
}

func TestParseUserPublicKeyRejectsEmpty(t *testing.T) {
	if _, err := ParseUserPublicKey(""); err == nil {
		t.Fatal("ParseUserPublicKey accepted empty input")
	}
	if _, err := ParseUserPublicKey("garbage"); err == nil {
		t.Fatal("ParseUserPublicKey accepted garbage")
	}
}

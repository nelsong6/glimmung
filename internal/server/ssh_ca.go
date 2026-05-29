package server

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// errSSHCAUnconfigured is returned when no CA private key was supplied
// through KV. Callers translate it into an HTTP 503 so the endpoint
// fails closed (a missing secret must never silently bypass the cert
// mint).
var errSSHCAUnconfigured = errors.New("ssh ca private key not configured")

// sshCertDefaultTTL is the bounded validity window glimmung is willing
// to sign by default. Short enough that a leaked cert isn't long-lived;
// long enough to cover orchestrator setup plus the rest of the phase.
// See docs/remote-host-execution.md for the rationale.
const sshCertDefaultTTL = 10 * time.Minute

// sshCertSkew backdates ValidAfter so a small clock skew between
// glimmung and the remote host does not kill the cert before it lands.
const sshCertSkew = 30 * time.Second

// sshCertMinTTL and sshCertMaxTTL bound any operator-supplied TTL
// override. The lower bound stops a misconfiguration from making certs
// useless; the upper bound stops it from issuing long-lived bearers.
const (
	sshCertMinTTL = 60 * time.Second
	sshCertMaxTTL = time.Hour
)

// CertSigner produces short-TTL OpenSSH user certificates from a fixed
// CA private key. One signer per glimmung process; per-request callers
// ask for a cert over their own public key with a server-derived KeyId
// and a server-derived principal. The Signer field is exported only
// so tests can stub it; production callers go through
// NewCertSignerFromPEM.
type CertSigner struct {
	Signer ssh.Signer
	TTL    time.Duration
}

// NewCertSignerFromPEM parses an OpenSSH-format private key (the modern
// `-----BEGIN OPENSSH PRIVATE KEY-----` shape produced by
// `ssh-keygen -t ed25519`) and returns a ready signer. Empty input
// returns errSSHCAUnconfigured so the endpoint can disable cleanly.
func NewCertSignerFromPEM(pem string) (*CertSigner, error) {
	return NewCertSignerFromPEMWithTTL(pem, sshCertDefaultTTL)
}

// NewCertSignerFromPEMWithTTL is the testable form of
// NewCertSignerFromPEM. ttl is clamped to [sshCertMinTTL, sshCertMaxTTL].
func NewCertSignerFromPEMWithTTL(pem string, ttl time.Duration) (*CertSigner, error) {
	trimmed := strings.TrimSpace(pem)
	if trimmed == "" {
		return nil, errSSHCAUnconfigured
	}
	signer, err := ssh.ParsePrivateKey([]byte(trimmed))
	if err != nil {
		return nil, fmt.Errorf("parse ssh ca private key: %w", err)
	}
	if ttl < sshCertMinTTL {
		ttl = sshCertMinTTL
	}
	if ttl > sshCertMaxTTL {
		ttl = sshCertMaxTTL
	}
	return &CertSigner{Signer: signer, TTL: ttl}, nil
}

// ParseUserPublicKey parses a request-supplied authorized_keys-format
// public key, rejecting empty or unparsable input. Callers do not get
// to choose principals — those are derived from the lease.
func ParseUserPublicKey(raw string) (ssh.PublicKey, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("public_key required")
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(trimmed))
	if err != nil {
		return nil, fmt.Errorf("parse public_key: %w", err)
	}
	if _, ok := key.(*ssh.Certificate); ok {
		// Re-signing an existing certificate is never the intended
		// flow; the orchestrator submits a plain public key.
		return nil, errors.New("public_key must not itself be a certificate")
	}
	return key, nil
}

// SignUserCert returns a marshaled OpenSSH user certificate over the
// supplied public key. The certificate is short-lived and carries
// permit-pty only — no port-forwarding, no agent-forwarding, no X11,
// no user-rc. Callers should treat the returned bytes as bearer
// credentials and stream them straight into the orchestrator without
// caching or logging the body.
func (c *CertSigner) SignUserCert(userPubKey ssh.PublicKey, keyID string, principals []string, now time.Time) (*ssh.Certificate, []byte, error) {
	return c.signUserCertWithRand(rand.Reader, userPubKey, keyID, principals, now)
}

func (c *CertSigner) signUserCertWithRand(r io.Reader, userPubKey ssh.PublicKey, keyID string, principals []string, now time.Time) (*ssh.Certificate, []byte, error) {
	if c == nil || c.Signer == nil {
		return nil, nil, errSSHCAUnconfigured
	}
	if userPubKey == nil {
		return nil, nil, errors.New("user public key required")
	}
	if strings.TrimSpace(keyID) == "" {
		return nil, nil, errors.New("key id required")
	}
	if len(principals) == 0 {
		return nil, nil, errors.New("at least one principal required")
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = sshCertDefaultTTL
	}
	cert := &ssh.Certificate{
		Key:             userPubKey,
		CertType:        ssh.UserCert,
		KeyId:           keyID,
		ValidPrincipals: principals,
		ValidAfter:      uint64(now.Add(-sshCertSkew).Unix()),
		ValidBefore:     uint64(now.Add(ttl).Unix()),
		Permissions: ssh.Permissions{
			Extensions: map[string]string{
				// Allow PTY only. The orchestrator runs `ssh host
				// "<command>"` style invocations and benefits from a
				// PTY for streaming output cleanly. Every other
				// extension (port/agent/X11 forwarding, user-rc) is
				// excluded.
				"permit-pty": "",
			},
		},
	}
	if err := cert.SignCert(r, c.Signer); err != nil {
		return nil, nil, fmt.Errorf("sign ssh cert: %w", err)
	}
	return cert, ssh.MarshalAuthorizedKey(cert), nil
}

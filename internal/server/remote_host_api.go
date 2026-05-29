package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SSHCertRequest is the orchestrator-submitted public key plus any
// options the server is willing to honor. Principal selection is
// intentionally not in this struct: it is derived server-side from the
// run's project so a caller in possession of a callback token cannot
// rebrand the cert for a different project's host.
type SSHCertRequest struct {
	PublicKey string `json:"public_key"`
}

// SSHCertResponse carries the signed certificate and its validity
// window back to the orchestrator. The certificate is signed by
// auth.romaine.life (the sole SSH CA issuer); glimmung is a gateway that
// derives the principal/key_id and relays auth's signed result. The
// consumer (`scripts/glimmung-native/*.sh`) reads only `.certificate`;
// the rest is surfaced for diagnosis. auth does not return a
// `valid_after`, so neither does glimmung.
type SSHCertResponse struct {
	Certificate string    `json:"certificate"`
	Principals  []string  `json:"principals"`
	KeyID       string    `json:"key_id"`
	ValidBefore time.Time `json:"valid_before"`
}

// remoteHostPrincipalForProject returns the cert principal a run
// belonging to the given project is allowed to use. The principal is
// not caller-supplied — possession of the callback token already
// proves the orchestrator owns the run, the run pins the project, and
// the project pins the principal. One degree of freedom, no more.
func remoteHostPrincipalForProject(project string) string {
	return strings.TrimSpace(project) + "-agent"
}

// remoteHostTagForProject returns the Tailscale tag a run belonging to
// the given project is allowed to mint orchestrator auth keys under.
func remoteHostTagForProject(project string) string {
	return "tag:" + strings.TrimSpace(project) + "-orchestrator"
}

// runCallbackTokenReader is the narrow read surface the remote-host
// handlers need — just the token→(runID, project) lookup. It is
// satisfied by RunCompletionStore (the heavyweight interface used by
// the completion handlers), and by anything else that implements the
// single method. Keeping it narrow lets test doubles avoid stubbing
// methods they don't exercise.
type runCallbackTokenReader interface {
	ReadRunIDForCallbackToken(ctx context.Context, token string) (string, string, string, error)
}

// mintRunCallbackSSHCert is the run-callback variant of the remote-host
// SSH cert mint. Phase pods carry the run's per-attempt token at
// `$GLIMMUNG_ATTEMPT_TOKEN` and consume the URL Glimmung pre-bakes into
// `$GLIMMUNG_SSH_CERT_URL` (`/v1/run-callbacks/{callback_token}/native/ssh-cert`).
// This mirrors how `github-token`, `pr-touchpoint`, `pr-merge`, and
// `completed` are surfaced to phase scripts — the lease's own callback
// token never reaches phase pods.
func mintRunCallbackSSHCert(store ReadStore, exchanger *SSHCertExchanger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if exchanger == nil {
			writeProblem(w, http.StatusServiceUnavailable, "ssh cert gateway is not configured")
			return
		}
		reader, ok := store.(runCallbackTokenReader)
		if !ok || reader == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run callback store not configured")
			return
		}
		runID, project, ok := readRunForRemoteHost(w, r, reader)
		if !ok {
			return
		}
		body, ok := decodeSSHCertRequest(w, r)
		if !ok {
			return
		}
		publicKey := strings.TrimSpace(body.PublicKey)
		if publicKey == "" {
			writeProblem(w, http.StatusBadRequest, "public_key required")
			return
		}
		principal := remoteHostPrincipalForProject(project)
		keyID := fmt.Sprintf("glimmung-run:%s/%s", project, runID)
		result, err := exchanger.Exchange(r.Context(), publicKey, keyID, []string{principal})
		if err != nil {
			if errors.Is(err, errSSHCertGatewayUnconfigured) {
				writeProblem(w, http.StatusServiceUnavailable, "ssh cert gateway is not configured")
				return
			}
			var upstream *sshCertUpstreamError
			if errors.As(err, &upstream) {
				// Faithfully propagate auth.romaine.life's decision: a 400
				// (bad principal/extension/ttl/public_key) is the caller's
				// fault and must surface as a 400; a 503 (auth has no CA
				// key) surfaces as a 503; anything else is an upstream
				// fault surfaced as a 502. Never mask a caller error as a
				// server error or vice versa.
				switch {
				case upstream.status == http.StatusBadRequest:
					writeProblem(w, http.StatusBadRequest, "auth.romaine.life rejected the ssh-cert request: "+upstream.body)
				case upstream.status == http.StatusServiceUnavailable:
					writeProblem(w, http.StatusServiceUnavailable, "auth.romaine.life ssh ca is not configured")
				default:
					writeProblem(w, http.StatusBadGateway, fmt.Sprintf("auth.romaine.life ssh-cert exchange failed (status %d)", upstream.status))
				}
				return
			}
			writeInternalError(w, r, err, "ssh-cert exchange failed")
			return
		}
		writeJSON(w, http.StatusOK, SSHCertResponse{
			Certificate: result.Certificate,
			Principals:  result.Principals,
			KeyID:       result.KeyID,
			ValidBefore: result.ValidBefore,
		})
	}
}

// mintRunCallbackTailscaleAuthKey is the run-callback variant of the
// Tailscale auth-key mint. Same auth model as
// `mintRunCallbackSSHCert`; the tag is derived from the run's project.
func mintRunCallbackTailscaleAuthKey(store ReadStore, minter *TailscaleAuthKeyMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if minter == nil {
			writeProblem(w, http.StatusServiceUnavailable, "tailscale auth-key minter is not configured")
			return
		}
		reader, ok := store.(runCallbackTokenReader)
		if !ok || reader == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run callback store not configured")
			return
		}
		_, project, ok := readRunForRemoteHost(w, r, reader)
		if !ok {
			return
		}
		tag := remoteHostTagForProject(project)
		result, err := minter.MintAuthKey(r.Context(), tag)
		if err != nil {
			if errors.Is(err, errTailscaleUnconfigured) {
				writeProblem(w, http.StatusServiceUnavailable, "tailscale auth-key minter is not configured")
				return
			}
			writeInternalError(w, r, err, "mint tailscale auth-key failed")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// readRunForRemoteHost resolves the run-callback token to a (runID,
// project) tuple suitable for KeyId construction and project-derived
// principal/tag selection. Validation matches the existing run-callback
// shape: ErrNotFound → 404, anything else internal.
func readRunForRemoteHost(w http.ResponseWriter, r *http.Request, store runCallbackTokenReader) (string, string, bool) {
	token := strings.TrimSpace(r.PathValue("callback_token"))
	if token == "" {
		writeProblem(w, http.StatusBadRequest, "callback_token required")
		return "", "", false
	}
	runID, project, _, err := store.ReadRunIDForCallbackToken(context.Background(), token)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run callback token not found")
		return "", "", false
	}
	if err != nil {
		writeInternalError(w, r, err, "read run by callback token failed")
		return "", "", false
	}
	project = strings.TrimSpace(project)
	if project == "" {
		writeProblem(w, http.StatusConflict, "run has no project; cannot derive remote-host identity")
		return "", "", false
	}
	return runID, project, true
}

// decodeSSHCertRequest tolerates an empty body but rejects any unknown
// fields, so an orchestrator that drifts from the documented shape
// fails loudly instead of silently dropping options.
func decodeSSHCertRequest(w http.ResponseWriter, r *http.Request) (SSHCertRequest, bool) {
	var body SSHCertRequest
	if r.Body == nil {
		writeProblem(w, http.StatusBadRequest, "public_key required")
		return SSHCertRequest{}, false
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			writeProblem(w, http.StatusBadRequest, "public_key required")
			return SSHCertRequest{}, false
		}
		writeProblem(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return SSHCertRequest{}, false
	}
	return body, true
}

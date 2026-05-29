package server

import (
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
// lease's project so a caller in possession of a callback token cannot
// rebrand the cert for a different project's host.
type SSHCertRequest struct {
	PublicKey string `json:"public_key"`
}

// SSHCertResponse carries the signed certificate and its validity
// window back to the orchestrator.
type SSHCertResponse struct {
	Certificate string    `json:"certificate"`
	Principals  []string  `json:"principals"`
	KeyID       string    `json:"key_id"`
	ValidAfter  time.Time `json:"valid_after"`
	ValidBefore time.Time `json:"valid_before"`
}

// remoteHostPrincipalForProject returns the cert principal a lease
// belonging to the given project is allowed to use. The principal is
// not caller-supplied — possession of the callback token already
// proves the orchestrator owns the lease, the lease pins the project,
// and the project pins the principal. One degree of freedom, no more.
func remoteHostPrincipalForProject(project string) string {
	return strings.TrimSpace(project) + "-agent"
}

// remoteHostTagForProject returns the Tailscale tag a lease belonging
// to the given project is allowed to mint orchestrator auth keys under.
func remoteHostTagForProject(project string) string {
	return "tag:" + strings.TrimSpace(project) + "-orchestrator"
}

func mintLeaseCallbackSSHCert(store ReadStore, signer *CertSigner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if signer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "ssh ca is not configured")
			return
		}
		callbackStore, ok := store.(LeaseCallbackReadStore)
		if !ok || callbackStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease callback store not configured")
			return
		}
		lease, ok := readClaimedLeaseByCallbackToken(w, r, callbackStore)
		if !ok {
			return
		}
		project := strings.TrimSpace(lease.Project)
		if project == "" {
			writeProblem(w, http.StatusConflict, "lease has no project; cannot derive principal")
			return
		}
		body, ok := decodeSSHCertRequest(w, r)
		if !ok {
			return
		}
		userKey, err := ParseUserPublicKey(body.PublicKey)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		principal := remoteHostPrincipalForProject(project)
		keyID := fmt.Sprintf("glimmung-lease:%s/%s", project, lease.ID)
		now := time.Now().UTC()
		cert, marshaled, err := signer.SignUserCert(userKey, keyID, []string{principal}, now)
		if err != nil {
			if errors.Is(err, errSSHCAUnconfigured) {
				writeProblem(w, http.StatusServiceUnavailable, "ssh ca is not configured")
				return
			}
			writeInternalError(w, r, err, "sign ssh cert failed")
			return
		}
		writeJSON(w, http.StatusOK, SSHCertResponse{
			Certificate: strings.TrimRight(string(marshaled), "\n"),
			Principals:  []string{principal},
			KeyID:       keyID,
			ValidAfter:  time.Unix(int64(cert.ValidAfter), 0).UTC(),
			ValidBefore: time.Unix(int64(cert.ValidBefore), 0).UTC(),
		})
	}
}

func mintLeaseCallbackTailscaleAuthKey(store ReadStore, minter *TailscaleAuthKeyMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if minter == nil {
			writeProblem(w, http.StatusServiceUnavailable, "tailscale auth-key minter is not configured")
			return
		}
		callbackStore, ok := store.(LeaseCallbackReadStore)
		if !ok || callbackStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease callback store not configured")
			return
		}
		lease, ok := readClaimedLeaseByCallbackToken(w, r, callbackStore)
		if !ok {
			return
		}
		project := strings.TrimSpace(lease.Project)
		if project == "" {
			writeProblem(w, http.StatusConflict, "lease has no project; cannot derive tag")
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

// readClaimedLeaseByCallbackToken resolves a callback token to a Lease,
// emitting the right HTTP error and returning ok=false on any failure
// path so handlers stay flat. A lease that resolves but isn't in
// `claimed` state is treated as a 409 — the same shape heartbeat uses
// for an inactive lease.
func readClaimedLeaseByCallbackToken(w http.ResponseWriter, r *http.Request, store LeaseCallbackReadStore) (Lease, bool) {
	token := strings.TrimSpace(r.PathValue("callback_token"))
	if token == "" {
		writeProblem(w, http.StatusBadRequest, "callback_token required")
		return Lease{}, false
	}
	lease, err := store.ReadLeaseByCallbackToken(r.Context(), token)
	switch {
	case errors.Is(err, ErrNotFound):
		writeProblem(w, http.StatusNotFound, "lease callback token not found")
		return Lease{}, false
	case errors.Is(err, ErrConflict):
		writeProblem(w, http.StatusConflict, "lease callback token is ambiguous")
		return Lease{}, false
	case err != nil:
		writeInternalError(w, r, err, "read lease callback failed")
		return Lease{}, false
	}
	if lease.State != "claimed" {
		writeProblem(w, http.StatusConflict, "lease is not claimed")
		return Lease{}, false
	}
	return lease, true
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

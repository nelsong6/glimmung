# Remote-host execution

Some workloads cannot be modeled as a Glimmung-managed Kubernetes Job in the
cluster: they need access to a stateful, host-pinned, scarce resource that
lives outside Glimmung's cluster. The canonical example is a desktop-game mod
(`nelsong6/spirelens`) whose verify loop requires a warm copy of the game
installed on a specific physical machine. Re-running the warm-state setup per
attempt is impractical; running the verify on a hosted cloud runner is
impossible because the game install is not available there.

Glimmung does not introduce a second kind of phase for this. The phase shape
stays `k8s_job`. The Job pod that runs that phase contains a thin orchestrator
script which acquires per-run, ephemeral credentials and shells out over SSH
to the remote host. The remote host has no Glimmung-controlled daemon
listening for it.

This document describes the server-side primitives glimmung exposes so
project-owned `scripts/glimmung-native/*.sh` can stand up that connection
safely.

## Threat model

The orchestrator Job pod is already authenticated to glimmung through the
run's per-attempt callback token — the same possession-is-proof token that
authorizes `POST /v1/run-callbacks/{callback_token}/native/completed`,
`…/github-token`, `…/pr-touchpoint`, and `…/pr-merge`. That token bounds
every credential glimmung mints under this contract to a single run
attempt. The lease's own callback token is intentionally not exposed to
phase pods.

There are no long-lived credentials deposited on the remote host. Trust
anchors only:

- The **auth.romaine.life** SSH CA public key in the remote host's
  `TrustedUserCAKeys`. auth.romaine.life is the sole SSH CA issuer for the
  `.romaine.life` ecosystem; glimmung trusts the same CA but does not own
  it. Fetch the CA public key line from `GET https://auth.romaine.life/api/ssh/ca`.
- A Tailscale device identity for the remote host, signed in once.

Glimmung holds **no SSH CA private material**. It authenticates to
auth.romaine.life with:

- A projected Kubernetes ServiceAccount token, audience-pinned to
  `https://auth.romaine.life`. The same mount the managed-origin reconciler
  and the Tailscale federation flow use. Glimmung presents this token as a
  Bearer credential to auth's ssh-cert signing endpoint; auth resolves the
  caller via TokenReview, checks it against its SA allowlist, validates the
  requested principal/extensions/ttl, and signs the cert with the CA private
  key that lives only in auth's Key Vault. No KV-resident SSH CA key on the
  glimmung side; no KV-resident Tailscale client secret either (see
  "Tailscale credential flow" below).

Per run, glimmung produces (by asking auth to sign):

- A 10-minute OpenSSH user certificate over the orchestrator's freshly
  generated public key, with `KeyId = glimmung-run:<project>/<run_id>` for
  audit and a single project-derived `principal`.
- A single-use, pre-authorized, ephemeral Tailscale auth key tagged
  `tag:<project>-orchestrator`, with a 15-minute expiry on the unconsumed
  key. The resulting tailnet node is `ephemeral` so it disappears shortly
  after the Job pod disconnects.

When the run terminates — success, abort, or fail — the certificate and auth
key are already expired or unusable. No revocation step is required for
correctness.

## HTTP surface

Both endpoints share the existing `/v1/run-callbacks/{callback_token}/native/*`
family, alongside `github-token`, `pr-touchpoint`, `pr-merge`, and
`completed`. Glimmung's native launcher pre-bakes the full URLs for the
phase script as `GLIMMUNG_SSH_CERT_URL` and `GLIMMUNG_TAILSCALE_AUTHKEY_URL`
env vars (the callback token is already baked into the path; the secret
attempt token rides as the `X-Glimmung-Attempt-Token` header).

Glimmung resolves the token to a run + project, derives the principal/tag
from the project, and mints credentials scoped to that project.

### `POST /v1/run-callbacks/{callback_token}/native/ssh-cert`

Obtain a short-TTL OpenSSH user certificate over a caller-supplied public key.
This endpoint is a **gateway**: glimmung does not sign the certificate itself.
It resolves the callback token to a run + project, derives the principal and
`KeyId` server-side, then calls auth.romaine.life's signing endpoint
(`POST /api/auth/exchange/ssh-cert`) authenticated with glimmung's projected
SA token. auth signs the cert with the CA private key and glimmung relays the
result. See "SSH credential flow" below.

Request body:

```json
{ "public_key": "ssh-ed25519 AAAA..." }
```

Response body:

```json
{
  "certificate": "ssh-ed25519-cert-v01@openssh.com AAAA...",
  "principals": ["spirelens-agent"],
  "key_id": "glimmung-run:spirelens/run_01J...",
  "valid_before": "2026-05-29T17:42:00Z"
}
```

(auth.romaine.life does not return a `valid_after`, so neither does this
gateway. The consuming orchestrator script reads only `.certificate`.)

Failure modes:

- `404 not found` — callback token does not resolve to a run.
- `409 conflict` — run has no project recorded.
- `400 bad request` — `public_key` is missing/empty, the body carries an
  unknown field, **or** auth.romaine.life rejected the request (unparsable
  public key, disallowed extension, out-of-range ttl, principal outside the
  allowed pattern). auth's 400 is propagated faithfully — never masked.
- `502 bad gateway` — auth.romaine.life returned an unexpected status or was
  unreachable.
- `503 service unavailable` — the gateway is not configured (auth base URL or
  SA-token path empty), or auth itself has no CA private key configured (auth
  returns 503, propagated).

The certificate is bound to a single `principal`, derived as `<project>-agent`.
The remote host's sshd is configured to accept that principal for the local
account that owns the warm state. The `KeyId` field is logged on accept and
serves as the audit anchor.

Permissions on the certificate are limited to `permit-pty`. Port forwarding,
agent forwarding, X11, and `user-rc` are not permitted. glimmung requests only
`permit-pty`; auth enforces the allowed-extension set server-side.

### `POST /v1/run-callbacks/{callback_token}/native/tailscale-authkey`

Mint a single-use, pre-authorized, ephemeral Tailscale auth key.

Request body: `{}` (no input — tag and expiry are server-controlled.)

Response body:

```json
{
  "authkey": "tskey-auth-...",
  "tags": ["tag:spirelens-orchestrator"],
  "expires_at": "2026-05-29T17:47:00Z"
}
```

Failure modes:

- `404 not found` — callback token does not resolve to a run.
- `409 conflict` — run has no project recorded.
- `502 bad gateway` (wrapped as `500 internal error`) — Tailscale's API
  rejected the request or was unreachable.
- `503 service unavailable` — glimmung has no Tailscale OIDC trust
  credential configured.

The tag is derived as `tag:<project>-orchestrator`. The tag must be declared
in the tenant's Tailscale ACL and the OAuth client must own it; tenant ACLs
restrict what the orchestrator tag can reach (e.g., only the remote host's
SSH port).

## Orchestrator flow

A typical `env-prep.sh` step on a remote-host-backed project looks like:

```bash
# 1. Generate a one-shot ed25519 keypair on the pod.
ssh-keygen -t ed25519 -N "" -f "${GLIMMUNG_WORKING_DIR}/id_ed25519" -C "run=${GLIMMUNG_RUN_REF}"

# 2. Ask glimmung to sign it.
cert=$(curl -fsS -X POST -H "Content-Type: application/json" \
  -H "X-Glimmung-Attempt-Token: ${GLIMMUNG_ATTEMPT_TOKEN}" \
  -d "$(jq -nc --arg pk "$(cat ${GLIMMUNG_WORKING_DIR}/id_ed25519.pub)" '{public_key:$pk}')" \
  "${GLIMMUNG_SSH_CERT_URL}" | jq -r .certificate)
printf '%s' "$cert" > "${GLIMMUNG_WORKING_DIR}/id_ed25519-cert.pub"

# 3. Ask glimmung for a Tailscale auth key.
authkey=$(curl -fsS -X POST \
  -H "X-Glimmung-Attempt-Token: ${GLIMMUNG_ATTEMPT_TOKEN}" \
  "${GLIMMUNG_TAILSCALE_AUTHKEY_URL}" | jq -r .authkey)

# 4. Bring up Tailscale in userspace networking mode.
tailscaled --tun=userspace-networking --statedir="${GLIMMUNG_WORKING_DIR}/ts" \
  --socket="${GLIMMUNG_WORKING_DIR}/ts.sock" &
tailscale --socket="${GLIMMUNG_WORKING_DIR}/ts.sock" up \
  --authkey="${authkey}" --ephemeral --hostname="glimmung-${GLIMMUNG_RUN_REF}"

# 5. Resolve the remote host's tailnet IP, then SSH in.
host_ip=$(tailscale --socket="${GLIMMUNG_WORKING_DIR}/ts.sock" status --json | jq -r '...')
ssh -i "${GLIMMUNG_WORKING_DIR}/id_ed25519" \
    -o IdentityFile="${GLIMMUNG_WORKING_DIR}/id_ed25519" \
    -o CertificateFile="${GLIMMUNG_WORKING_DIR}/id_ed25519-cert.pub" \
    -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=accept-new \
    "<project>-agent@${host_ip}" "<remote command>"
```

Concrete realizations of this flow live in the consuming project repo (for
spirelens, `scripts/glimmung-native/env-prep.sh` et al.). The project-side
scripts are responsible for the keypair lifecycle, Tailscale bring-up, and
SSH invocation. Glimmung only owns credential minting.

## SSH credential flow

The `ssh-cert` endpoint does NOT use an in-process CA private key. glimmung
holds no CA private material at all. Per call it:

1. **Read SA token from disk.** The pod has a projected ServiceAccount token
   mounted with audience `https://auth.romaine.life` (the same token the
   managed-origin reconciler and the Tailscale federation flow use).
2. **Derive the cert identity server-side.** The principal is
   `<project>-agent` and the `KeyId` is `glimmung-run:<project>/<run_id>`,
   both derived from the run the callback token resolves to — never
   caller-supplied.
3. **Exchange for a signed certificate.** glimmung POSTs to
   `https://auth.romaine.life/api/auth/exchange/ssh-cert` with the SA token as
   a Bearer credential and a body of
   `{ "public_key": "<caller key>", "key_id": "<derived>", "principals":
   ["<derived>"], "extensions": ["permit-pty"], "ttl_seconds": 600 }`. auth
   resolves the caller via TokenReview, checks it against its SA allowlist,
   validates/bounds the principal/extensions/ttl, and signs the cert with the
   CA private key (KV: `auth-ssh-ca-private-key`, env `SSH_CA_PRIVATE_KEY`,
   held only on the auth side).
4. **Relay the result.** glimmung returns auth's signed `certificate` plus the
   echoed `principals`/`key_id`/`valid_before`.

auth.romaine.life is the single SSH CA issuer. The hosts trust auth's CA
public key (`GET /api/ssh/ca`) in `TrustedUserCAKeys`; they never trusted a
glimmung-held CA. There is exactly one signing authority — no fallback, no
second local signer. For this flow to succeed, the auth.romaine.life side must
allowlist glimmung's service account (`glimmung/infra-shared`) for the
ssh-cert exchange endpoint.

## Tailscale credential flow

The `tailscale-authkey` endpoint does NOT use a stored OAuth client secret.
Instead it drives a four-step OIDC workload-identity federation flow per
cold call (with the resulting Tailscale API access token cached
in-process until just before its expiry):

1. **Read SA token from disk.** The pod has a projected
   ServiceAccount token mounted with audience
   `https://auth.romaine.life` (the same token the managed-origin
   reconciler uses).
2. **Exchange for an auth.romaine.life-signed JWT.** Glimmung POSTs the
   SA token to `https://auth.romaine.life/api/auth/exchange/federation`
   with `{ "audience": "api.tailscale.com/<oidc_client_id>" }`. The
   response contains an auth.romaine.life-signed JWT carrying
   `iss=https://auth.romaine.life`, `aud=<the requested audience>`, and
   bounded `exp`.
3. **Exchange for a Tailscale API access token.** Glimmung POSTs the
   JWT to `https://api.tailscale.com/api/v2/oauth/token-exchange` with
   `client_id=<oidc_client_id>` and `jwt=<the JWT>`. Tailscale validates the JWT
   against its OIDC trust credential (signature checked against the
   `iss`-discovered JWKS at
   `https://auth.romaine.life/.well-known/openid-configuration`).
4. **Mint the tailnet auth key.** Glimmung uses the returned access
   token as a normal Tailscale API bearer to call
   `/api/v2/tailnet/<tailnet>/keys`, asking for an ephemeral,
   pre-authorized, single-use key tagged `tag:<project>-orchestrator`.

Tailscale's trust credential is identified by an OIDC client ID — not
secret on its own, since Tailscale validates the JWT signature against
auth.romaine.life's JWKS, not by possession of the client ID. We store
the ID as a chart value rather than a KV secret.

## Configuration

glimmung holds **no SSH CA KV secret**. Both remote-host endpoints
authenticate to auth.romaine.life with the projected SA token; neither needs
private key material on the glimmung side.

Non-secret chart values (consumed via the deployment's `env`):

| Chart value                          | Env var                                  | Required for                       |
|--------------------------------------|------------------------------------------|------------------------------------|
| `authRomaineLife.baseUrl`            | `AUTH_ROMAINE_LIFE_BASE_URL`             | `ssh-cert`, `tailscale-authkey`    |
| `authRomaineLife.tokenMountPath`     | `AUTH_ROMAINE_LIFE_TOKEN_PATH`           | `ssh-cert`, `tailscale-authkey`    |
| `remoteHost.tailscaleOidcClientId`   | `GLIMMUNG_TAILSCALE_OIDC_CLIENT_ID`      | `tailscale-authkey`                |
| `remoteHost.tailscaleTailnet`        | `GLIMMUNG_TAILSCALE_TAILNET`             | `tailscale-authkey`                |

Optional environment overrides:

| Env var                              | Default                       | Purpose                                                 |
|--------------------------------------|-------------------------------|---------------------------------------------------------|
| `GLIMMUNG_TAILSCALE_API_BASE_URL`    | `https://api.tailscale.com`   | Overridden in tests.                                    |
| `GLIMMUNG_TAILSCALE_AUTHKEY_TTL_SECONDS` | `900`                     | Bounded ceiling — clamped to `[300, 3600]`.             |

The certificate TTL glimmung requests is fixed at 600 seconds in code; auth
bounds it to `[60, 3600]` and rejects (does not clamp) anything outside that
range.

When the auth base URL or SA-token mount is empty, both `ssh-cert` and
`tailscale-authkey` return `503 service unavailable`. When the Tailscale OIDC
client ID is also empty, `tailscale-authkey` returns `503`. The endpoints are
independently gated.

### Migration guard

The retired in-process SSH CA private key (`GLIMMUNG_SSH_CA_PRIVATE_KEY`, KV
`glimmung-ssh-ca-private-key`) has been removed entirely. glimmung's startup
guard (`server.GuardRetiredSSHCAEnv`) **refuses to boot** if that env var is
still set, so a stale chart/ExternalSecret/KV wiring surfaces loudly instead
of silently re-establishing a second signing authority. There is one SSH CA
issuer — auth.romaine.life — with no fallback.

### auth.romaine.life allowlists

Both flows run from glimmung's main pod (`glimmung/infra-shared`) — not the
per-run native runner — because the handlers execute inside the glimmung
server process when an orchestrator calls a lease-callback endpoint. The
auth.romaine.life side must therefore allowlist `glimmung/infra-shared` for:

- the **ssh-cert exchange** (`POST /api/auth/exchange/ssh-cert`) — caller SA
  allowlist; and
- the **federation exchange** (`POST /api/auth/exchange/federation`, added in
  `nelsong6/auth#63`) — `K8S_FEDERATION_SA_ALLOWLIST`, plus `api.tailscale.com/*`
  in `FEDERATION_AUDIENCE_ALLOWLIST`.

## Runner-image surface

The `native-runner` image ships `openssh-client` and `tailscale` (binaries
only, no daemon launched in the image). Project scripts invoke `tailscaled
--tun=userspace-networking` per-run so the pod does not require `NET_ADMIN`
or a `/dev/net/tun` device. The Tailscale state directory lives under the
per-run working directory and is discarded with it.

## What this is not

- This is not a generic "run anything anywhere" primitive. Two tightly
  scoped credential mints, both bound to a single lease. The orchestrator
  scripts are project-owned.
- This is not an inbound SSH service on glimmung. Glimmung never accepts
  SSH connections; it only signs certificates that other parties accept.
- This is not a long-lived agent on the remote host. The remote host runs
  sshd and tailscaled; neither is glimmung-aware.
- This is not a substitute for the existing PR/touchpoint primitives. PR
  open, merge, and review-feedback flow still go through `pr_touchpoint`,
  `pr_merge`, and the signal bus.

## Feature Contracts

This surface is governed by [Auth And API Surface](features/auth-and-api/contract.md).
The invariants this document and the `ssh-cert` gateway uphold:

- **Single SSH CA issuer, no fallback.** auth.romaine.life is the sole signer.
  glimmung holds no CA private material and has no local signer to fall back
  to. The retired `GLIMMUNG_SSH_CA_PRIVATE_KEY` path is deleted, and the
  startup migration guard refuses to boot if it reappears (per
  [migration-policy.md](migration-policy.md): no compatibility, no fallback).
  - *Evidence:* `TestGuardRetiredSSHCAEnvTrips` / `TestGuardRetiredSSHCAEnvPasses`
    (`internal/server/ssh_cert_gateway_test.go`); the deleted
    `internal/server/ssh_ca.go` + `ssh_ca_test.go`.
- **The gateway authenticates with the audience-pinned SA token and derives
  the cert identity server-side.** principal = `<project>-agent`,
  `KeyId = glimmung-run:<project>/<run_id>`; never caller-supplied.
  - *Evidence:* `TestSSHCertExchangerHappyPath`, `TestSSHCertHandlerHappyPath`
    (asserts Bearer token, derived `key_id`/`principals`, `extensions`, `ttl`).
- **Upstream status codes are propagated faithfully** — a 400 from auth stays a
  400, 503 stays 503, anything unexpected becomes 502; a caller error is never
  masked as a server error or vice versa.
  - *Evidence:* `TestSSHCertExchangerPropagatesUpstreamError`,
    `TestSSHCertHandlerPropagatesAuthBadRequest`,
    `TestSSHCertHandlerPropagatesAuthUnavailable`,
    `TestSSHCertHandlerPropagatesAuthUpstreamFault`.
- **The endpoint fails closed.** Empty auth base URL / SA-token path → 503;
  unknown callback token → 404; run without project → 409; missing
  `public_key` or unknown body field → 400.
  - *Evidence:* `TestSSHCertHandlerExchangerDisabledReturns503`,
    `TestSSHCertHandlerUnknownTokenReturns404`,
    `TestSSHCertHandlerRejectsRunWithoutProject`,
    `TestSSHCertHandlerMissingPubKeyReturns400`.
- **The route inventory is unchanged** — the wire path stays
  `POST /v1/run-callbacks/{callback_token}/native/ssh-cert`.
  - *Evidence:* `internal/server/route_inventory_test.go`.

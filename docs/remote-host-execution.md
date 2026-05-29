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
lease's callback token — the same possession-is-proof token that authorizes
`POST /v1/lease-callbacks/{callback_token}/heartbeat` and
`/release`. That token bounds every credential glimmung mints under this
contract to a single lease.

There are no long-lived credentials deposited on the remote host. Trust
anchors only:

- An SSH CA public key in the remote host's `TrustedUserCAKeys`.
- A Tailscale device identity for the remote host, signed in once.

Glimmung holds the corresponding private material:

- The SSH CA private key (KV: `glimmung-ssh-ca-private-key`).
- A projected Kubernetes ServiceAccount token, audience-pinned to
  `https://auth.romaine.life`. The same mount the managed-origin reconciler
  uses. No KV-resident Tailscale client secret — Tailscale auth-key mints
  flow through an OIDC workload-identity federation handshake against
  auth.romaine.life (see "Tailscale credential flow" below).

Per run, glimmung produces:

- A 10-minute OpenSSH user certificate over the orchestrator's freshly
  generated public key, with `KeyId = glimmung-lease:<project>/<lease_id>` for
  audit and a single project-derived `principal`.
- A single-use, pre-authorized, ephemeral Tailscale auth key tagged
  `tag:<project>-orchestrator`, with a 15-minute expiry on the unconsumed
  key. The resulting tailnet node is `ephemeral` so it disappears shortly
  after the Job pod disconnects.

When the lease ends — release, expiry, cancel, or terminal completion — the
certificate and auth key are already expired or unusable. No revocation step
is required for correctness.

## HTTP surface

Both endpoints share the existing `/v1/lease-callbacks/{callback_token}/...`
family. The callback token is the only credential the orchestrator presents;
glimmung resolves it to a lease row, requires `state=claimed`, derives the
project, and mints credentials scoped to that project.

### `POST /v1/lease-callbacks/{callback_token}/ssh-cert`

Sign a short-TTL OpenSSH user certificate over a caller-supplied public key.

Request body:

```json
{ "public_key": "ssh-ed25519 AAAA..." }
```

Response body:

```json
{
  "certificate": "ssh-ed25519-cert-v01@openssh.com AAAA...",
  "principals": ["spirelens-agent"],
  "key_id": "glimmung-lease:spirelens/lse_01J...",
  "valid_after": "2026-05-29T17:32:00Z",
  "valid_before": "2026-05-29T17:42:00Z"
}
```

Failure modes:

- `404 not found` — callback token does not resolve to a lease.
- `409 conflict` — lease is not in `claimed` state.
- `400 bad request` — public key is empty or unparsable.
- `503 service unavailable` — glimmung has no SSH CA private key configured.

The certificate is bound to a single `principal`, derived as `<project>-agent`.
The remote host's sshd is configured to accept that principal for the local
account that owns the warm state. The `KeyId` field is logged on accept and
serves as the audit anchor.

Permissions on the certificate are limited to `permit-pty`. Port forwarding,
agent forwarding, X11, and `user-rc` are not permitted.

### `POST /v1/lease-callbacks/{callback_token}/tailscale-authkey`

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

- `404 not found` — callback token does not resolve to a lease.
- `409 conflict` — lease is not in `claimed` state.
- `502 bad gateway` (wrapped as `500 internal error`) — Tailscale's API
  rejected the request or was unreachable.
- `503 service unavailable` — glimmung has no Tailscale OAuth credentials
  configured.

The tag is derived as `tag:<project>-orchestrator`. The tag must be declared
in the tenant's Tailscale ACL and the OAuth client must own it; tenant ACLs
restrict what the orchestrator tag can reach (e.g., only the remote host's
SSH port).

## Orchestrator flow

A typical `env-prep.sh` step on a remote-host-backed project looks like:

```bash
# 1. Generate a one-shot ed25519 keypair on the pod.
ssh-keygen -t ed25519 -N "" -f "${GLIMMUNG_WORKING_DIR}/id_ed25519" -C "lease=${GLIMMUNG_LEASE_REF}"

# 2. Ask glimmung to sign it.
cert=$(curl -fsS -X POST -H "Content-Type: application/json" \
  -d "$(jq -nc --arg pk "$(cat ${GLIMMUNG_WORKING_DIR}/id_ed25519.pub)" '{public_key:$pk}')" \
  "${GLIMMUNG_BASE}/v1/lease-callbacks/${GLIMMUNG_CALLBACK_TOKEN}/ssh-cert" | jq -r .certificate)
printf '%s' "$cert" > "${GLIMMUNG_WORKING_DIR}/id_ed25519-cert.pub"

# 3. Ask glimmung for a Tailscale auth key.
authkey=$(curl -fsS -X POST "${GLIMMUNG_BASE}/v1/lease-callbacks/${GLIMMUNG_CALLBACK_TOKEN}/tailscale-authkey" \
  | jq -r .authkey)

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

KV secrets (consumed via `ExternalSecret` → pod `envFrom`):

| KV secret                                | Env var                                  | Required for          |
|------------------------------------------|------------------------------------------|-----------------------|
| `glimmung-ssh-ca-private-key`            | `GLIMMUNG_SSH_CA_PRIVATE_KEY`            | `ssh-cert` endpoint   |

Non-secret chart values (consumed via the deployment's `env`):

| Chart value                          | Env var                                  | Required for          |
|--------------------------------------|------------------------------------------|-----------------------|
| `remoteHost.tailscaleOidcClientId`   | `GLIMMUNG_TAILSCALE_OIDC_CLIENT_ID`      | `tailscale-authkey`   |
| `remoteHost.tailscaleTailnet`        | `GLIMMUNG_TAILSCALE_TAILNET`             | `tailscale-authkey`   |
| `authRomaineLife.baseUrl`            | `AUTH_ROMAINE_LIFE_BASE_URL`             | `tailscale-authkey`   |
| `authRomaineLife.tokenMountPath`     | `AUTH_ROMAINE_LIFE_TOKEN_PATH`           | `tailscale-authkey`   |

Optional environment overrides:

| Env var                              | Default                       | Purpose                                                 |
|--------------------------------------|-------------------------------|---------------------------------------------------------|
| `GLIMMUNG_TAILSCALE_API_BASE_URL`    | `https://api.tailscale.com`   | Overridden in tests.                                    |
| `GLIMMUNG_SSH_CERT_TTL_SECONDS`      | `600`                         | Bounded ceiling — clamped to `[60, 3600]`.              |
| `GLIMMUNG_TAILSCALE_AUTHKEY_TTL_SECONDS` | `900`                     | Bounded ceiling — clamped to `[300, 3600]`.             |

When the SSH CA key is empty, `ssh-cert` returns `503 service
unavailable`. When the Tailscale OIDC client ID or auth.romaine.life
mount is empty, `tailscale-authkey` returns `503`. Both endpoints are
independently gated.

The auth.romaine.life federation endpoint
(`POST /api/auth/exchange/federation`, added in `nelsong6/auth#63`) is
the substrate this flow depends on. The federation handshake runs from
glimmung's main pod (`glimmung/infra-shared`) — not the per-run native
runner — because the `tailscale-authkey` handler executes inside the
glimmung server process when an orchestrator calls the lease-callback
endpoint. The auth.romaine.life side must therefore allowlist
`glimmung/infra-shared` in `K8S_FEDERATION_SA_ALLOWLIST` and
`api.tailscale.com/*` in `FEDERATION_AUDIENCE_ALLOWLIST` for this flow
to succeed.

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

"""Auth dependencies for admin endpoints.

Two paths land at the same `User` shape:

1. **Entra ID** (CLI, dashboard) — `az account get-access-token --resource
   <client-id>` or MSAL.js produces an access token with audience = glimmung's
   Entra app reg client_id. We validate signature + audience + issuer via JWKS
   and check the email claim against an allowlist.

2. **Kubernetes service-account token** (in-cluster callers — tank-operator,
   future agents) — the caller pod presents its projected SA token as
   `Authorization: Bearer <token>`. We POST it to `TokenReview` on the cluster
   API server and check the resolved `system:serviceaccount:<ns>:<name>`
   against the configured allowlist.

The TokenReview path mirrors the kube-rbac-proxy pattern used by the mcp-*
deployments (system:auth-delegator on glimmung's SA), but lives in-app
because glimmung is publicly exposed and can't bind its upstream
loopback-only the way an MCP pod can. JWT shape (three base64 segments
separated by dots, header decodes to `iss == kubernetes/serviceaccount`)
disambiguates SA tokens from Entra tokens before any network call.

Pattern lifted from tank-operator/backend/src/tank_operator/auth.py — kept
the JWKS validator, dropped the session-minting layer (which is for SPA UX,
not CLI).
"""

import asyncio
import base64
import json
import logging
import re
import ssl
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx
import jwt
from fastapi import Header, HTTPException
from jwt import PyJWKClient

from glimmung.settings import get_settings

log = logging.getLogger(__name__)

_JWKS_URL = "https://login.microsoftonline.com/common/discovery/v2.0/keys"
_ENTRA_ISSUER_PATTERN = re.compile(r"^https://login\.microsoftonline\.com/.+/v2\.0$")
_jwks_client = PyJWKClient(_JWKS_URL, cache_keys=True, lifespan=3600)
_ALG = "RS256"
# Cluster-issued SA tokens carry this issuer in the JWT header. Kubelet's
# legacy tokens use the literal "kubernetes/serviceaccount"; bound (projected)
# tokens use the cluster's OIDC issuer URL. Either way, we only use the header
# to *route* between the two validation paths — TokenReview is the actual
# authority on whether the token is valid, so a header guess that's wrong just
# means we 401 a legitimate Entra token (caller retries with a properly-shaped
# bearer). We don't fall back token-type → token-type because that doubles the
# error blast radius on misconfigured callers.
_K8S_SA_ISSUER_HINTS = (
    "kubernetes/serviceaccount",
    "https://kubernetes.default.svc",
    # AKS projected-token issuer prefix.
    "https://oidc.prod-aks.azure.com/",
)


@dataclass
class User:
    sub: str
    email: str
    name: str


def _allowed_emails() -> set[str]:
    raw = get_settings().allowed_emails
    return {e.strip().lower() for e in raw.split(",") if e.strip()}


def _allowed_service_accounts() -> set[str]:
    """Parsed `<namespace>/<sa-name>` allowlist as `system:serviceaccount:`-
    prefixed usernames (the form TokenReview returns)."""
    raw = get_settings().k8s_sa_allowlist
    out: set[str] = set()
    for entry in raw.split(","):
        entry = entry.strip()
        if not entry or "/" not in entry:
            continue
        ns, name = entry.split("/", 1)
        ns, name = ns.strip(), name.strip()
        if ns and name:
            out.add(f"system:serviceaccount:{ns}:{name}")
    return out


def _verify_entra_token(token: str) -> dict[str, Any]:
    settings = get_settings()
    if not settings.entra_client_id:
        raise HTTPException(503, "ENTRA_CLIENT_ID not configured")

    # JWKS lookup parses the unverified header, so a non-JWT bearer raises
    # PyJWTError here too — keep both calls under the same handler so callers
    # see 401, not 500.
    try:
        signing_key = _jwks_client.get_signing_key_from_jwt(token)
        payload = jwt.decode(
            token,
            signing_key.key,
            algorithms=[_ALG],
            audience=settings.entra_client_id,
            options={"verify_iss": False},  # we check issuer regex below
        )
    except jwt.PyJWTError as e:
        raise HTTPException(401, f"invalid token: {e}") from e

    iss = payload.get("iss", "")
    if not _ENTRA_ISSUER_PATTERN.match(iss):
        raise HTTPException(401, f"unexpected issuer: {iss}")
    return payload


def _looks_like_k8s_sa_token(token: str) -> bool:
    """Cheap iss-prefix check on the unverified JWT body to route between the
    two validators. TokenReview is the actual authority — this is just for
    picking the right code path."""
    parts = token.split(".")
    if len(parts) != 3:
        return False
    try:
        # base64url, no padding — JWT spec.
        body = parts[1] + "=" * (-len(parts[1]) % 4)
        claims = json.loads(base64.urlsafe_b64decode(body))
    except (ValueError, json.JSONDecodeError):
        return False
    iss = str(claims.get("iss", ""))
    return any(iss == hint or iss.startswith(hint) for hint in _K8S_SA_ISSUER_HINTS)


async def _verify_k8s_sa_token(token: str) -> str:
    """POST to TokenReview; return `system:serviceaccount:<ns>:<name>` if the
    token is valid. Raise HTTPException on invalid / unauthorized.

    Glimmung's pod SA needs `system:auth-delegator` to be allowed to call
    TokenReview; see k8s/templates/auth-delegator.yaml.
    """
    settings = get_settings()
    sa_token_path = Path(settings.k8s_sa_token_path)
    ca_cert_path = Path(settings.k8s_ca_cert_path)
    if not sa_token_path.is_file() or not ca_cert_path.is_file():
        # Not running in-cluster (or the SA isn't mounted). Disable the
        # path rather than 500 — caller can fall back to Entra.
        raise HTTPException(503, "k8s SA token validation unavailable (not in-cluster)")

    try:
        own_token = sa_token_path.read_text().strip()
    except OSError as e:
        raise HTTPException(503, f"could not read pod SA token: {e}") from e

    # Use a per-request SSL context that pins the cluster CA. httpx accepts a
    # configured SSLContext via `verify=`.
    ctx = ssl.create_default_context(cafile=str(ca_cert_path))

    review = {
        "apiVersion": "authentication.k8s.io/v1",
        "kind": "TokenReview",
        "spec": {"token": token},
    }
    url = f"{settings.k8s_api_host.rstrip('/')}/apis/authentication.k8s.io/v1/tokenreviews"
    headers = {
        "Authorization": f"Bearer {own_token}",
        "Content-Type": "application/json",
        "Accept": "application/json",
    }

    try:
        async with httpx.AsyncClient(verify=ctx, timeout=10.0) as client:
            resp = await client.post(url, json=review, headers=headers)
    except httpx.HTTPError as e:
        log.exception("TokenReview request failed")
        raise HTTPException(503, f"TokenReview unreachable: {e}") from e

    if resp.status_code == 403:
        # Glimmung's SA isn't bound to system:auth-delegator. Surface this
        # loudly — silent 401s on every SA-token request would be a nightmare
        # to diagnose otherwise.
        log.error(
            "TokenReview returned 403 for glimmung's pod SA — bind "
            "system:auth-delegator (k8s/templates/auth-delegator.yaml)"
        )
        raise HTTPException(503, "TokenReview not permitted; check glimmung RBAC")
    if resp.status_code >= 400:
        raise HTTPException(401, f"TokenReview error: {resp.status_code}")

    body = resp.json()
    status = body.get("status", {}) or {}
    if not status.get("authenticated"):
        err = status.get("error") or "token rejected by TokenReview"
        raise HTTPException(401, f"invalid SA token: {err}")

    user = (status.get("user") or {}).get("username", "")
    if not user.startswith("system:serviceaccount:"):
        # TokenReview accepts node tokens, user impersonation tokens, etc.
        # We only want SA-shaped principals on this path.
        raise HTTPException(403, f"non-service-account principal: {user}")

    return user


async def require_entra_user(authorization: str | None = Header(default=None)) -> User:
    if not authorization or not authorization.lower().startswith("bearer "):
        raise HTTPException(401, "missing bearer token")
    token = authorization[7:]

    allowed = _allowed_emails()
    if not allowed:
        raise HTTPException(503, "ALLOWED_EMAILS not configured")

    # JWKS fetch + verify is sync + network-bound; offload off the loop.
    payload = await asyncio.to_thread(_verify_entra_token, token)

    email = (payload.get("email") or payload.get("preferred_username") or "").lower()
    if not email:
        raise HTTPException(401, "token has no email or preferred_username claim")
    if email not in allowed:
        raise HTTPException(403, "email not allowed")

    return User(
        sub=str(payload.get("sub", "")),
        email=email,
        name=str(payload.get("name", "")),
    )


async def require_admin_user(authorization: str | None = Header(default=None)) -> User:
    """Admin auth: accept either an Entra ID token (humans + CLI) or a K8s
    service-account token whose `<ns>/<name>` is in K8S_SA_ALLOWLIST
    (in-cluster callers like tank-operator).

    Routing between the two is done by the unverified `iss` claim — Entra
    tokens issued by login.microsoftonline.com vs. cluster-issued tokens
    keyed off `_K8S_SA_ISSUER_HINTS`. The actual trust decision happens
    inside the path-specific validator (JWKS or TokenReview). Tokens that
    look neither way fall through to the Entra validator, which will 401
    with a JWKS error rather than silently accept anything.
    """
    if not authorization or not authorization.lower().startswith("bearer "):
        raise HTTPException(401, "missing bearer token")
    token = authorization[7:]

    if _looks_like_k8s_sa_token(token):
        allowed = _allowed_service_accounts()
        if not allowed:
            raise HTTPException(503, "K8S_SA_ALLOWLIST not configured")
        username = await _verify_k8s_sa_token(token)
        if username not in allowed:
            raise HTTPException(403, f"service account not allowed: {username}")
        # Synthesize a User with the SA principal as email/sub. Admin
        # endpoints don't read individual fields beyond logging/identity.
        return User(sub=username, email=username, name=username)

    return await require_entra_user(authorization)

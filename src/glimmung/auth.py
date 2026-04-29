"""Auth for admin endpoints.

Two paths, dispatched on the unverified `iss` claim:

  1. **Entra ID** (CLI flow). `az account get-access-token --resource <client-id>`
     yields a token whose audience is glimmung's Entra app reg client_id. Validate
     via JWKS; check email claim against `ALLOWED_EMAILS`.

  2. **Kubernetes ServiceAccount** (in-cluster flow). Pods present a projected
     SA token (the default one mounted at
     `/var/run/secrets/kubernetes.io/serviceaccount/token` works, audience
     `kubernetes.default.svc`). We forward the token to the cluster API server
     via TokenReview; the resulting `system:serviceaccount:<ns>:<name>` must be
     in `ALLOWED_SERVICE_ACCOUNTS`.

The k8s SA path was added so in-cluster MCP servers (specifically tank-operator
session pods) can register projects/workflows/hosts without minting Entra
tokens. Pattern is conceptually similar to ArgoCD's Dex `aks-sa` connector but
implemented directly via TokenReview to avoid pulling Dex into glimmung.
"""

import asyncio
import re
from dataclasses import dataclass
from typing import Any

import jwt
from fastapi import Header, HTTPException
from jwt import PyJWKClient
from kubernetes import client as k8s_client, config as k8s_config

from glimmung.settings import get_settings

_JWKS_URL = "https://login.microsoftonline.com/common/discovery/v2.0/keys"
_ENTRA_ISSUER_PATTERN = re.compile(r"^https://login\.microsoftonline\.com/.+/v2\.0$")
_jwks_client = PyJWKClient(_JWKS_URL, cache_keys=True, lifespan=3600)
_ALG = "RS256"

# Lazy-initialized k8s client. In-cluster config (SA token mounted at
# /var/run/secrets/kubernetes.io/serviceaccount). Out-of-cluster runs that
# never see a k8s SA token simply never hit this path.
_k8s_authn: k8s_client.AuthenticationV1Api | None = None


def _get_k8s_authn() -> k8s_client.AuthenticationV1Api:
    global _k8s_authn
    if _k8s_authn is None:
        k8s_config.load_incluster_config()
        _k8s_authn = k8s_client.AuthenticationV1Api()
    return _k8s_authn


@dataclass
class User:
    sub: str
    email: str  # for k8s SA: the system:serviceaccount:... username
    name: str


def _allowed_emails() -> set[str]:
    raw = get_settings().allowed_emails
    return {e.strip().lower() for e in raw.split(",") if e.strip()}


def _allowed_service_accounts() -> set[str]:
    raw = get_settings().allowed_service_accounts
    return {s.strip() for s in raw.split(",") if s.strip()}


def _verify_entra_token(token: str) -> dict[str, Any]:
    settings = get_settings()
    if not settings.entra_client_id:
        raise HTTPException(503, "ENTRA_CLIENT_ID not configured")

    signing_key = _jwks_client.get_signing_key_from_jwt(token)
    try:
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


def _verify_k8s_sa_token(token: str) -> str:
    """Run TokenReview against the cluster API. Return the resulting username
    on success (`system:serviceaccount:<ns>:<name>`)."""
    api = _get_k8s_authn()
    review = k8s_client.V1TokenReview(spec=k8s_client.V1TokenReviewSpec(token=token))
    try:
        result = api.create_token_review(review)
    except Exception as e:
        raise HTTPException(401, f"token review failed: {e}") from e

    status = result.status
    if not status or not status.authenticated:
        msg = status.error if status and status.error else "not authenticated"
        raise HTTPException(401, f"k8s token rejected: {msg}")
    user = status.user
    if not user or not user.username:
        raise HTTPException(401, "k8s token review returned no username")
    return user.username


async def require_entra_user(authorization: str | None = Header(default=None)) -> User:
    """Admin-endpoint dependency. Accepts either an Entra ID JWT (CLI) or a
    Kubernetes projected SA token (in-cluster). Path is chosen based on the
    unverified `iss` claim. Name kept as `require_entra_user` for back-compat
    with existing endpoint signatures."""
    if not authorization or not authorization.lower().startswith("bearer "):
        raise HTTPException(401, "missing bearer token")
    token = authorization[7:]

    # Pre-decode without signature/audience checks just to read the issuer.
    try:
        unverified = jwt.decode(
            token, options={"verify_signature": False, "verify_aud": False}
        )
    except jwt.PyJWTError as e:
        raise HTTPException(401, f"unparseable token: {e}") from e
    iss = str(unverified.get("iss", ""))

    if _ENTRA_ISSUER_PATTERN.match(iss):
        # Entra path
        allowed = _allowed_emails()
        if not allowed:
            raise HTTPException(503, "ALLOWED_EMAILS not configured")
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

    # Kubernetes SA path. The cluster API server's TokenReview is the
    # authoritative validator — issuer pinning would couple us to the AKS
    # OIDC URL format unnecessarily.
    allowed_sas = _allowed_service_accounts()
    if not allowed_sas:
        raise HTTPException(503, "ALLOWED_SERVICE_ACCOUNTS not configured")
    username = await asyncio.to_thread(_verify_k8s_sa_token, token)
    if username not in allowed_sas:
        raise HTTPException(403, f"service account not allowed: {username}")
    return User(sub=username, email=username, name=username)

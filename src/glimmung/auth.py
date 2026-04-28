"""Entra ID auth for admin endpoints.

CLI flow: `az account get-access-token --resource <client-id> --query accessToken -o tsv`
yields an access token whose audience is glimmung's Entra app reg client_id. We
validate signature + audience + issuer via JWKS and check the email claim
against an allowlist. No session cookie, no minted backend JWT — admin calls
are infrequent enough that revalidating each request is fine.

Pattern lifted from tank-operator/backend/src/tank_operator/auth.py — kept the
JWKS validator, dropped the session-minting layer (which is for SPA UX, not CLI).
"""

import asyncio
import re
from dataclasses import dataclass
from typing import Any

import jwt
from fastapi import Header, HTTPException
from jwt import PyJWKClient

from glimmung.settings import get_settings

_JWKS_URL = "https://login.microsoftonline.com/common/discovery/v2.0/keys"
_ENTRA_ISSUER_PATTERN = re.compile(r"^https://login\.microsoftonline\.com/.+/v2\.0$")
_jwks_client = PyJWKClient(_JWKS_URL, cache_keys=True, lifespan=3600)
_ALG = "RS256"


@dataclass
class User:
    sub: str
    email: str
    name: str


def _allowed_emails() -> set[str]:
    raw = get_settings().allowed_emails
    return {e.strip().lower() for e in raw.split(",") if e.strip()}


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

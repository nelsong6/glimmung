"""GitHub App auth — JWT mint, installation tokens, webhook signature verify,
and workflow_dispatch caller. Pattern lifted from
tank-operator/mcp-servers/github/src/mcp_github/auth.py.
"""

import asyncio
import hashlib
import hmac
import time
from typing import Any

import httpx
import jwt


class GitHubAppTokenMinter:
    """Mints + caches a GitHub App installation token. Tokens last ~1h; we
    refresh 5 min early. Async-safe via a Lock."""

    def __init__(self, app_id: str, installation_id: str, private_key: str) -> None:
        self._app_id = app_id
        self._installation_id = installation_id
        self._private_key = private_key
        self._lock = asyncio.Lock()
        self._token: str | None = None
        self._expires_at: float = 0.0

    async def installation_token(self) -> str:
        async with self._lock:
            if self._token and self._expires_at - time.time() > 300:
                return self._token
            self._token, self._expires_at = await self._fetch()
            return self._token

    async def _fetch(self) -> tuple[str, float]:
        now = int(time.time())
        app_jwt = jwt.encode(
            {"iat": now - 60, "exp": now + 540, "iss": self._app_id},
            self._private_key,
            algorithm="RS256",
        )
        async with httpx.AsyncClient(timeout=10.0) as client:
            r = await client.post(
                f"https://api.github.com/app/installations/{self._installation_id}/access_tokens",
                headers={
                    "Authorization": f"Bearer {app_jwt}",
                    "Accept": "application/vnd.github+json",
                    "X-GitHub-Api-Version": "2022-11-28",
                },
            )
            r.raise_for_status()
            body = r.json()
            return body["token"], time.time() + 3300


def verify_webhook_signature(secret: str, body: bytes, signature_header: str | None) -> bool:
    """Verify GitHub's X-Hub-Signature-256 header. Constant-time comparison."""
    if not signature_header or not signature_header.startswith("sha256="):
        return False
    expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature_header)


async def dispatch_workflow(
    minter: GitHubAppTokenMinter,
    *,
    repo: str,
    workflow_filename: str,
    ref: str,
    inputs: dict[str, Any],
) -> None:
    """POST /repos/{repo}/actions/workflows/{filename}/dispatches"""
    token = await minter.installation_token()
    async with httpx.AsyncClient(timeout=15.0) as client:
        r = await client.post(
            f"https://api.github.com/repos/{repo}/actions/workflows/{workflow_filename}/dispatches",
            headers={
                "Authorization": f"Bearer {token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
            json={"ref": ref, "inputs": inputs},
        )
        r.raise_for_status()


async def post_issue_comment(
    minter: GitHubAppTokenMinter,
    *,
    repo: str,
    issue_number: int,
    body: str,
) -> None:
    """POST /repos/{repo}/issues/{number}/comments"""
    token = await minter.installation_token()
    async with httpx.AsyncClient(timeout=15.0) as client:
        r = await client.post(
            f"https://api.github.com/repos/{repo}/issues/{issue_number}/comments",
            headers={
                "Authorization": f"Bearer {token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
            json={"body": body},
        )
        r.raise_for_status()


async def list_open_issues(
    minter: GitHubAppTokenMinter,
    *,
    repo: str,
    per_page: int = 100,
) -> list[dict[str, Any]]:
    """GET /repos/{repo}/issues?state=open. Filters out PRs (GitHub returns
    them in the issues endpoint by default — they're issues with a
    `pull_request` field). Single page; we don't paginate yet (single-user,
    handful of repos)."""
    token = await minter.installation_token()
    async with httpx.AsyncClient(timeout=15.0) as client:
        r = await client.get(
            f"https://api.github.com/repos/{repo}/issues",
            headers={
                "Authorization": f"Bearer {token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
            params={"state": "open", "per_page": per_page},
        )
        r.raise_for_status()
        items = r.json() or []
        return [item for item in items if "pull_request" not in item]


async def get_issue(
    minter: GitHubAppTokenMinter,
    *,
    repo: str,
    issue_number: int,
) -> dict[str, Any]:
    """GET /repos/{repo}/issues/{number}."""
    token = await minter.installation_token()
    async with httpx.AsyncClient(timeout=15.0) as client:
        r = await client.get(
            f"https://api.github.com/repos/{repo}/issues/{issue_number}",
            headers={
                "Authorization": f"Bearer {token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        r.raise_for_status()
        return r.json()

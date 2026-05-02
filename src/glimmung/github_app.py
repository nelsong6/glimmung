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


async def cancel_workflow_run(
    minter: GitHubAppTokenMinter,
    *,
    repo: str,
    run_id: int,
) -> bool:
    """POST /repos/{repo}/actions/runs/{run_id}/cancel.

    Returns True if GH accepted the cancel (202), False if the run was
    already terminal on the GH side (404 from the cancel endpoint, which
    GH returns once a run has finished naturally). Other HTTP errors
    propagate — the caller should log + still release the lease, since
    the lease release is independent of GH-side state.

    GH returns 202 (accepted, async). The actual run-state flip to
    `cancelled` lands later via the `workflow_run.completed` webhook;
    that handler's lock-release path is idempotent, so the cancel
    endpoint releasing the lease here doesn't conflict with the
    eventual completion handler doing the same.
    """
    token = await minter.installation_token()
    async with httpx.AsyncClient(timeout=15.0) as client:
        r = await client.post(
            f"https://api.github.com/repos/{repo}/actions/runs/{run_id}/cancel",
            headers={
                "Authorization": f"Bearer {token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        if r.status_code == 404:
            return False
        r.raise_for_status()
        return True


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


class PRCreateNoDiff(Exception):
    """Raised when `gh pr create` rejects because head and base have no
    diff. Glimmung treats this as a terminal failure of the PR primitive
    (per the v1 design — handled in #69 follow-up to make recoverable)."""


class PRCreateAlreadyExists(Exception):
    """Raised when a PR already exists for the head branch. Carries the
    existing PR number so the caller can record-and-continue without
    opening a duplicate. Future-proofs the rewind/recycle case."""

    def __init__(self, pr_number: int, html_url: str) -> None:
        super().__init__(f"PR already exists: #{pr_number}")
        self.pr_number = pr_number
        self.html_url = html_url


async def open_pull_request(
    minter: GitHubAppTokenMinter,
    *,
    repo: str,
    head: str,
    base: str,
    title: str,
    body: str,
) -> tuple[int, str]:
    """POST /repos/{repo}/pulls. Returns (pr_number, html_url).

    Maps GH's documented error shapes onto typed exceptions so the caller
    can branch policy on them:
      - 422 with "No commits between"  → PRCreateNoDiff
      - 422 with "A pull request already exists" → PRCreateAlreadyExists
        (carries the existing PR number, looked up via list-PRs by head).
      - anything else                  → httpx.HTTPStatusError
    """
    token = await minter.installation_token()
    async with httpx.AsyncClient(timeout=15.0) as client:
        r = await client.post(
            f"https://api.github.com/repos/{repo}/pulls",
            headers={
                "Authorization": f"Bearer {token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
            json={"title": title, "body": body, "head": head, "base": base},
        )
        if r.status_code == 422:
            payload = r.json()
            errors = payload.get("errors") or []
            messages = " ".join(str(e.get("message", "")) for e in errors)
            if "No commits between" in messages or "no commits between" in messages.lower():
                raise PRCreateNoDiff(messages or payload.get("message", ""))
            if "already exists" in messages.lower() or "already exists" in payload.get("message", "").lower():
                # Look up the existing PR by head ref to surface the number.
                # GH expects head=`<owner>:<branch>` for cross-fork lookups, but
                # for same-repo opens just the branch works.
                owner = repo.split("/")[0]
                head_qualifier = f"{owner}:{head}"
                lookup = await client.get(
                    f"https://api.github.com/repos/{repo}/pulls",
                    headers={
                        "Authorization": f"Bearer {token}",
                        "Accept": "application/vnd.github+json",
                        "X-GitHub-Api-Version": "2022-11-28",
                    },
                    params={"head": head_qualifier, "state": "all"},
                )
                lookup.raise_for_status()
                existing = lookup.json() or []
                if existing:
                    pr = existing[0]
                    raise PRCreateAlreadyExists(pr["number"], pr["html_url"])
                raise PRCreateAlreadyExists(0, "")
        r.raise_for_status()
        data = r.json()
        return int(data["number"]), str(data.get("html_url", ""))



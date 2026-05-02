"""HTTP client for glimmung.

Reads its own pod's projected SA token freshly per call. Kubelet rotates
the token file in-place at ~50 min (eager renewal inside the 1h TTL); a
cached/at-import-time read goes stale silently and 401s the next request
past the rotation. Same reason mcp-auth-proxy reads the file per request.

Glimmung validates the bearer via TokenReview against the cluster API and
checks the resolved `system:serviceaccount:<ns>:<name>` against its
`K8S_SA_ALLOWLIST`. The MCP server's SA `mcp-glimmung/mcp-glimmung` is
in that allowlist by default (see glimmung/src/glimmung/settings.py).
"""

from __future__ import annotations

import logging
from pathlib import Path
from typing import Any

import httpx

log = logging.getLogger(__name__)

DEFAULT_TOKEN_PATH = Path("/var/run/secrets/kubernetes.io/serviceaccount/token")
DEFAULT_BASE_URL = "http://glimmung.glimmung.svc"


class GlimmungClient:
    def __init__(
        self,
        base_url: str = DEFAULT_BASE_URL,
        token_path: Path = DEFAULT_TOKEN_PATH,
        timeout: float = 30.0,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._token_path = token_path
        # One Client for connection pooling; auth header re-built per call
        # because the file rotates underneath us.
        self._http = httpx.Client(timeout=timeout)

    def _headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self._token_path.read_text().strip()}"}

    def get(self, path: str, params: dict[str, Any] | None = None) -> Any:
        r = self._http.get(self._base_url + path, params=params, headers=self._headers())
        r.raise_for_status()
        return r.json()

    def patch(self, path: str, json: dict[str, Any]) -> Any:
        r = self._http.patch(self._base_url + path, json=json, headers=self._headers())
        r.raise_for_status()
        return r.json()

    def post(self, path: str, params: dict[str, Any] | None = None) -> Any:
        r = self._http.post(self._base_url + path, params=params, headers=self._headers())
        r.raise_for_status()
        return r.json()

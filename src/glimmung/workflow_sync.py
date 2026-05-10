"""Workflow upstream-sync helpers.

The "register-workflow.sh in each project repo" model worked when there
were two consumers, but didn't scale: every workflow-shape change required
somebody to remember to re-run the script against deployed glimmung. With
the always-run teardown phase landing (#297) and more projects coming
online, manual re-registration started actively breaking flows.

This module lets glimmung compare a project-owned desired-state manifest
against the workflow registration stored in Cosmos. The convention is
`.glimmung/workflows/<workflow-name>.yaml` in the project repo's default
branch. That file has the same shape `WorkflowRegister` accepts, but it is
not the runtime workflow source and it is not a GitHub-backed workflow.
Glimmung's DB stays the runtime source of truth. The upstream file is only
the *desired* state, and "Install"/`sync_workflow` is the explicit human
(or agent) action that promotes desired → running.

Two operations:
- `fetch_upstream` — read the file, parse YAML, return a `WorkflowRegister`.
  Read-only, safe to call from a non-mutating endpoint.
- `compute_in_sync` — given upstream + current, return whether they match.
  Uses Pydantic `model_dump(mode="json")` on both sides so the comparison
  ignores Python-object identity and only sees the serialized shape.
"""

from __future__ import annotations

from typing import Any

import httpx
import yaml

from glimmung.github_app import GitHubAppTokenMinter
from glimmung.models import Workflow, WorkflowRegister

WORKFLOW_FILE_TEMPLATE = ".glimmung/workflows/{name}.yaml"


class UpstreamFetchError(Exception):
    """Raised when the upstream workflow file cannot be fetched or parsed.
    Callers translate this into HTTP 4xx/5xx; the message is human-readable
    and intended for the workflow detail view."""

    def __init__(self, message: str, *, status_code: int = 502) -> None:
        super().__init__(message)
        self.status_code = status_code


async def fetch_upstream(
    *,
    repo: str,
    workflow_name: str,
    project_name: str,
    minter: GitHubAppTokenMinter | None,
    ref: str = "main",
    timeout_seconds: float = 10.0,
) -> WorkflowRegister:
    """Fetch `.glimmung/workflows/<workflow_name>.yaml` from the given repo
    at `ref`, parse it, and return a `WorkflowRegister`. Fills in `project`
    and `name` from the call site so the upstream file doesn't have to
    repeat them (and can't lie about them).

    Raises `UpstreamFetchError` on any of: missing minter, repo not in
    `<owner>/<name>` form, file not found, file not valid YAML, file not a
    valid workflow payload."""
    if minter is None:
        raise UpstreamFetchError(
            "GitHub App token minter is not configured; cannot fetch workflow upstream",
            status_code=503,
        )
    if "/" not in repo:
        raise UpstreamFetchError(
            f"project github_repo {repo!r} is not in <owner>/<name> form",
            status_code=400,
        )

    path = WORKFLOW_FILE_TEMPLATE.format(name=workflow_name)
    token, _ = await minter.repository_token(
        repo=repo, permissions={"contents": "read", "metadata": "read"},
    )

    url = f"https://api.github.com/repos/{repo}/contents/{path}"
    headers = {
        "Authorization": f"Bearer {token}",
        # raw mediatype returns the file body directly. Default JSON
        # response embeds base64'd content in a JSON envelope which we'd
        # only have to decode anyway.
        "Accept": "application/vnd.github.v3.raw",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    async with httpx.AsyncClient(timeout=timeout_seconds) as client:
        try:
            r = await client.get(url, params={"ref": ref}, headers=headers)
        except httpx.HTTPError as exc:
            raise UpstreamFetchError(
                f"network error fetching {path} from {repo}@{ref}: {exc}",
            ) from exc

    if r.status_code == 404:
        raise UpstreamFetchError(
            f"{path} not found in {repo}@{ref}; "
            "either the project hasn't migrated to upstream-sync yet, or "
            "the workflow name doesn't match the file name",
            status_code=404,
        )
    if r.status_code >= 400:
        raise UpstreamFetchError(
            f"GitHub returned {r.status_code} fetching {path} from {repo}@{ref}: "
            f"{r.text[:200]}",
            status_code=502,
        )

    try:
        raw = yaml.safe_load(r.text)
    except yaml.YAMLError as exc:
        raise UpstreamFetchError(
            f"{path} in {repo}@{ref} is not valid YAML: {exc}",
            status_code=422,
        ) from exc
    if not isinstance(raw, dict):
        raise UpstreamFetchError(
            f"{path} in {repo}@{ref} must contain a mapping at the top level; "
            f"got {type(raw).__name__}",
            status_code=422,
        )

    # The upstream file is allowed (and encouraged) to omit `project` and
    # `name` since both are determined by where the file lives. We fill
    # them in here so a hand-edit can't drift the file off the path that
    # served it.
    payload = dict(raw)
    payload["project"] = project_name
    payload["name"] = workflow_name
    try:
        return WorkflowRegister.model_validate(payload)
    except Exception as exc:  # pydantic ValidationError + others
        raise UpstreamFetchError(
            f"{path} in {repo}@{ref} did not validate as a WorkflowRegister: {exc}",
            status_code=422,
        ) from exc


_NORMALIZE_STRIP_KEYS = frozenset({
    # server-set on every write
    "createdAt", "created_at",
    # primary key, redundant with `name`
    "id",
    # absent from WorkflowRegister, defaulted to {} on Workflow — comparing
    # them would always disagree even when the user-authored shape is identical
    "metadata",
})


def _normalize_for_compare(payload: Any) -> Any:
    """Strip fields that are timestamps / server-set / Workflow-only so
    equality doesn't pivot on values the upstream file doesn't carry."""
    if isinstance(payload, dict):
        out = {}
        for k, v in payload.items():
            if k in _NORMALIZE_STRIP_KEYS:
                continue
            out[k] = _normalize_for_compare(v)
        return out
    if isinstance(payload, list):
        return [_normalize_for_compare(v) for v in payload]
    return payload


def compute_in_sync(*, upstream: WorkflowRegister, current: Workflow | None) -> bool:
    """Are the two definitions structurally identical? `current=None` (no
    workflow registered yet) trivially returns False so the UI offers
    Install."""
    if current is None:
        return False
    upstream_doc = _normalize_for_compare(upstream.model_dump(mode="json"))
    current_doc = _normalize_for_compare(current.model_dump(mode="json"))
    # Workflow has fields WorkflowRegister doesn't (id, created_at). We
    # already strip id/created_at via _normalize_for_compare so the
    # serialized shapes line up.
    return upstream_doc == current_doc

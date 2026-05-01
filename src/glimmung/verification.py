"""Verification artifact fetcher for the verify-loop substrate (#18).

The producer workflow uploads a `verification` artifact (a zip
containing `verification.json` at the root) at the end of its verify
phase. Glimmung fetches this on each `workflow_run.completed` event for
a tracked run, parses the contract, and hands it to the decision
engine.

The artifact name (`verification`) and the file name within the zip
(`verification.json`) are part of the long-term contract — producers in
every consumer repo must conform. Both are documented in the README.

Errors are returned as None rather than raised so the webhook handler
can map them through the decision engine cleanly: "missing or malformed
artifact" is a *legitimate decision input* (-> ABORT_MALFORMED), not an
exception.
"""

import io
import json
import logging
import zipfile
from typing import Any

import httpx
from pydantic import ValidationError

from glimmung.github_app import GitHubAppTokenMinter
from glimmung.models import VerificationResult

log = logging.getLogger(__name__)

ARTIFACT_NAME = "verification"
ARTIFACT_FILENAME = "verification.json"


async def fetch_verification(
    minter: GitHubAppTokenMinter,
    repo: str,
    run_id: int,
) -> tuple[VerificationResult | None, str | None]:
    """Fetch and parse the verification artifact for a GH Actions run.

    Returns `(parsed_result, archive_download_url)`. Both fields may be
    None:

    - `parsed_result is None` means missing or malformed; the caller
      treats this as ABORT_MALFORMED via the decision engine.
    - `archive_download_url is None` means no `verification` artifact
      was uploaded at all. When the URL is non-None it is the GitHub
      Actions API endpoint (requires bearer auth, redirects to a
      short-lived presigned blob URL); the retry workflow consumes it
      as `prior_verification_artifact_url`.

    Network errors propagate (the webhook handler returns 5xx and
    GitHub will redeliver); contract violations (artifact missing,
    zip corrupt, JSON invalid, schema mismatch) become None.
    """
    token = await minter.installation_token()
    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    async with httpx.AsyncClient(timeout=30.0) as client:
        list_url = f"https://api.github.com/repos/{repo}/actions/runs/{run_id}/artifacts"
        r = await client.get(list_url, headers=headers)
        r.raise_for_status()
        artifacts = r.json().get("artifacts", [])
        match = next((a for a in artifacts if a.get("name") == ARTIFACT_NAME), None)
        if match is None:
            log.warning("no %r artifact on run %s/%d", ARTIFACT_NAME, repo, run_id)
            return None, None

        archive_url = match.get("archive_download_url")
        if not archive_url:
            log.warning(
                "%r artifact on run %s/%d had no archive_download_url",
                ARTIFACT_NAME, repo, run_id,
            )
            return None, None

        try:
            r = await client.get(archive_url, headers=headers, follow_redirects=True)
            r.raise_for_status()
        except httpx.HTTPError as e:
            log.warning("failed to download verification artifact: %s", e)
            return None, archive_url

        payload = _extract_json(r.content)
        if payload is None:
            return None, archive_url

        try:
            return VerificationResult.model_validate(payload), archive_url
        except ValidationError as e:
            log.warning("verification.json failed schema validation: %s", e)
            return None, archive_url


def _extract_json(zip_bytes: bytes) -> dict[str, Any] | None:
    try:
        with zipfile.ZipFile(io.BytesIO(zip_bytes)) as zf:
            with zf.open(ARTIFACT_FILENAME) as f:
                return json.loads(f.read())
    except (zipfile.BadZipFile, KeyError, json.JSONDecodeError) as e:
        log.warning("failed to extract %s from verification artifact: %s", ARTIFACT_FILENAME, e)
        return None

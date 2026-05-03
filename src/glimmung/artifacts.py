"""Private artifact storage for native runner logs and evidence."""

from __future__ import annotations

import json
import os
from typing import Any

from azure.identity.aio import DefaultAzureCredential
from azure.storage.blob import ContentSettings
from azure.storage.blob.aio import BlobServiceClient

from glimmung.settings import Settings


class ArtifactStore:
    """Uploads private artifacts and returns stable internal blob refs."""

    def __init__(self, settings: Settings) -> None:
        credential_kwargs: dict[str, bool] = {}
        if not os.environ.get("AZURE_CLIENT_ID"):
            credential_kwargs["exclude_workload_identity_credential"] = True
        self._credential = DefaultAzureCredential(**credential_kwargs)
        self._account = settings.artifacts_storage_account
        self._container = settings.artifacts_container
        self._service = BlobServiceClient(
            account_url=f"https://{self._account}.blob.core.windows.net",
            credential=self._credential,
        )

    async def upload_json(self, *, blob_name: str, payload: dict[str, Any]) -> str:
        blob = self._service.get_blob_client(
            container=self._container,
            blob=blob_name,
        )
        body = json.dumps(payload, separators=(",", ":"), sort_keys=True).encode("utf-8")
        await blob.upload_blob(
            body,
            overwrite=True,
            content_settings=ContentSettings(content_type="application/json"),
        )
        return f"blob://{self._container}/{blob_name}"

    async def close(self) -> None:
        await self._service.close()
        await self._credential.close()

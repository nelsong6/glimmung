"""Private artifact storage for native runner logs and evidence."""

from __future__ import annotations

import json
import mimetypes
import os
from pathlib import Path
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

    async def upload_file(
        self,
        *,
        blob_name: str,
        path: str | Path,
        content_type: str | None = None,
    ) -> str:
        blob = self._service.get_blob_client(
            container=self._container,
            blob=blob_name,
        )
        guessed_type = content_type or mimetypes.guess_type(str(path))[0]
        with Path(path).open("rb") as f:
            await blob.upload_blob(
                f,
                overwrite=True,
                content_settings=ContentSettings(
                    content_type=guessed_type or "application/octet-stream",
                ),
            )
        return f"blob://{self._container}/{blob_name}"

    async def download(self, *, blob_name: str) -> tuple[bytes, str]:
        blob = self._service.get_blob_client(
            container=self._container,
            blob=blob_name,
        )
        props = await blob.get_blob_properties()
        stream = await blob.download_blob()
        body = await stream.readall()
        content_type = (
            props.content_settings.content_type
            if props.content_settings is not None
            else None
        )
        return body, content_type or "application/octet-stream"

    async def close(self) -> None:
        await self._service.close()
        await self._credential.close()

from __future__ import annotations

from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from glimmung.app import read_artifact


class _ArtifactStore:
    def __init__(self, *, body: bytes = b"data", content_type: str = "text/plain"):
        self.body = body
        self.content_type = content_type
        self.downloads: list[str] = []

    async def download(self, *, blob_name: str):
        self.downloads.append(blob_name)
        return self.body, self.content_type


def _app_with(artifact_store):
    return SimpleNamespace(state=SimpleNamespace(artifact_store=artifact_store))


@pytest.mark.asyncio
async def test_read_artifact_serves_run_blob(monkeypatch):
    store = _ArtifactStore(body=b"png-bytes", content_type="image/png")
    monkeypatch.setattr("glimmung.app.app", _app_with(store))

    response = await read_artifact("runs/ambience/01RUN/screenshots/home.png")

    assert response.body == b"png-bytes"
    assert response.media_type == "image/png"
    assert response.headers["cache-control"] == "public, max-age=300"
    assert store.downloads == ["runs/ambience/01RUN/screenshots/home.png"]


@pytest.mark.asyncio
async def test_read_artifact_rejects_unscoped_blob_path(monkeypatch):
    store = _ArtifactStore()
    monkeypatch.setattr("glimmung.app.app", _app_with(store))

    with pytest.raises(HTTPException) as exc:
        await read_artifact("private/secret.png")

    assert exc.value.status_code == 404
    assert store.downloads == []


@pytest.mark.asyncio
async def test_read_artifact_rejects_dotdot_blob_path(monkeypatch):
    store = _ArtifactStore()
    monkeypatch.setattr("glimmung.app.app", _app_with(store))

    with pytest.raises(HTTPException) as exc:
        await read_artifact("runs/ambience/../secret.png")

    assert exc.value.status_code == 404
    assert store.downloads == []

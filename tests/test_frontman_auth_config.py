from __future__ import annotations

from types import SimpleNamespace

import pytest
from fastapi import HTTPException
from starlette.requests import Request

from glimmung import auth as auth_mod
from glimmung.app import _frontend_entra_client_id, _request_host, public_config


def _request(host: str, *, forwarded_host: str | None = None) -> Request:
    headers = [(b"host", host.encode("utf-8"))]
    if forwarded_host:
        headers.append((b"x-forwarded-host", forwarded_host.encode("utf-8")))
    return Request({"type": "http", "headers": headers})


def _settings(**kwargs):
    defaults = {
        "entra_client_id": "prod-client",
        "entra_test_client_id": "test-client",
        "tank_operator_base_url": "https://tank.romaine.life/",
    }
    defaults.update(kwargs)
    return SimpleNamespace(**defaults)


@pytest.mark.asyncio
async def test_public_config_uses_test_client_for_frontman_hosts(monkeypatch):
    settings = _settings()
    monkeypatch.setattr("glimmung.app.app", SimpleNamespace(state=SimpleNamespace(
        settings=settings,
    )))

    result = await public_config(_request(
        "frontman-1.glimmung.dev.romaine.life",
    ))

    assert result["entra_client_id"] == "test-client"
    assert result["tank_operator_base_url"] == "https://tank.romaine.life"


@pytest.mark.asyncio
async def test_public_config_uses_prod_client_for_prod_hosts(monkeypatch):
    settings = _settings()
    monkeypatch.setattr("glimmung.app.app", SimpleNamespace(state=SimpleNamespace(
        settings=settings,
    )))

    result = await public_config(_request("glimmung.romaine.life"))

    assert result["entra_client_id"] == "prod-client"


@pytest.mark.asyncio
async def test_forwarded_host_is_used_for_client_selection(monkeypatch):
    settings = _settings()
    monkeypatch.setattr("glimmung.app.app", SimpleNamespace(state=SimpleNamespace(
        settings=settings,
    )))

    request = _request(
        "internal-service:8000",
        forwarded_host="frontman.glimmung.dev.romaine.life",
    )

    assert _request_host(request) == "frontman.glimmung.dev.romaine.life"
    assert (await public_config(request))["entra_client_id"] == "test-client"


def test_frontend_client_selection_falls_back_without_test_client():
    settings = _settings(entra_test_client_id="")

    assert _frontend_entra_client_id(
        settings,
        "frontman.glimmung.dev.romaine.life",
    ) == "prod-client"


def test_entra_audiences_accepts_prod_and_test(monkeypatch):
    monkeypatch.setattr(auth_mod, "get_settings", lambda: _settings())

    assert auth_mod._entra_audiences() == ["prod-client", "test-client"]


def test_entra_audiences_requires_at_least_one_client(monkeypatch):
    monkeypatch.setattr(auth_mod, "get_settings", lambda: _settings(
        entra_client_id="",
        entra_test_client_id="",
    ))

    with pytest.raises(HTTPException) as exc:
        auth_mod._entra_audiences()
    assert exc.value.status_code == 503

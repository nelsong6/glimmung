"""Workflow upstream-sync (#298 — `Check for updates` / `Install`).

Three layers tested:
- `compute_in_sync`: pure-function shape comparison after stripping
  server-set fields (createdAt, id).
- `fetch_upstream`: GitHub fetch + YAML parse + WorkflowRegister
  validation, with httpx.MockTransport for hermetic tests.
- The two endpoints (`GET .../upstream`, `POST .../sync`): end-to-end
  through the FastAPI app with a stubbed `fetch_upstream` and the
  in-memory cosmos fake.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace
from typing import Any

import httpx
import pytest

from glimmung import app as glimmung_app
from glimmung import leases as lease_ops
from glimmung.models import (
    BudgetConfig,
    NativeJobSpec,
    NativeStepSpec,
    PhaseSpec,
    PrPrimitiveSpec,
    Workflow,
    WorkflowRegister,
)
from glimmung.workflow_sync import (
    UpstreamFetchError,
    compute_in_sync,
    fetch_upstream,
)

from tests.cosmos_fake import FakeContainer


# ─── helpers ─────────────────────────────────────────────────────────────


def _phase(name: str, *, always: bool = False) -> PhaseSpec:
    return PhaseSpec(
        name=name,
        kind="k8s_job",
        always=always,
        jobs=[
            NativeJobSpec(
                id=name,
                image="ghcr.io/example/runner:latest",
                command=["/bin/true"],
                steps=[NativeStepSpec(slug="run", title="run")],
            )
        ],
    )


def _workflow_register(*, project="p1", name="agent-run") -> WorkflowRegister:
    return WorkflowRegister(
        project=project,
        name=name,
        phases=[_phase("env-prep"), _phase("env-destroy", always=True)],
    )


def _workflow_doc_for(register: WorkflowRegister) -> dict[str, Any]:
    """Mirror of `_workflow_to_doc`, rebuilt here so tests don't depend on
    the private helper signature."""
    return glimmung_app._workflow_to_doc(register)


# ─── compute_in_sync ────────────────────────────────────────────────────


def test_in_sync_false_when_current_missing():
    upstream = _workflow_register()
    assert compute_in_sync(upstream=upstream, current=None) is False


def test_in_sync_true_when_shape_matches():
    upstream = _workflow_register()
    doc = _workflow_doc_for(upstream)
    current = glimmung_app._doc_to_workflow(doc)
    assert compute_in_sync(upstream=upstream, current=current) is True


def test_in_sync_false_when_phase_count_differs():
    upstream = _workflow_register()
    # always=True phases must come at the end, so the extra phase has to
    # be inserted before env-destroy, not appended.
    bigger = WorkflowRegister(
        project="p1",
        name="agent-run",
        phases=[
            upstream.phases[0],          # env-prep
            _phase("agent-execute"),     # extra non-always
            upstream.phases[-1],         # env-destroy (always)
        ],
    )
    bigger_doc = _workflow_doc_for(bigger)
    current = glimmung_app._doc_to_workflow(bigger_doc)
    assert compute_in_sync(upstream=upstream, current=current) is False


def test_in_sync_true_ignores_created_at_drift():
    """createdAt is server-set on every write. A workflow whose only
    'difference' is when it was registered must still report in-sync, or
    every check-for-updates after a sync would say 'install' again."""
    upstream = _workflow_register()
    doc = _workflow_doc_for(upstream)
    doc["createdAt"] = datetime(2026, 1, 1, tzinfo=UTC).isoformat()
    current = glimmung_app._doc_to_workflow(doc)
    # mutate the in-memory current's created_at to something else;
    # serialization should still match upstream after normalization.
    current = current.model_copy(update={"created_at": datetime.now(UTC)})
    assert compute_in_sync(upstream=upstream, current=current) is True


# ─── fetch_upstream ─────────────────────────────────────────────────────


class _StubMinter:
    async def repository_token(
        self, *, repo: str, permissions: dict[str, str] | None = None,
    ) -> tuple[str, str | None]:
        return "ghs_stub_token", None


def _yaml_for(register: WorkflowRegister) -> str:
    """Render a WorkflowRegister as YAML the way an upstream file would
    look on disk (project/name omitted — fetch_upstream fills them in
    from the call site)."""
    import yaml as _yaml

    payload = register.model_dump(mode="json")
    payload.pop("project", None)
    payload.pop("name", None)
    return _yaml.dump(payload, sort_keys=False)


@pytest.fixture(autouse=True)
def _mock_httpx(monkeypatch):
    """Each test sets `_mock_httpx_handler` to a transport handler;
    this fixture redirects httpx.AsyncClient to use that transport."""
    captured: dict[str, Any] = {"handler": None}

    real_init = httpx.AsyncClient.__init__

    def patched_init(self, *args, **kwargs):
        if captured["handler"] is not None:
            kwargs["transport"] = httpx.MockTransport(captured["handler"])
        real_init(self, *args, **kwargs)

    monkeypatch.setattr(httpx.AsyncClient, "__init__", patched_init)
    return captured


async def test_fetch_upstream_happy_path(_mock_httpx):
    register = _workflow_register()
    yaml_body = _yaml_for(register)

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.host == "api.github.com"
        assert "/repos/owner/repo/contents/.glimmung/workflows/agent-run.yaml" in str(request.url)
        assert request.url.params.get("ref") == "main"
        assert request.headers["Authorization"] == "Bearer ghs_stub_token"
        assert request.headers["Accept"] == "application/vnd.github.v3.raw"
        return httpx.Response(200, text=yaml_body)

    _mock_httpx["handler"] = handler

    out = await fetch_upstream(
        repo="owner/repo",
        workflow_name="agent-run",
        project_name="p1",
        minter=_StubMinter(),
    )
    assert isinstance(out, WorkflowRegister)
    assert out.project == "p1"
    assert out.name == "agent-run"
    assert [p.name for p in out.phases] == ["env-prep", "env-destroy"]
    assert out.phases[1].always is True


async def test_fetch_upstream_404(_mock_httpx):
    _mock_httpx["handler"] = lambda req: httpx.Response(404, text="Not Found")
    with pytest.raises(UpstreamFetchError) as excinfo:
        await fetch_upstream(
            repo="owner/repo",
            workflow_name="agent-run",
            project_name="p1",
            minter=_StubMinter(),
        )
    assert excinfo.value.status_code == 404


async def test_fetch_upstream_invalid_yaml(_mock_httpx):
    _mock_httpx["handler"] = lambda req: httpx.Response(200, text=":\n  - bad: [")
    with pytest.raises(UpstreamFetchError) as excinfo:
        await fetch_upstream(
            repo="owner/repo",
            workflow_name="agent-run",
            project_name="p1",
            minter=_StubMinter(),
        )
    assert excinfo.value.status_code == 422


async def test_fetch_upstream_invalid_workflow_payload(_mock_httpx):
    # Valid YAML but missing required fields (no phases).
    _mock_httpx["handler"] = lambda req: httpx.Response(200, text="phases: []")
    with pytest.raises(UpstreamFetchError) as excinfo:
        await fetch_upstream(
            repo="owner/repo",
            workflow_name="agent-run",
            project_name="p1",
            minter=_StubMinter(),
        )
    assert excinfo.value.status_code == 422


async def test_fetch_upstream_no_minter():
    with pytest.raises(UpstreamFetchError) as excinfo:
        await fetch_upstream(
            repo="owner/repo",
            workflow_name="agent-run",
            project_name="p1",
            minter=None,
        )
    assert excinfo.value.status_code == 503


async def test_fetch_upstream_overrides_project_and_name_from_call_site(_mock_httpx):
    """If the YAML lies about its project/name, the call site's values
    win. This makes the file location authoritative — moving a file from
    `.glimmung/workflows/agent-run.yaml` to `.glimmung/workflows/foo.yaml`
    can't change the registered workflow name out from under glimmung."""
    register = _workflow_register(project="liar", name="liar-name")
    _mock_httpx["handler"] = lambda req: httpx.Response(200, text=_yaml_for(register))
    out = await fetch_upstream(
        repo="owner/repo",
        workflow_name="agent-run",
        project_name="p1",
        minter=_StubMinter(),
    )
    assert out.project == "p1"
    assert out.name == "agent-run"


# ─── endpoint integration ───────────────────────────────────────────────


@pytest.fixture
async def app_with_project(monkeypatch):
    """Spin up the FastAPI app against an in-memory cosmos fake with a
    pre-registered project. Returns (TestClient, captured_state)."""
    from fastapi.testclient import TestClient

    from glimmung.app import app
    from glimmung.auth import require_admin_user, resolve_caller_identity

    cosmos = SimpleNamespace(
        projects=FakeContainer("projects", "/name"),
        workflows=FakeContainer("workflows", "/project"),
        hosts=FakeContainer("hosts", "/name"),
        leases=FakeContainer("leases", "/project"),
        runs=FakeContainer("runs", "/project"),
        locks=FakeContainer("locks", "/scope"),
        issues=FakeContainer("issues", "/project"),
    )
    settings = SimpleNamespace(lease_default_ttl_seconds=14400)
    state = SimpleNamespace(cosmos=cosmos, settings=settings, gh_minter=_StubMinter())
    monkeypatch.setattr(app, "state", state)

    async def _admin_override():
        return SimpleNamespace(name="admin", email="admin@example.com")

    app.dependency_overrides[require_admin_user] = _admin_override
    app.dependency_overrides[resolve_caller_identity] = _admin_override

    await cosmos.projects.create_item({
        "id": "p1",
        "name": "p1",
        "githubRepo": "owner/repo",
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })

    client = TestClient(app)
    yield client, state
    app.dependency_overrides.clear()


async def _stub_fetch_returning(register: WorkflowRegister, monkeypatch):
    async def fake(*, repo, workflow_name, project_name, minter, ref="main", timeout_seconds=10.0):
        return register.model_copy(update={"project": project_name, "name": workflow_name})

    monkeypatch.setattr("glimmung.workflow_sync.fetch_upstream", fake)
    monkeypatch.setattr("glimmung.app.fetch_upstream", fake, raising=False)


async def test_get_upstream_returns_in_sync_false_when_no_workflow_registered(
    app_with_project, monkeypatch,
):
    client, _ = app_with_project
    register = _workflow_register()
    await _stub_fetch_returning(register, monkeypatch)

    r = client.get("/v1/projects/p1/workflows/agent-run/upstream")
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["in_sync"] is False
    assert body["current"] is None
    assert body["upstream"] is not None
    assert body["repo"] == "owner/repo"


async def test_post_sync_creates_workflow_when_missing(
    app_with_project, monkeypatch,
):
    client, state = app_with_project
    register = _workflow_register()
    await _stub_fetch_returning(register, monkeypatch)

    r = client.post("/v1/projects/p1/workflows/agent-run/sync")
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["in_sync"] is True
    assert body["current"]["name"] == "agent-run"

    # And verify it landed in cosmos
    doc = await state.cosmos.workflows.read_item(item="agent-run", partition_key="p1")
    assert doc["name"] == "agent-run"


async def test_post_sync_is_noop_when_in_sync(
    app_with_project, monkeypatch,
):
    client, state = app_with_project
    register = _workflow_register()
    await _stub_fetch_returning(register, monkeypatch)

    # First sync writes the workflow.
    r1 = client.post("/v1/projects/p1/workflows/agent-run/sync")
    assert r1.status_code == 200
    first_doc = await state.cosmos.workflows.read_item(item="agent-run", partition_key="p1")
    first_etag = first_doc["_etag"]

    # Second sync against the same upstream should not write again.
    r2 = client.post("/v1/projects/p1/workflows/agent-run/sync")
    assert r2.status_code == 200
    assert r2.json()["in_sync"] is True
    second_doc = await state.cosmos.workflows.read_item(item="agent-run", partition_key="p1")
    assert second_doc["_etag"] == first_etag


async def test_post_sync_updates_when_upstream_changed(
    app_with_project, monkeypatch,
):
    client, state = app_with_project
    register = _workflow_register()
    await _stub_fetch_returning(register, monkeypatch)

    client.post("/v1/projects/p1/workflows/agent-run/sync")
    first_doc = await state.cosmos.workflows.read_item(item="agent-run", partition_key="p1")
    first_etag = first_doc["_etag"]

    # Upstream gains a phase.
    bigger = WorkflowRegister(
        project="p1",
        name="agent-run",
        phases=[
            *register.phases[:-1],   # keep env-prep
            _phase("agent-execute"),
            register.phases[-1],     # keep env-destroy at the end
        ],
    )
    await _stub_fetch_returning(bigger, monkeypatch)

    r = client.post("/v1/projects/p1/workflows/agent-run/sync")
    assert r.status_code == 200
    assert r.json()["in_sync"] is True
    second_doc = await state.cosmos.workflows.read_item(item="agent-run", partition_key="p1")
    assert second_doc["_etag"] != first_etag
    assert len(second_doc["phases"]) == 3


async def test_get_upstream_404_for_unknown_project(app_with_project):
    client, _ = app_with_project
    r = client.get("/v1/projects/does-not-exist/workflows/agent-run/upstream")
    assert r.status_code == 404

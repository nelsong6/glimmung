from __future__ import annotations

from types import SimpleNamespace

import pytest

import glimmung.app as app_module
from glimmung.models import LeaseState
from glimmung.models import TestSlotRequestState as SlotRequestState

from tests.cosmos_fake import FakeContainer
from tests.test_dispatch import _register_project


def _lease_doc_by_slot(app_state, project: str, slot_name: str) -> dict:
    for doc in app_state.cosmos.leases._items.values():
        metadata = doc.get("metadata") or {}
        if doc.get("project") == project and metadata.get("native_slot_name") == slot_name:
            return doc
    raise AssertionError(f"lease for {project}/{slot_name} not found")


def _settings() -> SimpleNamespace:
    return SimpleNamespace(
        lease_default_ttl_seconds=14400,
        sweep_interval_seconds=60,
        native_runner_project_concurrency=2,
        native_runner_global_concurrency=5,
    )


def _cosmos() -> SimpleNamespace:
    return SimpleNamespace(
        projects=FakeContainer("projects", "/name"),
        workflows=FakeContainer("workflows", "/project"),
        hosts=FakeContainer("hosts", "/name"),
        leases=FakeContainer("leases", "/project"),
        runs=FakeContainer("runs", "/project"),
        locks=FakeContainer("locks", "/scope"),
        issues=FakeContainer("issues", "/project"),
    )


def _glimmung_requester() -> app_module.LeaseRequester:
    return app_module.LeaseRequester(
        consumer="glimmung",
        kind="test",
        ref="glimmung/tests/test-slot-checkout",
    )


class _RecordingTestSlotLauncher:
    def __init__(self) -> None:
        self.ensured_namespaces: list[dict] = []
        self.ensured_playwright: list[dict] = []
        self.ensured_helm_releases: list[dict] = []
        self.deleted_namespaces: list[str] = []
        self.deleted_playwright: list[dict] = []
        self.deleted_helm_releases: list[dict] = []
        self.reconciled_playwright_leases: list[dict] | None = None
        self.reconciled_entra_projects: list[dict] | None = None

    async def ensure_test_slot_namespace(self, lease_doc: dict) -> None:
        self.ensured_namespaces.append(lease_doc)

    async def ensure_playwright_slot(self, lease_doc: dict) -> None:
        self.ensured_playwright.append(lease_doc)

    async def ensure_test_slot_helm_release(
        self,
        *,
        lease_doc: dict,
        project_doc: dict,
        repo_token: str,
    ) -> None:
        self.ensured_helm_releases.append({
            "lease_doc": lease_doc,
            "project_doc": project_doc,
            "repo_token": repo_token,
        })

    async def delete_test_slot_namespace(self, namespace: str) -> None:
        self.deleted_namespaces.append(namespace)

    async def delete_playwright_slot(self, lease_doc: dict) -> None:
        self.deleted_playwright.append(lease_doc)

    async def delete_test_slot_helm_release(self, lease_doc: dict) -> None:
        self.deleted_helm_releases.append(lease_doc)

    async def reconcile_playwright_slots(self, active_native_leases: list[dict]) -> None:
        self.reconciled_playwright_leases = active_native_leases

    async def reconcile_standby_entra_redirects(self, project_docs: list[dict]) -> None:
        self.reconciled_entra_projects = project_docs


@pytest.fixture
def app_state():
    native_k8s_launcher = _RecordingTestSlotLauncher()
    state = SimpleNamespace(
        cosmos=_cosmos(),
        settings=_settings(),
        gh_minter=None,
        native_k8s_launcher=native_k8s_launcher,
    )
    app_module.app.state.cosmos = state.cosmos
    app_module.app.state.settings = state.settings
    app_module.app.state.gh_minter = state.gh_minter
    app_module.app.state.native_k8s_launcher = state.native_k8s_launcher
    return state


@pytest.mark.asyncio
async def test_checkout_test_slot_prepares_clean_slate_namespace_and_playwright(app_state):
    launcher = _RecordingTestSlotLauncher()
    app_module.app.state.native_k8s_launcher = launcher

    await _register_project(
        SimpleNamespace(state=app_state),
        "glimmung",
        "nelsong6/glimmung",
        metadata={"native_webapp": True},
    )

    result = await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="glimmung",
            workflow="manual-slot",
            slot_index=2,
            mode="clean_slate",
            requester=_glimmung_requester(),
        )
    )

    assert result.state == LeaseState.CLAIMED.value
    assert result.workflow == "manual-slot"
    assert result.slot_index == 2
    assert result.slot_name == "glimmung-2"
    assert result.url is None
    assert result.lease == "glimmung-2"
    assert result.host == "native-k8s"
    lease_doc = _lease_doc_by_slot(app_state, "glimmung", "glimmung-2")
    assert lease_doc["state"] == LeaseState.CLAIMED.value
    assert lease_doc["metadata"]["native_slot_index"] == "2"
    assert lease_doc["metadata"]["native_slot_name"] == "glimmung-2"
    assert lease_doc["metadata"]["test_slot_checkout"] is True
    assert lease_doc["metadata"]["requester"] == {
        "consumer": "glimmung",
        "kind": "test",
        "ref": "glimmung/tests/test-slot-checkout",
        "metadata": {},
    }
    assert lease_doc["metadata"]["requester_ref"] == "glimmung/tests/test-slot-checkout"
    assert lease_doc["metadata"]["requester_consumer"] == "glimmung"
    assert lease_doc["metadata"]["requester_kind"] == "test"
    assert lease_doc["metadata"]["phase_inputs"] == {
        "validation_slot_index": "2",
        "slot_name": "glimmung-2",
        "namespace": "glimmung-2",
        "test_slot_mode": "clean_slate",
        "clean_slate": "true",
    }
    issue_docs = list(app_state.cosmos.issues._items.values())
    run_docs = list(app_state.cosmos.runs._items.values())
    assert issue_docs == []
    assert run_docs == []
    assert launcher.deleted_namespaces == ["glimmung-2"]
    assert len(launcher.ensured_namespaces) == 1
    assert launcher.ensured_namespaces[0]["metadata"]["native_slot_name"] == "glimmung-2"
    assert len(launcher.ensured_playwright) == 1
    assert launcher.ensured_playwright[0]["metadata"]["native_slot_name"] == "glimmung-2"


@pytest.mark.asyncio
async def test_checkout_test_slot_uses_project_standby_dns_slot_prefix(app_state):
    await _register_project(
        SimpleNamespace(state=app_state),
        "tank-operator",
        "nelsong6/tank-operator",
        metadata={
            "native_standby_dns": {
                "enabled": True,
                "record_base": "tank.dev.romaine.life",
                "slot_prefix": "tank-slot",
                "count": 2,
            },
        },
    )

    result = await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="tank-operator",
            slot_index=1,
            tank_session_id="abc123",
        )
    )

    assert result.slot_name == "tank-slot-1"
    assert result.url == "https://tank-slot-1.tank.dev.romaine.life"
    lease_doc = _lease_doc_by_slot(app_state, "tank-operator", "tank-slot-1")
    assert lease_doc["metadata"]["native_slot_name"] == "tank-slot-1"
    assert lease_doc["metadata"]["requester"] == {
        "consumer": "tank-operator",
        "kind": "tank_session",
        "ref": "tank-operator/session/abc123",
        "label": "tank-operator/session/abc123",
        "metadata": {"tank_session_id": "abc123"},
    }
    assert lease_doc["metadata"]["requester_ref"] == "tank-operator/session/abc123"
    assert "native_slot_prefix" not in lease_doc["metadata"]
    assert lease_doc["metadata"]["phase_inputs"]["slot_name"] == "tank-slot-1"
    assert lease_doc["metadata"]["phase_inputs"]["namespace"] == "tank-slot-1"
    assert app_state.native_k8s_launcher.reconciled_entra_projects is not None


@pytest.mark.asyncio
async def test_checkout_test_slot_uses_project_slot_prefix_without_explicit_slot(app_state):
    await _register_project(
        SimpleNamespace(state=app_state),
        "glimmung",
        "nelsong6/glimmung",
        metadata={
            "native_standby_dns": {
                "enabled": True,
                "record_base": "glimmung.dev.romaine.life",
                "slot_prefix": "glimmung-slot",
                "count": 1,
            },
        },
    )

    result = await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="glimmung",
            tank_session_id="abc123",
        )
    )

    assert result.slot_index == 1
    assert result.slot_name == "glimmung-slot-1"
    assert result.url == "https://glimmung-slot-1.glimmung.dev.romaine.life"
    lease_doc = _lease_doc_by_slot(app_state, "glimmung", "glimmung-slot-1")
    assert lease_doc["metadata"]["native_slot_index"] == "1"
    assert lease_doc["metadata"]["native_slot_name"] == "glimmung-slot-1"
    assert lease_doc["metadata"]["native_slot_prefix"] == "glimmung-slot"


@pytest.mark.asyncio
async def test_scale_project_test_environments_updates_standby_count(app_state):
    await _register_project(
        SimpleNamespace(state=app_state),
        "tank-operator",
        "nelsong6/tank-operator",
        metadata={"native_standby_dns": {"enabled": True, "count": 2}},
    )

    project = await app_module.scale_project_test_environments(
        "tank-operator",
        app_module.TestEnvironmentScaleRequest(count=7),
    )

    assert project.metadata["native_standby_dns"]["enabled"] is True
    assert project.metadata["native_standby_dns"]["count"] == 7
    doc = await app_state.cosmos.projects.read_item(
        item="tank-operator",
        partition_key="tank-operator",
    )
    assert doc["metadata"]["native_standby_dns"]["count"] == 7


@pytest.mark.asyncio
async def test_checkout_test_slot_pending_when_preferred_slot_is_busy(app_state):
    await _register_project(
        SimpleNamespace(state=app_state),
        "glimmung",
        "nelsong6/glimmung",
    )
    first = await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="glimmung",
            slot_index=1,
            requester=_glimmung_requester(),
        )
    )

    second = await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="glimmung",
            slot_index=1,
            requester=_glimmung_requester(),
        )
    )

    assert first.state == LeaseState.CLAIMED.value
    assert second.state == SlotRequestState.WAITING.value
    assert second.host is None
    assert second.detail == "slot unavailable; checkout request is waiting"
    request_doc = next(
        doc for doc in app_state.cosmos.leases._items.values()
        if doc.get("kind") == "test_slot_request"
    )
    assert request_doc["state"] == SlotRequestState.WAITING.value
    assert "native_slot_index" not in request_doc["metadata"]
    assert request_doc["metadata"]["phase_inputs"]["validation_slot_index"] == "1"


@pytest.mark.asyncio
async def test_checkout_test_slot_requires_requester(app_state):
    await _register_project(
        SimpleNamespace(state=app_state),
        "glimmung",
        "nelsong6/glimmung",
    )

    with pytest.raises(app_module.HTTPException) as exc_info:
        await app_module.checkout_test_slot(
            app_module.TestSlotCheckoutRequest(project="glimmung", slot_index=1)
        )

    assert exc_info.value.status_code == 400
    assert "requester required" in str(exc_info.value.detail)


@pytest.mark.asyncio
async def test_create_lease_records_explicit_requester(app_state):
    result = await app_module.create_lease(
        app_module.LeaseRequest(
            project="glimmung",
            workflow="manual",
            requester=app_module.LeaseRequester(
                consumer="glimmung",
                kind="run",
                ref="glimmung#12/runs/3",
                url="https://glimmung.romaine.life/issues/12/runs/3",
            ),
            metadata={"purpose": "manual check"},
        )
    )

    assert result.lease.state == LeaseState.PENDING
    lease_doc = next(
        doc for doc in app_state.cosmos.leases._items.values()
        if doc.get("project") == "glimmung" and doc.get("kind") != "lease_number_counter"
    )
    assert lease_doc["metadata"]["purpose"] == "manual check"
    assert lease_doc["metadata"]["requester_ref"] == "glimmung#12/runs/3"
    assert lease_doc["metadata"]["requester_consumer"] == "glimmung"
    assert lease_doc["metadata"]["requester_kind"] == "run"
    assert lease_doc["metadata"]["requester"]["url"] == (
        "https://glimmung.romaine.life/issues/12/runs/3"
    )


@pytest.mark.asyncio
async def test_release_test_slot_cleans_up_namespace_before_releasing(app_state):
    await _register_project(
        SimpleNamespace(state=app_state),
        "glimmung",
        "nelsong6/glimmung",
    )
    await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="glimmung",
            slot_index=2,
            requester=_glimmung_requester(),
        )
    )

    lease_doc = _lease_doc_by_slot(app_state, "glimmung", "glimmung-2")
    released = await app_module.release_lease(lease_doc["id"], project="glimmung")

    assert released.state == LeaseState.RELEASED
    assert app_state.native_k8s_launcher.deleted_namespaces == ["glimmung-2"]
    lease_doc = await app_state.cosmos.leases.read_item(
        item=lease_doc["id"],
        partition_key="glimmung",
    )
    assert lease_doc["state"] == LeaseState.RELEASED.value


@pytest.mark.asyncio
async def test_lease_callback_token_reads_heartbeats_and_releases_without_storage_id(app_state):
    await _register_project(
        SimpleNamespace(state=app_state),
        "glimmung",
        "nelsong6/glimmung",
    )
    await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="glimmung",
            slot_index=2,
            requester=_glimmung_requester(),
        )
    )
    lease_doc = _lease_doc_by_slot(app_state, "glimmung", "glimmung-2")
    await app_state.cosmos.hosts.create_item({
        "id": "native-k8s",
        "name": "native-k8s",
        "capabilities": {},
        "currentLeaseId": lease_doc["id"],
        "drained": False,
        "createdAt": "2026-01-01T00:00:00+00:00",
    })
    token = lease_doc["metadata"]["lease_callback_token"]

    read = await app_module.read_lease_by_callback_token(token)
    assert read.ref == "glimmung-2"
    assert read.state == LeaseState.CLAIMED

    heartbeat = await app_module.heartbeat_lease_by_callback_token(token)
    assert heartbeat.ref == "glimmung-2"
    assert heartbeat.state == LeaseState.CLAIMED

    released = await app_module.release_lease_by_callback_token(token)
    assert released.ref == "glimmung-2"
    assert released.state == LeaseState.RELEASED
    lease_doc = await app_state.cosmos.leases.read_item(
        item=lease_doc["id"],
        partition_key="glimmung",
    )
    assert lease_doc["state"] == LeaseState.RELEASED.value


@pytest.mark.asyncio
async def test_return_test_slot_resolves_by_slot_index(app_state):
    await _register_project(
        SimpleNamespace(state=app_state),
        "glimmung",
        "nelsong6/glimmung",
    )
    checked_out = await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="glimmung",
            slot_index=1,
            requester=_glimmung_requester(),
        )
    )

    returned = await app_module.return_test_slot(
        app_module.TestSlotReturnRequest(project="glimmung", slot_index=1)
    )

    assert returned.state == LeaseState.RELEASED.value
    assert checked_out.lease == "glimmung-1"
    assert returned.lease == "glimmung-1"
    assert returned.slot_index == 1
    assert returned.slot_name == "glimmung-1"
    assert returned.cleanup_started is True
    assert app_state.native_k8s_launcher.deleted_namespaces == ["glimmung-1"]


@pytest.mark.asyncio
async def test_playwright_reconcile_includes_checked_out_test_slots(app_state):
    await _register_project(
        SimpleNamespace(state=app_state),
        "glimmung",
        "nelsong6/glimmung",
    )
    await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="glimmung",
            slot_index=1,
            requester=_glimmung_requester(),
        )
    )

    await app_module._reconcile_playwright_slots(app_module.app, app_state.cosmos)

    assert len(app_state.native_k8s_launcher.reconciled_playwright_leases or []) == 1
    lease_doc = app_state.native_k8s_launcher.reconciled_playwright_leases[0]
    assert lease_doc["metadata"]["native_slot_name"] == "glimmung-1"

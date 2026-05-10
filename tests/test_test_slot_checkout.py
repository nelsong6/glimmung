from __future__ import annotations

from types import SimpleNamespace

import pytest

import glimmung.app as app_module
from glimmung.models import LeaseState

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
        )
    )

    assert result.state == LeaseState.ACTIVE.value
    assert result.workflow == "manual-slot"
    assert result.slot_index == 2
    assert result.slot_name == "glimmung-2"
    assert result.lease == "glimmung-2"
    assert result.host == "native-k8s"
    lease_doc = _lease_doc_by_slot(app_state, "glimmung", "glimmung-2")
    assert lease_doc["state"] == LeaseState.ACTIVE.value
    assert lease_doc["metadata"]["native_slot_index"] == "2"
    assert lease_doc["metadata"]["native_slot_name"] == "glimmung-2"
    assert lease_doc["metadata"]["test_slot_checkout"] is True
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
async def test_checkout_test_slot_ignores_project_standby_dns_slot_prefix(app_state):
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
        )
    )

    assert result.slot_name == "tank-operator-1"
    lease_doc = _lease_doc_by_slot(app_state, "tank-operator", "tank-operator-1")
    assert lease_doc["metadata"]["native_slot_name"] == "tank-operator-1"
    assert "native_slot_prefix" not in lease_doc["metadata"]
    assert lease_doc["metadata"]["phase_inputs"]["slot_name"] == "tank-operator-1"
    assert lease_doc["metadata"]["phase_inputs"]["namespace"] == "tank-operator-1"
    assert app_state.native_k8s_launcher.reconciled_entra_projects is not None


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
        )
    )

    second = await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="glimmung",
            slot_index=1,
        )
    )

    assert first.state == LeaseState.ACTIVE.value
    assert second.state == LeaseState.PENDING.value
    assert second.host is None
    assert second.detail == "slot unavailable; reservation is pending"
    lease_doc = next(
        doc for doc in app_state.cosmos.leases._items.values()
        if doc.get("state") == LeaseState.PENDING.value
    )
    assert lease_doc["state"] == LeaseState.PENDING.value
    assert "native_slot_index" not in lease_doc["metadata"]
    assert lease_doc["metadata"]["phase_inputs"]["validation_slot_index"] == "1"


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
    assert read.state == LeaseState.ACTIVE

    heartbeat = await app_module.heartbeat_lease_by_callback_token(token)
    assert heartbeat.ref == "glimmung-2"
    assert heartbeat.state == LeaseState.ACTIVE

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
        )
    )

    await app_module._reconcile_playwright_slots(app_module.app, app_state.cosmos)

    assert len(app_state.native_k8s_launcher.reconciled_playwright_leases or []) == 1
    lease_doc = app_state.native_k8s_launcher.reconciled_playwright_leases[0]
    assert lease_doc["metadata"]["native_slot_name"] == "glimmung-1"

from __future__ import annotations

from types import SimpleNamespace

import pytest

import glimmung.app as app_module
from glimmung.models import LeaseState

from tests.cosmos_fake import FakeContainer
from tests.test_dispatch import _register_project


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


class _RecordingPlaywrightLauncher:
    def __init__(self) -> None:
        self.ensured: list[dict] = []
        self.deleted_namespaces: list[str] = []
        self.deleted_playwright: list[dict] = []

    async def ensure_playwright_slot(self, lease_doc: dict) -> None:
        self.ensured.append(lease_doc)

    async def delete_test_slot_namespace(self, namespace: str) -> None:
        self.deleted_namespaces.append(namespace)

    async def delete_playwright_slot(self, lease_doc: dict) -> None:
        self.deleted_playwright.append(lease_doc)


@pytest.fixture
def app_state():
    native_k8s_launcher = _RecordingPlaywrightLauncher()
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
async def test_checkout_test_slot_reserves_native_slot_with_clean_slate_inputs(app_state):
    launcher = _RecordingPlaywrightLauncher()
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
    assert result.slot_name == "glimmung-slot-2"
    assert result.lease_id is not None
    assert result.host == "native-k8s"
    lease_doc = await app_state.cosmos.leases.read_item(
        item=result.lease_id,
        partition_key="glimmung",
    )
    assert lease_doc["state"] == LeaseState.ACTIVE.value
    assert lease_doc["metadata"]["native_slot_index"] == "2"
    assert lease_doc["metadata"]["native_slot_name"] == "glimmung-slot-2"
    assert lease_doc["metadata"]["test_slot_checkout"] is True
    assert lease_doc["metadata"]["phase_inputs"] == {
        "validation_slot_index": "2",
        "slot_name": "glimmung-slot-2",
        "namespace": "glimmung-slot-2",
        "test_slot_mode": "clean_slate",
        "clean_slate": "true",
    }
    issue_docs = list(app_state.cosmos.issues._items.values())
    run_docs = list(app_state.cosmos.runs._items.values())
    assert issue_docs == []
    assert run_docs == []
    assert len(launcher.ensured) == 1
    assert launcher.ensured[0]["metadata"]["native_slot_name"] == "glimmung-slot-2"


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
    lease_doc = await app_state.cosmos.leases.read_item(
        item=second.lease_id,
        partition_key="glimmung",
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
    result = await app_module.checkout_test_slot(
        app_module.TestSlotCheckoutRequest(
            project="glimmung",
            slot_index=2,
        )
    )

    released = await app_module.release_lease(result.lease_id, project="glimmung")

    assert released.state == LeaseState.RELEASED
    assert app_state.native_k8s_launcher.deleted_namespaces == ["glimmung-slot-2"]
    lease_doc = await app_state.cosmos.leases.read_item(
        item=result.lease_id,
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
    assert returned.lease_id == checked_out.lease_id
    assert returned.slot_index == 1
    assert returned.slot_name == "glimmung-slot-1"
    assert returned.cleanup_started is True
    assert app_state.native_k8s_launcher.deleted_namespaces == ["glimmung-slot-1"]

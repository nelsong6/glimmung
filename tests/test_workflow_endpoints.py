"""Workflow API surface tests.

Covers the PATCH /v1/workflows/{project}/{name} endpoint introduced for
live rollout-knob flips (pr.enabled, budget.total) without re-running
register_workflow's full upsert. Mirrors the test_pr_endpoints style —
direct helper invocation against the in-memory cosmos fake.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from glimmung.app import (
    WorkflowUpdateRequest,
    _read_workflow,
    patch_workflow_endpoint,
)

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        workflows=FakeContainer("workflows", "/project"),
    )


async def _seed_workflow(
    cosmos,
    *,
    project: str = "ambience",
    name: str = "agent-run",
    pr_enabled: bool = False,
    budget_total: float = 25.0,
) -> None:
    await cosmos.workflows.create_item({
        "id": name,
        "name": name,
        "project": project,
        "phases": [{
            "name": "agent",
            "kind": "gha_dispatch",
            "workflowFilename": "issue-agent.yml",
            "workflowRef": "main",
            "requirements": None,
            "verify": True,
            "recyclePolicy": {
                "maxAttempts": 3,
                "on": ["verify_fail"],
                "landsAt": "self",
            },
        }],
        "pr": {"enabled": pr_enabled, "recyclePolicy": None},
        "budget": {"total": budget_total},
        "triggerLabel": "agent-run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


def _app_with(cosmos):
    return SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))


@pytest.mark.asyncio
async def test_patch_workflow_flips_pr_enabled(cosmos, monkeypatch):
    """The motivating use case: flip a live workflow's pr.enabled
    without re-registering its full schema."""
    await _seed_workflow(cosmos, pr_enabled=False)
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    result = await patch_workflow_endpoint(
        req=WorkflowUpdateRequest(pr_enabled=True),
        project="ambience",
        name="agent-run",
    )

    assert result.pr.enabled is True
    # Verify the row is persisted, not just the response shape.
    doc = await _read_workflow(cosmos, "ambience", "agent-run")
    assert doc["pr"]["enabled"] is True
    # Other pr fields untouched.
    assert doc["pr"]["recyclePolicy"] is None


@pytest.mark.asyncio
async def test_patch_workflow_updates_budget_total(cosmos, monkeypatch):
    await _seed_workflow(cosmos, budget_total=25.0)
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    result = await patch_workflow_endpoint(
        req=WorkflowUpdateRequest(budget_total=50.0),
        project="ambience",
        name="agent-run",
    )

    assert result.budget.total == 50.0
    doc = await _read_workflow(cosmos, "ambience", "agent-run")
    assert doc["budget"]["total"] == 50.0


@pytest.mark.asyncio
async def test_patch_workflow_none_means_no_change(cosmos, monkeypatch):
    """Empty request body leaves the doc untouched. Confirms the patch
    semantics actually patch — not zero out — when callers omit fields."""
    await _seed_workflow(cosmos, pr_enabled=True, budget_total=42.0)
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    await patch_workflow_endpoint(
        req=WorkflowUpdateRequest(),
        project="ambience",
        name="agent-run",
    )

    doc = await _read_workflow(cosmos, "ambience", "agent-run")
    assert doc["pr"]["enabled"] is True
    assert doc["budget"]["total"] == 42.0


@pytest.mark.asyncio
async def test_patch_workflow_404_when_missing(cosmos, monkeypatch):
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    with pytest.raises(HTTPException) as excinfo:
        await patch_workflow_endpoint(
            req=WorkflowUpdateRequest(pr_enabled=True),
            project="ambience",
            name="does-not-exist",
        )
    assert excinfo.value.status_code == 404


@pytest.mark.asyncio
async def test_patch_workflow_partition_isolation(cosmos, monkeypatch):
    """Two workflows with the same name in different projects: a patch
    against one must not mutate the other."""
    await _seed_workflow(cosmos, project="ambience", name="agent-run", pr_enabled=False)
    await _seed_workflow(cosmos, project="spirelens", name="agent-run", pr_enabled=False)
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    await patch_workflow_endpoint(
        req=WorkflowUpdateRequest(pr_enabled=True),
        project="ambience",
        name="agent-run",
    )

    ambience_doc = await _read_workflow(cosmos, "ambience", "agent-run")
    spirelens_doc = await _read_workflow(cosmos, "spirelens", "agent-run")
    assert ambience_doc["pr"]["enabled"] is True
    assert spirelens_doc["pr"]["enabled"] is False

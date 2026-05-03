"""Workflow API surface tests.

Covers POST /v1/workflows (register, idempotent upsert) and
PATCH /v1/workflows/{project}/{name} (live rollout-knob flips for
pr.enabled and budget.total). Mirrors the test_pr_endpoints style —
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
    register_workflow,
)
from glimmung.models import NativeJobSpec, NativeStepSpec, PhaseSpec, WorkflowRegister

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        workflows=FakeContainer("workflows", "/project"),
        projects=FakeContainer("projects", "/name"),
    )


async def _seed_project(cosmos, name: str = "ambience") -> None:
    await cosmos.projects.create_item({
        "id": name,
        "name": name,
        "githubRepo": f"nelsong6/{name}",
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


async def _seed_workflow(
    cosmos,
    *,
    project: str = "ambience",
    name: str = "agent-run",
    pr_enabled: bool = False,
    budget_total: float = 25.0,
    created_at: str | None = None,
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
        "createdAt": created_at or datetime.now(UTC).isoformat(),
    })


def _app_with(cosmos):
    return SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))


# ─── PATCH /v1/workflows/{project}/{name} ──────────────────────────────────


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


# ─── POST /v1/workflows (register / upsert) ────────────────────────────────


def _single_phase_register(
    project: str = "ambience",
    name: str = "agent-run",
) -> WorkflowRegister:
    return WorkflowRegister(
        project=project,
        name=name,
        phases=[
            PhaseSpec(
                name="agent",
                workflow_filename="issue-agent.yml",
                outputs=["validation_url"],
            ),
        ],
    )


@pytest.mark.asyncio
async def test_register_workflow_creates_new(cosmos, monkeypatch):
    """First-time registration: writes a new workflow doc and returns
    the persisted shape."""
    await _seed_project(cosmos, "ambience")
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    result = await register_workflow(_single_phase_register())

    assert result.project == "ambience"
    assert result.name == "agent-run"
    assert [p.name for p in result.phases] == ["agent"]
    doc = await _read_workflow(cosmos, "ambience", "agent-run")
    assert doc is not None
    assert doc["phases"][0]["workflowFilename"] == "issue-agent.yml"


@pytest.mark.asyncio
async def test_register_workflow_idempotent_replace(cosmos, monkeypatch):
    """Re-registering the same shape is a no-op replace, not a
    duplicate. This is what consumer-migration scripts rely on."""
    await _seed_project(cosmos, "ambience")
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    await register_workflow(_single_phase_register())
    # Second call must not raise on duplicate id.
    await register_workflow(_single_phase_register())

    doc = await _read_workflow(cosmos, "ambience", "agent-run")
    assert doc is not None


@pytest.mark.asyncio
async def test_register_workflow_preserves_created_at_on_replace(cosmos, monkeypatch):
    """Replacing an existing workflow keeps the original `createdAt`.
    A fresh stamp would lose the audit trail of when the row first
    appeared."""
    original_created = "2026-01-01T00:00:00+00:00"
    await _seed_project(cosmos, "ambience")
    await _seed_workflow(
        cosmos,
        project="ambience",
        name="agent-run",
        created_at=original_created,
    )
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    await register_workflow(_single_phase_register())

    doc = await _read_workflow(cosmos, "ambience", "agent-run")
    assert doc["createdAt"] == original_created


@pytest.mark.asyncio
async def test_register_workflow_400_when_project_missing(cosmos, monkeypatch):
    """Project must be registered first — register_workflow won't
    auto-create one. Surfacing this as 400 (not 404) matches the
    project_doc check in the endpoint."""
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    with pytest.raises(HTTPException) as excinfo:
        await register_workflow(_single_phase_register(project="does-not-exist"))
    assert excinfo.value.status_code == 400
    assert "does not exist" in excinfo.value.detail


@pytest.mark.asyncio
async def test_register_workflow_2phase_with_inputs_and_outputs(cosmos, monkeypatch):
    """The motivating use case: re-register a workflow as 2-phase with
    cross-phase ref expressions. Server-side validation must accept
    well-formed refs; the actual ref-validator unit tests live in
    test_phase_input_refs.py."""
    await _seed_project(cosmos, "glimmung")
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    reg = WorkflowRegister(
        project="glimmung",
        name="agent-run",
        phases=[
            PhaseSpec(
                name="env-prep",
                workflow_filename="env-prep.yml",
                outputs=["validation_url", "image_tag"],
            ),
            PhaseSpec(
                name="agent-execute",
                workflow_filename="agent-execute.yml",
                inputs={
                    "validation_url": "${{ phases.env-prep.outputs.validation_url }}",
                    "image_tag": "${{ phases.env-prep.outputs.image_tag }}",
                },
            ),
        ],
    )
    result = await register_workflow(reg)

    assert [p.name for p in result.phases] == ["env-prep", "agent-execute"]
    doc = await _read_workflow(cosmos, "glimmung", "agent-run")
    assert len(doc["phases"]) == 2
    assert doc["phases"][1]["inputs"] == {
        "validation_url": "${{ phases.env-prep.outputs.validation_url }}",
        "image_tag": "${{ phases.env-prep.outputs.image_tag }}",
    }


@pytest.mark.asyncio
async def test_register_workflow_accepts_native_k8s_job_phase(cosmos, monkeypatch):
    await _seed_project(cosmos, "ambience")
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    reg = WorkflowRegister(
        project="ambience",
        name="native-agent",
        phases=[
            PhaseSpec(
                name="agent-execute",
                kind="k8s_job",
                jobs=[
                    NativeJobSpec(
                        id="agent",
                        image="romainecr.azurecr.io/ambience-runner:abc123",
                        command=["/app/native-runner"],
                        steps=[
                            NativeStepSpec(slug="clone-repo"),
                            NativeStepSpec(slug="run-agent", title="run agent"),
                        ],
                    )
                ],
                outputs=["branch"],
                verify=True,
            ),
        ],
    )

    result = await register_workflow(reg)

    assert result.phases[0].kind == "k8s_job"
    assert result.phases[0].workflow_filename == ""
    assert result.phases[0].jobs[0].id == "agent"
    doc = await _read_workflow(cosmos, "ambience", "native-agent")
    assert doc["phases"][0]["jobs"][0]["steps"] == [
        {"slug": "clone-repo", "title": None},
        {"slug": "run-agent", "title": "run agent"},
    ]


def test_k8s_job_phase_requires_unique_steps():
    with pytest.raises(ValueError) as excinfo:
        WorkflowRegister(
            project="ambience",
            name="native-agent",
            phases=[
                PhaseSpec(
                    name="agent-execute",
                    kind="k8s_job",
                    jobs=[
                        NativeJobSpec(
                            id="agent",
                            image="runner:latest",
                            steps=[
                                NativeStepSpec(slug="clone-repo"),
                                NativeStepSpec(slug="clone-repo"),
                            ],
                        )
                    ],
                ),
            ],
        )
    assert "step slugs must be unique" in str(excinfo.value)

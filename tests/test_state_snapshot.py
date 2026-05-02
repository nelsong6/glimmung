"""_compute_snapshot tests.

Regression cover for the phases-v1 migration: workflow docs in Cosmos
have nested phases with camelCase keys (`workflowFilename`,
`workflowRef`, `recyclePolicy`), and the shallow `_camel_to_snake`
helper doesn't recurse into them. `Workflow.model_validate` then 500s
on `phases.0.workflow_filename Field required`, which kills the
/v1/state and /v1/events stream, which kills the dashboard's projects
view (and surfaces as the "dead" pill).

Same shape of bug as the #74 hot-fix (4babd13) for list_workflows /
register_workflow, just in a different reader.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

import pytest

from glimmung.app import _compute_snapshot

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        hosts=FakeContainer("hosts", "/name"),
        leases=FakeContainer("leases", "/project"),
        projects=FakeContainer("projects", "/name"),
        workflows=FakeContainer("workflows", "/project"),
    )


async def _seed_phases_v1_workflow(
    cosmos,
    *,
    project: str = "ambience",
    name: str = "agent-run",
) -> None:
    await cosmos.workflows.create_item({
        "id": name,
        "name": name,
        "project": project,
        "phases": [{
            "name": "agent",
            "kind": "gha_dispatch",
            "workflowFilename": "agent-run.yml",
            "workflowRef": "main",
            "requirements": None,
            "verify": True,
            "recyclePolicy": {
                "maxAttempts": 3,
                "on": ["verify_fail"],
                "landsAt": "self",
            },
        }],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": "agent:run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


@pytest.mark.asyncio
async def test_compute_snapshot_handles_phases_v1_workflows(cosmos):
    """Phases-v1 workflows have camelCase keys nested inside `phases`.
    The snapshot must walk that shape correctly, not crash with
    'phases.0.workflow_filename Field required'."""
    await _seed_phases_v1_workflow(cosmos)

    snap = await _compute_snapshot(cosmos)

    assert len(snap.workflows) == 1
    w = snap.workflows[0]
    assert w.project == "ambience"
    assert w.name == "agent-run"
    assert len(w.phases) == 1
    assert w.phases[0].workflow_filename == "agent-run.yml"
    assert w.phases[0].workflow_ref == "main"
    assert w.phases[0].verify is True
    assert w.phases[0].recycle_policy is not None
    assert w.phases[0].recycle_policy.max_attempts == 3


@pytest.mark.asyncio
async def test_compute_snapshot_with_no_workflows(cosmos):
    snap = await _compute_snapshot(cosmos)
    assert snap.workflows == []
    assert snap.projects == []
    assert snap.hosts == []
    assert snap.pending_leases == []
    assert snap.active_leases == []

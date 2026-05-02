"""`_maybe_dispatch_workflow` input-filter tests.

The lease metadata is dual-purpose — workflow-facing inputs share a dict
with glimmung-internal bookkeeping (`issue_id`, `issue_lock_holder_id`,
etc.). GitHub's workflow_dispatch endpoint 422s any input the workflow
file doesn't declare, so leaking internal fields produces a silent
failure: the dispatch raises, the exception is logged-not-surfaced, the
Run lives on as IN_PROGRESS with no workflow_run_id forever.

These tests pin the allowlist behavior at the unit level so a future
metadata field doesn't accidentally re-leak.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace
from typing import Any

import pytest

from glimmung.app import _DISPATCH_INPUT_KEYS, _maybe_dispatch_workflow
from glimmung.models import Host

from tests.cosmos_fake import FakeContainer


def _cosmos() -> SimpleNamespace:
    return SimpleNamespace(
        projects=FakeContainer("projects", "/name"),
        workflows=FakeContainer("workflows", "/project"),
        hosts=FakeContainer("hosts", "/name"),
        leases=FakeContainer("leases", "/project"),
        runs=FakeContainer("runs", "/project"),
        locks=FakeContainer("locks", "/scope"),
        issues=FakeContainer("issues", "/project"),
        prs=FakeContainer("prs", "/project"),
    )


class _StubMinter:
    pass


@pytest.fixture
def app_state():
    cosmos = _cosmos()
    state = SimpleNamespace(cosmos=cosmos, gh_minter=_StubMinter())
    return SimpleNamespace(state=state)


async def _seed(app_state, *, project: str, repo: str, workflow_name: str,
                workflow_filename: str = "agent-run.yml") -> None:
    await app_state.state.cosmos.projects.create_item({
        "id": project, "name": project, "githubRepo": repo,
        "metadata": {}, "createdAt": datetime.now(UTC).isoformat(),
    })
    await app_state.state.cosmos.workflows.create_item({
        "id": workflow_name, "name": workflow_name, "project": project,
        "phases": [{
            "name": "agent",
            "kind": "gha_dispatch",
            "workflowFilename": workflow_filename,
            "workflowRef": "main",
            "requirements": None,
            "verify": True,
            "recyclePolicy": {"maxAttempts": 3, "on": ["verify_fail"], "landsAt": "self"},
        }],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": "agent:run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


def _lease_doc(*, lease_id: str, project: str, workflow: str,
                metadata: dict[str, Any]) -> dict[str, Any]:
    return {
        "id": lease_id,
        "project": project,
        "workflow": workflow,
        "metadata": metadata,
    }


def _host(name: str = "ambience-slot-1") -> Host:
    return Host(
        id=name, name=name, capabilities={}, current_lease_id="l1",
        last_heartbeat=datetime.now(UTC), last_used_at=datetime.now(UTC),
        drained=False, created_at=datetime.now(UTC),
    )


# ─── allowlist boundary ──────────────────────────────────────────────


@pytest.mark.asyncio
async def test_internal_metadata_keys_are_filtered_out_of_inputs(
    app_state, monkeypatch,
):
    """The original orphan-producing bug. Metadata splat used to ship
    internal-only fields (issue_id, issue_lock_holder_id, issue_repo,
    phase) straight to GH; GH 422s them. Confirm the allowlist drops
    them before dispatch_workflow ever sees them."""
    await _seed(app_state, project="ambience", repo="nelsong6/ambience",
                workflow_name="agent-run")

    captured: dict[str, Any] = {}
    async def fake_dispatch(minter, *, repo, workflow_filename, ref, inputs):
        captured["repo"] = repo
        captured["workflow_filename"] = workflow_filename
        captured["ref"] = ref
        captured["inputs"] = inputs
    monkeypatch.setattr("glimmung.app.dispatch_workflow", fake_dispatch)

    lease = _lease_doc(
        lease_id="01ABC", project="ambience", workflow="agent-run",
        metadata={
            # Workflow-facing — should pass through.
            "issue_number": "124",
            "issue_title": "Cave crystals",
            "run_id": "01RUN",
            "attempt_index": "0",
            # Internal-only — must be dropped.
            "issue_id": "01ISSUE",
            "issue_lock_holder_id": "01HOLDER",
            "issue_repo": "nelsong6/ambience",
            "phase": "retry",
        },
    )
    await _maybe_dispatch_workflow(app_state, lease, _host("ambience-slot-1"))

    assert captured["inputs"] == {
        "host": "ambience-slot-1",
        "lease_id": "01ABC",
        "issue_number": "124",
        "issue_title": "Cave crystals",
        "run_id": "01RUN",
        "attempt_index": "0",
    }
    # Spot-check each internal key by name so a regression on any one
    # surfaces a precise failure instead of a generic dict mismatch.
    for forbidden in ("issue_id", "issue_lock_holder_id", "issue_repo", "phase"):
        assert forbidden not in captured["inputs"]


@pytest.mark.asyncio
async def test_empty_metadata_still_dispatches_with_required_inputs(
    app_state, monkeypatch,
):
    """Lease with no metadata still yields a valid dispatch — `host` and
    `lease_id` are added unconditionally."""
    await _seed(app_state, project="ambience", repo="nelsong6/ambience",
                workflow_name="agent-run")

    captured: dict[str, Any] = {}
    async def fake_dispatch(minter, *, repo, workflow_filename, ref, inputs):
        captured["inputs"] = inputs
    monkeypatch.setattr("glimmung.app.dispatch_workflow", fake_dispatch)

    lease = _lease_doc(
        lease_id="01ABC", project="ambience", workflow="agent-run",
        metadata={},
    )
    await _maybe_dispatch_workflow(app_state, lease, _host())

    assert captured["inputs"] == {"host": "ambience-slot-1", "lease_id": "01ABC"}


@pytest.mark.asyncio
async def test_triage_metadata_passes_through_workflow_facing_keys(
    app_state, monkeypatch,
):
    """Triage path stamps run_id, attempt_index, pr_number, feedback,
    prior_verification_artifact_url + the always-internal
    issue_lock_holder_id. Verify the workflow-facing five forward and
    the internal one is dropped."""
    await _seed(app_state, project="ambience", repo="nelsong6/ambience",
                workflow_name="agent-run")

    captured: dict[str, Any] = {}
    async def fake_dispatch(minter, *, repo, workflow_filename, ref, inputs):
        captured["inputs"] = inputs
    monkeypatch.setattr("glimmung.app.dispatch_workflow", fake_dispatch)

    lease = _lease_doc(
        lease_id="01ABC", project="ambience", workflow="agent-run",
        metadata={
            "issue_number": "124",
            "run_id": "01RUN",
            "attempt_index": "1",
            "pr_number": "42",
            "feedback": "please address X",
            "prior_verification_artifact_url": "https://api.github.com/...",
            "issue_lock_holder_id": "01HOLDER",  # internal
        },
    )
    await _maybe_dispatch_workflow(app_state, lease, _host())

    assert "issue_lock_holder_id" not in captured["inputs"]
    for forwarded in (
        "issue_number", "run_id", "attempt_index", "pr_number",
        "feedback", "prior_verification_artifact_url",
    ):
        assert forwarded in captured["inputs"], f"{forwarded} should forward"


def test_allowlist_is_intentional_not_accidental():
    """If someone widens the allowlist, this assertion forces them to
    update the test too — keeps the contract documented in test code."""
    assert _DISPATCH_INPUT_KEYS == frozenset({
        "issue_number",
        "issue_title",
        "gh_event",
        "gh_action",
        "run_id",
        "attempt_index",
        "prior_verification_artifact_url",
        "feedback",
        "pr_number",
    })

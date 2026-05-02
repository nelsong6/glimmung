"""Operator cancel-lease tests (#30).

Drives `_cancel_lease` directly with cosmos_fake + a stub minter,
matching the existing test pattern around the lower-level primitives.
The endpoint itself is a thin wrapper (admin-auth + arg unpacking);
the helper is where the logic lives.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace
from typing import Any

import pytest

from glimmung import locks as lock_ops
from glimmung.app import _cancel_lease
from glimmung.models import (
    BudgetConfig,
    LeaseState,
    PhaseAttempt,
    Run,
    RunState,
)

from tests.cosmos_fake import FakeContainer


# ─── fixtures ─────────────────────────────────────────────────────────


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
    """Records GH API calls without making any network requests. The cancel
    helper only ever calls `cancel_workflow_run`, which itself calls
    `installation_token` — by patching cancel_workflow_run we don't even
    need a real token. We pass this stub straight through to the helper;
    the test patches `cancel_workflow_run` to inspect call args."""
    pass


def _settings() -> SimpleNamespace:
    return SimpleNamespace(lease_default_ttl_seconds=14400)


async def _put_lease(cosmos, *, lease_id: str, project: str, state: LeaseState,
                     metadata: dict[str, Any] | None = None,
                     host: str | None = None) -> None:
    now = datetime.now(UTC).isoformat()
    await cosmos.leases.create_item({
        "id": lease_id,
        "project": project,
        "workflow": "issue-agent",
        "host": host,
        "state": state.value,
        "requirements": {},
        "metadata": metadata or {},
        "requestedAt": now,
        "assignedAt": now if state == LeaseState.ACTIVE else None,
        "releasedAt": None,
        "ttlSeconds": 14400,
    })


async def _put_host(cosmos, *, name: str, current_lease_id: str | None) -> None:
    now = datetime.now(UTC).isoformat()
    await cosmos.hosts.create_item({
        "id": name, "name": name,
        "capabilities": {}, "currentLeaseId": current_lease_id,
        "drained": False, "createdAt": now,
    })


async def _put_run(
    cosmos, *,
    run_id: str, project: str, issue_repo: str, issue_number: int,
    workflow_run_id: int | None,
    state: RunState = RunState.IN_PROGRESS,
    issue_lock_holder_id: str | None = None,
    pr_number: int | None = None, pr_lock_holder_id: str | None = None,
) -> None:
    now = datetime.now(UTC)
    attempts: list[dict[str, Any]] = []
    if workflow_run_id is not None:
        attempts.append(PhaseAttempt(
            attempt_index=0, phase="agent",
            workflow_filename="issue-agent.yml",
            workflow_run_id=workflow_run_id,
            dispatched_at=now,
        ).model_dump(mode="json"))
    run = Run(
        id=run_id, project=project,
        workflow="issue-agent",
        issue_id="",
        issue_repo=issue_repo, issue_number=issue_number,
        state=state,
        budget=BudgetConfig(),
        attempts=[],
        issue_lock_holder_id=issue_lock_holder_id,
        pr_number=pr_number, pr_lock_holder_id=pr_lock_holder_id,
        created_at=now, updated_at=now,
    )
    doc = run.model_dump(mode="json")
    doc["attempts"] = attempts
    await cosmos.runs.create_item(doc)


@pytest.fixture
def cosmos():
    return _cosmos()


@pytest.fixture
def minter():
    return _StubMinter()


# ─── already_terminal ─────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_cancel_returns_already_terminal_for_released_lease(cosmos, minter):
    await _put_lease(cosmos, lease_id="l1", project="p", state=LeaseState.RELEASED)
    result = await _cancel_lease(cosmos, minter, "l1", "p")
    assert result.state == "already_terminal"
    assert result.lease_id == "l1"
    assert result.run_id is None


@pytest.mark.asyncio
async def test_cancel_returns_already_terminal_for_expired_lease(cosmos, minter):
    await _put_lease(cosmos, lease_id="l1", project="p", state=LeaseState.EXPIRED)
    result = await _cancel_lease(cosmos, minter, "l1", "p")
    assert result.state == "already_terminal"


# ─── no_active_run ────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_cancel_releases_lease_with_no_run_returns_no_active_run(cosmos, minter):
    """A lease without issue-tracked metadata releases cleanly with no
    GH-side cancel attempted."""
    await _put_host(cosmos, name="laptop", current_lease_id="l1")
    await _put_lease(
        cosmos, lease_id="l1", project="p", state=LeaseState.ACTIVE,
        host="laptop", metadata={"adhoc": "true"},
    )
    result = await _cancel_lease(cosmos, minter, "l1", "p")
    assert result.state == "no_active_run"
    assert result.gh_run_cancelled is None
    assert result.run_id is None

    # Lease released; host freed.
    lease_doc = await cosmos.leases.read_item(item="l1", partition_key="p")
    assert lease_doc["state"] == LeaseState.RELEASED.value
    host_doc = await cosmos.hosts.read_item(item="laptop", partition_key="laptop")
    assert host_doc["currentLeaseId"] is None


@pytest.mark.asyncio
async def test_cancel_returns_no_active_run_when_run_has_no_workflow_run_id(cosmos, minter):
    """A Run that exists but hasn't been GH-dispatched yet (no
    workflow_run_id on its latest attempt) releases the lease and aborts
    the Run, but doesn't try to cancel anything on GH."""
    await _put_host(cosmos, name="laptop", current_lease_id="l1")
    await _put_lease(
        cosmos, lease_id="l1", project="p", state=LeaseState.ACTIVE, host="laptop",
        metadata={"issue_repo": "r/n", "issue_number": "42"},
    )
    await _put_run(
        cosmos, run_id="run-1", project="p", issue_repo="r/n",
        issue_number=42, workflow_run_id=None,  # no GH dispatch yet
    )
    result = await _cancel_lease(cosmos, minter, "l1", "p")
    assert result.state == "no_active_run"
    assert result.gh_run_cancelled is None
    # Run was found and aborted, so run_id is reported; only the GH-cancel
    # branch was skipped because there was no workflow_run_id to target.
    assert result.run_id == "run-1"
    run_doc = await cosmos.runs.read_item(item="run-1", partition_key="p")
    assert run_doc["state"] == RunState.ABORTED.value
    assert run_doc["abort_reason"] == "cancelled_via_ui"


# ─── cancelled (happy path) ───────────────────────────────────────────


@pytest.mark.asyncio
async def test_cancel_dispatches_gh_cancel_releases_lease_and_aborts_run(
    cosmos, minter, monkeypatch,
):
    """The full happy path: Run with a workflow_run_id gets a GH cancel,
    the Run is marked ABORTED, the lease releases, and the issue lock
    releases."""
    await _put_host(cosmos, name="laptop", current_lease_id="l1")
    await _put_lease(
        cosmos, lease_id="l1", project="p", state=LeaseState.ACTIVE, host="laptop",
        metadata={
            "issue_repo": "nelsong6/ambience", "issue_number": "42",
            "issue_lock_holder_id": "holder-1",
        },
    )
    # Pre-claim the issue lock so release has something to release.
    await lock_ops.claim_lock(
        cosmos, scope="issue", key="nelsong6/ambience#42",
        holder_id="holder-1", ttl_seconds=14400,
    )
    await _put_run(
        cosmos, run_id="run-1", project="p", issue_repo="nelsong6/ambience",
        issue_number=42, workflow_run_id=999_999,
        issue_lock_holder_id="holder-1",
    )

    cancel_calls: list[dict[str, Any]] = []
    async def fake_cancel(minter_arg, *, repo, run_id):
        cancel_calls.append({"repo": repo, "run_id": run_id})
        return True
    monkeypatch.setattr("glimmung.app.cancel_workflow_run", fake_cancel)

    result = await _cancel_lease(cosmos, minter, "l1", "p")
    assert result.state == "cancelled"
    assert result.run_id == "run-1"
    assert result.gh_run_cancelled is True
    assert result.issue_lock_released is True

    # GH cancel hit the right run.
    assert cancel_calls == [{"repo": "nelsong6/ambience", "run_id": 999_999}]

    # Run aborted.
    run_doc = await cosmos.runs.read_item(item="run-1", partition_key="p")
    assert run_doc["state"] == RunState.ABORTED.value
    assert run_doc["abort_reason"] == "cancelled_via_ui"

    # Lease released; host freed.
    lease_doc = await cosmos.leases.read_item(item="l1", partition_key="p")
    assert lease_doc["state"] == LeaseState.RELEASED.value
    host_doc = await cosmos.hosts.read_item(item="laptop", partition_key="laptop")
    assert host_doc["currentLeaseId"] is None

    # Issue lock released.
    lock_doc = await cosmos.locks.read_item(
        item="issue::nelsong6%2Fambience%2342", partition_key="issue",
    )
    assert lock_doc["state"] == "released"


@pytest.mark.asyncio
async def test_cancel_releases_pr_lock_when_run_holds_one(cosmos, minter, monkeypatch):
    """A Run currently in a triage cycle holds a PR lock too; cancel
    releases both."""
    await _put_host(cosmos, name="laptop", current_lease_id="l1")
    await _put_lease(
        cosmos, lease_id="l1", project="p", state=LeaseState.ACTIVE, host="laptop",
        metadata={
            "issue_repo": "r/n", "issue_number": "42",
            "issue_lock_holder_id": "issue-holder",
        },
    )
    await lock_ops.claim_lock(cosmos, scope="issue", key="r/n#42",
                              holder_id="issue-holder", ttl_seconds=14400)
    await lock_ops.claim_lock(cosmos, scope="pr", key="r/n#7",
                              holder_id="pr-holder", ttl_seconds=14400)
    await _put_run(
        cosmos, run_id="run-1", project="p", issue_repo="r/n",
        issue_number=42, workflow_run_id=123,
        issue_lock_holder_id="issue-holder",
        pr_number=7, pr_lock_holder_id="pr-holder",
    )
    monkeypatch.setattr(
        "glimmung.app.cancel_workflow_run",
        lambda m, *, repo, run_id: _async_true(),
    )

    result = await _cancel_lease(cosmos, minter, "l1", "p")
    assert result.state == "cancelled"
    assert result.issue_lock_released is True
    assert result.pr_lock_released is True


async def _async_true() -> bool:
    return True


# ─── GH 404 (run already terminal) ────────────────────────────────────


@pytest.mark.asyncio
async def test_cancel_handles_gh_404_as_already_terminal_on_gh_side(
    cosmos, minter, monkeypatch,
):
    """If the GH run already finished naturally between the operator's
    click and our POST, GH returns 404 → cancel_workflow_run returns
    False. The state is still 'cancelled' (the operator's intent was
    processed), but `gh_run_cancelled` records the actual outcome."""
    await _put_host(cosmos, name="laptop", current_lease_id="l1")
    await _put_lease(
        cosmos, lease_id="l1", project="p", state=LeaseState.ACTIVE, host="laptop",
        metadata={"issue_repo": "r/n", "issue_number": "42"},
    )
    await _put_run(
        cosmos, run_id="run-1", project="p", issue_repo="r/n",
        issue_number=42, workflow_run_id=999,
    )
    async def fake_cancel(_m, *, repo, run_id):
        return False
    monkeypatch.setattr("glimmung.app.cancel_workflow_run", fake_cancel)

    result = await _cancel_lease(cosmos, minter, "l1", "p")
    assert result.state == "cancelled"
    assert result.gh_run_cancelled is False


# ─── lookup errors ────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_cancel_404s_for_missing_lease(cosmos, minter):
    from fastapi import HTTPException
    with pytest.raises(HTTPException) as exc:
        await _cancel_lease(cosmos, minter, "no-such", "p")
    assert exc.value.status_code == 404

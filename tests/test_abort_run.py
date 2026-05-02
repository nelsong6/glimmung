"""Operator abort-run tests.

Drives `_abort_run` directly with cosmos_fake + a stub minter, matching
the existing test pattern around `_cancel_lease`. The endpoint itself is
a thin wrapper (admin-auth + arg unpacking); the helper is where the
logic lives.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace
from typing import Any

import pytest

from glimmung import locks as lock_ops
from glimmung.app import _abort_run
from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    Run,
    RunPhase,
    RunState,
)

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
            attempt_index=0, phase=RunPhase.INITIAL,
            workflow_filename="agent-run.yml",
            workflow_run_id=workflow_run_id,
            dispatched_at=now,
        ).model_dump(mode="json"))
    else:
        # Orphan-shape: dispatch never recorded a workflow_run_id.
        attempts.append(PhaseAttempt(
            attempt_index=0, phase=RunPhase.INITIAL,
            workflow_filename="agent-run.yml",
            workflow_run_id=None,
            dispatched_at=now,
        ).model_dump(mode="json"))
    run = Run(
        id=run_id, project=project,
        workflow="agent-run",
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


# ─── 404 ──────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_abort_404s_for_missing_run(cosmos, minter):
    from fastapi import HTTPException
    with pytest.raises(HTTPException) as exc:
        await _abort_run(
            cosmos, minter, run_id="no-such", project="p", reason="r",
        )
    assert exc.value.status_code == 404


# ─── already_terminal ─────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_abort_returns_already_terminal_for_passed_run(cosmos, minter):
    await _put_run(
        cosmos, run_id="run-1", project="p", issue_repo="r/n",
        issue_number=42, workflow_run_id=None, state=RunState.PASSED,
    )
    result = await _abort_run(
        cosmos, minter, run_id="run-1", project="p", reason="r",
    )
    assert result.state == "already_terminal"
    assert result.run_id == "run-1"


@pytest.mark.asyncio
async def test_abort_returns_already_terminal_for_aborted_run(cosmos, minter):
    await _put_run(
        cosmos, run_id="run-1", project="p", issue_repo="r/n",
        issue_number=42, workflow_run_id=None, state=RunState.ABORTED,
    )
    result = await _abort_run(
        cosmos, minter, run_id="run-1", project="p", reason="r",
    )
    assert result.state == "already_terminal"


# ─── aborted (orphan: no workflow_run_id) ─────────────────────────────


@pytest.mark.asyncio
async def test_abort_orphan_with_no_workflow_run_id_skips_gh_cancel(cosmos, minter):
    """The exact stuck-record case: Run is IN_PROGRESS, dispatch never
    recorded a workflow_run_id, lock has expired but Run is still
    IN_PROGRESS. Abort flips state, doesn't touch GH."""
    await _put_run(
        cosmos, run_id="run-1", project="p", issue_repo="r/n",
        issue_number=42, workflow_run_id=None,
    )
    result = await _abort_run(
        cosmos, minter, run_id="run-1", project="p", reason="orphaned",
    )
    assert result.state == "aborted"
    assert result.run_id == "run-1"
    assert result.gh_run_cancelled is None  # no dispatch happened, so no cancel attempted

    run_doc = await cosmos.runs.read_item(item="run-1", partition_key="p")
    assert run_doc["state"] == RunState.ABORTED.value
    assert run_doc["abort_reason"] == "orphaned"


# ─── aborted (with GH cancel) ─────────────────────────────────────────


@pytest.mark.asyncio
async def test_abort_dispatches_gh_cancel_and_releases_locks(
    cosmos, minter, monkeypatch,
):
    await lock_ops.claim_lock(
        cosmos, scope="issue", key="r/n#42",
        holder_id="issue-holder", ttl_seconds=14400,
    )
    await _put_run(
        cosmos, run_id="run-1", project="p", issue_repo="r/n",
        issue_number=42, workflow_run_id=999_999,
        issue_lock_holder_id="issue-holder",
    )

    cancel_calls: list[dict[str, Any]] = []
    async def fake_cancel(minter_arg, *, repo, run_id):
        cancel_calls.append({"repo": repo, "run_id": run_id})
        return True
    monkeypatch.setattr("glimmung.app.cancel_workflow_run", fake_cancel)

    result = await _abort_run(
        cosmos, minter, run_id="run-1", project="p", reason="admin",
    )
    assert result.state == "aborted"
    assert result.gh_run_cancelled is True
    assert result.issue_lock_released is True
    assert cancel_calls == [{"repo": "r/n", "run_id": 999_999}]

    run_doc = await cosmos.runs.read_item(item="run-1", partition_key="p")
    assert run_doc["state"] == RunState.ABORTED.value
    assert run_doc["abort_reason"] == "admin"

    lock_doc = await cosmos.locks.read_item(
        item="issue::r%2Fn%2342", partition_key="issue",
    )
    assert lock_doc["state"] == "released"


@pytest.mark.asyncio
async def test_abort_releases_pr_lock_when_run_holds_one(
    cosmos, minter, monkeypatch,
):
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

    result = await _abort_run(
        cosmos, minter, run_id="run-1", project="p", reason="admin",
    )
    assert result.state == "aborted"
    assert result.issue_lock_released is True
    assert result.pr_lock_released is True


async def _async_true() -> bool:
    return True


# ─── aborted (GH 404) ─────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_abort_handles_gh_404_as_already_terminal_on_gh_side(
    cosmos, minter, monkeypatch,
):
    """If the GH run finished naturally between dispatch and abort, GH
    returns 404 → cancel_workflow_run returns False. State is still
    'aborted' (the operator's intent was processed)."""
    await _put_run(
        cosmos, run_id="run-1", project="p", issue_repo="r/n",
        issue_number=42, workflow_run_id=999,
    )
    async def fake_cancel(_m, *, repo, run_id):
        return False
    monkeypatch.setattr("glimmung.app.cancel_workflow_run", fake_cancel)

    result = await _abort_run(
        cosmos, minter, run_id="run-1", project="p", reason="r",
    )
    assert result.state == "aborted"
    assert result.gh_run_cancelled is False


# ─── idempotence ──────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_abort_is_idempotent(cosmos, minter):
    await _put_run(
        cosmos, run_id="run-1", project="p", issue_repo="r/n",
        issue_number=42, workflow_run_id=None,
    )
    first = await _abort_run(
        cosmos, minter, run_id="run-1", project="p", reason="r",
    )
    assert first.state == "aborted"
    second = await _abort_run(
        cosmos, minter, run_id="run-1", project="p", reason="r",
    )
    assert second.state == "already_terminal"

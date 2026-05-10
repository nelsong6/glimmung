from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

import pytest
from pydantic import ValidationError

from glimmung.app import _public_signal
from glimmung.models import (
    BudgetConfig,
    Issue,
    IssueMetadata,
    IssueState,
    Run,
    RunState,
    Signal,
    SignalEnqueueRequest,
    SignalSource,
    SignalState,
    SignalTargetType,
)

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        issues=FakeContainer("issues", "/project"),
        runs=FakeContainer("runs", "/project"),
    )


def _now() -> datetime:
    return datetime(2026, 1, 1, tzinfo=UTC)


def _signal(*, target_type: SignalTargetType, target_id: str) -> Signal:
    return Signal(
        id="01K00000000000000000000000",
        target_type=target_type,
        target_repo="ambience",
        target_id=target_id,
        source=SignalSource.GLIMMUNG_UI,
        payload={},
        state=SignalState.PENDING,
        enqueued_at=_now(),
    )


def test_signal_enqueue_request_uses_public_target_ref() -> None:
    req = SignalEnqueueRequest.model_validate({
        "target_type": "pr",
        "target_repo": "ambience",
        "target_ref": "nelsong6/ambience#42",
    })

    assert req.target_ref == "nelsong6/ambience#42"

    with pytest.raises(ValidationError):
        SignalEnqueueRequest.model_validate({
            "target_type": "pr",
            "target_repo": "ambience",
            "target_id": "01K00000000000000000000000",
        })


@pytest.mark.asyncio
async def test_public_signal_returns_issue_ref_not_storage_id(cosmos) -> None:
    issue = Issue(
        id="01KISSUE000000000000000000",
        number=17,
        project="ambience",
        title="Fix",
        body="",
        labels=[],
        state=IssueState.OPEN,
        metadata=IssueMetadata(),
        created_at=_now(),
        updated_at=_now(),
    )
    await cosmos.issues.create_item(issue.model_dump(mode="json"))

    public = await _public_signal(
        cosmos,
        _signal(target_type=SignalTargetType.ISSUE, target_id=issue.id),
    )

    assert public.target_ref == "ambience#17"
    assert issue.id not in public.ref


@pytest.mark.asyncio
async def test_public_signal_returns_run_ref_not_storage_id(cosmos) -> None:
    run = Run(
        id="01KRUN00000000000000000000",
        project="ambience",
        workflow="issue-agent",
        run_number=2,
        run_display_number="2.1",
        issue_id="01KISSUE000000000000000000",
        issue_repo="nelsong6/ambience",
        issue_number=17,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(),
        attempts=[],
        created_at=_now(),
        updated_at=_now(),
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))

    public = await _public_signal(
        cosmos,
        _signal(target_type=SignalTargetType.RUN, target_id=run.id),
    )

    assert public.target_ref == "ambience#17/runs/2.1"
    assert run.id not in public.ref


from datetime import UTC, datetime, timedelta
from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from glimmung.app import get_run_report
from glimmung.models import BudgetConfig, PhaseAttempt, Run, RunState

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        runs=FakeContainer("runs", "/project"),
    )


@pytest.fixture
def app_state(cosmos):
    return SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))


@pytest.mark.asyncio
async def test_run_report_is_derived_from_one_run(app_state, cosmos, monkeypatch):
    monkeypatch.setattr("glimmung.app.app", app_state)
    now = datetime(2026, 5, 6, 2, 20, tzinfo=UTC)
    run = Run(
        id="run-1",
        project="glimmung",
        workflow="issue-agent",
        issue_id="issue-1",
        issue_repo="nelsong6/glimmung",
        issue_number=42,
        state=RunState.PASSED,
        budget=BudgetConfig(total=10),
        attempts=[
            PhaseAttempt(
                attempt_index=0,
                phase="implement",
                workflow_filename="issue-agent.yaml",
                workflow_run_id=100,
                dispatched_at=now,
                completed_at=now + timedelta(minutes=4),
                conclusion="success",
                decision="advance",
                cost_usd=1.25,
            ),
            PhaseAttempt(
                attempt_index=1,
                phase="verify",
                workflow_filename="verify.yaml",
                workflow_run_id=101,
                dispatched_at=now + timedelta(minutes=5),
                completed_at=now + timedelta(minutes=7),
                conclusion="success",
                decision="advance",
                cost_usd=0.75,
            ),
        ],
        cumulative_cost_usd=2.0,
        validation_url="https://preview.example",
        screenshots_markdown="![screen](artifact.png)",
        created_at=now,
        updated_at=now + timedelta(minutes=8),
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))

    report = await get_run_report(project="glimmung", run_id="run-1")

    assert report.id == "run-1:report"
    assert report.project == "glimmung"
    assert report.run_id == "run-1"
    assert report.issue_id == "issue-1"
    assert report.issue_number == 42
    assert report.state == RunState.PASSED
    assert report.current_phase == "verify"
    assert report.attempts_count == 2
    assert report.cumulative_cost_usd == 2.0
    assert report.validation_url == "https://preview.example"
    assert report.screenshots_markdown == "![screen](artifact.png)"
    assert report.completed_at == now + timedelta(minutes=7)
    assert [a.phase for a in report.attempts] == ["implement", "verify"]
    assert [a.cost_usd for a in report.attempts] == [1.25, 0.75]


@pytest.mark.asyncio
async def test_run_report_404s_for_missing_run(app_state, monkeypatch):
    monkeypatch.setattr("glimmung.app.app", app_state)

    with pytest.raises(HTTPException) as exc:
        await get_run_report(project="glimmung", run_id="missing")

    assert exc.value.status_code == 404

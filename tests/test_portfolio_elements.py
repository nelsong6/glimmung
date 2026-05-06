from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from glimmung.app import (
    PortfolioElementsDispatchRequest,
    PortfolioElementPatchRequest,
    PortfolioElementUpsertRequest,
    dispatch_portfolio_elements,
    list_portfolio_elements,
    patch_portfolio_element,
    upsert_portfolio_element,
)
from glimmung.dispatch import DispatchResult
from glimmung.models import PortfolioReviewState

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        issues=FakeContainer("issues", "/project"),
    )


@pytest.fixture
def app_state(cosmos):
    return SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))


@pytest.mark.asyncio
async def test_agent_can_mark_portfolio_element_for_review(app_state, monkeypatch):
    monkeypatch.setattr("glimmung.app.app", app_state)

    element = await upsert_portfolio_element(
        PortfolioElementUpsertRequest(
            project="glimmung",
            route="/_design-portfolio",
            element_id="sidebar.nav",
            title="Sidebar navigation",
            screenshot_url="/v1/artifacts/runs/glimmung/run-1/sidebar.png",
            status=PortfolioReviewState.NEEDS_REVIEW,
            notes="Changed spacing and selected state.",
            last_touched_run_id="run-1",
        )
    )

    assert element.project == "glimmung"
    assert element.status == PortfolioReviewState.NEEDS_REVIEW
    assert element.last_touched_run_id == "run-1"

    rows = await list_portfolio_elements(project="glimmung", status=None, limit=None)
    assert [r.id for r in rows] == [element.id]


@pytest.mark.asyncio
async def test_human_review_state_persists_without_dispatch(app_state, monkeypatch):
    monkeypatch.setattr("glimmung.app.app", app_state)
    element = await upsert_portfolio_element(
        PortfolioElementUpsertRequest(
            project="glimmung",
            route="/_design-portfolio",
            element_id="toolbar.actions",
            title="Toolbar actions",
        )
    )

    patched = await patch_portfolio_element(
        PortfolioElementPatchRequest(
            status=PortfolioReviewState.APPROVED,
            notes="Looks good.",
        ),
        project="glimmung",
        element_doc_id=element.id,
    )

    assert patched.status == PortfolioReviewState.APPROVED
    assert patched.notes == "Looks good."

    approved = await list_portfolio_elements(
        project="glimmung",
        status=PortfolioReviewState.APPROVED,
        limit=None,
    )
    assert [r.id for r in approved] == [element.id]


@pytest.mark.asyncio
async def test_patch_does_not_mutate_issue_documents(app_state, cosmos, monkeypatch):
    monkeypatch.setattr("glimmung.app.app", app_state)
    await cosmos.issues.create_item(
        {
            "id": "issue-1",
            "project": "glimmung",
            "title": "Regular issue",
            "state": "open",
        }
    )

    with pytest.raises(HTTPException) as exc:
        await patch_portfolio_element(
            PortfolioElementPatchRequest(notes="Do not write this."),
            project="glimmung",
            element_doc_id="issue-1",
        )

    assert exc.value.status_code == 404
    doc = await cosmos.issues.read_item(item="issue-1", partition_key="glimmung")
    assert "notes" not in doc


@pytest.mark.asyncio
async def test_dispatch_creates_issue_from_selected_portfolio_rows(app_state, cosmos, monkeypatch):
    monkeypatch.setattr("glimmung.app.app", app_state)
    captured: dict[str, object] = {}

    async def fake_dispatch(app, **kwargs):
        captured.update(kwargs)
        return DispatchResult(
            state="dispatched",
            lease_id="lease-1",
            run_id="run-2",
            workflow=kwargs.get("workflow_name"),
        )

    monkeypatch.setattr("glimmung.app.dispatch_run", fake_dispatch)

    selected = await upsert_portfolio_element(
        PortfolioElementUpsertRequest(
            project="glimmung",
            route="/_design-portfolio",
            element_id="sidebar.nav",
            title="Sidebar navigation",
            notes="Needs contrast pass.",
            status=PortfolioReviewState.NEEDS_REVIEW,
        )
    )
    await upsert_portfolio_element(
        PortfolioElementUpsertRequest(
            project="glimmung",
            route="/_design-portfolio",
            element_id="toolbar.actions",
            title="Toolbar actions",
            status=PortfolioReviewState.APPROVED,
        )
    )

    result = await dispatch_portfolio_elements(
        PortfolioElementsDispatchRequest(
            project="glimmung",
            status=PortfolioReviewState.NEEDS_REVIEW,
            workflow="portfolio-agent",
        )
    )

    assert result.state == "dispatched"
    assert captured["project"] == "glimmung"
    assert captured["workflow_name"] == "portfolio-agent"
    assert captured["trigger_source"] == {
        "kind": "portfolio_review",
        "status": "needs_review",
        "route": None,
        "element_count": 1,
    }
    issue = await cosmos.issues.read_item(
        item=captured["issue_id"],
        partition_key="glimmung",
    )
    assert issue["title"] == "Review portfolio element: Sidebar navigation"
    assert issue["labels"] == ["design-portfolio", "needs_review"]
    assert "`/_design-portfolio` / `sidebar.nav`" in issue["body"]
    assert "Needs contrast pass." in issue["body"]
    assert "Toolbar actions" not in issue["body"]
    assert captured["extra_metadata"] == {
        "portfolio_review": {
            "status": "needs_review",
            "route": None,
            "element_ids": [selected.id],
        },
    }


@pytest.mark.asyncio
async def test_dispatch_requires_matching_portfolio_rows(app_state, monkeypatch):
    monkeypatch.setattr("glimmung.app.app", app_state)

    with pytest.raises(HTTPException) as exc:
        await dispatch_portfolio_elements(
            PortfolioElementsDispatchRequest(project="glimmung")
        )

    assert exc.value.status_code == 400
    assert "no needs_review portfolio elements" in exc.value.detail

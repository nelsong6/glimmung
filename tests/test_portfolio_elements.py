from types import SimpleNamespace

import pytest

from glimmung.app import (
    PortfolioElementPatchRequest,
    PortfolioElementUpsertRequest,
    list_portfolio_elements,
    patch_portfolio_element,
    upsert_portfolio_element,
)
from glimmung.models import PortfolioReviewState

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        portfolio_elements=FakeContainer("portfolio_elements", "/project"),
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

import json
from pathlib import Path

from glimmung.models import PlaybookCreate, PlaybookIntegrationStrategy


ROOT = Path(__file__).resolve().parents[1]


def test_design_portfolio_bootstrap_template_matches_playbook_create_model():
    payload = json.loads(
        (ROOT / "docs/playbooks/design-portfolio-bootstrap.json").read_text(),
    )

    template = PlaybookCreate.model_validate(payload)

    assert template.metadata["template"] == "design-portfolio-bootstrap"
    assert template.concurrency_limit == 1
    assert template.integration_strategy == PlaybookIntegrationStrategy.ISOLATED_PRS
    assert [entry.id for entry in template.entries] == [
        "detect-frontend-shape",
        "add-portfolio-route",
        "add-screenshot-coverage",
        "review-handoff",
    ]
    assert template.entries[-1].manual_gate is True
    assert "needs_review" in template.entries[1].issue.body

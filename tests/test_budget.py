"""Budget label parsing + resolution."""

from glimmung.budget import parse_budget_label, resolve_budget
from glimmung.models import BudgetConfig


def test_parses_well_formed_label():
    b = parse_budget_label("agent-budget:5x50")
    assert b == BudgetConfig(max_attempts=5, max_cost_usd=50.0)


def test_parses_decimal_cost():
    b = parse_budget_label("agent-budget:3x12.5")
    assert b == BudgetConfig(max_attempts=3, max_cost_usd=12.5)


def test_returns_none_for_unrelated_label():
    assert parse_budget_label("bug") is None
    assert parse_budget_label("agent-run") is None


def test_returns_none_for_missing_separator():
    assert parse_budget_label("agent-budget:5") is None


def test_returns_none_for_non_numeric():
    assert parse_budget_label("agent-budget:fivex25") is None
    assert parse_budget_label("agent-budget:5xfree") is None


def test_returns_none_for_non_positive():
    assert parse_budget_label("agent-budget:0x25") is None
    assert parse_budget_label("agent-budget:5x0") is None
    assert parse_budget_label("agent-budget:-3x25") is None


def test_resolve_label_wins_over_workflow_default():
    workflow_default = BudgetConfig(max_attempts=2, max_cost_usd=10.0)
    resolved = resolve_budget(["bug", "agent-budget:5x50"], workflow_default)
    assert resolved == BudgetConfig(max_attempts=5, max_cost_usd=50.0)


def test_resolve_workflow_default_when_no_label():
    workflow_default = BudgetConfig(max_attempts=7, max_cost_usd=100.0)
    resolved = resolve_budget(["bug", "needs-design"], workflow_default)
    assert resolved == workflow_default


def test_resolve_falls_back_to_global_defaults():
    resolved = resolve_budget([], None)
    assert resolved == BudgetConfig()  # 3 attempts / $25


def test_resolve_first_matching_label_wins():
    """If a user (somehow) has two budget labels, take the first."""
    resolved = resolve_budget(
        ["agent-budget:1x1", "agent-budget:99x999"], None,
    )
    assert resolved == BudgetConfig(max_attempts=1, max_cost_usd=1.0)


def test_resolve_skips_malformed_label_falls_to_default():
    workflow_default = BudgetConfig(max_attempts=4, max_cost_usd=40.0)
    resolved = resolve_budget(["agent-budget:bogus"], workflow_default)
    assert resolved == workflow_default

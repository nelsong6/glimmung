"""Budget label parsing + resolution (#69 schema)."""

from glimmung.budget import parse_budget_label, resolve_budget
from glimmung.models import BudgetConfig


def test_parses_new_format_total_only():
    b = parse_budget_label("agent-budget:50")
    assert b == BudgetConfig(total=50.0)


def test_parses_decimal_cost():
    b = parse_budget_label("agent-budget:12.5")
    assert b == BudgetConfig(total=12.5)


def test_parses_legacy_NxM_keeps_only_M():
    """Legacy `agent-budget:NxM` keeps working — N (attempt count) is
    silently dropped because per-attempt counts moved off the run-level
    budget under #69."""
    b = parse_budget_label("agent-budget:5x50")
    assert b == BudgetConfig(total=50.0)


def test_returns_none_for_unrelated_label():
    assert parse_budget_label("bug") is None
    assert parse_budget_label("agent-run") is None


def test_returns_none_for_non_numeric():
    # Legacy NxM with bad M still fails. (Bad N is silently dropped under
    # #69 since N is no longer used — `fivex25` parses as total=25.)
    assert parse_budget_label("agent-budget:5xfree") is None
    assert parse_budget_label("agent-budget:bogus") is None


def test_returns_none_for_non_positive():
    assert parse_budget_label("agent-budget:0") is None
    assert parse_budget_label("agent-budget:5x0") is None
    assert parse_budget_label("agent-budget:-3") is None


def test_resolve_label_wins_over_workflow_default():
    workflow_default = BudgetConfig(total=10.0)
    resolved = resolve_budget(["bug", "agent-budget:50"], workflow_default)
    assert resolved == BudgetConfig(total=50.0)


def test_resolve_workflow_default_when_no_label():
    workflow_default = BudgetConfig(total=100.0)
    resolved = resolve_budget(["bug", "needs-design"], workflow_default)
    assert resolved == workflow_default


def test_resolve_falls_back_to_global_defaults():
    resolved = resolve_budget([], None)
    assert resolved == BudgetConfig()  # $25 default


def test_resolve_first_matching_label_wins():
    """If a user (somehow) has two budget labels, take the first."""
    resolved = resolve_budget(
        ["agent-budget:1", "agent-budget:999"], None,
    )
    assert resolved == BudgetConfig(total=1.0)


def test_resolve_skips_malformed_label_falls_to_default():
    workflow_default = BudgetConfig(total=40.0)
    resolved = resolve_budget(["agent-budget:bogus"], workflow_default)
    assert resolved == workflow_default

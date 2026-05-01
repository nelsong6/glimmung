"""Budget config parsing for the verify-loop substrate (#18).

Resolution order, first match wins:

  1. Issue label `agent-budget:NxM`   (N = max_attempts, M = max_cost_usd)
  2. Workflow.default_budget          (per-repo default registered via /v1/workflows)
  3. BudgetConfig() defaults          (3 attempts, $25)

The label format is intentionally one-token: GitHub label names get
echoed in dozens of places (issue UI, search, mobile, gh CLI), and a
multi-label or JSON-in-label scheme would degrade all of them.

Per-issue config is frozen at run-creation time; relabeling mid-run does
not move the goalposts. (See `runs.create_run` in runs.py.)
"""

import logging
from collections.abc import Iterable

from glimmung.models import BudgetConfig

log = logging.getLogger(__name__)

LABEL_PREFIX = "agent-budget:"


def parse_budget_label(label: str) -> BudgetConfig | None:
    """Parse a single label. Returns None if the label doesn't apply
    *or* if it's malformed (logged as a warning so the user finds out
    why their override didn't take effect)."""
    if not label.startswith(LABEL_PREFIX):
        return None
    spec = label[len(LABEL_PREFIX):]
    if "x" not in spec:
        log.warning("budget label %r missing 'x' separator; ignoring", label)
        return None
    n_raw, m_raw = spec.split("x", 1)
    try:
        n = int(n_raw)
        m = float(m_raw)
    except ValueError:
        log.warning("budget label %r has non-numeric N or M; ignoring", label)
        return None
    if n <= 0 or m <= 0:
        log.warning("budget label %r has non-positive value; ignoring", label)
        return None
    return BudgetConfig(max_attempts=n, max_cost_usd=m)


def resolve_budget(
    label_names: Iterable[str],
    workflow_default: BudgetConfig | None,
) -> BudgetConfig:
    for label in label_names:
        budget = parse_budget_label(label)
        if budget is not None:
            return budget
    if workflow_default is not None:
        return workflow_default
    return BudgetConfig()

"""Budget config parsing for the verify-loop substrate (#18, #69).

Resolution order, first match wins:

  1. Issue label `agent-budget:M` or legacy `agent-budget:NxM`
     (M = total cost cap in USD; N is ignored under the #69 schema —
     per-phase attempt counts now live on the phase's recycle_policy).
  2. Workflow.budget                  (per-repo default registered via /v1/workflows)
  3. BudgetConfig() defaults          ($25 cumulative)

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
    """Parse a single label. Returns None if the label doesn't apply or
    is malformed (logged as a warning so the user finds out why their
    override didn't take effect). Accepts both `agent-budget:M` (#69
    shape) and the legacy `agent-budget:NxM` (N silently dropped — the
    per-attempt count moved off the run-level budget)."""
    if not label.startswith(LABEL_PREFIX):
        return None
    spec = label[len(LABEL_PREFIX):]
    raw = spec.split("x", 1)[1] if "x" in spec else spec
    try:
        m = float(raw)
    except ValueError:
        log.warning("budget label %r has non-numeric cost cap; ignoring", label)
        return None
    if m <= 0:
        log.warning("budget label %r has non-positive cost cap; ignoring", label)
        return None
    return BudgetConfig(total=m)


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

"""Public, human-readable handles for operator-facing contracts.

Storage rows still keep their Cosmos `id` values internally. Helpers in this
module are for API/MCP/frontend surfaces where exposing backing IDs would make
the storage key part of the product contract.
"""

from __future__ import annotations


def issue_ref(project: str, number: int | None) -> str:
    if number is None:
        return project
    return f"{project}#{number}"


def run_ref(project: str, issue_number: int | None, run_display: str | int | None) -> str:
    run_part = str(run_display).strip() if run_display is not None else ""
    if issue_number is None:
        return f"{project}/runs/{run_part or 'unknown'}"
    return f"{project}#{issue_number}/runs/{run_part or 'unknown'}"


def report_ref(repo: str, number: int | None) -> str:
    if number is None:
        return repo
    return f"{repo}#{number}"


def lease_ref(project: str, *, slot_name: str | None = None, lease_number: int | None = None) -> str:
    if slot_name:
        return slot_name
    if lease_number is not None:
        return f"{project}/leases/{lease_number}"
    return f"{project}/lease"

"""One-shot migration from legacy `prs` docs to `reports`.

The migration is idempotent: each legacy PR doc becomes one canonical Report
and one initial ReportVersion (`<report_id>.0`). When a PR is linked to a
Glimmung Issue, the Report id is the Issue id; otherwise the legacy PR id is
preserved for manually mirrored GitHub PRs that do not have a canonical Issue.
"""

from __future__ import annotations

import asyncio
import logging
import os
import sys
from datetime import datetime
from typing import Any

from azure.cosmos.exceptions import CosmosResourceNotFoundError

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from glimmung.db import Cosmos, query_all  # noqa: E402
from glimmung.models import Report, ReportState, ReportVersion  # noqa: E402
from glimmung.settings import get_settings  # noqa: E402

log = logging.getLogger("glimmung.migrate_prs_to_reports")


def _strip_meta(doc: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in doc.items() if not k.startswith("_")}


def _report_state(doc: dict[str, Any]) -> ReportState:
    if doc.get("merged_at"):
        return ReportState.MERGED
    raw = str(doc.get("state") or "").lower()
    if raw == "closed":
        return ReportState.CLOSED
    if raw in {"ready", "needs_review", "failed", "merged"}:
        return ReportState(raw)
    return ReportState.READY


def _build_report(doc: dict[str, Any]) -> Report:
    clean = _strip_meta(doc)
    report_id = clean.get("linked_issue_id") or clean["id"]
    return Report(
        schema_version=1,
        id=report_id,
        project=clean["project"],
        repo=clean["repo"],
        number=int(clean["number"]),
        title=clean.get("title") or f"{clean['repo']}#{clean['number']}",
        body=clean.get("body") or "",
        state=_report_state(clean),
        branch=clean.get("branch") or "",
        base_ref=clean.get("base_ref") or "main",
        head_sha=clean.get("head_sha") or "",
        html_url=clean.get("html_url") or "",
        comments=clean.get("comments") or [],
        reviews=clean.get("reviews") or [],
        linked_issue_id=clean.get("linked_issue_id"),
        linked_run_id=clean.get("linked_run_id"),
        created_at=clean["created_at"],
        updated_at=clean.get("updated_at") or clean["created_at"],
        merged_at=clean.get("merged_at"),
        merged_by=clean.get("merged_by"),
    )


def _build_version(report: Report) -> ReportVersion:
    return ReportVersion(
        id=f"{report.id}.0",
        project=report.project,
        report_id=report.id,
        version=0,
        state=report.state,
        title=report.title,
        body=report.body,
        linked_run_id=report.linked_run_id,
        github_repo=report.repo or None,
        github_pr_number=report.number or None,
        github_html_url=report.html_url or None,
        created_at=report.updated_at if isinstance(report.updated_at, datetime) else report.created_at,
    )


async def _put(container: Any, *, item: str, partition_key: str, body: dict[str, Any]) -> str:
    try:
        await container.read_item(item=item, partition_key=partition_key)
    except CosmosResourceNotFoundError:
        await container.create_item(body)
        return "created"
    await container.replace_item(item=item, body=body)
    return "updated"


async def main() -> int:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    cosmos = Cosmos(get_settings())
    await cosmos.start()
    try:
        assert cosmos.legacy_prs is not None
        assert cosmos.reports is not None
        assert cosmos.report_versions is not None
        legacy_docs = await query_all(cosmos.legacy_prs, "SELECT * FROM c")
        counts = {"reports_created": 0, "reports_updated": 0, "versions_created": 0, "versions_updated": 0}
        for legacy in legacy_docs:
            report = _build_report(legacy)
            version = _build_version(report)
            report_outcome = await _put(
                cosmos.reports,
                item=report.id,
                partition_key=report.project,
                body=report.model_dump(mode="json"),
            )
            version_outcome = await _put(
                cosmos.report_versions,
                item=version.id,
                partition_key=version.project,
                body=version.model_dump(mode="json"),
            )
            counts[f"reports_{report_outcome}"] += 1
            counts[f"versions_{version_outcome}"] += 1
        log.info("migrated legacy prs to reports: %s", counts)
        return 0
    finally:
        await cosmos.stop()


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))

"""One-shot seed of glimmung's `reports` container from GitHub.

Glimmung is the system of record for issues — they are not seeded
from GH. PRs still pull from GH because the dashboards want the
historical view. The post-#50 webhook keeps ongoing PR activity in
sync, so this script's only job is the historical backfill.

Usage::

    az login  # so DefaultAzureCredential picks up your auth
    GLIMMUNG_GH_APP_ID=...
    GLIMMUNG_GH_APP_PRIVATE_KEY=...
    GLIMMUNG_GH_APP_INSTALLATION_ID=...
    python scripts/seed-from-github.py --project glimmung

Or in-cluster (where the workload identity is already wired):

    kubectl -n glimmung exec deploy/glimmung-api -- \\
        python -m scripts.seed_from_github --project glimmung

Steps per project:
    1. Pull every open + recently-merged PR. ensure_report_for_github each
       one (with the live GH-side title / body / branch / etc.).
    2. Best-effort populate `linked_issue_id` by parsing `Closes #N` /
       `Fixes #N` in seeded PR bodies + matching to existing glimmung
       Issues by their `metadata.github_issue_url`.
    3. Best-effort populate `linked_run_id` from existing `runs` records
       where `runs.pr_number` matches.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import re
import sys
from typing import Any

import httpx

# Make `glimmung.*` importable when running as `python scripts/seed-from-github.py`.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from glimmung import issues as issue_ops  # noqa: E402
from glimmung import reports as report_ops  # noqa: E402
from glimmung import runs as run_ops  # noqa: E402
from glimmung.db import Cosmos, query_all  # noqa: E402
from glimmung.github_app import GitHubAppTokenMinter  # noqa: E402
from glimmung.settings import get_settings  # noqa: E402

log = logging.getLogger("glimmung.seed")

_CLOSES_KEYWORDS_RE = re.compile(
    r"\b(?:closes|fixes|resolves)\s+#(\d+)\b", re.IGNORECASE,
)


async def list_all_prs(
    minter: GitHubAppTokenMinter, *, repo: str, per_page: int = 100,
) -> list[dict[str, Any]]:
    """GET /repos/{repo}/pulls?state=all, paginated."""
    token = await minter.installation_token()
    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    out: list[dict[str, Any]] = []
    page = 1
    async with httpx.AsyncClient(timeout=30.0) as client:
        while True:
            r = await client.get(
                f"https://api.github.com/repos/{repo}/pulls",
                headers=headers,
                params={
                    "state": "all", "per_page": per_page, "page": page,
                    "sort": "updated", "direction": "desc",
                },
            )
            r.raise_for_status()
            items = r.json() or []
            if not items:
                break
            out.extend(items)
            if len(items) < per_page:
                break
            page += 1
    return out


async def seed_prs(
    cosmos: Cosmos,
    minter: GitHubAppTokenMinter,
    *,
    project: str,
    repo: str,
) -> dict[str, int]:
    """Pull every open + closed PR. ensure_report_for_github each one with
    the live GH-side fields, then close/merge as appropriate based on
    GH's state.

    Returns a per-action counter. Linkage population is a separate step
    (`link_prs`) so it can be re-run independently."""
    log.info("seeding PRs from %s", repo)
    gh_prs = await list_all_prs(minter, repo=repo)
    log.info("  found %d PRs on GH", len(gh_prs))

    counts = {"created": 0, "merged": 0, "closed": 0, "reopened": 0, "unchanged": 0}
    for gh_pr in gh_prs:
        number = int(gh_pr["number"])
        title = gh_pr.get("title") or f"{repo}#{number}"
        body = gh_pr.get("body") or ""
        branch = ((gh_pr.get("head") or {}).get("ref") or "")
        base_ref = ((gh_pr.get("base") or {}).get("ref") or "main")
        head_sha = ((gh_pr.get("head") or {}).get("sha") or "")
        html_url = gh_pr.get("html_url") or ""

        pr, etag, created = await report_ops.ensure_report_for_github(
            cosmos, project=project, repo=repo, number=number,
            title=title, branch=branch, body=body,
            base_ref=base_ref, head_sha=head_sha, html_url=html_url,
        )
        if created:
            counts["created"] += 1
        else:
            # Refresh fields on existing PRs in case GH state has drifted.
            pr, etag = await report_ops.update_report(
                cosmos, pr=pr, etag=etag,
                title=title, body=body, branch=branch,
                base_ref=base_ref, head_sha=head_sha, html_url=html_url,
            )

        gh_state = gh_pr.get("state") or "open"
        merged_at = gh_pr.get("merged_at")
        merged_by = (gh_pr.get("merged_by") or {}).get("login") or "unknown"
        if merged_at:
            from datetime import datetime
            try:
                ts = datetime.fromisoformat(merged_at.replace("Z", "+00:00"))
            except ValueError:
                ts = None
            if pr.merged_at is None:
                pr, etag = await report_ops.merge_report(
                    cosmos, pr=pr, etag=etag, merged_by=merged_by, merged_at=ts,
                )
                counts["merged"] += 1
            else:
                counts["unchanged"] += 1
        elif gh_state == "closed":
            from glimmung.models import ReportState
            if pr.state == ReportState.READY:
                pr, etag = await report_ops.close_report(cosmos, pr=pr, etag=etag)
                counts["closed"] += 1
            else:
                counts["unchanged"] += 1
        else:
            from glimmung.models import ReportState
            if pr.state == ReportState.CLOSED and pr.merged_at is None:
                pr, etag = await report_ops.reopen_report(cosmos, pr=pr, etag=etag)
                counts["reopened"] += 1
            else:
                counts["unchanged"] += 1
    return counts


async def link_prs(
    cosmos: Cosmos, *, project: str, repo: str,
) -> dict[str, int]:
    """Best-effort populate `linked_issue_id` and `linked_run_id` on
    seeded PRs.

    `linked_issue_id`: parse `Closes #N` / `Fixes #N` / `Resolves #N`
    keywords in the PR body (case-insensitive), look up the matching
    Issue by `(repo, N)` via `find_issue_by_github_url`, attach the id.
    Multiple Closes refs land the first match deterministically (sorted).

    `linked_run_id`: query `runs` for the run with this PR's
    `(issue_repo, pr_number)` and attach its id.

    Both are skipped if the linkage is already set (idempotent re-run)."""
    log.info("linking seeded PRs in %s", repo)
    pr_docs = await query_all(
        cosmos.reports,
        "SELECT * FROM c WHERE c.project = @p AND c.repo = @r",
        parameters=[
            {"name": "@p", "value": project},
            {"name": "@r", "value": repo},
        ],
    )
    counts = {"linked_issue": 0, "linked_run": 0, "skipped": 0}
    for pr_doc in pr_docs:
        pr_id = pr_doc["id"]
        body = pr_doc.get("body") or ""
        existing_issue_link = pr_doc.get("linked_issue_id")
        existing_run_link = pr_doc.get("linked_run_id")

        new_issue_id: str | None = existing_issue_link
        new_run_id: str | None = existing_run_link

        if not existing_issue_link:
            for issue_n_str in sorted(set(_CLOSES_KEYWORDS_RE.findall(body))):
                issue_url = issue_ops.github_issue_url_for(repo, int(issue_n_str))
                found = await issue_ops.find_issue_by_github_url(
                    cosmos, github_issue_url=issue_url,
                )
                if found is not None:
                    new_issue_id = found[0].id
                    break

        if not existing_run_link:
            run_lookup = await run_ops.find_run_by_pr(
                cosmos, issue_repo=repo, pr_number=int(pr_doc["number"]),
            )
            if run_lookup is not None:
                new_run_id = run_lookup[0].id

        if new_issue_id == existing_issue_link and new_run_id == existing_run_link:
            counts["skipped"] += 1
            continue

        # Re-read for etag safety + apply.
        found = await report_ops.read_report(cosmos, project=project, report_id=pr_id)
        if found is None:
            continue
        pr_obj, etag = found
        await report_ops.update_report(
            cosmos, pr=pr_obj, etag=etag,
            linked_issue_id=new_issue_id,
            linked_run_id=new_run_id,
        )
        if new_issue_id and new_issue_id != existing_issue_link:
            counts["linked_issue"] += 1
        if new_run_id and new_run_id != existing_run_link:
            counts["linked_run"] += 1
    return counts


async def main() -> int:
    parser = argparse.ArgumentParser(
        description=(
            "One-shot seed of glimmung's reports container from the "
            "registered project's GitHub repo. Idempotent."
        ),
    )
    parser.add_argument(
        "--project", required=True,
        help="Glimmung project name (must be registered in `projects`).",
    )
    parser.add_argument(
        "--skip-reports", action="store_true",
        help="Skip the PRs import phase.",
    )
    parser.add_argument(
        "--skip-linking", action="store_true",
        help="Skip the linked_issue_id / linked_run_id backfill phase.",
    )
    args = parser.parse_args()

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    settings = get_settings()
    cosmos = Cosmos(settings)
    await cosmos.start()

    try:
        if not (settings.github_app_id and settings.github_app_private_key
                and settings.github_app_installation_id):
            print(
                "github app credentials not configured "
                "(GLIMMUNG_GH_APP_ID / GLIMMUNG_GH_APP_PRIVATE_KEY / "
                "GLIMMUNG_GH_APP_INSTALLATION_ID required)",
                file=sys.stderr,
            )
            return 2
        minter = GitHubAppTokenMinter(
            app_id=settings.github_app_id,
            installation_id=settings.github_app_installation_id,
            private_key=settings.github_app_private_key,
        )

        # Resolve the project's GH repo.
        try:
            project_doc = await cosmos.projects.read_item(
                item=args.project, partition_key=args.project,
            )
        except Exception as exc:
            print(f"project {args.project!r} not registered: {exc}", file=sys.stderr)
            return 3
        repo = project_doc.get("githubRepo") or ""
        if not repo:
            print(f"project {args.project!r} has no github_repo set", file=sys.stderr)
            return 3
        log.info("seeding project=%s repo=%s", args.project, repo)

        if not args.skip_reports:
            pr_counts = await seed_prs(
                cosmos, minter, project=args.project, repo=repo,
            )
            log.info("reports: %s", pr_counts)
        if not args.skip_linking:
            link_counts = await link_prs(
                cosmos, project=args.project, repo=repo,
            )
            log.info("linking: %s", link_counts)
        log.info("done")
        return 0
    finally:
        await cosmos.stop()


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))

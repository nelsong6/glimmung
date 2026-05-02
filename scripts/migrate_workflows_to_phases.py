"""One-shot migration: rewrite workflow docs from the pre-#69 trio shape
into the new phases/pr/budget shape.

Run this **after** glimmung's deploy of the #69 code lands and **before**
operator traffic resumes. The new code paths read `phases[]`; old docs
deserialize as `phases=[]` and dispatch will skip them.

Mapping rules:
  - `workflowFilename` → phases[0].workflowFilename
  - `workflowRef`      → phases[0].workflowRef
  - phases[0].name = "agent" (matches the existing GHA job name in
     ambience/spirelens/glimmung/tank-operator)
  - phases[0].kind = "gha_dispatch"
  - phases[0].verify = True (every existing workflow opted into the
     verify-loop substrate via retryWorkflowFilename)
  - phases[0].recyclePolicy = {maxAttempts: 3, on: ["verify_fail"], landsAt: "self"}
  - pr = {enabled: False, recyclePolicy: {maxAttempts: 3,
     on: ["pr_review_changes_requested"], landsAt: "agent"}} when the
     legacy `triageWorkflowFilename` is set; pr.enabled=False keeps the
     consumer in charge of `gh pr create` until each repo's YAML migrates.
  - budget.total = legacy defaultBudget.max_cost_usd (or 25.0 default)

Old fields (workflowFilename, workflowRef, retryWorkflowFilename,
triageWorkflowFilename, defaultBudget) are removed from the migrated
doc. Leftover camelCase keys at the top-level get dropped via the
field-allowlist in `_new_doc`.

Usage:
    PYTHONPATH=src python3 scripts/migrate_workflows_to_phases.py [--dry-run]

Auth: DefaultAzureCredential, same as glimmung's runtime. Run from the
glimmung pod or any environment with the relevant Azure identity.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import sys
from typing import Any

from azure.identity.aio import DefaultAzureCredential
from azure.cosmos.aio import CosmosClient

logging.basicConfig(level=logging.INFO, format="%(message)s")
log = logging.getLogger("migrate")

DEFAULT_ENDPOINT = "https://infra-cosmos-serverless.documents.azure.com:443/"
DEFAULT_DATABASE = "glimmung"
WORKFLOWS_CONTAINER = "workflows"


def _new_doc(old: dict[str, Any]) -> dict[str, Any]:
    """Translate one workflow doc. Returns the rewritten doc; caller
    upserts."""
    legacy_filename = old.get("workflowFilename") or old.get("workflow_filename") or ""
    legacy_ref = old.get("workflowRef") or old.get("workflow_ref") or "main"
    legacy_retry = old.get("retryWorkflowFilename") or old.get("retry_workflow_filename") or ""
    legacy_triage = old.get("triageWorkflowFilename") or old.get("triage_workflow_filename") or ""
    legacy_budget = old.get("defaultBudget") or old.get("default_budget") or {}

    # All four current consumers opt into the verify loop (retryWorkflowFilename
    # is set). Map verify=True + recycle on verify_fail.
    verify = bool(legacy_retry) or bool(legacy_filename)
    recycle = (
        {"maxAttempts": 3, "on": ["verify_fail"], "landsAt": "self"}
        if verify else None
    )

    pr = {"enabled": False, "recyclePolicy": None}
    if legacy_triage:
        pr["recyclePolicy"] = {
            "maxAttempts": 3,
            "on": ["pr_review_changes_requested"],
            "landsAt": "agent",
        }

    total = 25.0
    if isinstance(legacy_budget, dict):
        m = legacy_budget.get("max_cost_usd") or legacy_budget.get("maxCostUsd")
        if m is not None:
            total = float(m)

    new = {
        "id": old["id"],
        "name": old["name"],
        "project": old["project"],
        "phases": [{
            "name": "agent",
            "kind": "gha_dispatch",
            "workflowFilename": legacy_filename,
            "workflowRef": legacy_ref,
            "requirements": None,
            "verify": verify,
            "recyclePolicy": recycle,
        }],
        "pr": pr,
        "budget": {"total": total},
        "triggerLabel": old.get("triggerLabel") or old.get("trigger_label") or "issue-agent",
        "defaultRequirements": old.get("defaultRequirements") or old.get("default_requirements") or {},
        "metadata": old.get("metadata") or {},
        "createdAt": old.get("createdAt") or old.get("created_at"),
    }
    # Preserve Cosmos system fields the upsert needs (etag handled by
    # match_condition externally; _self/_rid/_attachments etc. don't need
    # to round-trip — Cosmos will recompute them).
    return new


async def migrate(*, endpoint: str, database: str, dry_run: bool) -> int:
    """Returns 0 on success, non-zero if any doc fails to translate.
    Upserts in place; on dry-run, just logs the transformation."""
    credential = DefaultAzureCredential()
    client = CosmosClient(endpoint, credential=credential)
    try:
        db = client.get_database_client(database)
        container = db.get_container_client(WORKFLOWS_CONTAINER)

        items: list[dict[str, Any]] = []
        async for it in container.query_items("SELECT * FROM c"):
            items.append(it)
        log.info("found %d workflow docs", len(items))

        bad = 0
        for old in items:
            project = old.get("project")
            name = old.get("name")
            try:
                new = _new_doc(old)
            except Exception as e:
                log.error("translation failed for %s/%s: %s", project, name, e)
                bad += 1
                continue
            if dry_run:
                log.info(
                    "DRY-RUN %s/%s: would write phases=[%s], pr=%s, budget=%s",
                    project, name,
                    [p["name"] for p in new["phases"]],
                    {"enabled": new["pr"]["enabled"], "has_recycle": new["pr"]["recyclePolicy"] is not None},
                    new["budget"],
                )
                continue
            await container.upsert_item(new)
            log.info("rewrote %s/%s", project, name)
        return bad
    finally:
        await client.close()
        await credential.close()


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("--endpoint", default=DEFAULT_ENDPOINT)
    p.add_argument("--database", default=DEFAULT_DATABASE)
    p.add_argument("--dry-run", action="store_true")
    args = p.parse_args()
    failures = asyncio.run(migrate(
        endpoint=args.endpoint, database=args.database, dry_run=args.dry_run,
    ))
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()

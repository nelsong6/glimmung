"""Glimmung Playbooks (#189).

Storage-only substrate for coordinated batches of issues. This slice
persists operator intent and exposes create/list/get surfaces; execution
semantics land separately so the schema can be reviewed before Glimmung
starts minting or dispatching issues from playbooks.
"""

from __future__ import annotations

import logging
from datetime import UTC, datetime
from typing import Any

from azure.core import MatchConditions
from azure.cosmos.exceptions import CosmosResourceNotFoundError
from ulid import ULID

from glimmung.db import Cosmos, query_all
from glimmung.models import Playbook, PlaybookCreate

log = logging.getLogger(__name__)


def _now() -> datetime:
    return datetime.now(UTC)


def _strip_meta(doc: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in doc.items() if not k.startswith("_")}


async def create_playbook(cosmos: Cosmos, req: PlaybookCreate) -> Playbook:
    now = _now()
    playbook = Playbook(
        id=str(ULID()),
        project=req.project,
        title=req.title,
        description=req.description,
        entries=req.entries,
        concurrency_limit=req.concurrency_limit,
        metadata=req.metadata,
        created_at=now,
        updated_at=now,
    )
    await cosmos.playbooks.create_item(playbook.model_dump(mode="json"))
    log.info(
        "created playbook %s in project=%s with %d entries",
        playbook.id,
        playbook.project,
        len(playbook.entries),
    )
    return playbook


async def read_playbook(
    cosmos: Cosmos,
    *,
    project: str,
    playbook_id: str,
) -> tuple[Playbook, str] | None:
    try:
        doc = await cosmos.playbooks.read_item(item=playbook_id, partition_key=project)
    except CosmosResourceNotFoundError:
        return None
    return Playbook.model_validate(_strip_meta(doc)), doc["_etag"]


async def list_playbooks(
    cosmos: Cosmos,
    *,
    project: str | None = None,
    state: str | None = None,
    limit: int | None = None,
) -> list[Playbook]:
    predicates: list[str] = []
    parameters: list[dict[str, Any]] = []
    if project:
        predicates.append("c.project = @p")
        parameters.append({"name": "@p", "value": project})
    if state:
        predicates.append("c.state = @s")
        parameters.append({"name": "@s", "value": state})
    where = f" WHERE {' AND '.join(predicates)}" if predicates else ""
    top = f"TOP {limit} " if limit is not None else ""
    docs = await query_all(
        cosmos.playbooks,
        f"SELECT {top}* FROM c{where} ORDER BY c.created_at DESC",
        parameters=parameters or None,
    )
    return [Playbook.model_validate(_strip_meta(d)) for d in docs]


async def replace_playbook(
    cosmos: Cosmos,
    *,
    playbook: Playbook,
    etag: str,
) -> tuple[Playbook, str]:
    playbook.updated_at = _now()
    response = await cosmos.playbooks.replace_item(
        item=playbook.id,
        body=playbook.model_dump(mode="json"),
        etag=etag,
        match_condition=MatchConditions.IfNotModified,
    )
    return playbook, response.get("_etag", etag)

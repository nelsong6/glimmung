from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from glimmung import playbooks as playbook_ops
from glimmung.app import (
    create_playbook_endpoint,
    get_playbook_endpoint,
    list_playbooks_endpoint,
)
from glimmung.models import PlaybookCreate, PlaybookEntry, PlaybookIssueSpec

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        projects=FakeContainer("projects", "/name"),
        playbooks=FakeContainer("playbooks", "/project"),
    )


async def _seed_project(cosmos, name: str = "glimmung") -> None:
    await cosmos.projects.create_item({
        "id": name,
        "name": name,
        "githubRepo": f"nelsong6/{name}",
        "metadata": {},
        "createdAt": datetime.now(UTC).isoformat(),
    })


def _entry(entry_id: str, *, depends_on: list[str] | None = None) -> PlaybookEntry:
    return PlaybookEntry(
        id=entry_id,
        issue=PlaybookIssueSpec(
            title=f"issue {entry_id}",
            body="do the thing",
            labels=["issue-agent"],
        ),
        depends_on=depends_on or [],
    )


@pytest.mark.asyncio
async def test_create_read_and_list_playbooks(cosmos):
    await _seed_project(cosmos)
    created = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="native rollout",
            description="stage the repo moves",
            entries=[_entry("one"), _entry("two", depends_on=["one"])],
            concurrency_limit=1,
            metadata={"source": "test"},
        ),
    )

    assert created.project == "glimmung"
    assert created.state.value == "draft"
    assert [entry.id for entry in created.entries] == ["one", "two"]
    assert created.entries[1].depends_on == ["one"]

    found = await playbook_ops.read_playbook(
        cosmos,
        project="glimmung",
        playbook_id=created.id,
    )
    assert found is not None
    read_back, etag = found
    assert etag
    assert read_back == created

    rows = await playbook_ops.list_playbooks(cosmos, project="glimmung")
    assert [row.id for row in rows] == [created.id]


@pytest.mark.asyncio
async def test_playbook_endpoints_validate_project_title_entries_and_dependencies(
    cosmos,
    monkeypatch,
):
    monkeypatch.setattr(
        "glimmung.app.app",
        SimpleNamespace(state=SimpleNamespace(cosmos=cosmos)),
    )

    with pytest.raises(HTTPException) as exc:
        await create_playbook_endpoint(
            PlaybookCreate(project="missing", title="x", entries=[]),
        )
    assert exc.value.status_code == 400

    await _seed_project(cosmos)
    with pytest.raises(HTTPException) as exc:
        await create_playbook_endpoint(
            PlaybookCreate(project="glimmung", title=" ", entries=[]),
        )
    assert exc.value.status_code == 400

    duplicate = [_entry("same"), _entry("same")]
    with pytest.raises(HTTPException) as exc:
        await create_playbook_endpoint(
            PlaybookCreate(project="glimmung", title="dup", entries=duplicate),
        )
    assert exc.value.status_code == 422

    with pytest.raises(HTTPException) as exc:
        await create_playbook_endpoint(
            PlaybookCreate(
                project="glimmung",
                title="bad dep",
                entries=[_entry("one", depends_on=["missing"])],
            ),
        )
    assert exc.value.status_code == 422

    with pytest.raises(HTTPException) as exc:
        await create_playbook_endpoint(
            PlaybookCreate(project="glimmung", title="bad limit", concurrency_limit=0),
        )
    assert exc.value.status_code == 422


@pytest.mark.asyncio
async def test_playbook_endpoints_create_list_get(cosmos, monkeypatch):
    await _seed_project(cosmos)
    monkeypatch.setattr(
        "glimmung.app.app",
        SimpleNamespace(state=SimpleNamespace(cosmos=cosmos)),
    )

    created = await create_playbook_endpoint(
        PlaybookCreate(
            project="glimmung",
            title="browser inspector rollout",
            entries=[_entry("inspector")],
        ),
    )

    listed = await list_playbooks_endpoint(project="glimmung")
    assert [p.id for p in listed] == [created.id]

    fetched = await get_playbook_endpoint(project="glimmung", playbook_id=created.id)
    assert fetched.id == created.id

    with pytest.raises(HTTPException) as exc:
        await get_playbook_endpoint(project="glimmung", playbook_id="missing")
    assert exc.value.status_code == 404

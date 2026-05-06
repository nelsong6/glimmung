from __future__ import annotations

import json
from datetime import UTC, datetime
from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from glimmung import playbooks as playbook_ops
from glimmung.app import (
    _advance_playbook,
    PlaybookEntryGateRequest,
    create_playbook_endpoint,
    get_playbook_endpoint,
    list_playbooks_endpoint,
    run_playbook_endpoint,
    set_playbook_entry_gate_endpoint,
)
from glimmung.dispatch import DispatchResult
from glimmung.models import (
    BudgetConfig,
    PhaseAttempt,
    PlaybookCreate,
    PlaybookEntry,
    PlaybookEntryState,
    PlaybookIntegrationStrategy,
    PlaybookIssueSpec,
    PlaybookState,
    Run,
    RunState,
)

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        projects=FakeContainer("projects", "/name"),
        playbooks=FakeContainer("playbooks", "/project"),
        issues=FakeContainer("issues", "/project"),
        runs=FakeContainer("runs", "/project"),
        locks=FakeContainer("locks", "/scope"),
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
    assert created.integration_strategy == PlaybookIntegrationStrategy.ISOLATED_PRS
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
async def test_playbook_integration_strategy_is_persisted(cosmos):
    await _seed_project(cosmos)
    created = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="shared branch feature",
            entries=[_entry("one")],
            integration_strategy=PlaybookIntegrationStrategy.SHARED_FEATURE_BRANCH,
        ),
    )

    assert created.integration_strategy == PlaybookIntegrationStrategy.SHARED_FEATURE_BRANCH
    found = await playbook_ops.read_playbook(
        cosmos,
        project="glimmung",
        playbook_id=created.id,
    )
    assert found is not None
    read_back, _etag = found
    assert read_back.integration_strategy == PlaybookIntegrationStrategy.SHARED_FEATURE_BRANCH


def test_rolling_main_playbooks_must_be_serial():
    with pytest.raises(ValueError, match="rolling_main playbooks must be serial"):
        PlaybookCreate(
            project="glimmung",
            title="parallel main writes",
            entries=[_entry("one")],
            integration_strategy=PlaybookIntegrationStrategy.ROLLING_MAIN,
            concurrency_limit=2,
        )


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


@pytest.mark.asyncio
async def test_advance_playbook_dispatches_ready_entries_up_to_limit(cosmos, monkeypatch):
    await _seed_project(cosmos)
    playbook = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="batch",
            entries=[_entry("one"), _entry("two")],
            concurrency_limit=1,
        ),
    )
    calls: list[str] = []

    async def fake_dispatch_run(app, **kwargs):
        calls.append(kwargs["issue_id"])
        return DispatchResult(state="pending", run_id="run-one")

    monkeypatch.setattr("glimmung.app.dispatch_run", fake_dispatch_run)
    app = SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))

    advanced = await _advance_playbook(app, playbook=playbook)

    assert advanced.state == PlaybookState.RUNNING
    assert calls == [advanced.entries[0].created_issue_id]
    assert advanced.entries[0].state == PlaybookEntryState.RUNNING
    assert advanced.entries[0].run_id == "run-one"
    assert advanced.entries[1].state == PlaybookEntryState.PENDING


@pytest.mark.asyncio
async def test_shared_branch_playbook_serializes_on_work_context_lock(cosmos, monkeypatch):
    await _seed_project(cosmos)
    playbook = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="shared",
            entries=[_entry("one"), _entry("two")],
            concurrency_limit=2,
            integration_strategy=PlaybookIntegrationStrategy.SHARED_FEATURE_BRANCH,
        ),
    )
    dispatch_metadata: list[dict[str, object]] = []

    async def fake_dispatch_run(app, **kwargs):
        dispatch_metadata.append(kwargs["extra_metadata"])
        return DispatchResult(state="pending", run_id=f"run-{len(dispatch_metadata)}")

    monkeypatch.setattr("glimmung.app.dispatch_run", fake_dispatch_run)
    app = SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))

    advanced = await _advance_playbook(app, playbook=playbook)

    assert advanced.state == PlaybookState.RUNNING
    assert len(dispatch_metadata) == 1
    branch = f"glimmung/playbooks/{advanced.id[:8]}"
    assert advanced.metadata["work_context"]["branch"] == branch
    assert dispatch_metadata[0]["work_context_branch"] == branch
    assert json.loads(str(dispatch_metadata[0]["work_context"]))["branch"] == branch

    first = advanced.entries[0]
    assert first.created_issue_id is not None
    issue = await cosmos.issues.read_item(
        item=first.created_issue_id,
        partition_key="glimmung",
    )
    assert issue["metadata"]["playbook_id"] == advanced.id
    assert issue["metadata"]["playbook_entry_id"] == first.id
    assert issue["metadata"]["playbook_integration_strategy"] == "shared_feature_branch"
    assert issue["metadata"]["work_context"]["branch"] == branch
    assert first.metadata["work_context"]["branch"] == branch
    assert first.metadata["work_context_lock_state"] == "held"

    second = advanced.entries[1]
    assert second.state == PlaybookEntryState.PENDING
    assert second.created_issue_id is None
    assert second.metadata["dispatch_state"] == "work_context_busy"
    assert second.metadata["work_context"]["branch"] == branch


@pytest.mark.asyncio
async def test_work_context_lock_release_allows_next_shared_branch_entry(cosmos, monkeypatch):
    await _seed_project(cosmos)
    playbook = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="shared",
            entries=[_entry("one"), _entry("two")],
            concurrency_limit=2,
            integration_strategy=PlaybookIntegrationStrategy.SHARED_FEATURE_BRANCH,
        ),
    )
    dispatch_run_ids = ["run-one", "run-two"]
    dispatched: list[str] = []

    async def fake_dispatch_run(app, **kwargs):
        dispatched.append(kwargs["issue_id"])
        return DispatchResult(state="pending", run_id=dispatch_run_ids[len(dispatched) - 1])

    monkeypatch.setattr("glimmung.app.dispatch_run", fake_dispatch_run)
    app = SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))

    advanced = await _advance_playbook(app, playbook=playbook)
    assert [entry.state for entry in advanced.entries] == [
        PlaybookEntryState.RUNNING,
        PlaybookEntryState.PENDING,
    ]

    now = datetime.now(UTC)
    await cosmos.runs.create_item(
        Run(
            id="run-one",
            project="glimmung",
            workflow="issue-agent",
            issue_id=advanced.entries[0].created_issue_id or "",
            issue_repo="",
            issue_number=0,
            state=RunState.PASSED,
            budget=BudgetConfig(total=25.0),
            attempts=[],
            created_at=now,
            updated_at=now,
        ).model_dump(mode="json")
    )

    advanced = await _advance_playbook(app, playbook=advanced)

    assert [entry.state for entry in advanced.entries] == [
        PlaybookEntryState.SUCCEEDED,
        PlaybookEntryState.RUNNING,
    ]
    assert advanced.entries[0].metadata["work_context_lock_state"] == "released"
    assert advanced.entries[1].run_id == "run-two"
    assert advanced.metadata["work_context"]["state"] == "in_use"
    assert len(dispatched) == 2


@pytest.mark.asyncio
async def test_rolling_main_playbook_stamps_main_work_context(cosmos, monkeypatch):
    await _seed_project(cosmos)
    playbook = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="rolling",
            entries=[_entry("one")],
            integration_strategy=PlaybookIntegrationStrategy.ROLLING_MAIN,
            concurrency_limit=1,
        ),
    )
    captured: dict[str, object] = {}

    async def fake_dispatch_run(app, **kwargs):
        captured.update(kwargs["extra_metadata"])
        return DispatchResult(state="pending", run_id="run-main")

    monkeypatch.setattr("glimmung.app.dispatch_run", fake_dispatch_run)
    app = SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))

    advanced = await _advance_playbook(app, playbook=playbook)

    assert advanced.entries[0].state == PlaybookEntryState.RUNNING
    assert captured["work_context_branch"] == "main"
    assert captured["work_context_base_ref"] == "main"
    assert json.loads(str(captured["work_context"]))["strategy"] == "rolling_main"
    assert advanced.entries[0].metadata["work_context"]["id"] == "project:glimmung:main:main"


@pytest.mark.asyncio
async def test_rolling_main_playbooks_share_project_main_lock(cosmos, monkeypatch):
    await _seed_project(cosmos)
    first = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="rolling one",
            entries=[_entry("one")],
            integration_strategy=PlaybookIntegrationStrategy.ROLLING_MAIN,
            concurrency_limit=1,
        ),
    )
    second = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="rolling two",
            entries=[_entry("two")],
            integration_strategy=PlaybookIntegrationStrategy.ROLLING_MAIN,
            concurrency_limit=1,
        ),
    )
    dispatches: list[str] = []

    async def fake_dispatch_run(app, **kwargs):
        dispatches.append(kwargs["issue_id"])
        return DispatchResult(state="pending", run_id=f"run-{len(dispatches)}")

    monkeypatch.setattr("glimmung.app.dispatch_run", fake_dispatch_run)
    app = SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))

    first = await _advance_playbook(app, playbook=first)
    second = await _advance_playbook(app, playbook=second)

    assert first.entries[0].state == PlaybookEntryState.RUNNING
    assert first.entries[0].metadata["work_context"]["id"] == "project:glimmung:main:main"
    assert second.entries[0].state == PlaybookEntryState.PENDING
    assert second.entries[0].created_issue_id is None
    assert second.entries[0].metadata["dispatch_state"] == "work_context_busy"
    assert second.entries[0].metadata["work_context"]["id"] == "project:glimmung:main:main"
    assert len(dispatches) == 1


@pytest.mark.asyncio
async def test_advance_playbook_starts_dependencies_after_prior_passes(cosmos, monkeypatch):
    await _seed_project(cosmos)
    now = datetime.now(UTC)
    prior = Run(
        id="run-one",
        project="glimmung",
        workflow="agent-run",
        issue_id="issue-one",
        issue_repo="",
        issue_number=0,
        state=RunState.PASSED,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0,
                phase="agent",
                workflow_filename="native:agent",
                dispatched_at=now,
                completed_at=now,
                conclusion="success",
            ),
        ],
        created_at=now,
        updated_at=now,
    )
    await cosmos.runs.create_item(prior.model_dump(mode="json"))
    playbook = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="batch",
            entries=[_entry("one"), _entry("two", depends_on=["one"])],
            concurrency_limit=1,
        ),
    )
    playbook.entries[0].state = PlaybookEntryState.RUNNING
    playbook.entries[0].created_issue_id = "issue-one"
    playbook.entries[0].run_id = "run-one"

    async def fake_dispatch_run(app, **kwargs):
        return DispatchResult(state="dispatched", run_id="run-two", host="native-k8s")

    monkeypatch.setattr("glimmung.app.dispatch_run", fake_dispatch_run)
    app = SimpleNamespace(state=SimpleNamespace(cosmos=cosmos))

    advanced = await _advance_playbook(app, playbook=playbook)

    assert advanced.state == PlaybookState.RUNNING
    assert advanced.entries[0].state == PlaybookEntryState.SUCCEEDED
    assert advanced.entries[1].state == PlaybookEntryState.RUNNING
    assert advanced.entries[1].run_id == "run-two"


@pytest.mark.asyncio
async def test_run_playbook_endpoint_404s_on_missing(cosmos, monkeypatch):
    monkeypatch.setattr(
        "glimmung.app.app",
        SimpleNamespace(state=SimpleNamespace(cosmos=cosmos)),
    )
    with pytest.raises(HTTPException) as exc:
        await run_playbook_endpoint(project="glimmung", playbook_id="missing")
    assert exc.value.status_code == 404


@pytest.mark.asyncio
async def test_playbook_gate_endpoint_clears_gate_and_advances(cosmos, monkeypatch):
    await _seed_project(cosmos)
    playbook = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="gated batch",
            entries=[
                PlaybookEntry(
                    id="review",
                    issue=PlaybookIssueSpec(title="review", body="check it"),
                    manual_gate=True,
                )
            ],
        ),
    )
    calls: list[str] = []

    async def fake_dispatch_run(app, **kwargs):
        calls.append(kwargs["issue_id"])
        return DispatchResult(state="dispatched", run_id="run-review")

    monkeypatch.setattr("glimmung.app.dispatch_run", fake_dispatch_run)
    monkeypatch.setattr(
        "glimmung.app.app",
        SimpleNamespace(state=SimpleNamespace(cosmos=cosmos)),
    )

    advanced = await set_playbook_entry_gate_endpoint(
        PlaybookEntryGateRequest(manual_gate=False),
        project="glimmung",
        playbook_id=playbook.id,
        entry_id="review",
    )

    assert advanced.state == PlaybookState.RUNNING
    assert advanced.entries[0].manual_gate is False
    assert advanced.entries[0].state == PlaybookEntryState.RUNNING
    assert advanced.entries[0].run_id == "run-review"
    assert calls == [advanced.entries[0].created_issue_id]


@pytest.mark.asyncio
async def test_playbook_gate_endpoint_can_set_gate_without_advancing(cosmos, monkeypatch):
    await _seed_project(cosmos)
    playbook = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(
            project="glimmung",
            title="set gate",
            entries=[_entry("one")],
        ),
    )
    monkeypatch.setattr(
        "glimmung.app.app",
        SimpleNamespace(state=SimpleNamespace(cosmos=cosmos)),
    )

    updated = await set_playbook_entry_gate_endpoint(
        PlaybookEntryGateRequest(manual_gate=True, advance=False),
        project="glimmung",
        playbook_id=playbook.id,
        entry_id="one",
    )

    assert updated.entries[0].manual_gate is True
    assert updated.entries[0].state == PlaybookEntryState.PENDING
    assert updated.state == PlaybookState.DRAFT


@pytest.mark.asyncio
async def test_playbook_gate_endpoint_404s_for_missing_entry(cosmos, monkeypatch):
    await _seed_project(cosmos)
    playbook = await playbook_ops.create_playbook(
        cosmos,
        PlaybookCreate(project="glimmung", title="missing entry", entries=[_entry("one")]),
    )
    monkeypatch.setattr(
        "glimmung.app.app",
        SimpleNamespace(state=SimpleNamespace(cosmos=cosmos)),
    )

    with pytest.raises(HTTPException) as exc:
        await set_playbook_entry_gate_endpoint(
            PlaybookEntryGateRequest(),
            project="glimmung",
            playbook_id=playbook.id,
            entry_id="missing",
        )
    assert exc.value.status_code == 404

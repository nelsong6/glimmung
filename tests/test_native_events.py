from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

import pytest
from fastapi import HTTPException
from starlette.requests import Request

from glimmung.app import (
    NativeRunCompletedRequest,
    NativeRunEventRequest,
    _attempt_token_sha256,
    native_run_completed,
    native_run_event,
)
from glimmung.models import (
    BudgetConfig,
    NativeJobSpec,
    NativeRunEventType,
    NativeStepSpec,
    PhaseAttempt,
    Run,
    RunState,
    VerificationStatus,
    native_job_attempts_from_specs,
)

from tests.cosmos_fake import FakeContainer


@pytest.fixture
def cosmos():
    return SimpleNamespace(
        projects=FakeContainer("projects", "/name"),
        workflows=FakeContainer("workflows", "/project"),
        hosts=FakeContainer("hosts", "/name"),
        leases=FakeContainer("leases", "/project"),
        runs=FakeContainer("runs", "/project"),
        run_events=FakeContainer("run_events", "/project"),
        locks=FakeContainer("locks", "/scope"),
        issues=FakeContainer("issues", "/project"),
        reports=FakeContainer("reports", "/project"),
        report_versions=FakeContainer("report_versions", "/project"),
    )


def _app_with(cosmos):
    return SimpleNamespace(state=SimpleNamespace(cosmos=cosmos, gh_minter=None, settings=None))


def _request(token: str | None = None) -> Request:
    headers = []
    if token is not None:
        headers.append((b"x-glimmung-attempt-token", token.encode("utf-8")))
    return Request({"type": "http", "headers": headers})


def _native_jobs():
    return native_job_attempts_from_specs([
        NativeJobSpec(
            id="agent",
            image="runner:latest",
            steps=[
                NativeStepSpec(slug="clone-repo"),
                NativeStepSpec(slug="run-agent"),
            ],
        )
    ])


async def _seed_native_run(cosmos) -> Run:
    now = datetime.now(UTC)
    await cosmos.workflows.create_item({
        "id": "native-agent",
        "name": "native-agent",
        "project": "ambience",
        "phases": [{
            "name": "agent-execute",
            "kind": "k8s_job",
            "workflowFilename": "",
            "workflowRef": "main",
            "requirements": None,
            "verify": True,
            "recyclePolicy": None,
            "inputs": {},
            "outputs": [],
            "jobs": [{
                "id": "agent",
                "name": None,
                "image": "runner:latest",
                "command": [],
                "args": [],
                "env": {},
                "steps": [
                    {"slug": "clone-repo", "title": None},
                    {"slug": "run-agent", "title": None},
                ],
                "timeoutSeconds": None,
            }],
        }],
        "pr": {"enabled": False, "recyclePolicy": None},
        "budget": {"total": 25.0},
        "triggerLabel": "agent-run",
        "defaultRequirements": {},
        "metadata": {},
        "createdAt": now.isoformat(),
    })
    run = Run(
        id="01KRNATIVE0000000000000000",
        project="ambience",
        workflow="native-agent",
        issue_id="01KRISSUE0000000000000000",
        issue_repo="nelsong6/ambience",
        issue_number=117,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0,
                phase="agent-execute",
                phase_kind="k8s_job",
                workflow_filename="k8s_job:agent-execute",
                dispatched_at=now,
                jobs=_native_jobs(),
            )
        ],
        cumulative_cost_usd=0.0,
        created_at=now,
        updated_at=now,
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))
    return run


@pytest.mark.asyncio
async def test_native_step_events_update_run_and_persist_ordered_logs(cosmos, monkeypatch):
    run = await _seed_native_run(cosmos)
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    await native_run_event(
        NativeRunEventRequest(
            job_id="agent",
            seq=1,
            event=NativeRunEventType.STEP_STARTED,
            step_slug="clone-repo",
        ),
        request=_request(),
        project=run.project,
        run_id=run.id,
    )
    await native_run_event(
        NativeRunEventRequest(
            job_id="agent",
            seq=2,
            event=NativeRunEventType.LOG,
            step_slug="clone-repo",
            message="cloned main",
        ),
        request=_request(),
        project=run.project,
        run_id=run.id,
    )
    await native_run_event(
        NativeRunEventRequest(
            job_id="agent",
            seq=3,
            event=NativeRunEventType.STEP_COMPLETED,
            step_slug="clone-repo",
            exit_code=0,
        ),
        request=_request(),
        project=run.project,
        run_id=run.id,
    )

    doc = await cosmos.runs.read_item(item=run.id, partition_key=run.project)
    attempt = doc["attempts"][0]
    assert attempt["jobs"][0]["last_seq"] == 3
    assert attempt["jobs"][0]["steps"][0]["state"] == "succeeded"
    log_doc = await cosmos.run_events.read_item(
        item=f"{run.id}::agent::000000000002",
        partition_key=run.project,
    )
    assert log_doc["message"] == "cloned main"


@pytest.mark.asyncio
async def test_native_completion_requires_no_sequence_gaps(cosmos, monkeypatch):
    run = await _seed_native_run(cosmos)
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))

    for seq, step in ((1, "clone-repo"), (3, "run-agent")):
        await native_run_event(
            NativeRunEventRequest(
                job_id="agent",
                seq=seq,
                event=NativeRunEventType.STEP_STARTED,
                step_slug=step,
            ),
            request=_request(),
            project=run.project,
            run_id=run.id,
        )
        await native_run_event(
            NativeRunEventRequest(
                job_id="agent",
                seq=seq + 10,
                event=NativeRunEventType.STEP_COMPLETED,
                step_slug=step,
                exit_code=0,
            ),
            request=_request(),
            project=run.project,
            run_id=run.id,
        )

    with pytest.raises(HTTPException) as excinfo:
        await native_run_completed(
            NativeRunCompletedRequest(
                verification={
                    "schema_version": 1,
                    "status": VerificationStatus.PASS.value,
                    "reasons": [],
                    "evidence_refs": [],
                    "cost_usd": 0.01,
                },
            ),
            request=_request(),
            project=run.project,
            run_id=run.id,
        )
    assert excinfo.value.status_code == 409
    assert "event sequence has gaps" in excinfo.value.detail


@pytest.mark.asyncio
async def test_native_completion_drives_decision_and_marks_run_passed(cosmos, monkeypatch):
    run = await _seed_native_run(cosmos)
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))
    await cosmos.leases.create_item({
        "id": "01LEASE",
        "project": run.project,
        "workflow": run.workflow,
        "host": "native-k8s",
        "state": "active",
        "requirements": {},
        "metadata": {
            "native_k8s": True,
            "run_id": run.id,
            "attempt_index": "0",
        },
        "requestedAt": datetime.now(UTC).isoformat(),
        "assignedAt": datetime.now(UTC).isoformat(),
        "releasedAt": None,
        "ttlSeconds": 14400,
    })

    seq = 1
    for step in ("clone-repo", "run-agent"):
        await native_run_event(
            NativeRunEventRequest(
                job_id="agent",
                seq=seq,
                event=NativeRunEventType.STEP_STARTED,
                step_slug=step,
            ),
            request=_request(),
            project=run.project,
            run_id=run.id,
        )
        seq += 1
        await native_run_event(
            NativeRunEventRequest(
                job_id="agent",
                seq=seq,
                event=NativeRunEventType.STEP_COMPLETED,
                step_slug=step,
                exit_code=0,
            ),
            request=_request(),
            project=run.project,
            run_id=run.id,
        )
        seq += 1

    result = await native_run_completed(
        NativeRunCompletedRequest(
            verification={
                "schema_version": 1,
                "status": VerificationStatus.PASS.value,
                "reasons": [],
                "evidence_refs": ["blob://evidence/native-run"],
                "cost_usd": 0.05,
            },
        ),
        request=_request(),
        project=run.project,
        run_id=run.id,
    )

    assert result.decision == "advance"
    doc = await cosmos.runs.read_item(item=run.id, partition_key=run.project)
    assert doc["state"] == "passed"
    assert doc["attempts"][0]["workflow_run_id"] is None
    assert doc["attempts"][0]["verification"]["status"] == "pass"
    lease = await cosmos.leases.read_item(item="01LEASE", partition_key=run.project)
    assert lease["state"] == "released"


@pytest.mark.asyncio
async def test_native_callbacks_require_bound_attempt_token(cosmos, monkeypatch):
    run = await _seed_native_run(cosmos)
    monkeypatch.setattr("glimmung.app.app", _app_with(cosmos))
    doc = await cosmos.runs.read_item(item=run.id, partition_key=run.project)
    doc["attempts"][0]["capability_token_sha256"] = _attempt_token_sha256("secret-token")
    await cosmos.runs.replace_item(item=run.id, body=doc)

    req = NativeRunEventRequest(
        job_id="agent",
        seq=1,
        event=NativeRunEventType.STEP_STARTED,
        step_slug="clone-repo",
    )
    with pytest.raises(HTTPException) as missing:
        await native_run_event(
            req,
            request=_request(),
            project=run.project,
            run_id=run.id,
        )
    assert missing.value.status_code == 401

    with pytest.raises(HTTPException) as wrong:
        await native_run_event(
            req,
            request=_request("wrong-token"),
            project=run.project,
            run_id=run.id,
        )
    assert wrong.value.status_code == 403

    result = await native_run_event(
        req,
        request=_request("secret-token"),
        project=run.project,
        run_id=run.id,
    )
    assert result.accepted is True

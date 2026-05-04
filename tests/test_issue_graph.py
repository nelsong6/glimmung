"""Issue lineage graph endpoint (#42) — coverage for the post-resume
metadata surface (#111).

Test surface that didn't previously exist; new tests cover both the
classic case (no resume involved) and the resume case (cloned_from
edges, skipped attempt rendering, entrypoint_phase metadata) so the
dashboard's resumed-Run rendering has a stable contract to consume.
"""

from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from fastapi import HTTPException

from glimmung.app import _build_issue_graph, _build_system_graph, issue_graph
from glimmung.models import (
    BudgetConfig,
    NativeJobAttempt,
    NativeStepAttempt,
    NativeStepState,
    PhaseAttempt,
    Report,
    ReportState,
    Run,
    RunState,
    Signal,
    SignalSource,
    SignalState,
    SignalTargetType,
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
        locks=FakeContainer("locks", "/scope"),
        signals=FakeContainer("signals", "/target_repo"),
        issues=FakeContainer("issues", "/project"),
        reports=FakeContainer("reports", "/project"),
    )


@pytest.fixture
def app_state(cosmos):
    state = SimpleNamespace(cosmos=cosmos, settings=None, gh_minter=None)
    return SimpleNamespace(state=state)


async def _seed_issue(
    cosmos,
    *,
    project: str = "ambience",
    repo: str = "nelsong6/ambience",
    issue_number: int = 116,
    issue_id: str = "01HZGRAPHTESTISSUE",
    title: str = "Effect: Lava lamp",
) -> str:
    now = datetime.now(UTC).isoformat()
    await cosmos.issues.create_item({
        "id": issue_id,
        "project": project,
        "title": title,
        "body": "",
        "labels": [],
        "state": "open",
        "metadata": {
            "source": "github_webhook_import",
            "github_issue_url": f"https://github.com/{repo}/issues/{issue_number}",
            "github_issue_repo": repo,
            "github_issue_number": issue_number,
        },
        "created_at": now,
        "updated_at": now,
    })
    return issue_id


def _now() -> datetime:
    return datetime.now(UTC)


def _native_job() -> NativeJobAttempt:
    return NativeJobAttempt(
        job_id="agent",
        name="Agent",
        state=NativeStepState.ACTIVE,
        steps=[
            NativeStepAttempt(
                slug="clone",
                title="Clone repo",
                state=NativeStepState.SUCCEEDED,
                exit_code=0,
            ),
            NativeStepAttempt(
                slug="edit",
                title="Edit files",
                state=NativeStepState.ACTIVE,
            ),
        ],
    )


async def _seed_run(cosmos, run: Run) -> None:
    await cosmos.runs.create_item(run.model_dump(mode="json"))


async def _seed_pr(cosmos, pr: Report) -> None:
    await cosmos.reports.create_item(pr.model_dump(mode="json"))


async def _seed_signal(cosmos, signal: Signal) -> None:
    await cosmos.signals.create_item(signal.model_dump(mode="json"))


# ─── Classic graph (no resume involved) ───────────────────────────────────


@pytest.mark.asyncio
async def test_system_graph_renders_open_issues_inflight_runs_prs_and_signals(cosmos):
    issue_id = await _seed_issue(cosmos)
    await _seed_issue(
        cosmos,
        project="ambience",
        repo="nelsong6/ambience",
        issue_number=117,
        issue_id="01HZGRAPHOTHERISSUE",
        title="Effect: Cottage smoke",
    )
    now = _now()
    run = Run(
        id="01KQSYSTEM_RUN",
        project="ambience",
        workflow="agent-run",
        issue_id=issue_id,
        issue_repo="nelsong6/ambience",
        issue_number=116,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0,
                phase="env-prep",
                workflow_filename="env-prep.yml",
                dispatched_at=now,
                completed_at=now,
                conclusion="success",
            ),
            PhaseAttempt(
                attempt_index=1,
                phase="agent-execute",
                phase_kind="k8s_job",
                workflow_filename="agent-execute.yml",
                dispatched_at=now,
                jobs=[_native_job()],
                log_archive_url="blob://artifacts/runs/ambience/01KQSYSTEM_RUN/attempts/1/native-events.json",
            ),
        ],
        created_at=now,
        updated_at=now,
    )
    await _seed_run(cosmos, run)
    await _seed_pr(cosmos, Report(
        id="01KQSYSTEM_PR",
        project="ambience",
        repo="nelsong6/ambience",
        number=201,
        title="agent: lava lamp",
        state=ReportState.READY,
        branch="agent/lava",
        linked_issue_id=issue_id,
        linked_run_id=run.id,
        created_at=now,
        updated_at=now,
    ))
    await _seed_signal(cosmos, Signal(
        id="01KQSYSTEM_SIGNAL",
        target_type=SignalTargetType.RUN,
        target_repo="ambience",
        target_id=run.id,
        source=SignalSource.GLIMMUNG_UI,
        payload={"reason": "operator feedback"},
        state=SignalState.PENDING,
        enqueued_at=now,
    ))

    graph = await _build_system_graph(cosmos)

    node_kinds = sorted(n.kind for n in graph.nodes)
    assert node_kinds.count("issue") == 2
    assert "run" in node_kinds
    assert "attempt" in node_kinds
    assert "pr" in node_kinds
    assert "signal" in node_kinds
    assert graph.issue_id == "system"
    assert any(e.kind == "spawned" and e.source == f"issue:{issue_id}" for e in graph.edges)
    assert any(e.kind == "opened" and e.source == f"run:{run.id}" for e in graph.edges)
    assert any(e.kind == "feedback" and e.source == f"run:{run.id}" for e in graph.edges)
    native_attempt = next(n for n in graph.nodes if n.id == f"attempt:{run.id}:1")
    assert native_attempt.metadata["phase_kind"] == "k8s_job"
    assert native_attempt.metadata["jobs_count"] == 1
    assert native_attempt.metadata["steps_count"] == 2
    assert native_attempt.metadata["jobs"][0]["steps"][0]["slug"] == "clone"
    assert native_attempt.metadata["log_archive_url"].endswith("/native-events.json")


@pytest.mark.asyncio
async def test_system_graph_filters_by_project(cosmos):
    ambience_issue_id = await _seed_issue(cosmos, project="ambience")
    await _seed_issue(
        cosmos,
        project="glimmung",
        repo="nelsong6/glimmung",
        issue_number=41,
        issue_id="01HZGRAPHGLIMMUNG",
        title="Report reviews",
    )
    now = _now()
    await _seed_run(cosmos, Run(
        id="01KQSYSTEM_AMBIENCE_RUN",
        project="ambience",
        workflow="agent-run",
        issue_id=ambience_issue_id,
        issue_repo="nelsong6/ambience",
        issue_number=116,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[],
        created_at=now,
        updated_at=now,
    ))

    graph = await _build_system_graph(cosmos, project="glimmung")

    assert [n.metadata["project"] for n in graph.nodes if n.kind == "issue"] == ["glimmung"]
    assert all(n.kind != "run" for n in graph.nodes)


@pytest.mark.asyncio
async def test_graph_renders_issue_run_attempts(cosmos, app_state):
    """Smoke-test of the basic shape: one issue, one run with two
    attempts, edges issue→run→attempt0→attempt1."""
    issue_id = await _seed_issue(cosmos)
    now = _now()
    run = Run(
        id="01KQGRAPH_RUN_AAA",
        project="ambience",
        workflow="agent-run",
        issue_id=issue_id,
        issue_repo="nelsong6/ambience",
        issue_number=116,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0,
                phase="env-prep",
                workflow_filename="env-prep.yml",
                dispatched_at=now, completed_at=now, conclusion="success",
                phase_outputs={"validation_url": "https://x.romaine.life"},
            ),
            PhaseAttempt(
                attempt_index=1,
                phase="agent-execute",
                phase_kind="k8s_job",
                workflow_filename="agent-execute.yml",
                dispatched_at=now,
                jobs=[_native_job()],
                log_archive_url="blob://artifacts/runs/ambience/01KQGRAPH_RUN_AAA/attempts/1/native-events.json",
            ),
        ],
        created_at=now, updated_at=now,
    )
    await _seed_run(cosmos, run)

    graph = await _build_issue_graph(
        cosmos, repo="nelsong6/ambience", issue_number=116,
    )
    kinds = sorted({n.kind for n in graph.nodes})
    assert kinds == ["attempt", "issue", "run"]
    edge_kinds = sorted(e.kind for e in graph.edges)
    assert edge_kinds == ["attempted", "retried", "spawned"]

    run_node = next(n for n in graph.nodes if n.kind == "run")
    # Resume metadata is None on a non-resumed Run — the dashboard
    # branches on these to decide whether to render the lineage arrow.
    assert run_node.metadata["cloned_from_run_id"] is None
    assert run_node.metadata["entrypoint_phase"] is None

    native_attempt = next(n for n in graph.nodes if n.id == f"attempt:{run.id}:1")
    assert native_attempt.metadata["phase_kind"] == "k8s_job"
    assert native_attempt.metadata["jobs_count"] == 1
    assert native_attempt.metadata["steps_count"] == 2
    assert native_attempt.metadata["jobs"][0]["state"] == "active"
    assert native_attempt.metadata["log_archive_url"].endswith("/native-events.json")


@pytest.mark.asyncio
async def test_graph_renders_report_terminal_node_for_run(cosmos, app_state):
    issue_id = await _seed_issue(cosmos)
    now = _now()
    run = Run(
        id="01KQGRAPH_REPORT_RUN",
        project="ambience",
        workflow="agent-run",
        issue_id=issue_id,
        issue_repo="nelsong6/ambience",
        issue_number=116,
        state=RunState.PASSED,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0,
                phase="agent-execute",
                workflow_filename="agent-execute.yml",
                dispatched_at=now,
                completed_at=now,
                conclusion="success",
            ),
        ],
        pr_number=42,
        pr_branch="glimmung/01KQGRAPH_REPORT_RUN",
        created_at=now,
        updated_at=now,
    )
    await _seed_run(cosmos, run)
    report = Report(
        id="01KQREPORTNODE",
        project="ambience",
        repo="nelsong6/ambience",
        number=42,
        title="Report terminal",
        branch="glimmung/01KQGRAPH_REPORT_RUN",
        head_sha="abc123",
        html_url="https://github.com/nelsong6/ambience/pull/42",
        linked_issue_id=issue_id,
        linked_run_id=run.id,
        state=ReportState.READY,
        created_at=now,
        updated_at=now,
    )
    await _seed_pr(cosmos, report)

    graph = await _build_issue_graph(
        cosmos, repo="nelsong6/ambience", issue_number=116,
    )

    report_node = next(n for n in graph.nodes if n.id == "pr:01KQREPORTNODE")
    assert report_node.kind == "pr"
    assert report_node.label == "Report #42"
    assert report_node.metadata["report_id"] == "01KQREPORTNODE"
    run_node = next(n for n in graph.nodes if n.id == f"run:{run.id}")
    assert run_node.metadata["report_id"] == "01KQREPORTNODE"
    assert run_node.metadata["report_state"] == "ready"
    assert any(
        e.source == f"run:{run.id}"
        and e.target == "pr:01KQREPORTNODE"
        and e.kind == "opened"
        for e in graph.edges
    )


# ─── Resume case ──────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_graph_surfaces_resume_lineage(cosmos, app_state):
    """A prior aborted Run + a resumed Run on the same issue should
    render with: cloned_from_run_id metadata on the new Run, a
    `resumed_from` edge, and skipped_from_run_id on the synthesized
    attempt with state="skipped"."""
    issue_id = await _seed_issue(cosmos)
    now = _now()

    prior = Run(
        id="01KQGRAPH_PRIOR",
        project="ambience",
        workflow="agent-run",
        issue_id=issue_id,
        issue_repo="nelsong6/ambience",
        issue_number=116,
        state=RunState.ABORTED,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0, phase="env-prep",
                workflow_filename="env-prep.yml",
                dispatched_at=now, completed_at=now, conclusion="success",
                phase_outputs={"validation_url": "https://abc.romaine.life"},
            ),
            PhaseAttempt(
                attempt_index=1, phase="agent-execute",
                workflow_filename="agent-execute.yml",
                dispatched_at=now, completed_at=now, conclusion="success",
                decision="abort_malformed",
            ),
        ],
        abort_reason="verify=true mismatch",
        created_at=now, updated_at=now,
    )
    resumed = Run(
        id="01KQGRAPH_RESUMED",
        project="ambience",
        workflow="agent-run",
        issue_id=issue_id,
        issue_repo="nelsong6/ambience",
        issue_number=116,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0, phase="env-prep",
                workflow_filename="env-prep.yml",
                dispatched_at=now, completed_at=now, conclusion="success",
                phase_outputs={"validation_url": "https://abc.romaine.life"},
                skipped_from_run_id=prior.id,
            ),
            PhaseAttempt(
                attempt_index=1, phase="agent-execute",
                workflow_filename="agent-execute.yml",
                dispatched_at=now,
            ),
        ],
        cloned_from_run_id=prior.id,
        entrypoint_phase="agent-execute",
        created_at=now, updated_at=now,
    )
    await _seed_run(cosmos, prior)
    await _seed_run(cosmos, resumed)

    graph = await _build_issue_graph(
        cosmos, repo="nelsong6/ambience", issue_number=116,
    )

    # Resumed Run carries cloned_from + entrypoint_phase metadata.
    resumed_node = next(
        n for n in graph.nodes if n.id == f"run:{resumed.id}"
    )
    assert resumed_node.metadata["cloned_from_run_id"] == prior.id
    assert resumed_node.metadata["entrypoint_phase"] == "agent-execute"

    # Prior Run renders with no resume metadata.
    prior_node = next(n for n in graph.nodes if n.id == f"run:{prior.id}")
    assert prior_node.metadata["cloned_from_run_id"] is None

    # Lineage edge prior → resumed exists with kind="resumed_from".
    lineage_edges = [e for e in graph.edges if e.kind == "resumed_from"]
    assert len(lineage_edges) == 1
    assert lineage_edges[0].source == f"run:{prior.id}"
    assert lineage_edges[0].target == f"run:{resumed.id}"

    # Skipped attempt: state="skipped", metadata.skipped_from_run_id set.
    skipped_attempt = next(
        n for n in graph.nodes
        if n.id == f"attempt:{resumed.id}:0"
    )
    assert skipped_attempt.state == "skipped"
    assert skipped_attempt.metadata["skipped_from_run_id"] == prior.id

    # The fresh entrypoint attempt is NOT skipped.
    fresh_attempt = next(
        n for n in graph.nodes
        if n.id == f"attempt:{resumed.id}:1"
    )
    assert fresh_attempt.state != "skipped"
    assert fresh_attempt.metadata["skipped_from_run_id"] is None


@pytest.mark.asyncio
async def test_graph_no_lineage_edge_when_prior_is_outside_graph(cosmos, app_state):
    """Defensive: if a resumed Run's `cloned_from_run_id` points at a
    Run that isn't on this issue (cross-issue resume — unsupported but
    don't crash on it), no dangling edge gets emitted."""
    issue_id = await _seed_issue(cosmos)
    now = _now()
    resumed = Run(
        id="01KQGRAPH_FORGN",
        project="ambience",
        workflow="agent-run",
        issue_id=issue_id,
        issue_repo="nelsong6/ambience",
        issue_number=116,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[],
        cloned_from_run_id="01_NOT_IN_GRAPH",
        entrypoint_phase="env-prep",
        created_at=now, updated_at=now,
    )
    await _seed_run(cosmos, resumed)

    graph = await _build_issue_graph(
        cosmos, repo="nelsong6/ambience", issue_number=116,
    )
    assert all(e.kind != "resumed_from" for e in graph.edges)


@pytest.mark.asyncio
async def test_graph_endpoint_404_on_unknown_issue(cosmos, app_state):
    """Endpoint surface: missing issue → 404 (the same shape consumers
    rely on for error handling)."""
    with patch("glimmung.app.app", app_state):
        with pytest.raises(HTTPException) as exc:
            await issue_graph(
                repo_owner="nelsong6", repo_name="ambience", issue_number=999,
            )
        assert exc.value.status_code == 404

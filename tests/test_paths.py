"""Path-typed identity helpers — unit tests."""

from __future__ import annotations

import json
from datetime import UTC, datetime
from pathlib import Path

import pytest
from glimmung import paths
from glimmung.models import (
    BudgetConfig,
    NativeJobAttempt,
    NativeStepAttempt,
    PhaseAttempt,
    PhaseSpec,
    Project,
    Run,
    RunState,
    Workflow,
)


PATH_CASES = json.loads(
    (Path(__file__).resolve().parents[1] / "testdata" / "path_cases.json").read_text()
)


@pytest.mark.parametrize("case", PATH_CASES, ids=lambda case: case["name"])
def test_path_golden_cases(case):
    match case["function"]:
        case "project_path":
            got = paths.project_path(case["project"])
        case "workflow_path":
            got = paths.workflow_path(case["project"], case["workflow"])
        case "phase_path":
            got = paths.phase_path(case["project"], case["workflow"], case["phase"])
        case "run_path":
            got = paths.run_path(case["project"], case["run_id"])
        case "attempt_path":
            got = paths.attempt_path(case["project"], case["run_id"], case["attempt_index"])
        case "job_path":
            got = paths.job_path(
                case["project"],
                case["run_id"],
                case["attempt_index"],
                case["job_id"],
            )
        case "step_path":
            got = paths.step_path(
                case["project"],
                case["run_id"],
                case["attempt_index"],
                case["job_id"],
                case["step_slug"],
            )
        case _:
            raise AssertionError(f"unknown path function {case['function']!r}")
    assert got == case["want"]


def test_project_path():
    assert paths.project_path("ambience") == "projects/ambience"


def test_workflow_path():
    assert paths.workflow_path("ambience", "agent-run") == "projects/ambience/workflows/agent-run"


def test_phase_path():
    assert (
        paths.phase_path("ambience", "agent-run", "env-prep")
        == "projects/ambience/workflows/agent-run/phases/env-prep"
    )


def test_run_path():
    assert (
        paths.run_path("ambience", "01KR23YZE6TE92JZPK6ZV4NNRY")
        == "projects/ambience/runs/01KR23YZE6TE92JZPK6ZV4NNRY"
    )


def test_attempt_path():
    assert paths.attempt_path("ambience", "01KR23", 1) == "projects/ambience/runs/01KR23/attempts/1"


def test_job_path():
    assert (
        paths.job_path("ambience", "01KR23", 1, "agent-execute")
        == "projects/ambience/runs/01KR23/attempts/1/jobs/agent-execute"
    )


def test_step_path():
    assert (
        paths.step_path("ambience", "01KR23", 1, "agent-execute", "verify-result")
        == "projects/ambience/runs/01KR23/attempts/1/jobs/agent-execute/steps/verify-result"
    )


# ─── domain-model wrappers ────────────────────────────────────────────


def test_path_for_project():
    p = Project(
        id="ambience",
        name="ambience",
        github_repo="nelsong6/ambience",
        created_at=datetime.now(UTC),
    )
    assert paths.path_for_project(p) == "projects/ambience"


def test_path_for_workflow_uses_project_field():
    """`workflow.project` carries the project name; `path_for_workflow`
    reads it directly so callers don't have to thread the project
    separately."""
    w = Workflow(
        id="agent-run",
        project="ambience",
        name="agent-run",
        phases=[],
        budget=BudgetConfig(total=25.0),
        created_at=datetime.now(UTC),
    )
    assert paths.path_for_workflow(w) == "projects/ambience/workflows/agent-run"


def test_path_for_phase_takes_workflow_for_parent_context():
    """`PhaseSpec` doesn't carry the project/workflow it belongs to;
    callers pass the parent workflow."""
    w = Workflow(
        id="agent-run",
        project="ambience",
        name="agent-run",
        phases=[],
        budget=BudgetConfig(total=25.0),
        created_at=datetime.now(UTC),
    )
    phase = PhaseSpec(name="env-prep", kind="k8s_job")
    assert paths.path_for_phase(w, phase) == "projects/ambience/workflows/agent-run/phases/env-prep"


def test_path_for_run_uses_run_fields():
    now = datetime.now(UTC)
    run = Run(
        id="01KR23",
        project="ambience",
        workflow="agent-run",
        run_number=1,
        issue_number=172,
        attempts=[],
        cumulative_cost_usd=0.0,
        budget=BudgetConfig(total=25.0),
        state=RunState.IN_PROGRESS,
        created_at=now,
        updated_at=now,
    )
    assert paths.path_for_run(run) == "projects/ambience/runs/01KR23"


def test_path_for_attempt_and_job_and_step():
    now = datetime.now(UTC)
    step = NativeStepAttempt(slug="verify-result", title=None)
    job = NativeJobAttempt(
        job_id="agent-execute",
        name="run agent and verify result",
        steps=[step],
    )
    attempt = PhaseAttempt(
        attempt_index=1,
        phase="agent-execute",
        workflow_filename="k8s_job:agent-execute",
        dispatched_at=now,
        jobs=[job],
    )
    run = Run(
        id="01KR23",
        project="ambience",
        workflow="agent-run",
        run_number=1,
        issue_number=172,
        attempts=[attempt],
        cumulative_cost_usd=0.0,
        budget=BudgetConfig(total=25.0),
        state=RunState.IN_PROGRESS,
        created_at=now,
        updated_at=now,
    )
    assert paths.path_for_attempt(run, attempt) == ("projects/ambience/runs/01KR23/attempts/1")
    assert paths.path_for_job(run, attempt, job) == (
        "projects/ambience/runs/01KR23/attempts/1/jobs/agent-execute"
    )
    assert paths.path_for_step(run, attempt, job, step) == (
        "projects/ambience/runs/01KR23/attempts/1/jobs/agent-execute/steps/verify-result"
    )

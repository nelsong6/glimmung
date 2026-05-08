"""Path-typed identity for glimmung domain entities.

Entities (Project, Workflow, Phase, Run, PhaseAttempt, Job, Step) are
addressed by URL-shaped paths that match the HTTP API surface and the
glimmung UI's URL bar:

    projects/<project>
    projects/<project>/workflows/<workflow>
    projects/<project>/workflows/<workflow>/phases/<phase>
    projects/<project>/runs/<run_id>
    projects/<project>/runs/<run_id>/attempts/<attempt_index>
    projects/<project>/runs/<run_id>/attempts/<i>/jobs/<job_id>
    projects/<project>/runs/<run_id>/attempts/<i>/jobs/<j>/steps/<slug>

Paths are emitted by every text-producing surface — log messages, MCP
tool outputs, error messages, slack notifications, PR descriptions —
so a reader anywhere in the system can identify the entity uniquely
and (since paths are URLs) click through to the corresponding view.

Paths are computed at render time from canonical slugs + IDs, never
stored. This avoids the renumbering/churn problem of decorated
identifier conventions like `p1-env-prep` (where adding a phase
between `p1` and `p2` requires renumbering everything downstream).

Glimmung's voice is lowercase and terse; paths follow that — no
prefixes like `glimmung://`, no version segment (the API has /v1/
because of HTTP versioning, but identity paths drop it). Inside a
known context, callers can use the trailing segment alone: when a
log line is already scoped to a run, `attempts/1/jobs/agent-execute`
is enough; cross-run search wants the full path.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from glimmung.models import (
        NativeJobAttempt,
        NativeStepAttempt,
        PhaseAttempt,
        PhaseSpec,
        Project,
        Run,
        Workflow,
    )


def project_path(project_name: str) -> str:
    """Path for a project. Bottom of the identity hierarchy."""
    return f"projects/{project_name}"


def workflow_path(project_name: str, workflow_name: str) -> str:
    """Path for a workflow registered to a project."""
    return f"projects/{project_name}/workflows/{workflow_name}"


def phase_path(project_name: str, workflow_name: str, phase_name: str) -> str:
    """Path for a phase declared in a workflow."""
    return f"{workflow_path(project_name, workflow_name)}/phases/{phase_name}"


def run_path(project_name: str, run_id: str) -> str:
    """Path for a run on a project."""
    return f"projects/{project_name}/runs/{run_id}"


def attempt_path(project_name: str, run_id: str, attempt_index: int) -> str:
    """Path for one attempt of a phase within a run."""
    return f"{run_path(project_name, run_id)}/attempts/{attempt_index}"


def job_path(
    project_name: str, run_id: str, attempt_index: int, job_id: str,
) -> str:
    """Path for a native k8s_job within an attempt."""
    return f"{attempt_path(project_name, run_id, attempt_index)}/jobs/{job_id}"


def step_path(
    project_name: str,
    run_id: str,
    attempt_index: int,
    job_id: str,
    step_slug: str,
) -> str:
    """Path for a step within a job within an attempt."""
    return f"{job_path(project_name, run_id, attempt_index, job_id)}/steps/{step_slug}"


# Convenience wrappers that take domain models directly. Each takes the
# minimum set of parents needed because the models themselves don't
# always carry every parent ID (e.g. PhaseSpec doesn't carry the
# project/workflow it's declared in; Run does carry project + run_id
# directly).


def path_for_project(project: "Project") -> str:
    return project_path(project.name)


def path_for_workflow(workflow: "Workflow") -> str:
    return workflow_path(workflow.project, workflow.name)


def path_for_phase(workflow: "Workflow", phase: "PhaseSpec") -> str:
    return phase_path(workflow.project, workflow.name, phase.name)


def path_for_run(run: "Run") -> str:
    return run_path(run.project, run.id)


def path_for_attempt(run: "Run", attempt: "PhaseAttempt") -> str:
    return attempt_path(run.project, run.id, attempt.attempt_index)


def path_for_job(
    run: "Run", attempt: "PhaseAttempt", job: "NativeJobAttempt",
) -> str:
    return job_path(run.project, run.id, attempt.attempt_index, job.job_id)


def path_for_step(
    run: "Run",
    attempt: "PhaseAttempt",
    job: "NativeJobAttempt",
    step: "NativeStepAttempt",
) -> str:
    return step_path(
        run.project, run.id, attempt.attempt_index, job.job_id, step.slug,
    )

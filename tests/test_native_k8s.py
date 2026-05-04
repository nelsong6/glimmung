from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

import httpx
import pytest

from glimmung.native_k8s import NativeKubernetesLauncher, _job_manifest
from glimmung.models import (
    BudgetConfig,
    NativeJobSpec,
    NativeStepSpec,
    PhaseAttempt,
    PhaseSpec,
    Run,
    RunState,
    native_job_attempts_from_specs,
)

from tests.cosmos_fake import FakeContainer


def _settings():
    return SimpleNamespace(
        native_runner_namespace="glimmung-runs",
        native_runner_service_account="glimmung-native-runner",
        native_runner_callback_base_url="http://glimmung.glimmung.svc.cluster.local",
        native_runner_job_ttl_seconds=259200,
        native_runner_namespace_role="admin",
        k8s_sa_token_path="/var/run/token",
        k8s_ca_cert_path="/var/run/ca.crt",
        k8s_api_host="https://kubernetes.default.svc",
    )


class _RecordingLauncher(NativeKubernetesLauncher):
    def __init__(self, settings):
        super().__init__(settings)
        self.calls = []

    async def _request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        return {}


class _FailingRoleBindingLauncher(_RecordingLauncher):
    async def _request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        if method == "POST" and path.endswith("/rolebindings"):
            request = httpx.Request(method, f"https://kubernetes.default.svc{path}")
            response = httpx.Response(403, request=request)
            raise httpx.HTTPStatusError("forbidden", request=request, response=response)
        return {}


def _cosmos():
    return SimpleNamespace(
        runs=FakeContainer("runs", "/project"),
        leases=FakeContainer("leases", "/project"),
    )


def test_job_manifest_maps_phase_jobs_to_sequential_pod_containers():
    phase = PhaseSpec(
        name="agent",
        kind="k8s_job",
        jobs=[
            NativeJobSpec(
                id="clone",
                image="runner:clone",
                command=["/bin/clone"],
                steps=[NativeStepSpec(slug="clone-repo")],
                timeout_seconds=30,
            ),
            NativeJobSpec(
                id="agent",
                image="runner:agent",
                args=["run"],
                env={"APP_ENV": "test"},
                steps=[NativeStepSpec(slug="run-agent")],
                timeout_seconds=60,
            ),
        ],
    )
    lease_doc = {
        "id": "01LEASE",
        "project": "ambience",
        "workflow": "native-agent",
        "metadata": {
            "run_id": "01KRNATIVE0000000000000000",
            "attempt_index": "2",
            "issue_id": "01ISSUE",
            "issue_body": "Use the validation URL and make the requested change.",
            "phase_inputs": {
                "target-ref": "main",
                "prod_namespace": "glimmung",
                "validation_url": "https://preview.invalid",
            },
            "entrypoint_job_id": "agent",
            "entrypoint_step_slug": "run-agent",
            "artifact_refs": {"source": "blob://artifacts/source.tgz"},
            "context": {"operator_note": "resume at agent step"},
        },
        "requestedAt": datetime.now(UTC).isoformat(),
    }

    manifest = _job_manifest(
        settings=_settings(),
        lease_doc=lease_doc,
        workflow_doc={"name": "native-agent"},
        phase=phase,
        job_name="glim-01krnative-2",
        secret_name="glim-01krnative-2-token",
    )

    spec = manifest["spec"]["template"]["spec"]
    assert manifest["spec"]["template"]["metadata"]["labels"]["azure.workload.identity/use"] == "true"
    assert spec["serviceAccountName"] == "glimmung-native-runner"

    assert spec["activeDeadlineSeconds"] == 90
    assert spec["initContainers"][0]["name"] == "clone"
    assert spec["containers"][0]["name"] == "agent"
    assert spec["containers"][0]["args"] == ["run"]
    env = {item["name"]: item for item in spec["containers"][0]["env"]}
    assert env["APP_ENV"]["value"] == "test"
    assert env["GLIMMUNG_RUN_ID"]["value"] == "01KRNATIVE0000000000000000"
    assert env["GLIMMUNG_JOB_ID"]["value"] == "agent"
    assert env["GLIMMUNG_ISSUE_BODY"]["value"] == (
        "Use the validation URL and make the requested change."
    )
    assert env["GLIMMUNG_INPUT_TARGET_REF"]["value"] == "main"
    assert env["GLIMMUNG_ENTRYPOINT_JOB_ID"]["value"] == "agent"
    assert env["GLIMMUNG_ENTRYPOINT_STEP_SLUG"]["value"] == "run-agent"
    assert env["GLIMMUNG_ARTIFACT_REFS"]["value"] == (
        '{"source": "blob://artifacts/source.tgz"}'
    )
    assert env["GLIMMUNG_CONTEXT"]["value"] == '{"operator_note": "resume at agent step"}'
    assert env["GLIMMUNG_VALIDATION_NAMESPACE"]["value"] == (
        "glim-run-01krnative0000000000000000-2"
    )
    assert env["GLIMMUNG_K8S_NAMESPACES"]["value"] == (
        "glim-run-01krnative0000000000000000-2,glimmung"
    )
    assert env["GLIMMUNG_GITHUB_TOKEN_URL"]["value"] == (
        "http://glimmung.glimmung.svc.cluster.local"
        "/v1/runs/ambience/01KRNATIVE0000000000000000/native/github-token"
    )
    assert env["GLIMMUNG_STATUS_URL"]["value"] == (
        "http://glimmung.glimmung.svc.cluster.local"
        "/v1/runs/ambience/01KRNATIVE0000000000000000/native/status"
    )
    assert env["GLIMMUNG_ATTEMPT_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == (
        "glim-01krnative-2-token"
    )


@pytest.mark.asyncio
async def test_delete_attempt_job_uses_graceful_foreground_delete():
    launcher = _RecordingLauncher(_settings())

    await launcher.delete_attempt_job(
        run_id="01KRNATIVE0000000000000000",
        attempt_index=2,
        grace_period_seconds=60,
    )

    assert launcher.calls == [{
        "method": "DELETE",
        "path": (
            "/apis/batch/v1/namespaces/glimmung-runs/jobs/"
            "glim-01krnative0000000000000000-2"
        ),
        "json": {
            "apiVersion": "v1",
            "kind": "DeleteOptions",
            "propagationPolicy": "Foreground",
            "gracePeriodSeconds": 60,
        },
    }]


@pytest.mark.asyncio
async def test_launch_scaffolds_run_namespaces_and_rbac_before_job():
    settings = _settings()
    launcher = _RecordingLauncher(settings)
    cosmos = _cosmos()
    now = datetime.now(UTC)
    phase = PhaseSpec(
        name="agent",
        kind="k8s_job",
        jobs=[
            NativeJobSpec(
                id="agent",
                image="runner:agent",
                steps=[NativeStepSpec(slug="run-agent")],
            )
        ],
    )
    run = Run(
        id="01KRNATIVE0000000000000000",
        project="ambience",
        workflow="native-agent",
        issue_id="01ISSUE",
        issue_repo="nelsong6/ambience",
        issue_number=117,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0,
                phase="agent",
                phase_kind="k8s_job",
                workflow_filename="k8s_job:agent",
                dispatched_at=now,
                jobs=native_job_attempts_from_specs(phase.jobs),
            )
        ],
        created_at=now,
        updated_at=now,
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))
    lease_doc = {
        "id": "01LEASE",
        "project": "ambience",
        "workflow": "native-agent",
        "state": "active",
        "host": "native-k8s",
        "requirements": {},
        "metadata": {
            "native_k8s": True,
            "run_id": run.id,
            "attempt_index": "0",
            "phase_inputs": {
                "prod_namespace": "glimmung",
                "validation_url": "https://preview.invalid",
            },
        },
        "requestedAt": now.isoformat(),
        "assignedAt": now.isoformat(),
        "releasedAt": None,
        "ttlSeconds": 14400,
    }
    await cosmos.leases.create_item(lease_doc)

    await launcher.launch(
        cosmos,
        lease_doc=lease_doc,
        workflow_doc={"name": "native-agent"},
        phase=phase,
    )

    paths = [call["path"] for call in launcher.calls]
    job_index = paths.index("/apis/batch/v1/namespaces/glimmung-runs/jobs")
    rolebinding_indexes = [
        i for i, path in enumerate(paths)
        if path.endswith("/rolebindings")
    ]
    assert rolebinding_indexes
    assert max(rolebinding_indexes) < job_index
    rolebinding_namespaces = {
        path.split("/namespaces/", 1)[1].split("/", 1)[0]
        for path in paths
        if path.endswith("/rolebindings")
    }
    assert rolebinding_namespaces == {
        "glim-run-01krnative0000000000000000-0",
        "glimmung",
    }
    rolebinding = next(
        call["json"] for call in launcher.calls
        if call["path"].endswith("/glimmung/rolebindings")
    )
    assert rolebinding["roleRef"] == {
        "apiGroup": "rbac.authorization.k8s.io",
        "kind": "ClusterRole",
        "name": "admin",
    }
    assert rolebinding["subjects"] == [{
        "kind": "ServiceAccount",
        "name": "glimmung-native-runner",
        "namespace": "glimmung-runs",
    }]


@pytest.mark.asyncio
async def test_launch_cleans_attempt_secret_when_scaffolding_fails():
    launcher = _FailingRoleBindingLauncher(_settings())
    cosmos = _cosmos()
    now = datetime.now(UTC)
    phase = PhaseSpec(
        name="agent",
        kind="k8s_job",
        jobs=[
            NativeJobSpec(
                id="agent",
                image="runner:agent",
                steps=[NativeStepSpec(slug="run-agent")],
            )
        ],
    )
    run = Run(
        id="01KRNATIVE0000000000000000",
        project="ambience",
        workflow="native-agent",
        issue_id="01ISSUE",
        issue_repo="nelsong6/ambience",
        issue_number=117,
        state=RunState.IN_PROGRESS,
        budget=BudgetConfig(total=25.0),
        attempts=[
            PhaseAttempt(
                attempt_index=0,
                phase="agent",
                phase_kind="k8s_job",
                workflow_filename="k8s_job:agent",
                dispatched_at=now,
                jobs=native_job_attempts_from_specs(phase.jobs),
            )
        ],
        created_at=now,
        updated_at=now,
    )
    await cosmos.runs.create_item(run.model_dump(mode="json"))

    lease_doc = {
        "id": "01LEASE",
        "project": "ambience",
        "workflow": "native-agent",
        "state": "active",
        "host": "native-k8s",
        "requirements": {},
        "metadata": {
            "native_k8s": True,
            "run_id": run.id,
            "attempt_index": "0",
        },
        "requestedAt": now.isoformat(),
        "assignedAt": now.isoformat(),
        "releasedAt": None,
        "ttlSeconds": 14400,
    }

    with pytest.raises(httpx.HTTPStatusError):
        await launcher.launch(
            cosmos,
            lease_doc=lease_doc,
            workflow_doc={"name": "native-agent"},
            phase=phase,
        )

    assert {
        "method": "DELETE",
        "path": (
            "/api/v1/namespaces/glimmung-runs/secrets/"
            "glim-01krnative0000000000000000-0-token"
        ),
        "json": None,
    } in launcher.calls

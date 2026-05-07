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
        native_runner_codex_credentials_secret="codex-credentials",
        native_runner_codex_credentials_mount_path="/etc/codex-creds",
        native_runner_project_concurrency=5,
        native_standby_dns_namespace="glimmung",
        native_standby_dns_gateway_namespace="envoy-gateway-system",
        native_standby_dns_gateway_name="main",
        native_standby_dns_target="172.179.163.96",
        native_standby_dns_default_ttl=300,
        k8s_sa_token_path="/var/run/token",
        k8s_ca_cert_path="/var/run/ca.crt",
        k8s_api_host="https://kubernetes.default.svc",
    )


class _RecordingLauncher(NativeKubernetesLauncher):
    def __init__(self, settings):
        super().__init__(settings)
        self.calls = []
        self.mcp_calls = []

    async def _request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        return {}

    def _workload_identity_issuer(self) -> str:
        return "https://issuer.example/"

    async def _call_mcp_tool(self, name: str, arguments: dict):
        self.mcp_calls.append({"name": name, "arguments": arguments})
        return {}


class _FailingRoleBindingLauncher(_RecordingLauncher):
    async def _request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        if method == "POST" and path.endswith("/rolebindings"):
            request = httpx.Request(method, f"https://kubernetes.default.svc{path}")
            response = httpx.Response(403, request=request)
            raise httpx.HTTPStatusError("forbidden", request=request, response=response)
        return {}


class _PodLogLauncher(_RecordingLauncher):
    async def _request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        if path.startswith("/api/v1/namespaces/glimmung-runs/pods?"):
            return {
                "items": [
                    {
                        "metadata": {"name": "glim-run-pod"},
                        "status": {"phase": "Running", "startTime": "2026-05-06T06:00:00Z"},
                    }
                ]
            }
        return {}

    async def _request_text(self, method: str, path: str):
        self.calls.append({"method": method, "path": path, "json": None})
        return "line one\nline two\n"


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
            "native_slot_index": "3",
            "native_slot_name": "ambience-slot-3",
            "work_context_id": "playbook:01PB:shared",
            "work_context_branch": "glimmung/playbooks/01pb",
            "work_context_base_ref": "main",
            "work_context_state": "in_use",
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
    assert (
        manifest["spec"]["template"]["metadata"]["labels"]["azure.workload.identity/use"] == "true"
    )
    assert spec["serviceAccountName"] == "glimmung-native-runner"
    volumes = {item["name"]: item for item in spec["volumes"]}
    assert volumes["codex-credentials"]["secret"] == {
        "secretName": "codex-credentials",
        "optional": False,
    }

    assert spec["activeDeadlineSeconds"] == 90
    assert spec["initContainers"][0]["name"] == "clone"
    assert spec["containers"][0]["name"] == "agent"
    assert spec["containers"][0]["args"] == ["run"]
    mounts = {item["name"]: item for item in spec["containers"][0]["volumeMounts"]}
    assert mounts["codex-credentials"] == {
        "name": "codex-credentials",
        "mountPath": "/etc/codex-creds",
        "readOnly": True,
    }
    env = {item["name"]: item for item in spec["containers"][0]["env"]}
    assert env["APP_ENV"]["value"] == "test"
    assert env["GLIMMUNG_RUN_ID"]["value"] == "01KRNATIVE0000000000000000"
    assert env["GLIMMUNG_JOB_ID"]["value"] == "agent"
    assert env["GLIMMUNG_ISSUE_BODY"]["value"] == (
        "Use the validation URL and make the requested change."
    )
    assert env["GLIMMUNG_NATIVE_SLOT_INDEX"]["value"] == "3"
    assert env["GLIMMUNG_NATIVE_SLOT_NAME"]["value"] == "ambience-slot-3"
    assert env["GLIMMUNG_WORK_CONTEXT_ID"]["value"] == "playbook:01PB:shared"
    assert env["GLIMMUNG_WORK_CONTEXT_BRANCH"]["value"] == "glimmung/playbooks/01pb"
    assert env["GLIMMUNG_WORK_CONTEXT_BASE_REF"]["value"] == "main"
    assert env["GLIMMUNG_WORK_CONTEXT_STATE"]["value"] == "in_use"
    assert env["GLIMMUNG_INPUT_TARGET_REF"]["value"] == "main"
    assert env["GLIMMUNG_ENTRYPOINT_JOB_ID"]["value"] == "agent"
    assert env["GLIMMUNG_ENTRYPOINT_STEP_SLUG"]["value"] == "run-agent"
    assert env["GLIMMUNG_ARTIFACT_REFS"]["value"] == ('{"source": "blob://artifacts/source.tgz"}')
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
async def test_reconcile_standby_dns_creates_project_slots():
    launcher = _RecordingLauncher(_settings())

    await launcher.reconcile_standby_dns([
        {
            "id": "ambience",
            "name": "ambience",
            "metadata": {
                "native_standby_dns": {
                    "enabled": True,
                    "record_base": "ambience.dev.romaine.life",
                    "slot_prefix": "ambience-slot",
                    "count": 2,
                }
            },
        }
    ])

    assert launcher.calls[0]["method"] == "GET"
    assert launcher.calls[0]["path"].startswith(
        "/apis/externaldns.k8s.io/v1alpha1/namespaces/glimmung/dnsendpoints?"
    )
    assert launcher.calls[1]["method"] == "POST"
    assert launcher.calls[1]["path"] == (
        "/apis/externaldns.k8s.io/v1alpha1/namespaces/glimmung/dnsendpoints"
    )
    body = launcher.calls[1]["json"]
    assert body["metadata"]["name"] == "native-standby-ambience"
    assert body["metadata"]["labels"]["glimmung.romaine.life/standby-dns"] == "true"
    assert body["spec"]["endpoints"] == [
        {
            "dnsName": "ambience-slot-1.ambience.dev.romaine.life",
            "recordType": "A",
            "recordTTL": 300,
            "targets": ["172.179.163.96"],
        },
        {
            "dnsName": "ambience-slot-2.ambience.dev.romaine.life",
            "recordType": "A",
            "recordTTL": 300,
            "targets": ["172.179.163.96"],
        },
    ]


@pytest.mark.asyncio
async def test_reconcile_standby_workload_identity_upserts_project_slots():
    settings = _settings()
    settings.native_standby_identity_subscription = "sub-123"
    launcher = _RecordingLauncher(settings)

    await launcher.reconcile_standby_workload_identity([
        {
            "id": "tank",
            "name": "tank",
            "metadata": {
                "native_standby_dns": {"enabled": True, "count": 2},
                "native_standby_workload_identity": {
                    "enabled": True,
                    "resource_group": "infra",
                    "slot_prefix": "tank-slot",
                    "credentials": [
                        {
                            "identity_name": "claude-credentials-refresher-identity",
                            "credential_name": "{slot_name}-orchestrator",
                            "subject": "system:serviceaccount:{namespace}:{slot_name}",
                        },
                        {
                            "identity_name": "claude-api-proxy-identity",
                            "credential_name": "{slot_name}-claude-api-proxy",
                            "subject": "system:serviceaccount:{namespace}:claude-api-proxy",
                        },
                    ],
                },
            },
        }
    ])

    assert [call["name"] for call in launcher.mcp_calls] == [
        "uami_upsert_federated_credential",
        "uami_upsert_federated_credential",
        "uami_upsert_federated_credential",
        "uami_upsert_federated_credential",
    ]
    assert launcher.mcp_calls[0]["arguments"] == {
        "subscription": "sub-123",
        "resource_group": "infra",
        "identity_name": "claude-credentials-refresher-identity",
        "credential_name": "tank-slot-1-orchestrator",
        "issuer": "https://issuer.example/",
        "subject": "system:serviceaccount:tank-slot-1:tank-slot-1",
        "dry_run": False,
    }
    assert launcher.mcp_calls[3]["arguments"]["identity_name"] == "claude-api-proxy-identity"
    assert launcher.mcp_calls[3]["arguments"]["credential_name"] == "tank-slot-2-claude-api-proxy"
    assert launcher.mcp_calls[3]["arguments"]["subject"] == (
        "system:serviceaccount:tank-slot-2:claude-api-proxy"
    )


@pytest.mark.asyncio
async def test_delete_attempt_job_uses_graceful_foreground_delete():
    launcher = _RecordingLauncher(_settings())

    await launcher.delete_attempt_job(
        run_id="01KRNATIVE0000000000000000",
        attempt_index=2,
        grace_period_seconds=60,
    )

    assert launcher.calls == [
        {
            "method": "DELETE",
            "path": (
                "/apis/batch/v1/namespaces/glimmung-runs/jobs/glim-01krnative0000000000000000-2"
            ),
            "json": {
                "apiVersion": "v1",
                "kind": "DeleteOptions",
                "propagationPolicy": "Foreground",
                "gracePeriodSeconds": 60,
            },
        }
    ]


@pytest.mark.asyncio
async def test_read_attempt_pod_logs_selects_job_pod_and_container_tail():
    launcher = _PodLogLauncher(_settings())

    result = await launcher.read_attempt_pod_logs(
        run_id="01KRNATIVE0000000000000000",
        attempt_index=2,
        job_id="codex-agent",
        tail_lines=200,
    )

    assert result == {
        "namespace": "glimmung-runs",
        "pod_name": "glim-run-pod",
        "container": "codex-agent",
        "phase": "Running",
        "logs": "line one\nline two\n",
    }
    assert launcher.calls == [
        {
            "method": "GET",
            "path": (
                "/api/v1/namespaces/glimmung-runs/pods?"
                "labelSelector=job-name%3Dglim-01krnative0000000000000000-2"
            ),
            "json": None,
        },
        {
            "method": "GET",
            "path": (
                "/api/v1/namespaces/glimmung-runs/pods/glim-run-pod/log?"
                "container=codex-agent&tailLines=200&timestamps=false"
            ),
            "json": None,
        },
    ]


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
    rolebinding_indexes = [i for i, path in enumerate(paths) if path.endswith("/rolebindings")]
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
        call["json"] for call in launcher.calls if call["path"].endswith("/glimmung/rolebindings")
    )
    assert rolebinding["roleRef"] == {
        "apiGroup": "rbac.authorization.k8s.io",
        "kind": "ClusterRole",
        "name": "admin",
    }
    assert rolebinding["subjects"] == [
        {
            "kind": "ServiceAccount",
            "name": "glimmung-native-runner",
            "namespace": "glimmung-runs",
        }
    ]


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
            "/api/v1/namespaces/glimmung-runs/secrets/glim-01krnative0000000000000000-0-token"
        ),
        "json": None,
    } in launcher.calls

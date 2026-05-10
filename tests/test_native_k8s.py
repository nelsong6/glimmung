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
        native_runner_playwright_enabled=True,
        native_runner_playwright_image="romainecr.azurecr.io/agent-container:latest",
        native_runner_playwright_port=3000,
        native_runner_playwright_cpu_request="100m",
        native_runner_playwright_memory_request="256Mi",
        native_runner_playwright_cpu_limit="1000m",
        native_runner_playwright_memory_limit="1Gi",
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


class _RecordingAzureLauncher(_RecordingLauncher):
    def __init__(self, settings, *, redirect_uris: list[str] | None = None):
        super().__init__(settings)
        self.redirect_uris = redirect_uris or ["https://existing.example/"]

    async def _arm_request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        return {}

    async def _graph_request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        if method == "GET" and path.startswith("/applications(appId="):
            return {
                "id": "app-object-id",
                "appId": "client-id",
                "displayName": "test-app",
                "spa": {"redirectUris": self.redirect_uris},
            }
        return {}


class _ExistingDnsLauncher(_RecordingLauncher):
    async def _request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        if method == "POST" and path.endswith("/dnsendpoints"):
            request = httpx.Request(method, f"https://kubernetes.default.svc{path}")
            response = httpx.Response(409, request=request)
            raise httpx.HTTPStatusError("conflict", request=request, response=response)
        if method == "GET" and path.endswith("/dnsendpoints/native-standby-ambience"):
            return {"metadata": {"resourceVersion": "12345"}}
        return {}


class _FailingRoleBindingLauncher(_RecordingLauncher):
    async def _request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        if method == "POST" and path.endswith("/rolebindings"):
            request = httpx.Request(method, f"https://kubernetes.default.svc{path}")
            response = httpx.Response(403, request=request)
            raise httpx.HTTPStatusError("forbidden", request=request, response=response)
        return {}


class _DeleteAttemptJobLauncher(_RecordingLauncher):
    """Returns two per-job k8s Jobs from the label-selector list call so
    `delete_attempt_job` exercises the per-job delete loop."""
    async def _request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        if method == "GET" and path.startswith(
            "/apis/batch/v1/namespaces/glimmung-runs/jobs?"
        ):
            return {
                "items": [
                    {"metadata": {
                        "name": "glim-01krnative0000000000000000-2-plan",
                    }},
                    {"metadata": {
                        "name": "glim-01krnative0000000000000000-2-impl",
                    }},
                ]
            }
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


class _ExistingPlaywrightLauncher(_RecordingLauncher):
    async def _request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        if method == "POST" and (
            path.endswith("/deployments") or path.endswith("/services")
        ):
            request = httpx.Request(method, f"https://kubernetes.default.svc{path}")
            response = httpx.Response(409, request=request)
            raise httpx.HTTPStatusError("conflict", request=request, response=response)
        return {}


class _ReconcilePlaywrightLauncher(_RecordingLauncher):
    async def _request(self, method: str, path: str, *, json=None):
        self.calls.append({"method": method, "path": path, "json": json})
        if method == "GET" and path.startswith(
            "/apis/apps/v1/namespaces/glimmung-runs/deployments?"
        ):
            return {
                "items": [
                    {"metadata": {"name": "glim-pw-ambience-ambience-slot-1"}},
                    {"metadata": {"name": "glim-pw-ambience-ambience-slot-2"}},
                ]
            }
        return {}


def _cosmos():
    return SimpleNamespace(
        runs=FakeContainer("runs", "/project"),
        leases=FakeContainer("leases", "/project"),
    )


def test_job_manifest_per_job_renders_one_container_pod():
    """Job-level concurrent dispatch: every `phase.jobs[*]` becomes its
    own k8s Job with a single-container Pod. The pre-fan-out shape
    (initContainers + main container in one Pod) is gone — siblings now
    run in parallel and report back via `job_id` on the completion
    callback."""
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
        job_spec=phase.jobs[1],
        job_name="glim-01krnative-2-agent",
        secret_name="glim-01krnative-2-token",
        attempt_base="glim-01krnative-2",
    )

    spec = manifest["spec"]["template"]["spec"]
    assert (
        manifest["spec"]["template"]["metadata"]["labels"]["azure.workload.identity/use"] == "true"
    )
    assert manifest["metadata"]["labels"]["glimmung.romaine.life/job-id"] == "agent"
    assert manifest["metadata"]["labels"]["glimmung.romaine.life/attempt-base"] == "glim-01krnative-2"
    assert manifest["metadata"]["annotations"]["glimmung.romaine.life/job-id"] == "agent"
    assert spec["serviceAccountName"] == "glimmung-native-runner"
    volumes = {item["name"]: item for item in spec["volumes"]}
    assert volumes["codex-credentials"]["secret"] == {
        "secretName": "codex-credentials",
        "optional": False,
    }

    # One Pod per job: no initContainers any more, exactly one container.
    assert "initContainers" not in spec
    assert len(spec["containers"]) == 1
    assert spec["activeDeadlineSeconds"] == 60
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
    assert env["GLIMMUNG_PLAYWRIGHT_WS_ENDPOINT"]["value"] == (
        "ws://glim-pw-ambience-ambience-slot-3.glimmung-runs.svc.cluster.local:3000/"
    )
    assert env["PLAYWRIGHT_WS_ENDPOINT"]["value"] == (
        "ws://glim-pw-ambience-ambience-slot-3.glimmung-runs.svc.cluster.local:3000/"
    )
    assert env["PW_TEST_CONNECT_WS_ENDPOINT"]["value"] == (
        "ws://glim-pw-ambience-ambience-slot-3.glimmung-runs.svc.cluster.local:3000/"
    )
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
async def test_reconcile_standby_dns_puts_existing_resource_version():
    launcher = _ExistingDnsLauncher(_settings())

    await launcher.reconcile_standby_dns([
        {
            "id": "ambience",
            "name": "ambience",
            "metadata": {
                "native_standby_dns": {
                    "enabled": True,
                    "record_base": "ambience.dev.romaine.life",
                    "slot_prefix": "ambience-slot",
                    "count": 1,
                }
            },
        }
    ])

    put_call = next(call for call in launcher.calls if call["method"] == "PUT")
    assert put_call["path"] == (
        "/apis/externaldns.k8s.io/v1alpha1/namespaces/glimmung"
        "/dnsendpoints/native-standby-ambience"
    )
    assert put_call["json"]["metadata"]["resourceVersion"] == "12345"


@pytest.mark.asyncio
async def test_reconcile_standby_workload_identity_upserts_slot_credentials():
    launcher = _RecordingAzureLauncher(_settings())

    await launcher.reconcile_standby_workload_identity([
        {
            "id": "tank-operator",
            "name": "tank-operator",
            "metadata": {
                "native_standby_workload_identity": {
                    "enabled": True,
                    "subscription": "00000000-0000-0000-0000-000000000000",
                    "resource_group": "infra",
                    "issuer": "https://issuer.invalid/",
                    "slot_prefix": "tank-slot",
                    "count": 2,
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
                }
            },
        }
    ])

    assert [call["method"] for call in launcher.calls] == ["PUT", "PUT", "PUT", "PUT"]
    first = launcher.calls[0]
    assert first["path"] == (
        "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/infra"
        "/providers/Microsoft.ManagedIdentity/userAssignedIdentities"
        "/claude-credentials-refresher-identity/federatedIdentityCredentials"
        "/tank-slot-1-orchestrator?api-version=2023-01-31"
    )
    assert first["json"] == {
        "properties": {
            "issuer": "https://issuer.invalid/",
            "subject": "system:serviceaccount:tank-slot-1:tank-slot-1",
            "audiences": ["api://AzureADTokenExchange"],
        }
    }
    assert launcher.calls[3]["path"].endswith(
        "/claude-api-proxy-identity/federatedIdentityCredentials"
        "/tank-slot-2-claude-api-proxy?api-version=2023-01-31"
    )
    assert launcher.calls[3]["json"]["properties"]["subject"] == (
        "system:serviceaccount:tank-slot-2:claude-api-proxy"
    )


@pytest.mark.asyncio
async def test_reconcile_standby_entra_redirects_upserts_slot_redirect_uris():
    launcher = _RecordingAzureLauncher(_settings())

    await launcher.reconcile_standby_entra_redirects([
        {
            "id": "tank-operator",
            "name": "tank-operator",
            "metadata": {
                "native_standby_dns": {
                    "enabled": True,
                    "record_base": "tank.dev.romaine.life",
                    "slot_prefix": "tank-slot",
                    "count": 2,
                },
                "native_standby_entra_redirects": {
                    "enabled": True,
                    "application_app_id": "client-id",
                },
            },
        }
    ])

    assert [call["method"] for call in launcher.calls] == ["GET", "PATCH"]
    assert launcher.calls[0]["path"] == "/applications(appId='client-id')"
    assert launcher.calls[1]["path"] == "/applications/app-object-id"
    assert launcher.calls[1]["json"] == {
        "spa": {
            "redirectUris": [
                "https://existing.example/",
                "https://tank-slot-1.tank.dev.romaine.life/",
                "https://tank-slot-2.tank.dev.romaine.life/",
            ]
        }
    }


@pytest.mark.asyncio
async def test_reconcile_standby_entra_redirects_defaults_to_project_prefix():
    launcher = _RecordingAzureLauncher(_settings())

    await launcher.reconcile_standby_entra_redirects([
        {
            "id": "tank-operator",
            "name": "tank-operator",
            "metadata": {
                "native_standby_dns": {
                    "enabled": True,
                    "record_base": "tank.dev.romaine.life",
                    "count": 2,
                },
                "native_standby_entra_redirects": {
                    "enabled": True,
                    "application_app_id": "client-id",
                },
            },
        }
    ])

    assert launcher.calls[1]["json"] == {
        "spa": {
            "redirectUris": [
                "https://existing.example/",
                "https://tank-operator-1.tank.dev.romaine.life/",
                "https://tank-operator-2.tank.dev.romaine.life/",
            ]
        }
    }


@pytest.mark.asyncio
async def test_reconcile_standby_entra_redirects_prunes_stale_managed_slots():
    launcher = _RecordingAzureLauncher(
        _settings(),
        redirect_uris=[
            "https://existing.example/",
            "https://tank-slot-1.tank.dev.romaine.life/",
            "https://tank-slot-2.tank.dev.romaine.life/",
            "https://tank-slot-3.tank.dev.romaine.life/",
            "https://other-slot-3.tank.dev.romaine.life/",
        ],
    )

    await launcher.reconcile_standby_entra_redirects([
        {
            "id": "tank-operator",
            "name": "tank-operator",
            "metadata": {
                "native_standby_dns": {
                    "enabled": True,
                    "record_base": "tank.dev.romaine.life",
                    "slot_prefix": "tank-slot",
                    "count": 2,
                },
                "native_standby_entra_redirects": {
                    "enabled": True,
                    "application_app_id": "client-id",
                },
            },
        }
    ])

    assert [call["method"] for call in launcher.calls] == ["GET", "PATCH"]
    assert launcher.calls[1]["json"] == {
        "spa": {
            "redirectUris": [
                "https://existing.example/",
                "https://tank-slot-1.tank.dev.romaine.life/",
                "https://tank-slot-2.tank.dev.romaine.life/",
                "https://other-slot-3.tank.dev.romaine.life/",
            ]
        }
    }


@pytest.mark.asyncio
async def test_delete_attempt_job_label_selects_then_deletes_per_job():
    """delete_attempt_job lists every Job for the attempt by attempt-base
    label, deletes each (per-job fan-out), then falls through to the
    legacy per-attempt name for back-compat with attempts launched by
    older glimmung versions."""
    launcher = _DeleteAttemptJobLauncher(_settings())

    await launcher.delete_attempt_job(
        run_id="01KRNATIVE0000000000000000",
        attempt_index=2,
        grace_period_seconds=60,
    )

    list_call, del_a, del_b, legacy_del = launcher.calls
    assert list_call["method"] == "GET"
    assert "labelSelector=" in list_call["path"]
    assert "glimmung.romaine.life%2Fattempt-base%3Dglim-01krnative0000000000000000-2" in list_call["path"]
    body = {
        "apiVersion": "v1",
        "kind": "DeleteOptions",
        "propagationPolicy": "Foreground",
        "gracePeriodSeconds": 60,
    }
    assert del_a == {
        "method": "DELETE",
        "path": "/apis/batch/v1/namespaces/glimmung-runs/jobs/glim-01krnative0000000000000000-2-plan",
        "json": body,
    }
    assert del_b == {
        "method": "DELETE",
        "path": "/apis/batch/v1/namespaces/glimmung-runs/jobs/glim-01krnative0000000000000000-2-impl",
        "json": body,
    }
    assert legacy_del == {
        "method": "DELETE",
        "path": "/apis/batch/v1/namespaces/glimmung-runs/jobs/glim-01krnative0000000000000000-2",
        "json": body,
    }

@pytest.mark.asyncio
async def test_ensure_playwright_slot_creates_deployment_and_service():
    launcher = _RecordingLauncher(_settings())
    lease_doc = {
        "id": "01LEASE",
        "project": "ambience",
        "metadata": {
            "native_k8s": True,
            "native_slot_index": "1",
            "native_slot_name": "ambience-slot-1",
        },
    }

    endpoint = await launcher.ensure_playwright_slot(lease_doc)

    assert endpoint == (
        "ws://glim-pw-ambience-ambience-slot-1.glimmung-runs.svc.cluster.local:3000/"
    )
    deployment, service = launcher.calls
    assert deployment["method"] == "POST"
    assert deployment["path"] == "/apis/apps/v1/namespaces/glimmung-runs/deployments"
    deployment_body = deployment["json"]
    assert deployment_body["metadata"]["name"] == "glim-pw-ambience-ambience-slot-1"
    assert deployment_body["metadata"]["labels"]["glimmung.romaine.life/slot-playwright"] == "true"
    container = deployment_body["spec"]["template"]["spec"]["containers"][0]
    assert container["image"] == "romainecr.azurecr.io/agent-container:latest"
    assert container["command"] == [
        "npx",
        "playwright",
        "run-server",
        "--host",
        "0.0.0.0",
        "--port",
        "3000",
    ]
    assert service == {
        "method": "POST",
        "path": "/api/v1/namespaces/glimmung-runs/services",
        "json": {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {
                "name": "glim-pw-ambience-ambience-slot-1",
                "namespace": "glimmung-runs",
                "labels": deployment_body["metadata"]["labels"],
            },
            "spec": {
                "selector": {"app.kubernetes.io/name": "glim-pw-ambience-ambience-slot-1"},
                "ports": [{"name": "ws", "port": 3000, "targetPort": "ws"}],
            },
        },
    }


@pytest.mark.asyncio
async def test_ensure_playwright_slot_is_idempotent_on_existing_resources():
    launcher = _ExistingPlaywrightLauncher(_settings())

    await launcher.ensure_playwright_slot({
        "id": "01LEASE",
        "project": "ambience",
        "metadata": {
            "native_k8s": True,
            "native_slot_index": "1",
            "native_slot_name": "ambience-slot-1",
        },
    })

    assert [call["method"] for call in launcher.calls] == ["POST", "POST"]


@pytest.mark.asyncio
async def test_reconcile_playwright_slots_deletes_workers_without_active_lease():
    launcher = _ReconcilePlaywrightLauncher(_settings())

    await launcher.reconcile_playwright_slots([
        {
            "id": "01LEASE",
            "project": "ambience",
            "metadata": {
                "native_k8s": True,
                "native_slot_index": "1",
                "native_slot_name": "ambience-slot-1",
            },
        }
    ])

    assert [call["method"] for call in launcher.calls[:3]] == ["POST", "POST", "GET"]
    assert launcher.calls[3:] == [
        {
            "method": "DELETE",
            "path": (
                "/apis/apps/v1/namespaces/glimmung-runs/deployments/"
                "glim-pw-ambience-ambience-slot-2"
            ),
            "json": None,
        },
        {
            "method": "DELETE",
            "path": (
                "/api/v1/namespaces/glimmung-runs/services/"
                "glim-pw-ambience-ambience-slot-2"
            ),
            "json": None,
        },
    ]


@pytest.mark.asyncio
async def test_reconcile_playwright_slots_includes_test_slot_checkouts():
    launcher = _ReconcilePlaywrightLauncher(_settings())

    await launcher.reconcile_playwright_slots([
        {
            "id": "01LEASE",
            "project": "ambience",
            "metadata": {
                "native_k8s": True,
                "native_slot_index": "1",
                "native_slot_name": "ambience-slot-1",
                "test_slot_checkout": True,
            },
        }
    ])

    assert [call["method"] for call in launcher.calls] == [
        "POST", "POST", "GET", "DELETE", "DELETE",
    ]
    assert launcher.calls[0]["path"].endswith("/deployments")
    assert launcher.calls[1]["path"].endswith("/services")
    assert launcher.calls[3]["path"].endswith(
        "/deployments/glim-pw-ambience-ambience-slot-2"
    )
    assert launcher.calls[4]["path"].endswith(
        "/services/glim-pw-ambience-ambience-slot-2"
    )
    assert launcher.calls[0]["json"]["metadata"]["name"] == (
        "glim-pw-ambience-ambience-slot-1"
    )
    assert launcher.calls[1]["json"]["metadata"]["name"] == "glim-pw-ambience-ambience-slot-1"


@pytest.mark.asyncio
async def test_delete_test_slot_namespace_deletes_namespace():
    launcher = _RecordingLauncher(_settings())

    await launcher.delete_test_slot_namespace("glimmung-slot-2")

    assert launcher.calls == [{
        "method": "DELETE",
        "path": "/api/v1/namespaces/glimmung-slot-2",
        "json": {
            "apiVersion": "v1",
            "kind": "DeleteOptions",
            "propagationPolicy": "Foreground",
        },
    }]


@pytest.mark.asyncio
async def test_ensure_test_slot_helm_release_skips_when_project_not_opted_in():
    launcher = _RecordingLauncher(_settings())

    result = await launcher.ensure_test_slot_helm_release(
        lease_doc={
            "id": "01LEASE",
            "project": "tank-operator",
            "metadata": {
                "native_slot_index": "1",
                "native_slot_name": "tank-slot-1",
            },
        },
        project_doc={
            "id": "tank-operator",
            "githubRepo": "nelsong6/tank-operator",
            "metadata": {},
        },
        repo_token="ghs_dummy",
    )

    assert result is None
    assert launcher.calls == []


@pytest.mark.asyncio
async def test_ensure_test_slot_helm_release_creates_rolebinding_secret_and_job():
    launcher = _RecordingLauncher(_settings())

    result = await launcher.ensure_test_slot_helm_release(
        lease_doc={
            "id": "01LEASE",
            "project": "tank-operator",
            "metadata": {
                "native_slot_index": "1",
                "native_slot_name": "tank-slot-1",
            },
        },
        project_doc={
            "id": "tank-operator",
            "githubRepo": "nelsong6/tank-operator",
            "metadata": {
                "native_standby_dns": {
                    "enabled": True,
                    "record_base": "tank.dev.romaine.life",
                    "slot_prefix": "tank-slot",
                    "count": 5,
                },
                "test_slot_helm": {"enabled": True},
            },
        },
        repo_token="ghs_dummy",
    )

    methods_paths = [(call["method"], call["path"]) for call in launcher.calls]
    # RoleBinding for installer SA in slot namespace, sessions namespace
    # setup, then the clone-token Secret in the runner namespace and install Job.
    assert methods_paths == [
        (
            "POST",
            "/apis/rbac.authorization.k8s.io/v1/namespaces/tank-slot-1/rolebindings",
        ),
        ("POST", "/api/v1/namespaces"),
        (
            "POST",
            "/apis/rbac.authorization.k8s.io/v1/namespaces/tank-slot-1-sessions/rolebindings",
        ),
        ("POST", "/api/v1/namespaces/glimmung-runs/secrets"),
        ("POST", "/apis/batch/v1/namespaces/glimmung-runs/jobs"),
    ]
    assert result == launcher.calls[-1]["json"]["metadata"]["name"]

    rolebinding = launcher.calls[0]["json"]
    assert rolebinding["roleRef"]["name"] == "admin"
    assert rolebinding["subjects"][0] == {
        "kind": "ServiceAccount",
        "name": "glimmung-native-runner",
        "namespace": "glimmung-runs",
    }

    secret = launcher.calls[3]["json"]
    assert secret["stringData"] == {"token": "ghs_dummy"}
    assert (
        secret["metadata"]["labels"]["glimmung.romaine.life/lease-id"]
        == "01lease"
    )

    job = launcher.calls[4]["json"]
    assert job["kind"] == "Job"
    pod_spec = job["spec"]["template"]["spec"]
    assert pod_spec["serviceAccountName"] == "glimmung-native-runner"
    init_container = pod_spec["initContainers"][0]
    assert init_container["image"] == "alpine/git:latest"
    # The init container's clone script must inject the token via the
    # mounted Secret, not bake it into the manifest where it would be
    # readable by anyone with `get pods -o yaml` permission on the
    # runner namespace.
    assert "ghs_dummy" not in init_container["command"][2]
    assert "/var/run/glim-clone/token" in init_container["command"][2]
    assert "nelsong6/tank-operator" in init_container["command"][2]
    install_container = pod_spec["containers"][0]
    assert install_container["image"] == "alpine/k8s:1.30.0"
    install_script = install_container["command"][2]
    assert "helm template 'tank-slot-1' 'k8s'" in install_script
    assert 'select(.kind != "ClusterRoleBinding" and .kind != "ClusterRole")' in install_script
    assert "kubectl apply -f -" in install_script
    env = {item["name"]: item["value"] for item in install_container["env"]}
    assert env["GLIM_SLOT_NAME"] == "tank-slot-1"
    assert env["GLIM_SLOT_INDEX"] == "1"
    assert env["GLIM_HOST"] == "tank-slot-1.tank.dev.romaine.life"


@pytest.mark.asyncio
async def test_delete_test_slot_helm_release_deletes_job_and_secret():
    launcher = _RecordingLauncher(_settings())

    await launcher.delete_test_slot_helm_release({
        "id": "01LEASE",
        "project": "tank-operator",
        "metadata": {
            "native_slot_index": "1",
            "native_slot_name": "tank-slot-1",
        },
    })

    methods_paths = [(call["method"], call["path"]) for call in launcher.calls]
    assert methods_paths == [
        (
            "DELETE",
            "/apis/batch/v1/namespaces/glimmung-runs/jobs/glim-helm-install-01lease-0",
        ),
        (
            "DELETE",
            "/api/v1/namespaces/glimmung-runs/secrets/glim-helm-clone-01lease-0",
        ),
        (
            "GET",
            "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings?labelSelector=glimmung.romaine.life/native-slot-name=tank-slot-1",
        ),
    ]


@pytest.mark.asyncio
async def test_ensure_test_slot_namespace_creates_assigned_slot_namespace():
    launcher = _RecordingLauncher(_settings())

    result = await launcher.ensure_test_slot_namespace({
        "id": "01LEASE",
        "project": "glimmung",
        "workflow": "manual-slot",
        "metadata": {
            "native_slot_index": "2",
            "native_slot_name": "glimmung-slot-2",
        },
    })

    assert result == "glimmung-slot-2"
    assert launcher.calls == [{
        "method": "POST",
        "path": "/api/v1/namespaces",
        "json": {
            "apiVersion": "v1",
            "kind": "Namespace",
            "metadata": {
                "name": "glimmung-slot-2",
                "labels": {
                    "app.kubernetes.io/managed-by": "glimmung",
                    "app.kubernetes.io/part-of": "glimmung-native-runner",
                    "glimmung.romaine.life/test-slot": "true",
                    "glimmung.romaine.life/project": "glimmung",
                    "glimmung.romaine.life/workflow": "manual-slot",
                    "glimmung.romaine.life/native-slot-name": "glimmung-slot-2",
                    "glimmung.romaine.life/native-slot-index": "2",
                    "glimmung.romaine.life/lease-id": "01lease",
                },
            },
        },
    }]


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
    # Per-job dispatch: the first selector tries the per-job
    # attempt-base + job-id combination so siblings in a parallel phase
    # don't return each other's logs. _PodLogLauncher always returns the
    # same pod, so the first GET matches and we move straight to the
    # log read.
    pod_query = launcher.calls[0]
    assert pod_query["method"] == "GET"
    assert pod_query["path"].startswith(
        "/api/v1/namespaces/glimmung-runs/pods?"
    )
    assert (
        "glimmung.romaine.life%2Fattempt-base%3D"
        "glim-01krnative0000000000000000-2"
    ) in pod_query["path"]
    assert "glimmung.romaine.life%2Fjob-id%3Dcodex-agent" in pod_query["path"]
    assert launcher.calls[1] == {
        "method": "GET",
        "path": (
            "/api/v1/namespaces/glimmung-runs/pods/glim-run-pod/log?"
            "container=codex-agent&tailLines=200&timestamps=false"
        ),
        "json": None,
    }


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

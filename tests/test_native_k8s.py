from __future__ import annotations

from datetime import UTC, datetime
from types import SimpleNamespace

from glimmung.native_k8s import _job_manifest
from glimmung.models import NativeJobSpec, NativeStepSpec, PhaseSpec


def _settings():
    return SimpleNamespace(
        native_runner_namespace="glimmung-runs",
        native_runner_service_account="glimmung-native-runner",
        native_runner_callback_base_url="http://glimmung.glimmung.svc.cluster.local:8000",
        native_runner_job_ttl_seconds=259200,
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
            "phase_inputs": {"target-ref": "main"},
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
    assert env["GLIMMUNG_INPUT_TARGET_REF"]["value"] == "main"
    assert env["GLIMMUNG_GITHUB_TOKEN_URL"]["value"] == (
        "http://glimmung.glimmung.svc.cluster.local:8000"
        "/v1/runs/ambience/01KRNATIVE0000000000000000/native/github-token"
    )
    assert env["GLIMMUNG_ATTEMPT_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == (
        "glim-01krnative-2-token"
    )

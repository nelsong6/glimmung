"""Kubernetes Job launcher for native `k8s_job` phases."""

from __future__ import annotations

import base64
import hashlib
import json
import re
import secrets
from contextlib import suppress
from datetime import UTC, datetime
from pathlib import Path
from typing import Any
from urllib.parse import urlencode

import httpx

from glimmung import runs as run_ops
from glimmung.db import Cosmos
from glimmung.models import NativeJobSpec, PhaseSpec
from glimmung.settings import Settings


class NativeLaunchError(RuntimeError):
    pass


class NativePodLogError(RuntimeError):
    pass


_DNS_LABEL_RE = re.compile(r"[^a-z0-9-]+")
_ENV_NAME_RE = re.compile(r"[^A-Za-z0-9_]+")
_K8S_NAME_RE = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")


class NativeKubernetesLauncher:
    def __init__(self, settings: Settings):
        self._settings = settings

    async def launch(
        self,
        cosmos: Cosmos,
        *,
        lease_doc: dict[str, Any],
        workflow_doc: dict[str, Any],
        phase: PhaseSpec,
    ) -> str:
        """Create the per-attempt Secret and Kubernetes Job for a native phase.

        Returns the Kubernetes Job name. Repeated calls for the same lease/run
        are idempotent: if the Secret or Job already exists, the existing
        Secret token is reused and the existing Job is treated as launched.
        """
        metadata = lease_doc.get("metadata") or {}
        run_id = str(metadata.get("run_id") or "")
        if not run_id:
            raise NativeLaunchError(f"lease {lease_doc.get('id')} has no run_id metadata")
        attempt_index = int(str(metadata.get("attempt_index") or "0"))

        found = await run_ops.read_run(cosmos, project=lease_doc["project"], run_id=run_id)
        if found is None:
            raise NativeLaunchError(f"native run {lease_doc['project']}/{run_id} not found")
        run, etag = found

        job_name = _resource_name("glim", run_id, attempt_index)
        secret_name = f"{job_name}-token"
        token = await self._ensure_attempt_secret(secret_name)
        try:
            await run_ops.set_latest_attempt_token_hash(
                cosmos,
                run=run,
                etag=etag,
                token_sha256=_sha256(token),
            )
            await self._ensure_run_namespace_access(
                lease_doc=lease_doc,
                workflow_doc=workflow_doc,
                phase=phase,
                run_id=run_id,
                attempt_index=attempt_index,
            )

            manifest = _job_manifest(
                settings=self._settings,
                lease_doc=lease_doc,
                workflow_doc=workflow_doc,
                phase=phase,
                job_name=job_name,
                secret_name=secret_name,
            )
            await self._create_job(job_name, manifest)
        except Exception:
            with suppress(Exception):
                await self.delete_attempt_secret(run_id=run_id, attempt_index=attempt_index)
            raise
        await _stamp_lease_launched(
            cosmos,
            lease_doc=lease_doc,
            job_name=job_name,
            secret_name=secret_name,
        )
        return job_name

    async def _ensure_attempt_secret(self, name: str) -> str:
        namespace = self._settings.native_runner_namespace
        path = f"/api/v1/namespaces/{namespace}/secrets"
        token = secrets.token_urlsafe(32)
        body = {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {
                "name": name,
                "namespace": namespace,
                "labels": _managed_labels(),
            },
            "type": "Opaque",
            "stringData": {"attempt-token": token},
        }
        try:
            await self._request("POST", path, json=body)
            return token
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 409:
                raise

        existing = await self._request("GET", f"{path}/{name}")
        data = existing.get("data") or {}
        encoded = data.get("attempt-token")
        if not encoded:
            raise NativeLaunchError(f"existing Secret {namespace}/{name} has no attempt-token")
        return base64.b64decode(encoded).decode("utf-8")

    async def _create_job(self, name: str, manifest: dict[str, Any]) -> None:
        namespace = self._settings.native_runner_namespace
        path = f"/apis/batch/v1/namespaces/{namespace}/jobs"
        try:
            await self._request("POST", path, json=manifest)
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 409:
                return
            raise

    async def delete_attempt_secret(self, *, run_id: str, attempt_index: int) -> None:
        """Delete the per-attempt callback token Secret.

        Idempotent: a missing Secret means terminal cleanup already happened.
        """
        namespace = self._settings.native_runner_namespace
        job_name = _resource_name("glim", run_id, attempt_index)
        secret_name = f"{job_name}-token"
        try:
            await self._request(
                "DELETE",
                f"/api/v1/namespaces/{namespace}/secrets/{secret_name}",
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 404:
                return
            raise

    async def delete_attempt_job(
        self,
        *,
        run_id: str,
        attempt_index: int,
        grace_period_seconds: int = 60,
    ) -> None:
        """Delete the native Kubernetes Job for an attempt.

        Kubernetes sends SIGTERM to the pod and enforces the requested grace
        period before killing remaining containers, which gives the runner a
        bounded final-flush window on operator-initiated aborts.
        """
        namespace = self._settings.native_runner_namespace
        job_name = _resource_name("glim", run_id, attempt_index)
        body = {
            "apiVersion": "v1",
            "kind": "DeleteOptions",
            "propagationPolicy": "Foreground",
            "gracePeriodSeconds": grace_period_seconds,
        }
        try:
            await self._request(
                "DELETE",
                f"/apis/batch/v1/namespaces/{namespace}/jobs/{job_name}",
                json=body,
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 404:
                return
            raise

    async def read_attempt_pod_logs(
        self,
        *,
        run_id: str,
        attempt_index: int,
        job_id: str,
        tail_lines: int = 200,
    ) -> dict[str, Any]:
        """Read the latest logs from the Kubernetes pod/container for an attempt."""
        namespace = self._settings.native_runner_namespace
        job_name = _resource_name("glim", run_id, attempt_index)
        pods = await self._request(
            "GET",
            f"/api/v1/namespaces/{namespace}/pods?"
            + urlencode({"labelSelector": f"job-name={job_name}"}),
        )
        if not pods.get("items"):
            pods = await self._request(
                "GET",
                f"/api/v1/namespaces/{namespace}/pods?"
                + urlencode({"labelSelector": f"batch.kubernetes.io/job-name={job_name}"}),
            )
        pod = _select_log_pod(pods.get("items") or [])
        if pod is None:
            raise NativePodLogError(f"no pod found for native job {namespace}/{job_name}")
        pod_name = str((pod.get("metadata") or {}).get("name") or "")
        if not pod_name:
            raise NativePodLogError(f"native job {namespace}/{job_name} has a pod without a name")

        container = _dns_label(job_id)
        query = urlencode({
            "container": container,
            "tailLines": str(tail_lines),
            "timestamps": "false",
        })
        text = await self._request_text(
            "GET",
            f"/api/v1/namespaces/{namespace}/pods/{pod_name}/log?{query}",
        )
        return {
            "namespace": namespace,
            "pod_name": pod_name,
            "container": container,
            "phase": str((pod.get("status") or {}).get("phase") or ""),
            "logs": text,
        }

    async def _ensure_run_namespace_access(
        self,
        *,
        lease_doc: dict[str, Any],
        workflow_doc: dict[str, Any],
        phase: PhaseSpec,
        run_id: str,
        attempt_index: int,
    ) -> None:
        metadata = lease_doc.get("metadata") or {}
        validation_namespace = _validation_namespace(run_id, attempt_index)
        access_namespaces = _access_namespaces(validation_namespace, metadata)
        labels = {
            **_managed_labels(),
            "glimmung.romaine.life/project": _label_value(str(lease_doc["project"])),
            "glimmung.romaine.life/workflow": _label_value(str(workflow_doc["name"])),
            "glimmung.romaine.life/run-id": _label_value(run_id),
            "glimmung.romaine.life/phase": _label_value(phase.name),
            "glimmung.romaine.life/attempt-index": str(attempt_index),
        }
        for namespace in access_namespaces:
            await self._ensure_namespace(namespace, labels=labels)
            await self._ensure_runner_role_binding(
                namespace,
                run_id=run_id,
                attempt_index=attempt_index,
                labels=labels,
            )

    async def _ensure_namespace(self, namespace: str, *, labels: dict[str, str]) -> None:
        body = {
            "apiVersion": "v1",
            "kind": "Namespace",
            "metadata": {
                "name": namespace,
                "labels": labels,
            },
        }
        try:
            await self._request("POST", "/api/v1/namespaces", json=body)
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 409:
                return
            raise

    async def _ensure_runner_role_binding(
        self,
        namespace: str,
        *,
        run_id: str,
        attempt_index: int,
        labels: dict[str, str],
    ) -> None:
        name = _resource_name("glim-rbac", run_id, attempt_index)
        body = {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "RoleBinding",
            "metadata": {
                "name": name,
                "namespace": namespace,
                "labels": labels,
            },
            "roleRef": {
                "apiGroup": "rbac.authorization.k8s.io",
                "kind": "ClusterRole",
                "name": self._settings.native_runner_namespace_role,
            },
            "subjects": [
                {
                    "kind": "ServiceAccount",
                    "name": self._settings.native_runner_service_account,
                    "namespace": self._settings.native_runner_namespace,
                }
            ],
        }
        try:
            await self._request(
                "POST",
                f"/apis/rbac.authorization.k8s.io/v1/namespaces/{namespace}/rolebindings",
                json=body,
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 409:
                return
            raise

    async def _request(
        self,
        method: str,
        path: str,
        *,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        token = Path(self._settings.k8s_sa_token_path).read_text(encoding="utf-8").strip()
        verify: str | bool = self._settings.k8s_ca_cert_path
        async with httpx.AsyncClient(
            base_url=self._settings.k8s_api_host.rstrip("/"),
            verify=verify,
            timeout=20.0,
            headers={"Authorization": f"Bearer {token}"},
        ) as client:
            response = await client.request(method, path, json=json)
            response.raise_for_status()
            return response.json() if response.content else {}

    async def _request_text(self, method: str, path: str) -> str:
        token = Path(self._settings.k8s_sa_token_path).read_text(encoding="utf-8").strip()
        verify: str | bool = self._settings.k8s_ca_cert_path
        async with httpx.AsyncClient(
            base_url=self._settings.k8s_api_host.rstrip("/"),
            verify=verify,
            timeout=20.0,
            headers={"Authorization": f"Bearer {token}"},
        ) as client:
            response = await client.request(method, path)
            response.raise_for_status()
            return response.text


def _job_manifest(
    *,
    settings: Settings,
    lease_doc: dict[str, Any],
    workflow_doc: dict[str, Any],
    phase: PhaseSpec,
    job_name: str,
    secret_name: str,
) -> dict[str, Any]:
    labels = {
        **_managed_labels(),
        "glimmung.romaine.life/project": _label_value(lease_doc["project"]),
        "glimmung.romaine.life/workflow": _label_value(str(workflow_doc["name"])),
        "glimmung.romaine.life/run-id": _label_value(
            str((lease_doc.get("metadata") or {}).get("run_id", "")),
        ),
        "glimmung.romaine.life/phase": _label_value(phase.name),
    }
    pod_labels = {**labels, "azure.workload.identity/use": "true"}
    metadata = lease_doc.get("metadata") or {}
    universal_env = _universal_env(
        settings=settings,
        lease_doc=lease_doc,
        workflow_doc=workflow_doc,
        phase=phase,
        secret_name=secret_name,
    )
    containers = [
        _container_for_job(
            job,
            settings=settings,
            universal_env=universal_env,
            secret_name=secret_name,
        )
        for job in phase.jobs
    ]
    if not containers:
        raise NativeLaunchError(f"phase {phase.name!r} has no native jobs")

    active_deadline = _active_deadline_seconds(phase.jobs)
    pod_spec: dict[str, Any] = {
        "serviceAccountName": settings.native_runner_service_account,
        "restartPolicy": "Never",
        "volumes": [
            {
                "name": "glimmung-attempt-token",
                "secret": {"secretName": secret_name},
            },
            {
                "name": "codex-credentials",
                "secret": {
                    "secretName": settings.native_runner_codex_credentials_secret,
                    "optional": False,
                },
            },
        ],
        "containers": [containers[-1]],
    }
    if len(containers) > 1:
        pod_spec["initContainers"] = containers[:-1]
    if active_deadline is not None:
        pod_spec["activeDeadlineSeconds"] = active_deadline

    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": job_name,
            "namespace": settings.native_runner_namespace,
            "labels": labels,
            "annotations": {
                "glimmung.romaine.life/lease-id": str(lease_doc["id"]),
                "glimmung.romaine.life/attempt-index": str(metadata.get("attempt_index", "0")),
            },
        },
        "spec": {
            "backoffLimit": 0,
            "ttlSecondsAfterFinished": settings.native_runner_job_ttl_seconds,
            "template": {
                "metadata": {
                    "labels": pod_labels,
                },
                "spec": pod_spec,
            },
        },
    }


def _container_for_job(
    job: NativeJobSpec,
    *,
    settings: Settings,
    universal_env: list[dict[str, Any]],
    secret_name: str,
) -> dict[str, Any]:
    env = [{"name": str(k), "value": str(v)} for k, v in job.env.items()]
    env.extend(universal_env)
    env.append({"name": "GLIMMUNG_JOB_ID", "value": job.id})
    container: dict[str, Any] = {
        "name": _dns_label(job.id),
        "image": job.image,
        "env": env,
        "volumeMounts": [
            {
                "name": "glimmung-attempt-token",
                "mountPath": "/var/run/glimmung",
                "readOnly": True,
            },
            {
                "name": "codex-credentials",
                "mountPath": settings.native_runner_codex_credentials_mount_path,
                "readOnly": True,
            }
        ],
    }
    if job.command:
        container["command"] = list(job.command)
    if job.args:
        container["args"] = list(job.args)
    return container


def _universal_env(
    *,
    settings: Settings,
    lease_doc: dict[str, Any],
    workflow_doc: dict[str, Any],
    phase: PhaseSpec,
    secret_name: str,
) -> list[dict[str, Any]]:
    metadata = lease_doc.get("metadata") or {}
    base_url = settings.native_runner_callback_base_url.rstrip("/")
    project = str(lease_doc["project"])
    run_id = str(metadata.get("run_id") or "")
    attempt_index = int(str(metadata.get("attempt_index") or "0"))
    validation_namespace = _validation_namespace(run_id, attempt_index)
    access_namespaces = _access_namespaces(validation_namespace, metadata)
    env: list[dict[str, Any]] = [
        {"name": "GLIMMUNG_BASE_URL", "value": base_url},
        {"name": "GLIMMUNG_PROJECT", "value": project},
        {"name": "GLIMMUNG_WORKFLOW", "value": str(workflow_doc["name"])},
        {"name": "GLIMMUNG_PHASE", "value": phase.name},
        {"name": "GLIMMUNG_RUN_ID", "value": run_id},
        {"name": "GLIMMUNG_LEASE_ID", "value": str(lease_doc["id"])},
        {"name": "GLIMMUNG_ATTEMPT_INDEX", "value": str(attempt_index)},
        {"name": "GLIMMUNG_VALIDATION_NAMESPACE", "value": validation_namespace},
        {"name": "GLIMMUNG_K8S_NAMESPACES", "value": ",".join(access_namespaces)},
        {
            "name": "GLIMMUNG_EVENTS_URL",
            "value": f"{base_url}/v1/runs/{project}/{run_id}/native/events",
        },
        {
            "name": "GLIMMUNG_STATUS_URL",
            "value": f"{base_url}/v1/runs/{project}/{run_id}/native/status",
        },
        {
            "name": "GLIMMUNG_COMPLETED_URL",
            "value": f"{base_url}/v1/runs/{project}/{run_id}/native/completed",
        },
        {
            "name": "GLIMMUNG_FAILED_URL",
            "value": f"{base_url}/v1/runs/{project}/{run_id}/native/failed",
        },
        {
            "name": "GLIMMUNG_GITHUB_TOKEN_URL",
            "value": f"{base_url}/v1/runs/{project}/{run_id}/native/github-token",
        },
        {
            "name": "GLIMMUNG_ATTEMPT_TOKEN",
            "valueFrom": {
                "secretKeyRef": {
                    "name": secret_name,
                    "key": "attempt-token",
                }
            },
        },
    ]
    for key in (
        "issue_id",
        "issue_repo",
        "issue_number",
        "issue_title",
        "issue_body",
        "work_context_id",
        "work_context_branch",
        "work_context_base_ref",
        "work_context_state",
    ):
        if key in metadata:
            env.append({"name": f"GLIMMUNG_{_env_name(key)}", "value": str(metadata[key])})
    for key in ("entrypoint_job_id", "entrypoint_step_slug"):
        if key in metadata:
            env.append({"name": f"GLIMMUNG_{_env_name(key)}", "value": str(metadata[key])})
    for key in ("artifact_refs", "context"):
        value = metadata.get(key)
        if isinstance(value, dict) and value:
            env.append({
                "name": f"GLIMMUNG_{_env_name(key)}",
                "value": json.dumps(value, sort_keys=True),
            })
    phase_inputs = metadata.get("phase_inputs") or {}
    if isinstance(phase_inputs, dict):
        for key, value in phase_inputs.items():
            env.append({"name": f"GLIMMUNG_INPUT_{_env_name(str(key))}", "value": str(value)})
    return env


def _active_deadline_seconds(jobs: list[NativeJobSpec]) -> int | None:
    values = [job.timeout_seconds for job in jobs if job.timeout_seconds is not None]
    if not values:
        return None
    return sum(values)


def _managed_labels() -> dict[str, str]:
    return {
        "app.kubernetes.io/managed-by": "glimmung",
        "app.kubernetes.io/part-of": "glimmung-native-runner",
    }


def _resource_name(prefix: str, run_id: str, attempt_index: int) -> str:
    return _dns_label(f"{prefix}-{run_id.lower()}-{attempt_index}")[:63].rstrip("-")


def _validation_namespace(run_id: str, attempt_index: int) -> str:
    return _resource_name("glim-run", run_id, attempt_index)


def _access_namespaces(validation_namespace: str, metadata: dict[str, Any]) -> list[str]:
    namespaces = [validation_namespace]
    phase_inputs = metadata.get("phase_inputs") or {}
    if isinstance(phase_inputs, dict):
        for key, value in phase_inputs.items():
            key_name = str(key)
            if key_name == "namespace" or key_name.endswith("_namespace"):
                namespace = str(value)
                if _valid_namespace(namespace) and namespace not in namespaces:
                    namespaces.append(namespace)
    return namespaces


def _valid_namespace(value: str) -> bool:
    return bool(value and len(value) <= 63 and _K8S_NAME_RE.match(value))


def _select_log_pod(pods: list[dict[str, Any]]) -> dict[str, Any] | None:
    if not pods:
        return None
    phase_rank = {"Running": 0, "Pending": 1, "Succeeded": 2, "Failed": 3}

    def key(pod: dict[str, Any]) -> tuple[int, str]:
        status = pod.get("status") or {}
        metadata = pod.get("metadata") or {}
        phase = str(status.get("phase") or "")
        started = str(status.get("startTime") or metadata.get("creationTimestamp") or "")
        return (phase_rank.get(phase, 4), started)

    return sorted(pods, key=key)[0]


def _dns_label(value: str) -> str:
    label = _DNS_LABEL_RE.sub("-", value.lower()).strip("-")
    return label or "job"


def _label_value(value: str) -> str:
    label = _dns_label(value)
    return label[:63].rstrip("-") or "value"


def _env_name(value: str) -> str:
    return _ENV_NAME_RE.sub("_", value).upper().strip("_")


def _sha256(token: str) -> str:
    return hashlib.sha256(token.encode("utf-8")).hexdigest()


async def _stamp_lease_launched(
    cosmos: Cosmos,
    *,
    lease_doc: dict[str, Any],
    job_name: str,
    secret_name: str,
) -> None:
    body = {**lease_doc}
    metadata = dict(body.get("metadata") or {})
    metadata.update({
        "native_job_name": job_name,
        "native_secret_name": secret_name,
        "native_launched_at": datetime.now(UTC).isoformat(),
    })
    body["metadata"] = metadata
    try:
        await cosmos.leases.replace_item(item=body["id"], body=body)
    except Exception:
        # Launch succeeded. Lease metadata is observational, so do not fail
        # the dispatch on a best-effort stamp.
        return

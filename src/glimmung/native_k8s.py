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
from urllib.parse import quote, urlencode

import httpx
from azure.identity.aio import DefaultAzureCredential

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
    ) -> list[str]:
        """Create one per-job Secret + one Kubernetes Job per
        `phase.jobs[*]` entry.

        Returns the list of Kubernetes Job names launched. Each Pod
        mounts its own token Secret; the presented token on a callback
        identifies WHICH sibling is reporting. Repeated calls for the
        same lease/run are idempotent: existing Secrets and Jobs are
        treated as already-launched.
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

        attempt_base = _resource_name("glim", run_id, attempt_index)
        if not phase.jobs:
            raise NativeLaunchError(f"phase {phase.name!r} has no native jobs")
        try:
            await self._ensure_run_namespace_access(
                lease_doc=lease_doc,
                workflow_doc=workflow_doc,
                phase=phase,
                run_id=run_id,
                attempt_index=attempt_index,
            )
            await self.ensure_playwright_slot(lease_doc)
            job_names: list[str] = []
            first_secret_name = ""
            for job_spec in phase.jobs:
                job_name = _job_name_for(attempt_base, job_spec.id)
                secret_name = f"{job_name}-token"
                if not first_secret_name:
                    first_secret_name = secret_name
                token = await self._ensure_attempt_secret(
                    secret_name,
                    extra_labels={
                        "glimmung.romaine.life/attempt-base": _label_value(attempt_base),
                        "glimmung.romaine.life/job-id": _label_value(job_spec.id),
                    },
                )
                run, etag = await run_ops.set_native_job_token_hash(
                    cosmos, run=run, etag=etag,
                    attempt_index=attempt_index,
                    job_id=job_spec.id,
                    token_sha256=_sha256(token),
                )
                manifest = _job_manifest(
                    settings=self._settings,
                    lease_doc=lease_doc,
                    workflow_doc=workflow_doc,
                    phase=phase,
                    job_spec=job_spec,
                    job_name=job_name,
                    secret_name=secret_name,
                    attempt_base=attempt_base,
                )
                await self._create_job(job_name, manifest)
                job_names.append(job_name)
        except Exception:
            with suppress(Exception):
                await self.delete_attempt_secret(run_id=run_id, attempt_index=attempt_index)
            with suppress(Exception):
                await self.delete_playwright_slot(lease_doc)
            raise
        await _stamp_lease_launched(
            cosmos,
            lease_doc=lease_doc,
            job_name=attempt_base,
            secret_name=first_secret_name,
        )
        return job_names

    async def ensure_playwright_slot(self, lease_doc: dict[str, Any]) -> str | None:
        """Ensure a slot-scoped Playwright server exists for an active native lease.

        The worker is keyed by the assigned native slot, not by run or job,
        so every job using the same validation slot shares exactly one
        browser server and unrelated slots stay isolated.
        """
        if not _playwright_enabled(self._settings):
            return None
        slot = _playwright_slot(lease_doc)
        if slot is None:
            return None

        namespace = self._settings.native_runner_namespace
        name = _playwright_resource_name(slot["project"], slot["slot_name"])
        labels = _playwright_labels(slot)
        deployment = _playwright_deployment(
            settings=self._settings,
            name=name,
            namespace=namespace,
            labels=labels,
        )
        service = _playwright_service(
            settings=self._settings,
            name=name,
            namespace=namespace,
            labels=labels,
        )
        try:
            await self._create_deployment(name, deployment)
            await self._create_service(name, service)
        except Exception:
            with suppress(Exception):
                await self.delete_playwright_slot_by_name(
                    project=slot["project"],
                    slot_name=slot["slot_name"],
                )
            raise
        return _playwright_ws_endpoint(self._settings, name)

    async def delete_playwright_slot(self, lease_doc: dict[str, Any]) -> None:
        slot = _playwright_slot(lease_doc)
        if slot is None:
            return
        await self.delete_playwright_slot_by_name(
            project=slot["project"],
            slot_name=slot["slot_name"],
        )

    async def delete_playwright_slot_by_name(self, *, project: str, slot_name: str) -> None:
        name = _playwright_resource_name(project, slot_name)
        namespace = self._settings.native_runner_namespace
        for path in (
            f"/apis/apps/v1/namespaces/{namespace}/deployments/{name}",
            f"/api/v1/namespaces/{namespace}/services/{name}",
        ):
            try:
                await self._request("DELETE", path)
            except httpx.HTTPStatusError as exc:
                if exc.response.status_code != 404:
                    raise

    async def reconcile_playwright_slots(self, active_native_leases: list[dict[str, Any]]) -> None:
        """Delete Playwright workers whose native slot lease is no longer active."""
        if not _playwright_enabled(self._settings):
            return
        namespace = self._settings.native_runner_namespace
        desired = {
            _playwright_resource_name(slot["project"], slot["slot_name"])
            for lease_doc in active_native_leases
            if (slot := _playwright_slot(lease_doc)) is not None
        }
        for lease_doc in active_native_leases:
            await self.ensure_playwright_slot(lease_doc)
        selector = urlencode({"labelSelector": "glimmung.romaine.life/slot-playwright=true"})
        try:
            deployments = await self._request(
                "GET",
                f"/apis/apps/v1/namespaces/{namespace}/deployments?{selector}",
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 404:
                raise
            deployments = {"items": []}
        for item in deployments.get("items") or []:
            metadata = item.get("metadata") or {}
            name = str(metadata.get("name") or "")
            if name and name not in desired:
                try:
                    await self._request(
                        "DELETE",
                        f"/apis/apps/v1/namespaces/{namespace}/deployments/{name}",
                    )
                except httpx.HTTPStatusError as exc:
                    if exc.response.status_code != 404:
                        raise
                try:
                    await self._request(
                        "DELETE",
                        f"/api/v1/namespaces/{namespace}/services/{name}",
                    )
                except httpx.HTTPStatusError as exc:
                    if exc.response.status_code != 404:
                        raise

    async def _ensure_attempt_secret(
        self, name: str, *, extra_labels: dict[str, str] | None = None,
    ) -> str:
        namespace = self._settings.native_runner_namespace
        path = f"/api/v1/namespaces/{namespace}/secrets"
        token = secrets.token_urlsafe(32)
        labels = dict(_managed_labels())
        if extra_labels:
            labels.update(extra_labels)
        body = {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {
                "name": name,
                "namespace": namespace,
                "labels": labels,
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

    async def _create_deployment(self, name: str, manifest: dict[str, Any]) -> None:
        namespace = self._settings.native_runner_namespace
        path = f"/apis/apps/v1/namespaces/{namespace}/deployments"
        try:
            await self._request("POST", path, json=manifest)
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 409:
                return
            raise

    async def _create_service(self, name: str, manifest: dict[str, Any]) -> None:
        namespace = self._settings.native_runner_namespace
        path = f"/api/v1/namespaces/{namespace}/services"
        try:
            await self._request("POST", path, json=manifest)
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 409:
                return
            raise

    async def delete_attempt_secret(self, *, run_id: str, attempt_index: int) -> None:
        """Delete every callback-token Secret belonging to an attempt.

        Under per-job dispatch each `phase.jobs[*]` has its own Secret;
        this label-selects + deletes all of them, then falls through to
        the legacy per-attempt name (`{attempt_base}-token`) for
        attempts launched by older glimmung versions.

        Idempotent: missing Secrets are treated as already cleaned up.
        """
        namespace = self._settings.native_runner_namespace
        attempt_base = _resource_name("glim", run_id, attempt_index)
        selector = urlencode({
            "labelSelector": f"glimmung.romaine.life/attempt-base={attempt_base}",
        })
        try:
            secrets_doc = await self._request(
                "GET",
                f"/api/v1/namespaces/{namespace}/secrets?{selector}",
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 404:
                raise
            secrets_doc = {"items": []}
        for item in (secrets_doc.get("items") or []):
            name = str((item.get("metadata") or {}).get("name") or "")
            if not name:
                continue
            try:
                await self._request(
                    "DELETE",
                    f"/api/v1/namespaces/{namespace}/secrets/{name}",
                )
            except httpx.HTTPStatusError as exc:
                if exc.response.status_code != 404:
                    raise
        # Back-compat: pre-fan-out attempts named the Secret by attempt
        # base directly. Labels are missing on those, so the selector
        # above won't catch them.
        try:
            await self._request(
                "DELETE",
                f"/api/v1/namespaces/{namespace}/secrets/{attempt_base}-token",
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 404:
                raise

    async def delete_attempt_job(
        self,
        *,
        run_id: str,
        attempt_index: int,
        grace_period_seconds: int = 60,
    ) -> None:
        """Delete every native Kubernetes Job belonging to an attempt.

        With job-level concurrent dispatch, an attempt fans out to N
        Jobs; this deletes them all by attempt-base label so siblings
        receive SIGTERM together. Kubernetes enforces the requested
        grace period before killing remaining containers, giving each
        runner a bounded final-flush window on operator-initiated
        aborts.
        """
        namespace = self._settings.native_runner_namespace
        attempt_base = _resource_name("glim", run_id, attempt_index)
        body = {
            "apiVersion": "v1",
            "kind": "DeleteOptions",
            "propagationPolicy": "Foreground",
            "gracePeriodSeconds": grace_period_seconds,
        }
        selector = urlencode({
            "labelSelector": f"glimmung.romaine.life/attempt-base={attempt_base}",
        })
        try:
            jobs = await self._request(
                "GET",
                f"/apis/batch/v1/namespaces/{namespace}/jobs?{selector}",
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 404:
                return
            raise
        for item in (jobs.get("items") or []):
            name = str((item.get("metadata") or {}).get("name") or "")
            if not name:
                continue
            try:
                await self._request(
                    "DELETE",
                    f"/apis/batch/v1/namespaces/{namespace}/jobs/{name}",
                    json=body,
                )
            except httpx.HTTPStatusError as exc:
                if exc.response.status_code == 404:
                    continue
                raise
        # Back-compat: pre-fan-out attempts named the Job by attempt-base
        # directly (no per-job suffix). If anything matches that legacy
        # name, delete it too — labels are missing on those.
        try:
            await self._request(
                "DELETE",
                f"/apis/batch/v1/namespaces/{namespace}/jobs/{attempt_base}",
                json=body,
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 404:
                raise

    async def delete_test_slot_namespace(self, namespace: str) -> None:
        """Delete a checked-out native test-slot namespace."""
        namespace = namespace.strip()
        if not _valid_namespace(namespace):
            raise NativeLaunchError(f"invalid test slot namespace {namespace!r}")
        body = {
            "apiVersion": "v1",
            "kind": "DeleteOptions",
            "propagationPolicy": "Foreground",
        }
        try:
            await self._request("DELETE", f"/api/v1/namespaces/{namespace}", json=body)
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 404:
                return
            raise

    async def ensure_test_slot_namespace(self, lease_doc: dict[str, Any]) -> str | None:
        """Ensure the assigned native slot namespace exists for a lease."""
        slot = _native_slot(lease_doc)
        if slot is None:
            return None
        labels = {
            **_managed_labels(),
            "glimmung.romaine.life/test-slot": "true",
            "glimmung.romaine.life/project": _label_value(slot["project"]),
            "glimmung.romaine.life/workflow": _label_value(slot["workflow"]),
            "glimmung.romaine.life/native-slot-name": _label_value(slot["slot_name"]),
        }
        if slot.get("slot_index"):
            labels["glimmung.romaine.life/native-slot-index"] = _label_value(slot["slot_index"])
        if slot.get("lease_id"):
            labels["glimmung.romaine.life/lease-id"] = _label_value(slot["lease_id"])
        await self._ensure_namespace(slot["slot_name"], labels=labels)
        return slot["slot_name"]

    async def ensure_test_slot_helm_release(
        self,
        *,
        lease_doc: dict[str, Any],
        project_doc: dict[str, Any],
        repo_token: str,
    ) -> str | None:
        """Spawn a one-shot Job that renders the project's Helm chart for a
        slot and applies it to the slot namespace.

        Glimmung looks up the project's ArgoCD Application to discover the
        chart path (spec.source.path), then constructs a `helm template`
        command using the slot-scoped values from project metadata.
        The installer Job is generic: Glimmung grants its runner service
        account temporary cluster-admin for the checked-out slot lease, then
        applies the chart exactly as rendered. App-specific rendering belongs
        in the app chart, normally behind `testEnv.enabled=true`.

        Reads `metadata.test_slot_helm` from `project_doc`. Skips silently
        when the project has not opted in or has no `github_repo`. Returns
        the Job name on success, or None when no Job was launched.
        """
        config = _test_slot_helm_config(project_doc)
        if config is None:
            return None
        slot = _native_slot(lease_doc)
        if slot is None:
            return None
        repo = str(
            project_doc.get("github_repo")
            or project_doc.get("githubRepo")
            or ""
        ).strip()
        if not repo:
            return None
        lease_id = slot.get("lease_id") or ""
        if not lease_id:
            return None

        slot_name = slot["slot_name"]
        slot_index = slot.get("slot_index") or _slot_index_from_name(slot_name)
        host = _test_slot_host(project_doc, slot_name, self._settings)
        substitutions = {
            "slot_name": slot_name,
            "slot_index": slot_index,
            "host": host,
            "project": slot["project"],
        }

        # Discover chart path from the project's ArgoCD Application.
        argocd_app = str(
            project_doc.get("argocd_app")
            or project_doc.get("argocdApp")
            or project_doc.get("name")
            or ""
        ).strip()
        chart_path = _DEFAULT_CHART_PATH
        if argocd_app:
            argo = await self._fetch_argocd_app(argocd_app)
            if argo is not None:
                chart_path = str(
                    (argo.get("spec") or {}).get("source", {}).get("path")
                    or _DEFAULT_CHART_PATH
                ).strip() or _DEFAULT_CHART_PATH

        await self._ensure_slot_admin_binding(slot_name, slot=slot)
        # Pre-create the sessions namespace and give the runner admin access
        # there too so kubectl apply can write cross-namespace manifests.
        sessions_ns = f"{slot_name}-sessions"
        await self._ensure_namespace(sessions_ns, labels={
            **_managed_labels(),
            "glimmung.romaine.life/test-slot": "true",
            "glimmung.romaine.life/native-slot-name": _label_value(slot_name),
        })
        await self._ensure_slot_admin_binding(sessions_ns, slot=slot)
        await self._ensure_installer_cluster_admin_binding(slot_name, slot=slot)

        secret_name = _test_slot_install_secret_name(lease_id)
        await self._ensure_clone_token_secret(secret_name, repo_token, slot=slot)

        job_name = _test_slot_install_job_name(lease_id)
        manifest = _test_slot_install_manifest(
            settings=self._settings,
            config=config,
            chart_path=chart_path,
            slot=slot,
            slot_index=slot_index,
            host=host,
            repo=repo,
            substitutions=substitutions,
            clone_token_secret=secret_name,
            job_name=job_name,
        )
        await self._create_job(job_name, manifest)
        return job_name

    async def delete_test_slot_helm_release(self, lease_doc: dict[str, Any]) -> None:
        """Delete the helm-install Job, clone-token Secret, and slot CRBs.

        Idempotent: missing resources are treated as already cleaned up.
        Called on lease release so a stuck or in-flight install Job from a
        previous checkout cannot block the next one.
        """
        slot = _native_slot(lease_doc)
        if slot is None:
            return
        lease_id = slot.get("lease_id") or ""
        slot_name = slot.get("slot_name") or ""
        if not lease_id:
            return
        namespace = self._settings.native_runner_namespace
        job_name = _test_slot_install_job_name(lease_id)
        secret_name = _test_slot_install_secret_name(lease_id)
        body = {
            "apiVersion": "v1",
            "kind": "DeleteOptions",
            "propagationPolicy": "Foreground",
            "gracePeriodSeconds": 30,
        }
        try:
            await self._request(
                "DELETE",
                f"/apis/batch/v1/namespaces/{namespace}/jobs/{job_name}",
                json=body,
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 404:
                raise
        try:
            await self._request(
                "DELETE",
                f"/api/v1/namespaces/{namespace}/secrets/{secret_name}",
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 404:
                raise
        if slot_name:
            await self._delete_installer_cluster_admin_binding(slot_name)

    async def _ensure_installer_cluster_admin_binding(
        self,
        slot_name: str,
        *,
        slot: dict[str, str],
    ) -> None:
        """Temporarily allow the generic installer Job to apply full charts.

        Without this, every project that emits ClusterRoleBinding or other
        cluster-scoped resources has to teach Glimmung app-specific templates.
        The binding is labelled by slot and removed on test-slot return.
        """
        name = _installer_cluster_admin_binding_name(slot_name)
        body = {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "ClusterRoleBinding",
            "metadata": {
                "name": name,
                "labels": {
                    **_managed_labels(),
                    "glimmung.romaine.life/test-slot-installer": "true",
                    "glimmung.romaine.life/project": _label_value(slot["project"]),
                    "glimmung.romaine.life/native-slot-name": _label_value(slot_name),
                },
            },
            "subjects": [
                {
                    "kind": "ServiceAccount",
                    "name": self._settings.native_runner_service_account,
                    "namespace": self._settings.native_runner_namespace,
                }
            ],
            "roleRef": {
                "apiGroup": "rbac.authorization.k8s.io",
                "kind": "ClusterRole",
                "name": "cluster-admin",
            },
        }
        if slot.get("lease_id"):
            body["metadata"]["labels"]["glimmung.romaine.life/lease-id"] = _label_value(
                slot["lease_id"]
            )
        try:
            await self._request(
                "POST",
                "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings",
                json=body,
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 409:
                raise

    async def _delete_installer_cluster_admin_binding(self, slot_name: str) -> None:
        name = _installer_cluster_admin_binding_name(slot_name)
        try:
            await self._request(
                "DELETE",
                f"/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/{name}",
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 404:
                raise

    async def _ensure_slot_admin_binding(
        self,
        slot_name: str,
        *,
        slot: dict[str, str],
    ) -> None:
        """Bind the runner SA to admin on a slot namespace so the install
        Job (which runs under that SA) can apply the rendered chart.

        Mirrors `_ensure_runner_role_binding` but keyed by lease so each
        checkout owns one binding that goes away with the namespace.
        """
        namespace = slot_name
        labels = {
            **_managed_labels(),
            "glimmung.romaine.life/test-slot": "true",
            "glimmung.romaine.life/project": _label_value(slot["project"]),
            "glimmung.romaine.life/native-slot-name": _label_value(slot_name),
        }
        if slot.get("lease_id"):
            labels["glimmung.romaine.life/lease-id"] = _label_value(slot["lease_id"])
        body = {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "RoleBinding",
            "metadata": {
                "name": "glim-test-slot-installer",
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
            if exc.response.status_code != 409:
                raise

    async def _fetch_argocd_app(self, app_name: str) -> dict[str, Any] | None:
        """Read an ArgoCD Application CRD from the cluster."""
        try:
            return await self._request(
                "GET",
                f"/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/{app_name}",
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 404:
                return None
            raise

    async def _ensure_clone_token_secret(
        self,
        name: str,
        token: str,
        *,
        slot: dict[str, str],
    ) -> None:
        """Stash a short-lived GitHub installation token in a Secret the
        install Job's init container mounts to `git clone` the project repo.

        Token TTL is ~1h (GitHub App installation tokens); the Job is
        expected to complete in seconds. Replaced on every checkout so a
        stale token from a prior lease never lingers."""
        namespace = self._settings.native_runner_namespace
        path = f"/api/v1/namespaces/{namespace}/secrets"
        labels = {
            **_managed_labels(),
            "glimmung.romaine.life/test-slot-installer": "true",
            "glimmung.romaine.life/project": _label_value(slot["project"]),
            "glimmung.romaine.life/native-slot-name": _label_value(slot["slot_name"]),
        }
        if slot.get("lease_id"):
            labels["glimmung.romaine.life/lease-id"] = _label_value(slot["lease_id"])
        body = {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {
                "name": name,
                "namespace": namespace,
                "labels": labels,
            },
            "type": "Opaque",
            "stringData": {"token": token},
        }
        try:
            await self._request("POST", path, json=body)
            return
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 409:
                raise
        # Pre-existing Secret from a prior checkout would carry a stale token;
        # replace contents so the install Job sees a fresh one.
        existing = await self._request("GET", f"{path}/{name}")
        resource_version = (
            (existing.get("metadata") or {}).get("resourceVersion") or ""
        )
        body.setdefault("metadata", {})["resourceVersion"] = str(resource_version)
        await self._request("PUT", f"{path}/{name}", json=body)

    async def reconcile_standby_dns(self, project_docs: list[dict[str, Any]]) -> None:
        """Reconcile DNSEndpoint records for projects that opt into warm native DNS."""
        desired = {
            config["name"]: config
            for project_doc in project_docs
            if (config := _standby_dns_config(project_doc, self._settings)) is not None
        }
        by_namespace: dict[str, dict[str, dict[str, Any]]] = {}
        for name, config in desired.items():
            by_namespace.setdefault(config["namespace"], {})[name] = config

        namespaces = set(by_namespace) | {
            str(getattr(self._settings, "native_standby_dns_namespace", "glimmung"))
        }
        for namespace in namespaces:
            configs = by_namespace.get(namespace, {})
            existing = await self._list_standby_dns(namespace)
            if configs:
                target = await self._standby_dns_target(configs.values())
                if not target:
                    raise NativeLaunchError("standby DNS target could not be resolved from settings or Gateway")
                for name, config in configs.items():
                    await self._upsert_standby_dns(namespace, name, _standby_dns_body(config, target))
            for name in sorted(set(existing) - set(configs)):
                await self._delete_standby_dns(namespace, name)

    async def reconcile_standby_workload_identity(
        self,
        project_docs: list[dict[str, Any]],
    ) -> None:
        """Reconcile Azure workload identity FICs for warm native slots."""
        configs = [
            config
            for project_doc in project_docs
            if (config := _standby_workload_identity_config(project_doc, self._settings)) is not None
        ]
        for config in configs:
            for credential in _standby_workload_identity_credentials(config):
                await self._upsert_federated_identity_credential(credential)

    async def reconcile_standby_entra_redirects(
        self,
        project_docs: list[dict[str, Any]],
    ) -> None:
        """Reconcile Entra SPA redirect URIs for warm native webapp slots."""
        configs = [
            config
            for project_doc in project_docs
            if (config := _standby_entra_redirect_config(project_doc, self._settings)) is not None
        ]
        for config in configs:
            await self._upsert_spa_redirect_uris(config)

    async def _standby_dns_target(self, configs: Any) -> str:
        for config in configs:
            target = str(config.get("target") or "").strip()
            if target:
                return target
        target = str(getattr(self._settings, "native_standby_dns_target", "") or "").strip()
        if target:
            return target
        gateway_namespace = getattr(
            self._settings, "native_standby_dns_gateway_namespace", "envoy-gateway-system",
        )
        gateway_name = getattr(self._settings, "native_standby_dns_gateway_name", "main")
        gateway = await self._request(
            "GET",
            f"/apis/gateway.networking.k8s.io/v1/namespaces/{gateway_namespace}/gateways/{gateway_name}",
        )
        for address in (gateway.get("status") or {}).get("addresses") or []:
            value = str(address.get("value") or "").strip()
            if value:
                return value
        return ""

    async def _list_standby_dns(self, namespace: str) -> set[str]:
        result = await self._request(
            "GET",
            f"/apis/externaldns.k8s.io/v1alpha1/namespaces/{namespace}/dnsendpoints?"
            + urlencode({"labelSelector": "glimmung.romaine.life/standby-dns=true"}),
        )
        return {
            str((item.get("metadata") or {}).get("name"))
            for item in result.get("items") or []
            if (item.get("metadata") or {}).get("name")
        }

    async def _upsert_standby_dns(
        self,
        namespace: str,
        name: str,
        body: dict[str, Any],
    ) -> None:
        path = f"/apis/externaldns.k8s.io/v1alpha1/namespaces/{namespace}/dnsendpoints"
        try:
            await self._request("POST", path, json=body)
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 409:
                raise
            existing = await self._request("GET", f"{path}/{name}")
            resource_version = str(
                ((existing.get("metadata") or {}).get("resourceVersion") or "")
            ).strip()
            if resource_version:
                body.setdefault("metadata", {})["resourceVersion"] = resource_version
            await self._request("PUT", f"{path}/{name}", json=body)

    async def _delete_standby_dns(self, namespace: str, name: str) -> None:
        try:
            await self._request(
                "DELETE",
                f"/apis/externaldns.k8s.io/v1alpha1/namespaces/{namespace}/dnsendpoints/{name}",
            )
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 404:
                return
            raise

    async def _upsert_federated_identity_credential(self, credential: dict[str, Any]) -> None:
        subscription = quote(str(credential["subscription"]), safe="")
        resource_group = quote(str(credential["resource_group"]), safe="")
        identity_name = quote(str(credential["identity_name"]), safe="")
        credential_name = quote(str(credential["credential_name"]), safe="")
        path = (
            f"/subscriptions/{subscription}/resourceGroups/{resource_group}"
            "/providers/Microsoft.ManagedIdentity/userAssignedIdentities"
            f"/{identity_name}/federatedIdentityCredentials/{credential_name}"
            "?api-version=2023-01-31"
        )
        body = {
            "properties": {
                "issuer": credential["issuer"],
                "subject": credential["subject"],
                "audiences": ["api://AzureADTokenExchange"],
            }
        }
        await self._arm_request("PUT", path, json=body)

    async def _upsert_spa_redirect_uris(self, config: dict[str, Any]) -> None:
        app = await self._resolve_entra_application(config)
        app_id = str(app.get("id") or "").strip()
        if not app_id:
            raise NativeLaunchError("Entra application response did not include object id")

        current = list(((app.get("spa") or {}).get("redirectUris")) or [])
        desired = list(config["redirect_uris"])
        reconciled = [
            uri
            for uri in current
            if uri in desired or not _is_managed_standby_redirect_uri(uri, config)
        ]
        for uri in desired:
            if uri not in reconciled:
                reconciled.append(uri)
        if reconciled == current:
            return
        await self._graph_request(
            "PATCH",
            f"/applications/{quote(app_id, safe='')}",
            json={"spa": {"redirectUris": reconciled}},
        )

    async def _resolve_entra_application(self, config: dict[str, Any]) -> dict[str, Any]:
        if config.get("application_object_id"):
            return await self._graph_request(
                "GET",
                f"/applications/{quote(str(config['application_object_id']), safe='')}",
            )
        if config.get("application_app_id"):
            app_id = _graph_filter_literal(str(config["application_app_id"]))
            return await self._graph_request("GET", f"/applications(appId='{app_id}')")
        display_name = str(config.get("display_name") or "").strip()
        if not display_name:
            raise NativeLaunchError("standby Entra redirects need an application id or display name")
        result = await self._graph_request(
            "GET",
            "/applications?"
            + urlencode({"$filter": f"displayName eq '{_graph_filter_literal(display_name)}'"}),
        )
        values = result.get("value") or []
        if len(values) != 1:
            raise NativeLaunchError(
                f"standby Entra redirect display name {display_name!r} matched {len(values)} apps"
            )
        return values[0]

    async def read_attempt_pod_logs(
        self,
        *,
        run_id: str,
        attempt_index: int,
        job_id: str,
        tail_lines: int = 200,
    ) -> dict[str, Any]:
        """Read the latest logs from the Kubernetes pod/container for one
        job within an attempt.

        Under job-level concurrent dispatch each `phase.jobs[*]` runs in
        its own Pod; we filter by both the per-attempt label and the
        per-job label so the right sibling Pod is selected.
        """
        namespace = self._settings.native_runner_namespace
        attempt_base = _resource_name("glim", run_id, attempt_index)
        per_job_name = _job_name_for(attempt_base, job_id)
        # Prefer the per-job label (post-fan-out). Fall back to legacy
        # per-attempt Job name selectors so attempts launched by older
        # glimmung versions still surface logs.
        selectors = [
            f"glimmung.romaine.life/attempt-base={attempt_base},glimmung.romaine.life/job-id={_label_value(job_id)}",
            f"job-name={per_job_name}",
            f"batch.kubernetes.io/job-name={per_job_name}",
            f"job-name={attempt_base}",
            f"batch.kubernetes.io/job-name={attempt_base}",
        ]
        pod = None
        for selector in selectors:
            pods = await self._request(
                "GET",
                f"/api/v1/namespaces/{namespace}/pods?"
                + urlencode({"labelSelector": selector}),
            )
            pod = _select_log_pod(pods.get("items") or [])
            if pod is not None:
                break
        if pod is None:
            raise NativePodLogError(
                f"no pod found for native job {namespace}/{per_job_name}",
            )
        pod_name = str((pod.get("metadata") or {}).get("name") or "")
        if not pod_name:
            raise NativePodLogError(
                f"native job {namespace}/{per_job_name} has a pod without a name",
            )

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
            }
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

    async def _arm_request(
        self,
        method: str,
        path: str,
        *,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        credential = DefaultAzureCredential()
        try:
            token = await credential.get_token("https://management.azure.com/.default")
            async with httpx.AsyncClient(
                base_url="https://management.azure.com",
                timeout=20.0,
                headers={
                    "Authorization": f"Bearer {token.token}",
                    "Content-Type": "application/json",
                },
            ) as client:
                response = await client.request(method, path, json=json)
                response.raise_for_status()
                return response.json() if response.content else {}
        finally:
            await credential.close()

    async def _graph_request(
        self,
        method: str,
        path: str,
        *,
        json: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        credential = DefaultAzureCredential()
        try:
            token = await credential.get_token("https://graph.microsoft.com/.default")
            async with httpx.AsyncClient(
                base_url="https://graph.microsoft.com/v1.0",
                timeout=20.0,
                headers={
                    "Authorization": f"Bearer {token.token}",
                    "Content-Type": "application/json",
                },
            ) as client:
                response = await client.request(method, path, json=json)
                response.raise_for_status()
                return response.json() if response.content else {}
        finally:
            await credential.close()


def _job_manifest(
    *,
    settings: Settings,
    lease_doc: dict[str, Any],
    workflow_doc: dict[str, Any],
    phase: PhaseSpec,
    job_spec: NativeJobSpec,
    job_name: str,
    secret_name: str,
    attempt_base: str,
) -> dict[str, Any]:
    """Build a Kubernetes Job manifest for one `NativeJobSpec` in a phase.

    Each `phase.jobs[*]` becomes its own k8s Job; siblings run in
    parallel and report back independently via `job_id` on the
    completion callback. The shared per-attempt token Secret is mounted
    in every sibling Pod.
    """
    labels = {
        **_managed_labels(),
        "glimmung.romaine.life/project": _label_value(lease_doc["project"]),
        "glimmung.romaine.life/workflow": _label_value(str(workflow_doc["name"])),
        "glimmung.romaine.life/run-id": _label_value(
            str((lease_doc.get("metadata") or {}).get("run_id", "")),
        ),
        "glimmung.romaine.life/phase": _label_value(phase.name),
        "glimmung.romaine.life/attempt-base": _label_value(attempt_base),
        "glimmung.romaine.life/job-id": _label_value(job_spec.id),
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
    container = _container_for_job(
        job_spec,
        settings=settings,
        universal_env=universal_env,
        secret_name=secret_name,
    )
    active_deadline = _active_deadline_seconds([job_spec])
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
        "containers": [container],
    }
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
                "glimmung.romaine.life/job-id": str(job_spec.id),
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
            },
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
    run_callback_token = str(metadata.get("run_callback_token") or "").strip()
    issue_number = str(metadata.get("issue_number") or "").strip()
    run_number = str(metadata.get("run_display_number") or metadata.get("run_number") or "").strip()
    public_run_path = (
        f"/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native"
        if issue_number and run_number
        else f"/v1/run-callbacks/{run_callback_token}/native"
    )
    slot_name = str(metadata.get("native_slot_name") or "").strip()
    lease_ref = slot_name or f"{project}/leases/{lease_doc.get('leaseNumber') or lease_doc.get('lease_number') or 'active'}"
    attempt_index = int(str(metadata.get("attempt_index") or "0"))
    validation_namespace = _validation_namespace(run_id, attempt_index)
    access_namespaces = _access_namespaces(validation_namespace, metadata)
    env: list[dict[str, Any]] = [
        {"name": "GLIMMUNG_BASE_URL", "value": base_url},
        {"name": "GLIMMUNG_PROJECT", "value": project},
        {"name": "GLIMMUNG_WORKFLOW", "value": str(workflow_doc["name"])},
        {"name": "GLIMMUNG_PHASE", "value": phase.name},
        {"name": "GLIMMUNG_RUN_REF", "value": f"{project}#{issue_number}/runs/{run_number}" if issue_number and run_number else ""},
        {"name": "GLIMMUNG_LEASE_REF", "value": lease_ref},
        {"name": "GLIMMUNG_ATTEMPT_INDEX", "value": str(attempt_index)},
        {"name": "GLIMMUNG_VALIDATION_NAMESPACE", "value": validation_namespace},
        {"name": "GLIMMUNG_K8S_NAMESPACES", "value": ",".join(access_namespaces)},
        {
            "name": "GLIMMUNG_EVENTS_URL",
            "value": f"{base_url}{public_run_path}/events",
        },
        {
            "name": "GLIMMUNG_STATUS_URL",
            "value": f"{base_url}{public_run_path}/status",
        },
        {
            "name": "GLIMMUNG_COMPLETED_URL",
            "value": f"{base_url}{public_run_path}/completed",
        },
        {
            "name": "GLIMMUNG_FAILED_URL",
            "value": f"{base_url}{public_run_path}/failed",
        },
        {
            "name": "GLIMMUNG_GITHUB_TOKEN_URL",
            "value": f"{base_url}{public_run_path}/github-token",
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
    if _playwright_enabled(settings):
        slot_name = str(metadata.get("native_slot_name") or "").strip()
        if slot_name:
            service_name = _playwright_resource_name(project, slot_name)
            endpoint = _playwright_ws_endpoint(settings, service_name)
            env.extend([
                {"name": "GLIMMUNG_PLAYWRIGHT_WS_ENDPOINT", "value": endpoint},
                {"name": "PLAYWRIGHT_WS_ENDPOINT", "value": endpoint},
                {"name": "PW_TEST_CONNECT_WS_ENDPOINT", "value": endpoint},
            ])
    for key in (
        "issue_repo",
        "issue_number",
        "issue_title",
        "issue_body",
        "native_slot_index",
        "native_slot_name",
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


def _playwright_enabled(settings: Settings) -> bool:
    return bool(getattr(settings, "native_runner_playwright_enabled", True))


def _native_slot(lease_doc: dict[str, Any]) -> dict[str, str] | None:
    metadata = lease_doc.get("metadata") or {}
    slot_name = str(metadata.get("native_slot_name") or "").strip()
    if not slot_name:
        return None
    return {
        "project": str(lease_doc.get("project") or "").strip(),
        "workflow": str(lease_doc.get("workflow") or "").strip(),
        "slot_name": slot_name,
        "slot_index": str(metadata.get("native_slot_index") or "").strip(),
        "lease_id": str(lease_doc.get("id") or "").strip(),
    }


def _playwright_slot(lease_doc: dict[str, Any]) -> dict[str, str] | None:
    return _native_slot(lease_doc)


def _playwright_resource_name(project: str, slot_name: str) -> str:
    base = _dns_label(f"glim-pw-{project}-{slot_name}")
    if len(base) <= 63:
        return base
    digest = hashlib.sha256(base.encode("utf-8")).hexdigest()[:8]
    return f"{base[:54].rstrip('-')}-{digest}"


def _playwright_labels(slot: dict[str, str]) -> dict[str, str]:
    labels = {
        **_managed_labels(),
        "glimmung.romaine.life/slot-playwright": "true",
        "glimmung.romaine.life/project": _label_value(slot["project"]),
        "glimmung.romaine.life/native-slot-name": _label_value(slot["slot_name"]),
    }
    if slot.get("slot_index"):
        labels["glimmung.romaine.life/native-slot-index"] = _label_value(slot["slot_index"])
    if slot.get("lease_id"):
        labels["glimmung.romaine.life/lease-id"] = _label_value(slot["lease_id"])
    return labels


def _playwright_ws_endpoint(settings: Settings, service_name: str) -> str:
    namespace = settings.native_runner_namespace
    port = int(getattr(settings, "native_runner_playwright_port", 3000))
    return f"ws://{service_name}.{namespace}.svc.cluster.local:{port}/"


def _playwright_deployment(
    *,
    settings: Settings,
    name: str,
    namespace: str,
    labels: dict[str, str],
) -> dict[str, Any]:
    port = int(getattr(settings, "native_runner_playwright_port", 3000))
    return {
        "apiVersion": "apps/v1",
        "kind": "Deployment",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": labels,
        },
        "spec": {
            "replicas": 1,
            "selector": {"matchLabels": {"app.kubernetes.io/name": name}},
            "template": {
                "metadata": {
                    "labels": {
                        **labels,
                        "app.kubernetes.io/name": name,
                    },
                },
                "spec": {
                    "containers": [
                        {
                            "name": "playwright",
                            "image": getattr(
                                settings,
                                "native_runner_playwright_image",
                                "romainecr.azurecr.io/agent-container:latest",
                            ),
                            "command": [
                                "npx",
                                "playwright",
                                "run-server",
                                "--host",
                                "0.0.0.0",
                                "--port",
                                str(port),
                            ],
                            "ports": [{"name": "ws", "containerPort": port}],
                            "env": [
                                {
                                    "name": "PLAYWRIGHT_BROWSERS_PATH",
                                    "value": "/ms-playwright",
                                }
                            ],
                            "resources": {
                                "requests": {
                                    "cpu": getattr(
                                        settings,
                                        "native_runner_playwright_cpu_request",
                                        "100m",
                                    ),
                                    "memory": getattr(
                                        settings,
                                        "native_runner_playwright_memory_request",
                                        "256Mi",
                                    ),
                                },
                                "limits": {
                                    "cpu": getattr(
                                        settings,
                                        "native_runner_playwright_cpu_limit",
                                        "1000m",
                                    ),
                                    "memory": getattr(
                                        settings,
                                        "native_runner_playwright_memory_limit",
                                        "1Gi",
                                    ),
                                },
                            },
                        }
                    ]
                },
            },
        },
    }


def _playwright_service(
    *,
    settings: Settings,
    name: str,
    namespace: str,
    labels: dict[str, str],
) -> dict[str, Any]:
    port = int(getattr(settings, "native_runner_playwright_port", 3000))
    return {
        "apiVersion": "v1",
        "kind": "Service",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": labels,
        },
        "spec": {
            "selector": {"app.kubernetes.io/name": name},
            "ports": [
                {
                    "name": "ws",
                    "port": port,
                    "targetPort": "ws",
                }
            ],
        },
    }


def _standby_dns_config(project_doc: dict[str, Any], settings: Settings) -> dict[str, Any] | None:
    metadata = project_doc.get("metadata") or {}
    if not isinstance(metadata, dict):
        return None
    raw = metadata.get("native_standby_dns") or metadata.get("nativeStandbyDns")
    if not isinstance(raw, dict) or raw.get("enabled") is not True:
        return None

    project = str(project_doc.get("name") or project_doc.get("id") or "").strip()
    record_base = str(raw.get("record_base") or raw.get("recordBase") or "").strip().strip(".")
    if not project or not record_base:
        return None

    count_raw = raw.get("count")
    try:
        count = int(str(count_raw)) if count_raw is not None else int(
            getattr(settings, "native_runner_project_concurrency", 5)
        )
    except (TypeError, ValueError):
        count = 0
    if count < 1:
        return None

    ttl_raw = raw.get("ttl") or raw.get("record_ttl") or raw.get("recordTtl")
    try:
        ttl = int(str(ttl_raw)) if ttl_raw is not None else int(
            getattr(settings, "native_standby_dns_default_ttl", 300)
        )
    except (TypeError, ValueError):
        ttl = 300

    return {
        "project": project,
        "name": _dns_label(f"native-standby-{project}")[:63].rstrip("-"),
        "namespace": str(
            raw.get("namespace") or getattr(settings, "native_standby_dns_namespace", "glimmung")
        ),
        "slot_prefix": project.strip("."),
        "record_base": record_base,
        "count": count,
        "ttl": max(1, ttl),
        "target": str(raw.get("target") or "").strip(),
    }


def _standby_dns_body(config: dict[str, Any], target: str) -> dict[str, Any]:
    project = str(config["project"])
    return {
        "apiVersion": "externaldns.k8s.io/v1alpha1",
        "kind": "DNSEndpoint",
        "metadata": {
            "name": config["name"],
            "namespace": config["namespace"],
            "labels": {
                **_managed_labels(),
                "glimmung.romaine.life/project": _label_value(project),
                "glimmung.romaine.life/standby-dns": "true",
            },
        },
        "spec": {
            "endpoints": [
                {
                    "dnsName": f"{config['slot_prefix']}-{slot}.{config['record_base']}",
                    "recordType": "A",
                    "recordTTL": config["ttl"],
                    "targets": [target],
                }
                for slot in range(1, int(config["count"]) + 1)
            ],
        },
    }


def _standby_workload_identity_config(
    project_doc: dict[str, Any],
    settings: Settings,
) -> dict[str, Any] | None:
    metadata = project_doc.get("metadata") or {}
    if not isinstance(metadata, dict):
        return None
    raw = (
        metadata.get("native_standby_workload_identity")
        or metadata.get("nativeStandbyWorkloadIdentity")
    )
    if not isinstance(raw, dict) or raw.get("enabled") is not True:
        return None

    project = str(project_doc.get("name") or project_doc.get("id") or "").strip()
    if not project:
        return None

    subscription = str(
        raw.get("subscription")
        or raw.get("subscription_id")
        or raw.get("subscriptionId")
        or getattr(settings, "native_standby_identity_subscription", "")
    ).strip()
    resource_group = str(
        raw.get("resource_group")
        or raw.get("resourceGroup")
        or getattr(settings, "native_standby_identity_resource_group", "infra")
    ).strip()
    issuer = str(
        raw.get("issuer") or getattr(settings, "native_standby_identity_issuer", "")
    ).strip()
    if not issuer:
        issuer = _workload_identity_issuer_from_token(settings)
    if not subscription or not resource_group or not issuer:
        return None

    credentials = raw.get("credentials")
    if not isinstance(credentials, list) or not credentials:
        return None

    count_raw = raw.get("count")
    if count_raw is None:
        dns_raw = metadata.get("native_standby_dns") or metadata.get("nativeStandbyDns") or {}
        if isinstance(dns_raw, dict):
            count_raw = dns_raw.get("count")
    try:
        count = int(str(count_raw)) if count_raw is not None else int(
            getattr(settings, "native_runner_project_concurrency", 5)
        )
    except (TypeError, ValueError):
        count = 0
    if count < 1:
        return None

    slot_prefix = project
    if not slot_prefix:
        return None

    return {
        "project": project,
        "subscription": subscription,
        "resource_group": resource_group,
        "issuer": issuer,
        "slot_prefix": slot_prefix,
        "count": count,
        "credentials": credentials,
    }


def _standby_workload_identity_credentials(config: dict[str, Any]) -> list[dict[str, str]]:
    desired: list[dict[str, str]] = []
    project = str(config["project"])
    for slot_index in range(1, int(config["count"]) + 1):
        slot_name = f"{config['slot_prefix']}-{slot_index}"
        namespace = slot_name
        values = {
            "project": project,
            "slot": str(slot_index),
            "slot_index": str(slot_index),
            "slot_name": slot_name,
            "namespace": namespace,
        }
        for item in config["credentials"]:
            if not isinstance(item, dict):
                continue
            identity_name = str(
                item.get("identity_name") or item.get("identityName") or ""
            ).strip()
            subject_template = str(item.get("subject") or "").strip()
            if not identity_name or not subject_template:
                continue
            credential_template = str(
                item.get("credential_name")
                or item.get("credentialName")
                or "{slot_name}"
            ).strip()
            desired.append({
                "subscription": str(config["subscription"]),
                "resource_group": str(config["resource_group"]),
                "identity_name": _format_standby_template(identity_name, values),
                "credential_name": _format_standby_template(credential_template, values),
                "issuer": str(config["issuer"]),
                "subject": _format_standby_template(subject_template, values),
            })
    return desired


def _standby_entra_redirect_config(
    project_doc: dict[str, Any],
    settings: Settings,
) -> dict[str, Any] | None:
    metadata = project_doc.get("metadata") or {}
    if not isinstance(metadata, dict):
        return None
    raw = (
        metadata.get("native_standby_entra_redirects")
        or metadata.get("nativeStandbyEntraRedirects")
    )
    if not isinstance(raw, dict) or raw.get("enabled") is not True:
        return None

    application_object_id = str(
        raw.get("application_object_id") or raw.get("applicationObjectId") or ""
    ).strip()
    application_app_id = str(
        raw.get("application_app_id")
        or raw.get("applicationAppId")
        or raw.get("client_id")
        or raw.get("clientId")
        or ""
    ).strip()
    if not application_object_id and not application_app_id:
        application_app_id = str(getattr(settings, "entra_test_client_id", "") or "").strip()
    display_name = str(raw.get("display_name") or raw.get("displayName") or "").strip()
    if not application_object_id and not application_app_id and not display_name:
        return None

    dns_config = _standby_dns_config(project_doc, settings)
    redirect_uris = _standby_entra_redirect_uris(raw, dns_config)
    if not redirect_uris:
        return None
    config = {
        "application_object_id": application_object_id,
        "application_app_id": application_app_id,
        "display_name": display_name,
        "redirect_uris": redirect_uris,
    }
    if dns_config is not None:
        config["managed_slot_prefix"] = dns_config["slot_prefix"]
        config["managed_record_base"] = dns_config["record_base"]
    return config


def _standby_entra_redirect_uris(
    raw: dict[str, Any],
    dns_config: dict[str, Any] | None,
) -> list[str]:
    wanted: list[str] = []
    for key in ("redirect_uris", "redirectUris"):
        values = raw.get(key)
        if isinstance(values, list):
            wanted.extend(str(value) for value in values)

    if not wanted and dns_config is not None:
        wanted.extend(
            f"https://{dns_config['slot_prefix']}-{slot}.{dns_config['record_base']}/"
            for slot in range(1, int(dns_config["count"]) + 1)
        )

    normalized: list[str] = []
    seen: set[str] = set()
    for uri in wanted:
        value = _normalize_redirect_uri(uri)
        if value and value not in seen:
            normalized.append(value)
            seen.add(value)
    return normalized


def _normalize_redirect_uri(uri: str) -> str:
    value = str(uri).strip()
    if not value:
        return ""
    if "://" not in value:
        value = f"https://{value}"
    if not value.endswith("/"):
        value = f"{value}/"
    return value


def _is_managed_standby_redirect_uri(uri: str, config: dict[str, Any]) -> bool:
    slot_prefix = str(config.get("managed_slot_prefix") or "").strip()
    record_base = str(config.get("managed_record_base") or "").strip()
    if not slot_prefix or not record_base:
        return False
    value = _normalize_redirect_uri(uri)
    prefix = f"https://{slot_prefix}-"
    suffix = f".{record_base}/"
    if not value.startswith(prefix) or not value.endswith(suffix):
        return False
    slot = value[len(prefix):-len(suffix)]
    return slot.isdigit() and int(slot) > 0


def _graph_filter_literal(value: str) -> str:
    return value.replace("'", "''")


def _format_standby_template(template: str, values: dict[str, str]) -> str:
    try:
        return template.format(**values)
    except (KeyError, IndexError, ValueError):
        return template


def _workload_identity_issuer_from_token(settings: Settings) -> str:
    with suppress(Exception):
        token = Path(settings.k8s_sa_token_path).read_text(encoding="utf-8").strip()
        parts = token.split(".")
        if len(parts) < 2:
            return ""
        payload = parts[1] + ("=" * (-len(parts[1]) % 4))
        data = json.loads(base64.urlsafe_b64decode(payload.encode("ascii")))
        return str(data.get("iss") or "").strip()
    return ""


def _resource_name(prefix: str, run_id: str, attempt_index: int) -> str:
    return _dns_label(f"{prefix}-{run_id.lower()}-{attempt_index}")[:63].rstrip("-")


def _job_name_for(attempt_base: str, job_id: str) -> str:
    """Per-job k8s Job name.

    `attempt_base` is the per-attempt resource name (e.g.
    `glim-{run_id}-{attempt_index}`); appending the job_id slug lets
    multiple Jobs coexist in one attempt for parallel dispatch.

    Truncated to the 63-char DNS-label limit. When attempt_base is
    long enough that there's no room for a meaningful job suffix, the
    suffix is hashed to keep the name unique.
    """
    suffix = _dns_label(job_id)
    candidate = f"{attempt_base}-{suffix}"
    if len(candidate) <= 63:
        return candidate.rstrip("-")
    # Reserve 8 chars for a stable hash so collision-free with siblings.
    short_hash = hashlib.sha256(job_id.encode("utf-8")).hexdigest()[:8]
    head_room = 63 - 1 - 8  # for "-" + hash
    return f"{attempt_base[:head_room].rstrip('-')}-{short_hash}"


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


_DEFAULT_INSTALLER_IMAGE = "alpine/k8s:1.30.0"
_DEFAULT_CHART_PATH = "k8s"


def _test_slot_helm_config(project_doc: dict[str, Any]) -> dict[str, Any] | None:
    """Read the test-slot helm-install config off a project doc.

    Returns None when the project hasn't opted in.  The `values` dict
    carries helm --set overrides keyed by helm path; values may contain
    `{slot_name}`, `{host}`, `{slot_index}`, `{project}` placeholders
    which are substituted at install time. Glimmung always adds
    `testEnv.enabled=true` so app charts own their test-environment details.
    """
    metadata = project_doc.get("metadata") or {}
    if not isinstance(metadata, dict):
        return None
    raw = metadata.get("test_slot_helm") or metadata.get("testSlotHelm")
    if not isinstance(raw, dict) or raw.get("enabled") is not True:
        return None

    values = raw.get("values") or {}
    if not isinstance(values, dict):
        values = {}
    values = {"testEnv.enabled": "true", **values}

    image = str(
        raw.get("installer_image")
        or raw.get("installerImage")
        or _DEFAULT_INSTALLER_IMAGE
    ).strip()

    git_ref = str(raw.get("git_ref") or raw.get("gitRef") or "").strip()

    return {
        "values": values,
        "installer_image": image or _DEFAULT_INSTALLER_IMAGE,
        "git_ref": git_ref,
    }


def _test_slot_host(
    project_doc: dict[str, Any],
    slot_name: str,
    settings: Settings,
) -> str:
    """Per-slot ingress hostname, derived from `native_standby_dns.record_base`.

    Empty string when the project doesn't carry a record base — render
    commands that don't reference `{host}` still work in that case."""
    standby = _standby_dns_config(project_doc, settings)
    if standby is None:
        return ""
    record_base = str(standby.get("record_base") or "").strip(".")
    if not record_base:
        return ""
    return f"{slot_name}.{record_base}"


def _slot_index_from_name(slot_name: str) -> str:
    """Trailing integer of a slot name (e.g. `tank-slot-3` → `3`).

    Falls back to empty string for non-conforming names; render commands
    that need a slot index will fail loudly inside the Job in that case."""
    suffix = slot_name.rsplit("-", 1)[-1]
    return suffix if suffix.isdigit() else ""


def _test_slot_install_job_name(lease_id: str) -> str:
    return _resource_name("glim-helm-install", lease_id, 0)


def _test_slot_install_secret_name(lease_id: str) -> str:
    return _resource_name("glim-helm-clone", lease_id, 0)


def _installer_cluster_admin_binding_name(slot_name: str) -> str:
    return _resource_name("glim-test-slot-installer", slot_name, 0)


def _format_substitutions(template: str, substitutions: dict[str, str]) -> str:
    """`{slot_name}`-style substitution that tolerates missing keys.

    Used on both the render command and on values flowing into the
    install Pod's env, so a typo in a project's render config surfaces
    in the Pod logs rather than masking as a Python KeyError on the
    glimmung side."""
    try:
        return template.format(**substitutions)
    except (KeyError, IndexError, ValueError):
        return template


def _test_slot_install_manifest(
    *,
    settings: Settings,
    config: dict[str, Any],
    chart_path: str,
    slot: dict[str, str],
    slot_index: str,
    host: str,
    repo: str,
    substitutions: dict[str, str],
    clone_token_secret: str,
    job_name: str,
) -> dict[str, Any]:
    """Build the helm-install Job manifest for a checkout.

    Pod shape:
      - `clone` initContainer: alpine/git, clones the project repo.
      - `install` main container: alpine/k8s (carries helm + kubectl + yq),
        runs `helm template {chart_path}` with slot-scoped values from
        project metadata, strips ClusterRole/ClusterRoleBinding objects
        (created directly by the glimmung pod), and pipes the rest into
        `kubectl apply` against the slot namespace.
    """
    namespace = settings.native_runner_namespace
    slot_name = slot["slot_name"]
    project = slot["project"]
    lease_id = slot.get("lease_id") or ""
    git_ref = str(config.get("git_ref") or "").strip()

    # Build --set flags from project metadata values dict.
    set_flags = " ".join(
        f"--set {_shell_quote(k + '=' + _format_substitutions(str(v), substitutions))}"
        for k, v in (config.get("values") or {}).items()
    )

    labels = {
        **_managed_labels(),
        "glimmung.romaine.life/test-slot-installer": "true",
        "glimmung.romaine.life/project": _label_value(project),
        "glimmung.romaine.life/native-slot-name": _label_value(slot_name),
    }
    if lease_id:
        labels["glimmung.romaine.life/lease-id"] = _label_value(lease_id)
    if slot.get("slot_index"):
        labels["glimmung.romaine.life/native-slot-index"] = _label_value(slot["slot_index"])

    clone_script = (
        "set -eu\n"
        f"GIT_REF={_shell_quote(git_ref)}\n"
        "TOKEN=\"$(cat /var/run/glim-clone/token)\"\n"
        f"REPO_URL=\"https://x-access-token:${{TOKEN}}@github.com/{repo}.git\"\n"
        "if [ -n \"$GIT_REF\" ]; then\n"
        "  git clone --depth 1 --branch \"$GIT_REF\" \"$REPO_URL\" /workspace\n"
        "else\n"
        "  git clone --depth 1 \"$REPO_URL\" /workspace\n"
        "fi\n"
    )
    install_script = (
        "set -eu\n"
        "cd /workspace\n"
        f"helm template {_shell_quote(slot_name)} {_shell_quote(chart_path)}"
        f" --namespace {_shell_quote(slot_name)} {set_flags}"
        " | kubectl apply -f -\n"
    )
    pod_spec: dict[str, Any] = {
        "serviceAccountName": settings.native_runner_service_account,
        "restartPolicy": "Never",
        "volumes": [
            {"name": "workspace", "emptyDir": {}},
            {
                "name": "glim-clone",
                "secret": {
                    "secretName": clone_token_secret,
                    "defaultMode": 0o400,
                },
            },
        ],
        "initContainers": [
            {
                "name": "clone",
                "image": "alpine/git:latest",
                "command": ["sh", "-c", clone_script],
                "volumeMounts": [
                    {"name": "workspace", "mountPath": "/workspace"},
                    {
                        "name": "glim-clone",
                        "mountPath": "/var/run/glim-clone",
                        "readOnly": True,
                    },
                ],
            }
        ],
        "containers": [
            {
                "name": "install",
                "image": config["installer_image"],
                "command": ["sh", "-c", install_script],
                "env": [
                    {"name": "GLIM_SLOT_NAME", "value": slot_name},
                    {"name": "GLIM_SLOT_INDEX", "value": str(slot_index)},
                    {"name": "GLIM_HOST", "value": host},
                    {"name": "GLIM_PROJECT", "value": project},
                ],
                "volumeMounts": [
                    {"name": "workspace", "mountPath": "/workspace"},
                ],
            }
        ],
    }

    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": job_name,
            "namespace": namespace,
            "labels": labels,
            "annotations": {
                "glimmung.romaine.life/lease-id": lease_id,
                "glimmung.romaine.life/native-slot-name": slot_name,
            },
        },
        "spec": {
            "backoffLimit": 1,
            "ttlSecondsAfterFinished": settings.native_runner_job_ttl_seconds,
            "template": {
                "metadata": {"labels": labels},
                "spec": pod_spec,
            },
        },
    }


def _shell_quote(value: str) -> str:
    """Single-quote `value` for safe substitution into the install script.

    Avoids pulling in `shlex` for one call site. The render command and
    slot name flow into a `sh -c` script body, so any apostrophe-class
    metacharacter has to be escaped explicitly."""
    return "'" + value.replace("'", "'\"'\"'") + "'"


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

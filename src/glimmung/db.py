import logging
from typing import Any

from azure.cosmos.aio import ContainerProxy, CosmosClient, DatabaseProxy
from azure.identity.aio import DefaultAzureCredential

from glimmung.settings import Settings

log = logging.getLogger(__name__)


class Cosmos:
    """Owns the Cosmos client and configured containers.

    Containers are created on startup if they don't exist. Auth is via
    workload identity (DefaultAzureCredential picks up the AKS-injected
    federated token; glimmung-identity has Data Contributor on the
    glimmung database).
    """

    def __init__(self, settings: Settings):
        self._settings = settings
        self._credential: DefaultAzureCredential | None = None
        self._client: CosmosClient | None = None
        self._db: DatabaseProxy | None = None
        self.projects: ContainerProxy | None = None
        self.workflows: ContainerProxy | None = None
        self.hosts: ContainerProxy | None = None
        self.leases: ContainerProxy | None = None
        self.runs: ContainerProxy | None = None
        self.run_events: ContainerProxy | None = None
        self.locks: ContainerProxy | None = None
        self.signals: ContainerProxy | None = None
        self.issues: ContainerProxy | None = None
        self.playbooks: ContainerProxy | None = None
        self.portfolio_elements: ContainerProxy | None = None
        self.legacy_prs: ContainerProxy | None = None
        self.reports: ContainerProxy | None = None
        self.report_versions: ContainerProxy | None = None

    async def start(self) -> None:
        # Database + containers are pre-created by tofu/ (per-app pattern,
        # matches kill-me / plant-agent / my-homepage). The runtime pod
        # workload identity only has data-plane Cosmos perms — control-plane
        # CREATE DATABASE / CREATE CONTAINER is not allowed. get_*_client
        # returns a proxy without making any API call; write operations
        # against it use the workload identity's data-plane permissions.
        self._credential = DefaultAzureCredential()
        self._client = CosmosClient(self._settings.cosmos_endpoint, credential=self._credential)
        self._db = self._client.get_database_client(self._settings.cosmos_database)
        self.projects = self._db.get_container_client("projects")
        self.workflows = self._db.get_container_client("workflows")
        self.hosts = self._db.get_container_client("hosts")
        self.leases = self._db.get_container_client("leases")
        self.runs = self._db.get_container_client("runs")
        self.run_events = self._db.get_container_client("run_events")
        self.locks = self._db.get_container_client("locks")
        self.signals = self._db.get_container_client("signals")
        self.issues = self._db.get_container_client("issues")
        self.playbooks = self._db.get_container_client("playbooks")
        self.portfolio_elements = self._db.get_container_client("portfolio_elements")
        self.legacy_prs = self._db.get_container_client("prs")
        self.reports = self._db.get_container_client("reports")
        self.report_versions = self._db.get_container_client("report_versions")
        log.info(
            "cosmos clients ready: projects, workflows, hosts, leases, runs, run_events, locks, "
            "signals, issues, playbooks, portfolio_elements, reports, report_versions"
        )

    async def stop(self) -> None:
        if self._client is not None:
            await self._client.close()
        if self._credential is not None:
            await self._credential.close()


async def query_all(container: ContainerProxy, query: str, parameters: list[dict[str, Any]] | None = None) -> list[dict[str, Any]]:
    items: list[dict[str, Any]] = []
    async for item in container.query_items(query=query, parameters=parameters or []):
        items.append(item)
    return items

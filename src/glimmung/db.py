import logging
from typing import Any

from azure.cosmos.aio import ContainerProxy, CosmosClient, DatabaseProxy
from azure.identity.aio import DefaultAzureCredential

from glimmung.settings import Settings

log = logging.getLogger(__name__)


class Cosmos:
    """Owns the Cosmos client and the six containers.

    Containers are created on startup if they don't exist. Auth is via
    workload identity (DefaultAzureCredential picks up the AKS-injected
    federated token; the infra-shared-identity has Data Contributor at
    the account scope, granted in infra-bootstrap/tofu/cosmos-serverless.tf).
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
        self.locks: ContainerProxy | None = None

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
        self.locks = self._db.get_container_client("locks")
        log.info("cosmos clients ready: projects, workflows, hosts, leases, runs, locks")

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

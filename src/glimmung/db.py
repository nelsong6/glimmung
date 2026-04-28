import logging
from typing import Any

from azure.cosmos import PartitionKey
from azure.cosmos.aio import ContainerProxy, CosmosClient, DatabaseProxy
from azure.identity.aio import DefaultAzureCredential

from glimmung.settings import Settings

log = logging.getLogger(__name__)


class Cosmos:
    """Owns the Cosmos client and the three containers.

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
        self.hosts: ContainerProxy | None = None
        self.leases: ContainerProxy | None = None

    async def start(self) -> None:
        self._credential = DefaultAzureCredential()
        self._client = CosmosClient(self._settings.cosmos_endpoint, credential=self._credential)
        self._db = await self._client.create_database_if_not_exists(self._settings.cosmos_database)

        self.projects = await self._db.create_container_if_not_exists(
            id="projects",
            partition_key=PartitionKey(path="/name"),
        )
        self.hosts = await self._db.create_container_if_not_exists(
            id="hosts",
            partition_key=PartitionKey(path="/name"),
        )
        self.leases = await self._db.create_container_if_not_exists(
            id="leases",
            partition_key=PartitionKey(path="/project"),
        )
        log.info("cosmos containers ready: projects, hosts, leases")

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

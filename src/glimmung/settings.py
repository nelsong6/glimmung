from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    # Bare env var names — matches the tank-operator pattern. A repo-prefixed
    # prefix collides with the K8s-injected {SERVICE_NAME}_PORT env vars
    # because the service name and the prefix match.
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    port: int = 8000
    log_level: str = "INFO"

    cosmos_endpoint: str = "https://infra-cosmos-serverless.documents.azure.com:443/"
    cosmos_database: str = "glimmung"

    # GitHub App credentials. Mounted from K8s Secret synced from KV by ESO.
    # If unset (e.g. local dev without GH integration), webhook + dispatch
    # endpoints are disabled but lease primitives still work.
    github_app_id: str = ""
    github_app_installation_id: str = ""
    github_app_private_key: str = ""
    github_webhook_secret: str = ""

    # Shared admin bearer token for /v1/projects, /v1/hosts, and other
    # operator-only endpoints. Heartbeat/release endpoints are NOT
    # bearer-auth'd — the lease_id (a ULID) is unguessable, so possessing
    # it is itself the capability ("capability-based security").
    admin_token: str = ""

    # Default lease TTL — heartbeat must arrive within this window or the
    # sweep job reclaims the host.
    lease_default_ttl_seconds: int = 900

    # Sweep job cadence
    sweep_interval_seconds: int = 60


@lru_cache
def get_settings() -> Settings:
    return Settings()

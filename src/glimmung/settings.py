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

    # Entra ID for admin endpoints (POST /v1/projects, /v1/hosts). Pattern
    # matches tank-operator: validate Entra access token via JWKS, check
    # audience = entra_client_id, check email in allowed_emails. CLI mints
    # via `az account get-access-token --resource <client-id>`. Heartbeat
    # and release endpoints stay unauthenticated — the lease_id is the
    # capability (ULID, unguessable).
    entra_client_id: str = ""
    allowed_emails: str = ""  # comma-separated; auth.py splits + lowercases

    # Default lease TTL — heartbeat must arrive within this window or the
    # sweep job reclaims the host. 1h covers spirelens's 30-minute
    # implementation phase comfortably; phases heartbeat at start to reset
    # the clock between transitions.
    lease_default_ttl_seconds: int = 3600

    # Sweep job cadence
    sweep_interval_seconds: int = 60


@lru_cache
def get_settings() -> Settings:
    return Settings()

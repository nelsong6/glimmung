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

    # Kubernetes SA-token alternative for admin endpoints — for in-cluster
    # callers (tank-operator, future agents) that already carry a projected
    # SA token. auth.py validates the bearer via TokenReview against the
    # cluster API server; the pod's own SA needs `system:auth-delegator`
    # (see k8s/templates/auth-delegator.yaml). Same RBAC primitive the
    # mcp-* deployments use; no kube-rbac-proxy sidecar because glimmung is
    # publicly exposed and can't bind upstream-loopback-only.
    #
    # Allowlist is comma-separated `<namespace>/<sa-name>` pairs. Entries:
    #   - tank-operator/tank-operator: orchestrator pod (long-running)
    #   - tank-operator-sessions/claude-session: per-session pods where
    #     Claude runs; needed for in-conversation registration of new
    #     projects/workflows/hosts (the original CLI flow requires Entra
    #     login on a workstation, which session pods don't have).
    # Empty disables the path; Entra remains the only admin auth.
    k8s_sa_allowlist: str = "tank-operator/tank-operator,tank-operator-sessions/claude-session"
    k8s_api_host: str = "https://kubernetes.default.svc"
    # Standard projected paths inside the pod; overridable for tests.
    k8s_sa_token_path: str = "/var/run/secrets/kubernetes.io/serviceaccount/token"
    k8s_ca_cert_path: str = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

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

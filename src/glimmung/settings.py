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
    #   - mcp-glimmung/mcp-glimmung: the mcp-glimmung MCP server (sibling
    #     to mcp-k8s/mcp-github/etc. in tank-operator's monorepo). Calls
    #     us as itself; the session-pod identity stops at its kube-rbac-
    #     proxy gate, so the MCP server's own SA is what reaches us.
    # Empty disables the path; Entra remains the only admin auth.
    k8s_sa_allowlist: str = "tank-operator/tank-operator,tank-operator-sessions/claude-session,mcp-glimmung/mcp-glimmung"
    k8s_api_host: str = "https://kubernetes.default.svc"
    # Standard projected paths inside the pod; overridable for tests.
    k8s_sa_token_path: str = "/var/run/secrets/kubernetes.io/serviceaccount/token"
    k8s_ca_cert_path: str = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

    # Default lease TTL — sweep_expired reclaims any host whose
    # lastHeartbeat is older than this. Sized to be comfortably longer
    # than the longest project workflow's worst-case wall time so callers
    # can rely on workflow_run.completed (or an explicit release call)
    # for cleanup without per-phase heartbeats. spirelens runs ~85 min
    # worst case (max(test-plan, implementation) + verification + build/
    # prep/screenshot overhead); 4h leaves headroom for webhook latency
    # and slow-path scenarios.
    lease_default_ttl_seconds: int = 14400

    # Sweep job cadence
    sweep_interval_seconds: int = 60

    # Browser launch target for attended tank-operator sessions. Glimmung
    # passes canonical ids as URL params; tank-operator creates the session on
    # its own origin so the user's tank auth cookie applies.
    tank_operator_base_url: str = "https://tank.romaine.life"

    # Public dashboard origin. Used when syndicating thin pointers into
    # external systems like GitHub while keeping rich PR context in Glimmung.
    glimmung_base_url: str = "https://glimmung.romaine.life"

    # Native Kubernetes runner. `k8s_job` workflow phases do not consume
    # registered self-hosted GitHub runner hosts; they use virtual capacity
    # gates backed by leases, then Glimmung creates a Kubernetes Job in this
    # namespace. Jobs receive callback context plus a per-attempt token from
    # a short-lived Secret.
    native_runner_namespace: str = "glimmung-runs"
    native_runner_service_account: str = "glimmung-native-runner"
    native_runner_callback_base_url: str = "http://glimmung.glimmung.svc.cluster.local:8000"
    native_runner_project_concurrency: int = 5
    native_runner_global_concurrency: int = 5
    native_runner_job_ttl_seconds: int = 259200
    native_runner_namespace_role: str = "admin"

    # Private Blob container for native runner logs and future evidence.
    # Glimmung serves these back to users; callers should persist logical
    # blob:// refs rather than raw storage URLs.
    artifacts_storage_account: str = "romaineglimmungartifacts"
    artifacts_container: str = "artifacts"


@lru_cache
def get_settings() -> Settings:
    return Settings()

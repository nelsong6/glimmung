# glimmung

Agent dispatcher. Owns a queue of "agent runs" and assigns them to a pool of self-hosted runner machines based on each machine's capabilities.

> *The Glimmung scanned the assembled list of beings he had summoned. From a thousand worlds they had come, each with a craft to contribute.*
> — paraphrased from Philip K. Dick, *Galactic Pot-Healer*

## What it does

The agentic-CI pattern (issue label → run Claude on a host with GUI / state requirements → produce a PR) repeats across multiple projects (spirelens, ambience, tank-operator, …) and no off-the-shelf CI provider models the actual constraint well: **stateful, host-pinned, scarce-resource leases.** Glimmung owns that primitive.

GitHub Actions remains the execution layer (dumb runner). Glimmung owns the queue, the lease lifecycle, the dashboard, and the cross-project orchestration.

Full design: [issue #1](https://github.com/nelsong6/glimmung/issues/1).

## Layout

```
src/glimmung/         # FastAPI app, Cosmos client, lease lifecycle, GH webhook
k8s/                  # Helm chart, ArgoCD-synced from main
tofu/                 # Cosmos database + containers (per-app pattern)
Dockerfile            # builds the python wheel
.github/workflows/    # build + ACR push + chart bump + tofu plan/apply
```

## API

### Lease lifecycle (capability-based — possessing the lease_id is the auth)

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/lease`                       | Request a host. Returns lease + host (or pending lease if no capacity). |
| POST   | `/v1/lease/{id}/heartbeat`        | Keep the lease alive. `?project=<name>` required. |
| POST   | `/v1/lease/{id}/release`          | Release the lease. Idempotent. |
| GET    | `/v1/state`                       | Snapshot: hosts + pending leases + active leases. |
| GET    | `/healthz`                        | Liveness/readiness. |

### Admin (Entra ID — JWKS-validated bearer token)

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/projects`                    | Register/upsert a project. |
| GET    | `/v1/projects`                    | List projects. |
| POST   | `/v1/hosts`                       | Register/update a host. |

### GitHub webhook

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/webhook/github`              | Receives `issues` events from the configured GitHub App. |

The webhook handler:
1. Verifies `X-Hub-Signature-256` against `GITHUB_WEBHOOK_SECRET`
2. Ignores events other than `issues`
3. Looks up the project by `repository.full_name`
4. If the issue's labels include the project's `triggerLabel` (or the action is `labeled` with that label), creates a pending lease
5. If a host is free and matches the project's `defaultRequirements`, fires `workflow_dispatch` against the project's configured workflow

## Storage

Cosmos DB NoSQL on the shared `infra-cosmos-serverless` account. Database `glimmung`, three containers (all pre-created by [`tofu/db.tf`](tofu/db.tf)):

- `projects` (partition key `/name`)
- `hosts` (partition key `/name`)
- `leases` (partition key `/project`)

Runtime pod auth via the `infra-shared-identity` workload identity, which has `Cosmos DB Built-in Data Contributor` at the account scope (granted in [`infra-bootstrap/tofu/cosmos-serverless.tf`](https://github.com/nelsong6/infra-bootstrap/blob/main/tofu/cosmos-serverless.tf)). Container clients are obtained via `get_*_client` (no API call); reads/writes use the data-plane permissions.

## One-time setup

KV keys consumed by glimmung:

| KV secret                          | Source                                  |
|---|---|
| `github-app-id`                    | shared with `mcp-github`                |
| `github-app-installation-id`       | shared with `mcp-github`                |
| `github-app-private-key`           | shared with `mcp-github`                |
| `github-webhook-secret`            | shared (the GitHub App webhook secret)  |
| `glimmung-oauth-client-id`         | created by `glimmung/tofu/oauth.tf`     |
| `glimmung-oauth-allowed-emails`    | created by `glimmung/tofu/oauth.tf`     |

The two glimmung-specific secrets are managed by [`tofu/oauth.tf`](tofu/oauth.tf) — `tofu apply` creates the Entra app reg and writes both keys. The `github-*` keys already exist.

GitHub App webhook URL must be set in the App's settings page to:

```
https://glimmung.romaine.life/v1/webhook/github
```

with `Issues` checked under "Subscribe to events". The shared `github-webhook-secret` already in KV is what glimmung verifies signatures against.

## Admin auth (CLI)

Mint an Entra access token for the glimmung audience:

```sh
CLIENT_ID=$(az keyvault secret show --vault-name romaine-kv --name glimmung-oauth-client-id --query value -o tsv)
TOKEN=$(az account get-access-token --resource "$CLIENT_ID" --query accessToken -o tsv)
```

## Registering a project

```sh
curl -sS -X POST https://glimmung.romaine.life/v1/projects \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "spirelens",
    "github_repo": "nelsong6/spirelens",
    "workflow_filename": "issue-agent.yaml",
    "trigger_label": "issue-agent",
    "default_requirements": {"apps": ["sts2"]}
  }'
```

## Registering a host

```sh
curl -sS -X POST https://glimmung.romaine.life/v1/hosts \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "win-a",
    "capabilities": {"os": "windows", "apps": ["sts2"]}
  }'
```

## Running locally

```sh
pip install -e ".[dev]"
az login                                 # for DefaultAzureCredential
COSMOS_ENDPOINT=https://infra-cosmos-serverless.documents.azure.com:443/ \
  python -m glimmung
```

## Phases

1. **Phase 1** ✓ — lease primitive, sweep job, Cosmos backend.
2. **Phase 2** ✓ — GitHub App webhook receiver, `workflow_dispatch` firing, ingress at `glimmung.romaine.life`.
3. **Phase 2.5** — Migrate spirelens `issue-agent.yaml` to consume glimmung leases.
4. **Phase 3** — Dashboard with SSE driven by Cosmos Change Feed.
5. **Phase 4** — Migrate ambience, tank-operator agent flows.

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
src/glimmung/         # FastAPI app, Cosmos client, lease lifecycle
k8s/                  # Helm chart, ArgoCD-synced from main
Dockerfile            # multi-stage, builds the python wheel
.github/workflows/    # build + ACR push + chart bump
```

## API (Phase 1)

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/lease`                       | Request a host. Returns lease + host (or pending lease if no capacity matches). |
| POST   | `/v1/lease/{id}/heartbeat`        | Keep the lease alive. `?project=<name>` required. |
| POST   | `/v1/lease/{id}/release`          | Release the lease. Idempotent. |
| GET    | `/v1/state`                       | Snapshot: hosts + pending leases + active leases. |
| POST   | `/v1/hosts`                       | Register/update a host. Body: `{name, capabilities, drained?}`. Idempotent. |
| GET    | `/healthz`                        | Liveness/readiness. |

## Storage

Cosmos DB NoSQL on the shared `infra-cosmos-serverless` account. Database `glimmung`, three containers:

- `projects` (partition key `/name`)
- `hosts` (partition key `/name`)
- `leases` (partition key `/project`)

Pod auth via the `infra-shared-identity` workload identity (`Cosmos DB Built-in Data Contributor` at the account scope, granted in [`infra-bootstrap/tofu/cosmos-serverless.tf`](https://github.com/nelsong6/infra-bootstrap/blob/main/tofu/cosmos-serverless.tf)). Database + containers are auto-created by the app on startup.

## Running locally

```sh
pip install -e ".[dev]"
az login                                 # for DefaultAzureCredential
GLIMMUNG_COSMOS_ENDPOINT=https://infra-cosmos-serverless.documents.azure.com:443/ \
  python -m glimmung
```

## Phases

1. **Phase 1 (this scaffold)** — lease primitive, sweep job, internal-only. Test via `kubectl port-forward`.
2. **Phase 2** — GitHub App + webhook receiver + `workflow_dispatch` firing. Migrate spirelens `issue-agent.yaml`. Add ingress + bearer auth.
3. **Phase 3** — Dashboard with SSE driven by Cosmos Change Feed.
4. **Phase 4** — Migrate ambience, tank-operator agent flows.

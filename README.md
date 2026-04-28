# glimmung

Agent dispatcher. Owns a queue of "agent runs" and assigns them to a pool of self-hosted runner machines based on each machine's capabilities.

> *The Glimmung scanned the assembled list of beings he had summoned. From a thousand worlds they had come, each with a craft to contribute.*
> — paraphrased from Philip K. Dick, *Galactic Pot-Healer*

## What it does

The agentic-CI pattern (issue label → run Claude on a host with GUI / state requirements → produce a PR) repeats across multiple projects (spirelens, ambience, tank-operator, …) and no off-the-shelf CI provider models the actual constraint well: **stateful, host-pinned, scarce-resource leases.** Glimmung owns that primitive.

GitHub Actions remains the execution layer (dumb runner). Glimmung owns the queue, the lease lifecycle, the dashboard, and the cross-project orchestration.

Full design + intent: [issue #1](https://github.com/nelsong6/glimmung/issues/1).

## Mental model

```
Project ──< Workflow ──< Lease ──── Host (venue)
(repo)     (one trigger,           (matched via
            one yaml,               capabilities)
            one set of
            requirements)
```

- **Project** = a repo (e.g. `spirelens`), declares the github_repo only.
- **Workflow** = a specific automation pattern under a project (e.g. `issue-agent`), declares its trigger label, workflow filename, and required capabilities.
- **Lease** = "this workflow wants to run, and got assigned this host." Lifecycle: pending → active → released | expired.
- **Host** = a virtual venue we invent (named whatever — typically the GHA runner-route label so the workflow's `runs-on` works directly). Capabilities are our own vocabulary; the only thing glimmung uses to decide what venues match what workflows.

The "agent" — Claude Code, Codex, whatever runs inside the workflow — is opaque to glimmung. We dispatch a venue to a workflow; the workflow runs an agent on it.

## Layout

```
src/glimmung/         # FastAPI app, Cosmos client, lease lifecycle, GH webhook
frontend/             # Vite + React dashboard (live SSE state, MSAL admin)
k8s/                  # Helm chart, ArgoCD-synced from main
tofu/                 # Cosmos database + containers + Entra app reg
Dockerfile            # multi-stage: node frontend build → python backend
.github/workflows/    # build + ACR push + chart bump + tofu plan/apply
```

## API

### Lease lifecycle (capability auth via lease_id; ULID is unguessable)

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/lease`                       | Acquire (`{project, workflow?, requirements, metadata}`). Returns lease + host (or pending lease if no capacity). |
| GET    | `/v1/lease/{id}?project=<name>`   | Read a lease. Used by consumer workflows for the verify-lease step. |
| POST   | `/v1/lease/{id}/heartbeat`        | Keep the lease alive. `?project=<name>` required. |
| POST   | `/v1/lease/{id}/release`          | Release the lease. Idempotent. |
| GET    | `/v1/state`                       | Snapshot: hosts + workflows + projects + pending + active leases. |
| GET    | `/v1/events`                      | Server-Sent Events stream — yields `{event: "state", data: <snapshot>}` every 2s. |
| GET    | `/v1/config`                      | Public — `{entra_client_id, authority}` for SPA MSAL bootstrap. |
| GET    | `/healthz`                        | Liveness/readiness. |

### Admin (Entra ID JWKS-validated bearer token; email allowlist gate)

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/projects`                    | Register/upsert a project (`{name, github_repo}`). |
| GET    | `/v1/projects`                    | List projects. |
| POST   | `/v1/workflows`                   | Register/upsert a workflow under a project. |
| GET    | `/v1/workflows`                   | List workflows. |
| POST   | `/v1/hosts`                       | Register/update a host. |

### GitHub webhook

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/webhook/github`              | Receives `issues` and `workflow_run` events. |

The handler:

1. Verifies `X-Hub-Signature-256` against `GITHUB_WEBHOOK_SECRET`.
2. **`issues`** → look up project by `repository.full_name`, find the workflow whose `trigger_label` matches (action=labeled with that label, or action=opened/reopened with the label already on the issue), atomic-acquire a host via optimistic CAS on `_etag`, fire `workflow_dispatch` with `{lease_id, host, issue_number, ...}`.
3. **`workflow_run.completed`** → pull lease_id back out of `workflow_run.inputs`, look up project by repo, call `release()`. Belt-and-suspenders alongside any in-workflow release step. Idempotent.
4. Other events → ignore.

## Storage

Cosmos DB NoSQL on the shared `infra-cosmos-serverless` account. Database `glimmung`, four containers (all pre-created by [`tofu/db.tf`](tofu/db.tf)):

- `projects` (partition key `/name`)
- `workflows` (partition key `/project`)
- `hosts` (partition key `/name`)
- `leases` (partition key `/project`)

Runtime pod auth via the `infra-shared-identity` workload identity, which has `Cosmos DB Built-in Data Contributor` at the account scope (granted in [`infra-bootstrap/tofu/cosmos-serverless.tf`](https://github.com/nelsong6/infra-bootstrap/blob/main/tofu/cosmos-serverless.tf)). Container clients are obtained via `get_*_client` (no API call); reads/writes use the data-plane permissions. CREATE DATABASE / CREATE CONTAINER is control-plane and runs only via tofu under the app SP.

## Lock semantics

Optimistic concurrency on the host doc's `_etag`. Acquire reads matching candidates, sorts by `lastUsedAt` (NULLs first → bin-pack toward unused venues), tries each via `replace_item(match_condition=IfNotModified)`. 412 PreconditionFailed → try the next. Bounded retry; loop terminates after exhausting candidates.

Release paths:
- **Fast**: workflow's own release step (if it has one).
- **Safety net**: `workflow_run.completed` webhook handler. Covers UI-cancellation, runner-died, network blips mid-step.
- **Backstop**: 15-min sweep on stale heartbeat (`ttl_seconds`-driven; default 1h).

## One-time setup

KV keys consumed by glimmung:

| KV secret                          | Source                                                                       |
|---|---|
| `glimmung-github-app-id`           | dedicated GitHub App (created by hand; one App = one webhook URL, can't co-tenant) |
| `glimmung-github-app-installation-id` | same                                                                      |
| `glimmung-github-app-private-key`  | same                                                                         |
| `glimmung-github-webhook-secret`   | same                                                                         |
| `glimmung-oauth-client-id`         | created by `glimmung/tofu/oauth.tf` (Entra app reg)                          |
| `glimmung-oauth-allowed-emails`    | same                                                                         |

The Entra side is fully tofu-managed. The GitHub App is created via the GitHub UI — one webhook URL per App means glimmung needs its own (the shared `github-app-*` keys still serve mcp-github / diagrams). Configure the App with:

- Webhook URL: `https://glimmung.romaine.life/v1/webhook/github`
- Subscribe to events: **Issues**, **Workflow runs**
- Permissions: Actions `read+write`, Issues `read`, Metadata `read`
- Install on whichever repos use it

## Admin (dashboard)

Visit https://glimmung.romaine.life/, click **sign in** (top right) — MSAL popup against the `glimmung-oauth` Entra app. Once signed in (email must be in the allowlist), click **admin** to reveal the registration tabs:

- **Register project** → name + github_repo
- **Register workflow** → project (dropdown), name, filename, ref, trigger_label, requirements
- **Register host** → name + capabilities

The dashboard's left sidebar shows projects expandable into their workflows. Clicking a workflow filters the lease tables and highlights eligible hosts.

## Running locally

```sh
pip install -e ".[dev]"
az login                                 # for DefaultAzureCredential
COSMOS_ENDPOINT=https://infra-cosmos-serverless.documents.azure.com:443/ \
  python -m glimmung
```

For the frontend:

```sh
cd frontend && npm install && npm run dev
# proxies /v1/* to localhost:8000
```

## Phases

1. **Phase 1** ✓ — lease primitive, sweep job, Cosmos backend.
2. **Phase 2** ✓ — GitHub App webhook receiver, `workflow_dispatch` firing, ingress at `glimmung.romaine.life`, Entra ID auth on admin endpoints.
3. **Phase 3** ✓ — Dashboard with SSE, project side pane, workflow as first-class abstraction, MSAL sign-in + admin panel.
4. **Phase 2.5** ✓ — Migrate spirelens `issue-agent.yaml` to consume glimmung leases. (Numbered out of order; see [glimmung issue #2](https://github.com/nelsong6/glimmung/issues/2) for the build order that actually happened.)
5. **Phase 4** — Runner-grounding (verify GHA runner is online before dispatching), dashboard cancel/preempt, migrate ambience + tank-operator agent flows.

# glimmung

Go service for issue-driven agentic development. Glimmung stores projects,
workflows, issues, runs, leases, touchpoints, and signals in Cosmos DB; serves the
Vite + React dashboard; and coordinates native Kubernetes jobs.

> *The Glimmung scanned the assembled list of beings he had summoned. From a thousand worlds they had come, each with a craft to contribute.*
> — paraphrased from Philip K. Dick, *Galactic Pot-Healer*

## What it does

The agentic development pattern (issue -> bounded agent run -> verification ->
review report / PR) repeats across multiple projects, and off-the-shelf CI
systems do not model the orchestration cleanly. Glimmung owns the queue, the
database-backed workflow shape, the run/lease lifecycle, the callback surface,
the verify-loop decision engine, the dashboard, and the signal bus.

Native Kubernetes jobs are the execution layer for managed workflow phases.
Apps should not register app-specific GitHub runner pools or keep repo-backed
workflow files as the runtime source of truth.

Full design + intent: [issue #1](https://github.com/nelsong6/glimmung/issues/1).

## Mental model

```
Project -> Workflow -> Issue -> Run -> Phase/Job -> RunReport
                         \        \
                          \        -> Lease + callback token
                           -> Touchpoint / PR review surface
```

- **Project** = a repo (e.g. `spirelens`), declares the github_repo only.
- **Workflow** = a database-backed automation shape under a project. Dispatch
  reads the Workflow row from Cosmos: phases, native jobs, PR policy, budget,
  and requirements. Omitted phase kinds default to `k8s_job`; registered
  phases must use `k8s_job`.
- **Issue** = the canonical Glimmung issue row. GitHub Issues may still feed
  external backlog/tracker workflows, but the live run loop is issue-row based.
- **Run** = durable execution record for one issue/workflow invocation. Runs
  hold attempts, phase state, evidence refs, cost, terminal decision, and
  callback-token metadata.
- **Lease** = native capacity claim for a `k8s_job` run phase. Lease records are
  claimed only when native capacity is assigned, heartbeat/release through
  callback-token APIs, and complete through the native run completion callback.

Workflow registration is an admin/control-plane operation. Consumer repos do
not need `.glimmung/workflows/<name>.yaml` files for runtime dispatch; changing
repo files has no effect unless an operator explicitly writes a new Workflow
registration into Cosmos. The upstream-sync helper is an import convenience for
older desired-state flows, not the runtime source of truth.

The "agent" — Claude Code, Codex, whatever runs inside the workflow — is opaque to glimmung. We dispatch a venue to a workflow; the workflow runs an agent on it.

For larger feature work, Glimmung separates planning context from execution:

```
Epic -> Playbook -> ordered Entries -> Issue -> Run -> RunReport/evidence -> next Entry
```

- **Epic** = durable feature context: why, goal, constraints, non-goals, success criteria.
- **Playbook** = executable ordered plan: entries, dependencies, gates, concurrency, dispatch state.

The initial relationship is intentionally 1:1: one Epic owns one Playbook.
See [Epics and Playbooks](docs/epics-and-playbooks.md) for the object
boundary and follow-up implementation surface.
See [Quality Timeframes](docs/quality-timeframes.md) for the default
long-term engineering quality bar used when planning substantial work.
See
[Touchpoints, RunReports, And Playbook Integration](docs/touchpoints-runreports-playbooks.md)
for the review surface, per-run audit report, and integration-strategy
vocabulary.

For frontend repos that need a review surface after the app already exists,
use the reusable [Design Portfolio Bootstrap](docs/design-portfolio-bootstrap.md)
process and Playbook template.

For reusable frontend review work, treat each repo's UI package as the source
of truth and use the design portfolio route as the operator review surface.
See [Repo UI Packages And Design Portfolios](docs/ui-package-design-portfolios.md).

## Layout

```
cmd/glimmung-go/      # Go HTTP entrypoint used by the production image
cmd/glimmung-agent/   # Go ops CLI for agent jobs and validation previews
internal/             # Go domain/server/store packages
frontend/             # Vite + React dashboard (live SSE state, MSAL admin)
k8s/                  # Helm chart, ArgoCD-synced from main
tofu/                 # Cosmos database + containers + Entra app reg
Dockerfile            # multi-stage: node frontend build -> Go backend
.github/workflows/    # build + ACR push + chart bump + tofu plan/apply
```

The retired Python app and root Python test suite have been removed. The app
runtime, local dev path, repo-local ops CLI, and default CI authority are Go
plus the Vite dashboard.

## Contribution checklist

When adding or changing a human/operator-facing HTTP endpoint, update the
matching surface in the same PR:

- Add or update the frontend affordance when the action belongs in the
  dashboard.
- Add or update the matching tool in
  [`nelsong6/mcp-glimmung`](https://github.com/nelsong6/mcp-glimmung) when the
  action should be available to LLM/session callers.
- When an HTTP request schema changes, update the MCP tool signature,
  docstring, and payload tests in the same rollout. A server-side rejection is
  not enough; stale MCP schemas are still advertised to running sessions.
- For MCP server renames/removals, follow the rollout sequence in
  [MCP Surface Rollout](docs/mcp-surface-rollout.md) so stale sessions have a
  clear restart path.
- If the endpoint is intentionally system-only (webhooks, lease lifecycle,
  run callbacks, health/config/events), call that out in the PR so the HTTP
  API and MCP surface do not drift silently.

## API

The Go route registration in
[`internal/server/server.go`](internal/server/server.go) is the active HTTP
surface. [`internal/server/route_inventory_test.go`](internal/server/route_inventory_test.go)
keeps that route list explicit; the tables below summarize the operator-facing
surface.

### Lease lifecycle

| Method | Path                              | Purpose |
|---|---|---|
| GET    | `/v1/lease-callbacks/{callback_token}` | Read the public lease by callback token. Used by runner clients. |
| POST   | `/v1/lease-callbacks/{callback_token}/heartbeat` | Keep the lease alive. |
| POST   | `/v1/lease-callbacks/{callback_token}/release` | Release the lease. Idempotent. |
| POST   | `/v1/leases/cancel`               | Cancel a lease by public ref. Admin-auth guarded. |
| PATCH  | `/v1/leases/ttl`                  | Update a claimed lease TTL by public ref. Admin-auth guarded. |
| PATCH  | `/v1/test-slots/default-ttl`      | Set or reset the default TTL for generated test-slot leases globally or for one project. Admin-auth guarded. |
| GET    | `/v1/state`                       | Snapshot: projects, workflows, active leases, test environments, and waiting test-slot requests. |
| GET    | `/v1/events`                      | Server-Sent Events stream — yields `{event: "state", data: <snapshot>}` every 2s. |
| GET    | `/v1/config`                      | Public — `{entra_client_id, authority}` for SPA MSAL bootstrap. |
| GET    | `/healthz`                        | Liveness/readiness. |

### Admin (Entra ID JWKS-validated bearer token, OR cluster SA token; email or `<ns>/<sa>` allowlist gate)

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/projects`                    | Register/upsert a project (`{name, github_repo}`). |
| GET    | `/v1/projects`                    | List projects. |
| PATCH  | `/v1/projects/{project}/test-environments/count` | Set native test-slot capacity and reconcile configured preliminary resources, including auth redirect URIs and workload identities. |
| POST   | `/v1/test-slots/checkout`          | Lease an available test slot chosen by Glimmung; runtime activation may continue asynchronously. |
| POST   | `/v1/test-slots/return`            | Return a test-slot lease; runtime cleanup may continue asynchronously before the slot is available again. |
| POST   | `/v1/test-slots/extend`            | Extend a claimed test-slot lease TTL without tearing down the leased runtime. |
| POST   | `/v1/projects/{project}/test-environments/{slot_name}/repair` | Admin repair for error or stale lifecycle slot states without changing queue size. |
| POST   | `/v1/workflows`                   | Register/upsert a workflow under a project. |
| GET    | `/v1/workflows`                   | List workflows. |
| POST   | `/v1/playbooks`                   | Create a draft Playbook for a coordinated batch of issue specs. |
| GET    | `/v1/playbooks`                   | List Playbooks, optionally filtered by project. |
| GET    | `/v1/playbooks/{project}/{id}`    | Inspect a Playbook. |
| POST   | `/v1/playbooks/{project}/{id}/run` | Advance a Playbook: create ready entry Issues and dispatch their Runs through the canonical run path. |
| POST   | `/v1/playbooks/{project}/{id}/entries/{entry_id}/gate` | Set or clear a manual Playbook entry gate; optionally advances the Playbook after clearing. |
| GET    | `/v1/issues`                      | List Glimmung issues across registered projects. |
| GET    | `/v1/issues/by-number/{project}/{issue_number}` | Issue detail by canonical project issue number. |
| POST   | `/v1/runs/dispatch`               | UI/API-initiated dispatch (`{project, issue_number, workflow_name?}`); per-issue lock-serialized. |
| GET    | `/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/report` | Factual RunReport for one Run: attempts, cost, validation URL, screenshot markdown, and terminal status. |
| GET    | `/v1/touchpoints`                 | Touchpoint index across registered projects (GitHub PR syndication metadata + linked Issue/Run state). |
| GET    | `/v1/projects/{project}/issues/{n}/touchpoint` | Canonical live Touchpoint summary for one Glimmung Issue. |
| POST   | `/v1/signals`                     | Enqueue a Signal. PR signals use GitHub coordinates only: `{target_type:"pr", target_repo:"owner/repo", target_ref:"42", source:"glimmung_ui", payload:{kind:"reject", feedback:"..."}}`. |
| POST   | `/v1/signals/drain`               | Admin drain endpoint for queued signals; production also runs the Go signal drain loop in-process. |

Admin endpoints accept **either** auth path:

- **auth.romaine.life delegation** — humans + browsers. The frontend redirects users to `auth.romaine.life/sign-in/microsoft`; auth.romaine.life completes the Microsoft handshake and sets a `.romaine.life`-scoped session cookie. The browser auto-attaches that cookie on every subsequent request to `glimmung.romaine.life`. The backend forwards the cookie to `auth.romaine.life/api/auth/get-session` per request (cached 60s per cookie value) and gates on `role ∈ {admin, user}` — `pending` users return 403. Admin promotion happens at [auth.romaine.life/admin](https://auth.romaine.life/admin). No local JWT verification, no bearer-token handling on the frontend, no per-app signing secret.
- **K8s service-account token** — in-cluster callers (tank-operator, future agents). The pod presents its projected SA token as `Authorization: Bearer <token>`; backend validates it via `TokenReview` against the cluster API server and checks the resolved `system:serviceaccount:<ns>:<name>` against `K8S_SA_ALLOWLIST` (Helm default `tank-operator/tank-operator`). Glimmung's pod SA is bound to `system:auth-delegator` ([k8s/templates/auth-delegator.yaml](k8s/templates/auth-delegator.yaml)) so the review call is permitted. Same RBAC primitive the mcp-* deployments use; the validation runs in-app instead of via a kube-rbac-proxy sidecar because glimmung's listener is publicly exposed.

K8s SA tokens are routed by their distinct JWT shape (cluster issuer, `kubernetes.io` claim); everything else goes to the auth.romaine.life verifier. To allowlist additional SAs, set `K8S_SA_ALLOWLIST="ns1/sa1,ns2/sa2"`.

### Test-slot sign-in

Per-project test slots no longer need their own SPA redirect URI registered
on a project-owned Entra app. Sign-in for any slot URL flows through
auth.romaine.life: the project's frontend redirects to
`auth.romaine.life/sign-in/microsoft?callbackURL=https://slot-1.foo/`, the
auth service completes the Microsoft handshake under a single org-wide app
reg, and 302s back to the slot.

The trustedOrigins allowlist for slot hostnames is **owned by glimmung**,
not statically listed in `nelsong6/auth` source. Each project that opts in
sets `managed_auth_origins.enabled=true` in its project metadata;
glimmung's reconciler (`internal/server/managed_origins.go`) derives the
wildcard mechanically from `native_standby_dns.record_base`
(`https://*.<record_base>`) and PUTs it to
`auth.romaine.life/api/admin/origins/{project}` via the projected SA
token mounted with `audience: https://auth.romaine.life`. The wildcard
unions into Better Auth's `trustedOrigins` and Hono's CORS allowlist on
`/api/auth/*` at request time (60s in-process cache). See nelsong6/glimmung#142
for the full architecture; auth's CI guard
(`nelsong6/auth/scripts/check-static-slot-origins.mjs`) prevents any
project-specific slot wildcard from being re-added to auth source.

Reconciliation triggers on `scaleProjectTestEnvironments`,
`registerProject`, and (future) project deregister. Failures surface on
the project's `managed_auth_origins_status` row; re-issuing any trigger
is the idempotent self-heal.

Glimmung no longer reconciles redirect URIs against Microsoft Graph on slot
scale changes — that whole code path was deleted alongside the per-project
metadata field that used to drive it.

### Native test-slot provisioning

The native test-slot lifecycle contract lives in
[`docs/test-slot-lifecycle.md`](docs/test-slot-lifecycle.md). In short, queue
size controls prepared capacity, and checkout asks Glimmung to choose and lease
one available slot. Callers do not choose the slot.

A warm or available slot is preliminary capacity only. It may include slot
metadata, DNS and routing prerequisites, Entra redirect URIs, Azure federated
identity credentials, namespaces, service accounts, RBAC, ExternalSecrets, and
other zero-steady-runtime scaffolding. It must not keep project app, API proxy,
session, Playwright, or validation workload pods running.

Runtime materialization belongs after Glimmung assigns a lease. Activation
creates the lease-scoped runtime and, when native Playwright is enabled, waits
for the slot-local `slot-playwright` Deployment before the slot becomes usable.
Returning a slot tears down that lease-scoped runtime and keeps the preliminary
slot capacity. Changing queue size is the destructive path that may remove slot
capacity. Callback release for a test-slot lease uses the same cleanup path as
`/v1/test-slots/return`, and an expired claimed test-slot lease is cleaned by
the in-process test-slot reconciler.

Minimal Tank-style config:

```json
{
  "test_slot_helm": {
    "enabled": true
  }
}
```

Defaults are intentionally aligned with `tank-operator`: chart path `k8s`,
installer image `alpine/k8s:1.30.0`, and Helm value
`testEnv.enabled=true`. Other projects can set `chart_path`, `installer_image`,
`git_ref`, `values`, `set_string_values`, `sessions_namespace`, and
`cluster_role_bindings` under `test_slot_helm`.

Any implementation path that treats a Helm-rendered app/proxy/session/tool
runtime as part of an unleased warmed slot violates the lifecycle contract and
should be split into preliminary reconciliation and lease activation.

### Native test-slot hot swap

Native webapp projects can also advertise `metadata.test_slot_hot_swap` for
the no-rollout validation path. Static changes copy built assets into the
slot's static override directory. Backend changes run the project build command
from the session checkout, copy the compiled artifact into the selected pod as
`target.next`, atomically rename it to `target`, signal the supervisor, and
optionally poll the configured health path.

The developer-driven path is `POST /v1/test-slots/apply-hot-swap` (MCP tool
`apply_test_slot_hot_swap`). It takes a `git_ref` and dispatches a one-off
Kubernetes Job that clones, builds, kubectl-streams the artifact into the
target pod, sends the configured restart signal, and records hot-swap history
on the lease. Sync UX, 120s default timeout, 600s hard cap. See
[`docs/test-slot-hot-swap.md`](docs/test-slot-hot-swap.md) for the workflow
contract and the migration from the manual kubectl pattern.

Glimmung validates the metadata on project registration. The verify-loop
executor is the repo-local ops CLI:

```sh
glimmung-agent test-slot-hot-swap \
  --project glimmung \
  --namespace glimmung-slot-1 \
  --selector app.kubernetes.io/instance=glimmung-slot-1 \
  --container glimmung \
  --health-base-url https://glimmung-slot-1.glimmung.dev.romaine.life
```

The command reads project metadata from `GLIMMUNG_BASE_URL` or
`--glimmung-base-url`; callers can also pass `--contract-file` or
`--contract-json` directly.

Glimmung's own issue chart supports this in test slots. When
`testEnv.enabled=true`, the workload runs `/app/glimmung-supervisor` as PID 1,
mounts `/var/run/glimmung-hot`, and restarts the child process on `SIGHUP`.
Production installs keep the normal image command and do not enable restart
behavior.

Glimmung dogfood metadata:

```json
{
  "test_slot_hot_swap": {
    "enabled": true,
    "static": {
      "enabled": true,
      "source": "frontend/dist",
      "target": "/var/run/glimmung-static-override"
    },
    "backend": {
      "enabled": true,
      "strategy": "supervisor",
      "build_command": "go build -o /tmp/glimmung ./cmd/glimmung-go",
      "artifact": "/tmp/glimmung",
      "target": "/var/run/glimmung-hot/glimmung",
      "health_path": "/healthz"
    }
  }
}
```

### GitHub webhook

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/webhook/github`              | Receives `issues` and `workflow_run` events. |

The Go handler verifies `X-Hub-Signature-256` against `GITHUB_WEBHOOK_SECRET`
when configured and acknowledges the event. Rich issue/workflow_run processing
from the retired app is not part of the native lease dispatch path.

Current event contract: Glimmung accepts GitHub `issues` and `workflow_run`
webhooks for repair hooks. PR review decisions enter through
the `/v1/signals` contract instead of GitHub webhook side effects. `pull_request`,
`check_run`, `check_suite`, `deployment`, and `push` events are intentionally not
canonical run-state inputs unless a future syndication issue explicitly adds
them.

### Unified dispatch

`POST /v1/runs/dispatch` is handled in
[`internal/server/dispatch_api.go`](internal/server/dispatch_api.go), backed by
Cosmos operations in [`internal/store/cosmos`](internal/store/cosmos). It:

1. Resolves the project and workflow from Cosmos.
2. Reads the Glimmung issue by project issue number.
3. Claims the `("issue", "<project>#<number>")` lock; concurrent dispatches on the same issue return `state="already_running"`.
4. Creates the Run record while the issue lock serializes run-number allocation.
5. Acquires a native lease. If no native capacity is immediately available, the
   run waits without launching executor work.
6. Returns the callback-token metadata needed by the executor.
7. Launches the claimed `k8s_job` phase through the Go-managed native launcher.
8. Records completion through
   `/v1/run-callbacks/{callback_token}/native/completed`. Native completion is
   job-scoped: every callback must
   include `job_id`, and the Go decision engine only advances the phase after
   every registered job in that phase has completed.

Issue-lock TTL is 4h. Terminal Run transitions release issue/PR locks through
the Go store; leases still have their own TTL/callback lifecycle.

Runner clients that open or update a GitHub PR should use the dispatch inputs
and lease metadata as the PR body source of truth: include `issue_ref`,
`run_ref`, the Touchpoint/PR URL when known, the validation URL, and evidence
links from the RunReport. GitHub remains a syndication surface; the canonical
review state stays in the Glimmung Issue workspace.

## Storage

Cosmos DB NoSQL on the shared `infra-cosmos-serverless` account. Database `glimmung`, containers pre-created by [`tofu/db.tf`](tofu/db.tf):

- `projects` (partition key `/name`)
- `workflows` (partition key `/project`)
- `leases` (partition key `/project`)
- `runs` (partition key `/project`) — verify-loop run state, see below
- `locks` (partition key `/scope`) — generic mutual-exclusion primitive, see below
- `signals` (partition key `/target_repo`) — signal bus for triage / re-entry / future automations, see below
- `playbooks` (partition key `/project`) — stored operator plans for coordinated issue batches

Epics are not persisted yet. For now, Epic-level context lives in Playbook
descriptions or linked documentation; the model boundary is documented in
[Epics and Playbooks](docs/epics-and-playbooks.md).

Runtime pod auth via the `infra-shared-identity` workload identity, which has `Cosmos DB Built-in Data Contributor` at the account scope (granted in [`infra-bootstrap/tofu/cosmos-serverless.tf`](https://github.com/nelsong6/infra-bootstrap/blob/main/tofu/cosmos-serverless.tf)). The Go store opens existing containers with `azcosmos.NewContainer`; reads/writes use the data-plane permissions. CREATE DATABASE / CREATE CONTAINER is control-plane and runs only via tofu under the app SP.

## Lock semantics

Native lease acquisition is slot-backed. The allocator reads active native
leases, checks project and global concurrency, selects the first ready
unclaimed test slot, and writes one claimed lease document. If capacity is not
available, callers get `no_capacity`; executor work is not launched.

Release paths:
- **Fast**: workflow's own release step (if it has one).
- **Run terminal paths**: Go completion, abort, replay, and native failure
  handlers release related issue/PR locks and update run state.
- **Backstop**: lease TTL and stale heartbeat handling clean abandoned native
  claims so capacity can return to the allocator.

## One-time setup

KV keys consumed by glimmung:

| KV secret                          | Source                                                                       |
|---|---|
| `glimmung-github-app-id`           | dedicated GitHub App (created by hand; one App = one webhook URL, can't co-tenant) |
| `glimmung-github-app-installation-id` | same                                                                      |
| `glimmung-github-app-private-key`  | same                                                                         |
| `glimmung-github-webhook-secret`   | same                                                                         |

The GitHub App is created via the GitHub UI — one webhook URL per App means glimmung needs its own (the shared `github-app-*` keys still serve mcp-github / diagrams). Configure the App with:

- Webhook URL: `https://glimmung.romaine.life/v1/webhook/github`
- Subscribe to events: **Issues**, **Workflow runs**
- Permissions: Actions `read+write`, Issues `read`, Metadata `read`
- Install on whichever repos use it

## Admin (dashboard)

Visit https://glimmung.romaine.life/, click **sign in** (top right) — MSAL popup against the `glimmung-oauth` Entra app. Once signed in (email must be in the allowlist), click **admin** to reveal the registration tabs:

- **Register project** → name + github_repo
- **Register issue** → project + title + body for a Glimmung-owned run target

Workflow registrations are structural control-plane data. Apply them through
the Glimmung API/MCP workflow registration path with a full phase shape, not as
a project-creation side effect. The dashboard shows projects, workflows, leases,
runs, reports, and native test slots.

## Running locally

```sh
az login                                 # for DefaultAzureCredential
COSMOS_ENDPOINT=https://infra-cosmos-serverless.documents.azure.com:443/ \
  COSMOS_DATABASE=glimmung \
  go run ./cmd/glimmung-go
```

For the frontend:

```sh
cd frontend && npm install && npm run dev
# proxies /v1/* to localhost:8000
```

## Tests

The default app gate is Go plus the Vite dashboard:

```sh
go test ./...
go vet ./...
```

```sh
cd frontend
npm run test:run
npm run build
```

GitHub Actions runs those checks on pull requests and pushes. Pull requests
also run `.github/workflows/docker-build-check.yaml`, which performs a
throwaway app image build with `push: false`. Pushes to `main` also run a
Go-native live Cosmos smoke test for the lock lifecycle with GitHub OIDC, using
the database-scoped CI role assignment in [`tofu/test-access.tf`](tofu/test-access.tf).

The repository root no longer carries Python packaging; the retired
`src/glimmung/` app tree is gone, and the root Python `tests/` suite has been
deleted. Repo-local agent workflow operations live in the Go CLI under
`cmd/glimmung-agent` and reusable functions under `internal/ops/agentops`;
they are covered by `go test ./...`.

```sh
go run ./cmd/glimmung-agent --help
```

To exercise the Go live Cosmos smoke locally, opt in with:

```sh
az login
COSMOS_ENDPOINT=https://infra-cosmos-serverless.documents.azure.com:443/ \
  COSMOS_DATABASE=glimmung \
  GLIMMUNG_TEST_COSMOS=live \
  go test ./internal/store/cosmos -run TestLiveCosmosLockLifecycle -count=1 -v
```

Set `GLIMMUNG_TEST_PREFIX=test-my-run` to reuse or inspect a specific smoke-test
lock name. The test deletes its lock document before and after the run.

## Browser inspection

`mcp-glimmung` includes a generic Playwright-backed inspector for validation
URLs. This is optional external tooling, not part of Glimmung's app runtime,
local app setup, or repo-local ops CLI. Use the MCP `inspect_browser_url` tool,
or run the same implementation from the standalone repo:

```sh
git clone https://github.com/nelsong6/mcp-glimmung.git
cd mcp-glimmung
uv run glimmung-browser-inspect https://example.romaine.life \
  --width 1440 --height 900 --wait-ms 2000 --screenshot
```

The JSON result includes final URL/status, title/body summary, interesting
elements with selectors/roles/bounds/styles, console and page errors, failed
requests and HTTP >= 400 responses, optional accessibility data, optional
screenshot path, and canvas nonblank sampling. Use it when rendered browser
state matters more than a static screenshot alone.

First-time setup grants your `az login` principal data-plane access on the
glimmung Cosmos database (without it the first read fails with `readMetadata`
denied). Add your Entra object id to `dev_test_principal_ids` in
[`tofu/test-access.tf`](tofu/test-access.tf) and apply:

```sh
az ad signed-in-user show --query id -o tsv   # your object id
# append to dev_test_principal_ids in tofu, then `tofu apply` from tofu/
```

Scope is the glimmung database only; sibling apps on the same Cosmos account
stay unreachable.

The attended-pickup launch flow ([#127](https://github.com/nelsong6/glimmung/issues/127))
is dogfooded against real Glimmung PR rows: a Glimmung run produces an
actual PR in this repo, and that PR is the fixture used to exercise the
`start Tank session` flow before #127 can close. The launch URL hands the
glimmung run / issue / touchpoint refs, plus the validation URL embedded in
the PR body, to tank-operator, which gives the session its
`/workspace/GLIMMUNG_CONTEXT.{json,md}` and an mcp-glimmung route.

## Verify-loop substrate (#18)

Glimmung-as-orchestrator wedge: when a verify phase fails, glimmung launches
the configured native recycle phase with the prior verification context,
repeating until verification passes, attempt count exceeds N, or cumulative
cost exceeds $X. The substrate that lands here is reused by every other
[meta #17](https://github.com/nelsong6/glimmung/issues/17) child.

### Opting a workflow in

Register a workflow with explicit phases, marking the verification phase with
`verify: true` and adding `recycle_policy` on the phase or PR primitive where
needed. The current workflow shape is documented in
[`docs/workflow-shape.md`](docs/workflow-shape.md). Older fields such as
`retry_workflow_filename`, `default_budget`, and trigger-label dispatch are
retired.

### Per-issue budget overrides

Apply an `agent-budget:USD` label to the issue. Examples:

- `agent-budget:50` -> $50 ceiling
- `agent-budget:12.5` -> $12.50 ceiling

The budget is **frozen at run-creation time** — relabeling mid-run does not move the goalposts. Resolution order: issue label → `Workflow.budget` → glimmung global default ($25).

### Verification contract

Every verification `k8s_job` phase must report a typed verification payload
through the native completion callback. The decision engine reads the typed
verdict, never executor exit state alone. Schema:

```json
{
  "schema_version": 1,
  "status": "pass" | "fail" | "error",
  "reasons": ["short human-readable strings, one per failure"],
  "evidence_refs": ["screenshots/01.png", "logs/verify.log"],
  "cost_usd": 4.20,
  "prompt_version": "v17",
  "metadata": {}
}
```

`status` semantics:

- `pass`: verification reached a positive verdict; glimmung records `ADVANCE` and the next phase proceeds.
- `fail`: verification reached a negative verdict; glimmung launches the recycle phase if budget allows, otherwise aborts.
- `error`: verifier itself crashed before reaching a verdict. Treated as a substantive negative verdict, distinct from a missing payload.

A missing or schema-invalid verification payload is itself a decision input:
the engine returns `ABORT_MALFORMED` and records the contract violation.

### Recycle attempt inputs

When glimmung launches a recycled native phase, the job environment includes:

| Input | Description |
|---|---|
| `GLIMMUNG_LEASE_REF`                  | Fresh native lease ref for the attempt. |
| `GLIMMUNG_RUN_ID`                     | Glimmung Run ULID for log correlation. |
| `GLIMMUNG_RUN_REF`                    | Public run ref. |
| `GLIMMUNG_ATTEMPT_INDEX`              | 0-based attempt index (initial=0, first recycle=1, ...). |
| `GLIMMUNG_INPUT_*`                    | Resolved phase inputs, including prior phase outputs where the workflow declares them. |

### Decision engine

Pure decision logic lives in
[`internal/domain/decision/decision.go`](internal/domain/decision/decision.go).
Side effects live at the server call sites, primarily
[`internal/server/completion_api.go`](internal/server/completion_api.go) and
[`internal/server/replay_api.go`](internal/server/replay_api.go). Outputs:

- `advance` - verification passed; the next ready phase runs.
- `retry` - launch the configured recycle phase through the native path.
- `abort_budget_attempts` - `len(attempts) >= max_attempts`.
- `abort_budget_cost` - `cumulative_cost_usd >= max_cost_usd` (checked first; harder cap).
- `abort_malformed` - verification artifact missing or schema-invalid.

Decision logic and edge-case coverage live in
[`internal/domain/decision/decision_test.go`](internal/domain/decision/decision_test.go).

## Lock primitive (W1 substrate)

Generic mutual-exclusion claims are stored in the `locks` Cosmos container and
keyed by `(scope, key)`. Active Go callers use the primitive for per-issue
run serialization and PR/issue lock release at terminal run states.
The implementation lives in [`internal/store/cosmos`](internal/store/cosmos),
with handler coverage in dispatch, abort, signal-drain, and completion tests.

Important active operations:

| Call | Behavior |
|---|---|
| `ClaimIssueLock(project, issueNumber, holderID, ttlSeconds)` | Atomic create or ETag-protected take-over when the prior lock is released or expired. Returns `already_running` detail when held. |
| `ReleaseIssueLock(project, issueNumber, holderID)` | Best-effort release for rollback and terminal paths; validates holder before transitioning to released. |
| terminal run updates | Release issue/PR locks from stored holder fields when a run advances, aborts, or completes. |

### Doc id

Deterministic: `f"{scope}::{urllib.parse.quote(key, safe='')}"`. Cosmos forbids `/`, `\`, `?`, `#` in ids; URL-encoding handles all four uniformly. Same scope + same key → same doc → Cosmos's `id`-uniqueness constraint enforces "only one active claimer at a time" for free.

### Holder semantics

`holder_id` is opaque to the primitive. Callers pick a stable identifier for their critical section — typically a signal_id, a run_id, or a fresh ULID per claim attempt. `release_lock` and `extend_lock` validate `holder_id` matches before acting.

`claim_lock` is **strict**: a second claim by the same `holder_id` while the lock is held also raises `LockBusy`. Callers wanting refresh-or-claim should use `extend_lock`. Restart-after-crash: pick a new `holder_id`; the previous instance's claim expires via TTL.

### Test coverage

Remaining generic lock edge cases identified in the cleanup inventory should be
ported to Go store tests before the retired lock module is deleted.

## Signal bus + PR triage (#19)

`POST /v1/signals` enqueues Signal documents through
[`internal/server/signal_api.go`](internal/server/signal_api.go) and
[`internal/store/cosmos`](internal/store/cosmos). The Go service drains queued
signals in-process and exposes `POST /v1/signals/drain` as an admin/manual
drain endpoint.

Active triage behavior:

- **Per-PR serialization**: signals on a PR whose lock is held stay PENDING and
  re-evaluate later.
- **Triage decisioning**: non-actionable signals are ignored; actionable reject
  feedback reopens the linked Run through the workflow PR recycle policy.
- **Budget enforcement**: no-run and budget abort cases are recorded as
  processed decisions instead of dispatching more work.
- **One PR signal contract**: `target_repo` is the GitHub repo (`owner/repo`)
  and `target_ref` is the PR number. Glimmung project names and Touchpoint refs
  are resolved after the signal lands, not accepted as alternate PR targets.

### Triage recycle contract

Triage recycle launches a native phase with:

| Input | Description |
|---|---|
| `GLIMMUNG_LEASE_REF`                  | Fresh native lease ref for the triage run. |
| `GLIMMUNG_RUN_ID`                     | Glimmung Run ULID. |
| `GLIMMUNG_RUN_REF`                    | Public run ref. |
| `GLIMMUNG_ATTEMPT_INDEX`              | 0-based attempt index. |
| `GLIMMUNG_INPUT_*`                    | Resolved phase inputs, including human feedback when the workflow declares it. |

The triage contract runs implementation and verification with feedback in
context through the same native phase and verification callback contract.

## Historical Platform Phases

These are the original product build phases. The Go-runtime cleanup finished
the app/runtime retirement of the Python tree; final cleanup notes
live in [`docs/go-runtime-cleanup-inventory.md`](docs/go-runtime-cleanup-inventory.md).

1. **Phase 1** ✓ — lease primitive, sweep job, Cosmos backend.
2. **Phase 2** ✓ — GitHub App webhook receiver, ingress at `glimmung.romaine.life`, Entra ID auth on admin endpoints.
3. **Phase 3** ✓ — Dashboard with SSE, project side pane, workflow as first-class abstraction, MSAL sign-in + admin panel.
4. **Phase 2.5** ✓ — Migrate spirelens `issue-agent.yaml` to consume glimmung leases. (Numbered out of order; see [glimmung issue #2](https://github.com/nelsong6/glimmung/issues/2) for the build order that actually happened.)
5. **Phase 4** — Native-runner grounding, dashboard cancel/preempt, and project migrations onto the single native lease path.

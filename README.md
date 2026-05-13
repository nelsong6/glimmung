# glimmung

Go service for issue-driven agentic development. Glimmung stores projects,
workflows, issues, runs, leases, reports, and signals in Cosmos DB; serves the
Vite + React dashboard; and coordinates native Kubernetes jobs plus the legacy
GitHub Actions dispatch path.

> *The Glimmung scanned the assembled list of beings he had summoned. From a thousand worlds they had come, each with a craft to contribute.*
> — paraphrased from Philip K. Dick, *Galactic Pot-Healer*

## What it does

The agentic development pattern (issue -> bounded agent run -> verification ->
review report / PR) repeats across multiple projects, and off-the-shelf CI
systems do not model the orchestration cleanly. Glimmung owns the queue, the
database-backed workflow shape, the run/lease lifecycle, the callback surface,
the verify-loop decision engine, the dashboard, and the signal bus.

Native Kubernetes jobs are the default execution layer for web-native apps.
Legacy GitHub Actions dispatch is still present for older consumers, but new
native apps should not register app-specific GitHub runner pools or keep
repo-backed workflow files as the runtime source of truth.

Full design + intent: [issue #1](https://github.com/nelsong6/glimmung/issues/1).

## Mental model

```
Project -> Workflow -> Issue -> Run -> Phase/Job -> Report
                         \        \
                          \        -> Lease + callback token
                           -> Touchpoint / PR review surface
```

- **Project** = a repo (e.g. `spirelens`), declares the github_repo only.
- **Workflow** = a database-backed automation shape under a project. Dispatch
  reads the Workflow row from Cosmos: phases, native jobs, PR policy, budget,
  trigger label if any, and requirements. Native web app projects default
  omitted phase kinds to `k8s_job`; legacy/non-native projects keep the
  compatibility default of `gha_dispatch`.
- **Issue** = the canonical Glimmung issue row. GitHub Issues may still feed
  temporary backlog/tracker workflows, but the live run loop is issue-row based.
- **Run** = durable execution record for one issue/workflow invocation. Runs
  hold attempts, phase state, evidence refs, cost, terminal decision, and
  callback-token metadata.
- **Lease** = capacity claim for a run phase. Native `k8s_job` phases use the
  native capacity path and callback-token APIs; legacy `gha_dispatch` phases may
  still claim a registered Host and fire `workflow_dispatch`.
- **Host** = a legacy/self-hosted-runner venue kept for exception workflows and
  dashboard visibility.

Workflow registration is an admin/control-plane operation. Consumer repos do
not need `.glimmung/workflows/<name>.yaml` files for runtime dispatch; changing
repo files has no effect unless an operator explicitly writes a new Workflow
registration into Cosmos. The upstream-sync helper is an import convenience for
older desired-state flows, not the runtime source of truth.

The "agent" — Claude Code, Codex, whatever runs inside the workflow — is opaque to glimmung. We dispatch a venue to a workflow; the workflow runs an agent on it.

For larger feature work, Glimmung separates planning context from execution:

```
Epic -> Playbook -> ordered Entries -> Issue -> Run -> Report/evidence -> next Entry
```

- **Epic** = durable feature context: why, goal, constraints, non-goals, success criteria.
- **Playbook** = executable ordered plan: entries, dependencies, gates, concurrency, dispatch state.

The initial relationship is intentionally 1:1: one Epic owns one Playbook.
See [Epics and Playbooks](docs/epics-and-playbooks.md) for the object
boundary and follow-up implementation surface.
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

The legacy Python app and root Python test suite have been removed. The app
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
- For MCP server renames/removals, follow the rollout sequence in
  [MCP Surface Rollout](docs/mcp-surface-rollout.md) so stale sessions have a
  clear compatibility or restart path.
- If the endpoint is intentionally system-only (webhooks, lease lifecycle,
  run callbacks, health/config/events), call that out in the PR so the HTTP
  API and MCP surface do not drift silently.

## API

The Go route registration in
[`internal/server/server.go`](internal/server/server.go) is the active HTTP
surface. [`internal/server/route_inventory_test.go`](internal/server/route_inventory_test.go)
keeps that route list explicit; the tables below summarize the operator-facing
surface rather than every compatibility tombstone.

### Lease lifecycle

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/lease`                       | Acquire (`{project, workflow?, requirements, metadata, requester}`). Admin-auth guarded; returns a public lease ref, callback token metadata, and host when capacity is immediately available. |
| GET    | `/v1/lease-callbacks/{callback_token}` | Read the public lease by callback token. Used by runner clients. |
| POST   | `/v1/lease-callbacks/{callback_token}/heartbeat` | Keep the lease alive. |
| POST   | `/v1/lease-callbacks/{callback_token}/release` | Release the lease. Idempotent. |
| POST   | `/v1/leases/cancel`               | Cancel a lease by public ref. Admin-auth guarded. |
| GET    | `/v1/state`                       | Snapshot: hosts + workflows + projects + pending + active leases. |
| GET    | `/v1/events`                      | Server-Sent Events stream — yields `{event: "state", data: <snapshot>}` every 2s. |
| GET    | `/v1/config`                      | Public — `{entra_client_id, authority}` for SPA MSAL bootstrap. |
| GET    | `/healthz`                        | Liveness/readiness. |

### Admin (Entra ID JWKS-validated bearer token, OR cluster SA token; email or `<ns>/<sa>` allowlist gate)

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/projects`                    | Register/upsert a project (`{name, github_repo}`). |
| GET    | `/v1/projects`                    | List projects. |
| PATCH  | `/v1/projects/{project}/test-environments/count` | Set native validation slot capacity and reconcile managed auth redirect URIs when configured. |
| POST   | `/v1/workflows`                   | Register/upsert a workflow under a project. |
| GET    | `/v1/workflows`                   | List workflows. |
| POST   | `/v1/playbooks`                   | Create a draft Playbook for a coordinated batch of issue specs. |
| GET    | `/v1/playbooks`                   | List Playbooks, optionally filtered by project. |
| GET    | `/v1/playbooks/{project}/{id}`    | Inspect a Playbook. |
| POST   | `/v1/playbooks/{project}/{id}/run` | Advance a Playbook: create ready entry Issues and dispatch their Runs through the canonical run path. |
| POST   | `/v1/playbooks/{project}/{id}/entries/{entry_id}/gate` | Set or clear a manual Playbook entry gate; optionally advances the Playbook after clearing. |
| POST   | `/v1/hosts`                       | Register/update a host. |
| GET    | `/v1/issues`                      | List Glimmung issues across registered projects. |
| GET    | `/v1/issues/by-number/{project}/{issue_number}` | Issue detail by canonical project issue number. |
| POST   | `/v1/runs/dispatch`               | UI/API-initiated dispatch (`{project, issue_number, workflow_name?}`); per-issue lock-serialized. |
| GET    | `/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/report` | Factual RunReport for one Run: attempts, cost, validation URL, screenshot markdown, and terminal status. |
| GET    | `/v1/touchpoints`                 | Touchpoint index across registered projects (GitHub PR syndication metadata + linked Issue/Run state). |
| GET    | `/v1/projects/{project}/issues/{n}/touchpoint` | Canonical live Touchpoint summary for one Glimmung Issue. |
| GET    | `/v1/touchpoints/{owner}/{repo}/{n}` | Compatibility Touchpoint lookup by GitHub PR coordinates. |
| GET    | `/v1/reports`                     | Compatibility alias for `/v1/touchpoints`. |
| GET    | `/v1/reports/{owner}/{repo}/{n}`  | Compatibility alias for `/v1/touchpoints/{owner}/{repo}/{n}`. |
| POST   | `/v1/signals`                     | Enqueue a Signal. PR signals use GitHub coordinates only: `{target_type:"pr", target_repo:"owner/repo", target_ref:"42", source:"glimmung_ui", payload:{kind:"reject", feedback:"..."}}`. |
| POST   | `/v1/signals/drain`               | Admin drain endpoint for queued signals; production also runs the Go signal drain loop in-process. |

Admin endpoints accept **either** auth path:

- **Entra ID** — humans + CLI. `az account get-access-token --resource <client-id>` mints a token; backend validates it via JWKS and checks the email claim against `ALLOWED_EMAILS`. The dashboard uses MSAL.js to do the same thing.
- **K8s service-account token** — in-cluster callers (tank-operator, future agents). The pod presents its projected SA token as `Authorization: Bearer <token>`; backend validates it via `TokenReview` against the cluster API server and checks the resolved `system:serviceaccount:<ns>:<name>` against `K8S_SA_ALLOWLIST` (Helm default `tank-operator/tank-operator`). Glimmung's pod SA is bound to `system:auth-delegator` ([k8s/templates/auth-delegator.yaml](k8s/templates/auth-delegator.yaml)) so the review call is permitted. Same RBAC primitive the mcp-* deployments use; the validation runs in-app instead of via a kube-rbac-proxy sidecar because glimmung's listener is publicly exposed.

The two paths are routed by the unverified `iss` claim — Microsoft issuer vs. cluster issuer — and each goes through its own validator. To allowlist additional SAs, set `K8S_SA_ALLOWLIST="ns1/sa1,ns2/sa2"`.

### Native webapp auth redirects

Native webapps that use MSAL with `redirectUri = window.location.origin + "/"`
need each validation slot hostname registered on their dedicated Entra app
registration. Glimmung reconciles those SPA redirect URIs when
`PATCH /v1/projects/{project}/test-environments/count` changes
`metadata.native_standby_dns.count`.

Project metadata contract:

```json
{
  "native_webapp": true,
  "native_standby_dns": {
    "enabled": true,
    "record_base": "tank.dev.romaine.life",
    "slot_prefix": "tank-slot",
    "count": 3
  },
  "native_auth_redirects": {
    "enabled": true,
    "provider": "entra",
    "redirect_uri_mode": "spa",
    "application_object_id": "<entra application object id>",
    "production_redirect_uris": ["https://tank.romaine.life/"],
    "extra_redirect_uris": []
  }
}
```

`application_client_id` is also accepted when the object id is not known; the
reconciler resolves it through Microsoft Graph. Desired managed slot URIs are
derived as `https://{slot_prefix}-{i}.{record_base}/` for `i in 1..count`.
The reconciler adds missing managed URIs and removes stale managed slot URIs
above the current count. It preserves production, extra, and unrelated manual
portal entries. Reconciliation diagnostics are written to
`metadata.native_auth_redirects_status` so `/v1/projects` and `/v1/state`
show whether auth redirect sync is `ok` or `failed`.

`native_standby_entra_redirects` is not accepted. Use `native_auth_redirects`
for all native webapp auth redirect reconciliation.

### Native test-slot provisioning

When a project has `metadata.test_slot_helm.enabled=true`, test-slot checkout
does more than reserve a lease. The Go native launcher creates the slot
namespace, the matching sessions namespace, namespace-scoped installer
RoleBindings, any required slot ClusterRoleBindings, and a one-shot Helm
installer Job in the native runner namespace. The Job clones the project repo
with a short-lived GitHub App token, renders the chart, strips
cluster-scoped RBAC from the apply stream, and applies the namespaced resources
into the slot namespaces. Returning the slot deletes the installer artifacts,
slot CRBs, Playwright helper, and slot namespaces.

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

### GitHub webhook

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/webhook/github`              | Receives `issues` and `workflow_run` events. |

The Go handler verifies `X-Hub-Signature-256` against `GITHUB_WEBHOOK_SECRET`
when configured and acknowledges the event. Rich issue/workflow_run processing
from the legacy app is part of the runtime cleanup inventory and should be
ported only if live consumers still need it.

Current event contract: Glimmung accepts GitHub `issues` and `workflow_run`
webhooks for compatibility and repair hooks. PR review decisions enter through
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
5. Acquires a lease. If no capacity is immediately available, the run stays
   pending.
6. Returns the callback-token metadata needed by the executor.
7. Fires `workflow_dispatch` only for `gha_dispatch` phases when a host and
   GitHub dispatch client are available. Native `k8s_job` phases stay in the
   Go-managed native path and report through the native run callback APIs.
8. Records completion through `/v1/run-callbacks/{callback_token}/completed`
   for GitHub Actions phases or `/v1/run-callbacks/{callback_token}/native/completed`
   for native phases. Native completion is job-scoped: every callback must
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
- `hosts` (partition key `/name`)
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

Optimistic concurrency on the host doc's `_etag`. Acquire reads matching
candidates, sorts by `lastUsedAt` (NULLs first, so unused venues are preferred),
tries each via ETag-protected replace, and moves to the next candidate on a
precondition failure. The loop is bounded and terminates after exhausting
candidates.

Release paths:
- **Fast**: workflow's own release step (if it has one).
- **Run terminal paths**: Go completion, abort, replay, and native failure
  handlers release related issue/PR locks and update run state.
- **Backstop**: lease TTL and stale heartbeat handling remain compatibility
  behavior to preserve while the legacy cleanup completes.

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
- **Register workflow** → project (dropdown), name, phases/native jobs, budget,
  trigger label, requirements
- **Register legacy host** -> name + capabilities for explicit `gha_dispatch`
  exception workflows

The dashboard shows projects, workflows, leases, runs, and legacy host pools.
Host tables are retained for self-hosted GitHub Actions exceptions, not the
normal native web app path.

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

The repository root no longer carries Python packaging, the legacy
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

Glimmung-as-orchestrator wedge: when a verify phase fails, glimmung re-dispatches an implementation phase with the prior verification artifact as additional context, repeating until verification passes, attempt count exceeds N, or cumulative cost exceeds $X. The substrate that lands here is reused by every other [meta #17](https://github.com/nelsong6/glimmung/issues/17) child.

### Opting a workflow in

Register the workflow with `retry_workflow_filename` set:

```sh
curl -X POST https://glimmung.romaine.life/v1/workflows \
  -H "Authorization: Bearer $(az account get-access-token --resource <client-id> -o tsv --query accessToken)" \
  -H "Content-Type: application/json" \
  -d '{
    "project": "spirelens",
    "name": "issue-agent",
    "workflow_filename": "issue-agent.yml",
    "retry_workflow_filename": "agent-retry.yml",
    "trigger_label": "agent-run",
    "default_budget": {"max_attempts": 3, "max_cost_usd": 25.0}
  }'
```

Workflows without `retry_workflow_filename` keep the pre-#18 fire-and-forget behavior unchanged (no Run record, no decision engine, no retry path).

### Per-issue budget overrides

Apply an `agent-budget:NxM` label to the issue (`N` = max_attempts, `M` = max_cost_usd in USD). Examples:

- `agent-budget:5x50` → 5 attempts, $50 ceiling
- `agent-budget:1x10` → no retries, $10 ceiling

The budget is **frozen at run-creation time** — relabeling mid-run does not move the goalposts. Resolution order: issue label → `Workflow.default_budget` → glimmung global default (3 / $25).

### `verification.json` contract

Every consumer workflow that opts into the verify-loop **must** upload a GHA artifact named `verification` containing `verification.json` at its root. The decision engine reads the typed verdict, never the workflow_run conclusion alone. Schema:

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

- `pass` — verification reached a positive verdict; glimmung records `ADVANCE` and the consumer's PR-open step proceeds.
- `fail` — verification reached a negative verdict; glimmung dispatches the retry workflow if budget allows, otherwise aborts.
- `error` — verifier itself crashed before reaching a verdict. Treated as a substantive negative verdict (retry up to budget), distinct from a missing artifact.

A missing or schema-invalid artifact is itself a decision input: the engine returns `ABORT_MALFORMED` and posts an issue comment explaining the contract violation. (Retrying the same producer would just reproduce the broken artifact.)

### Retry workflow inputs

When glimmung dispatches the retry workflow, it sets:

| Input | Description |
|---|---|
| `lease_id`                            | Fresh lease ID for the retry attempt. |
| `host`                                | Host the retry was scheduled onto. |
| `issue_number`                        | Issue under which the run is tracked. |
| `run_id`                              | Glimmung Run ULID (for log correlation). |
| `attempt_index`                       | 0-based attempt index (initial=0, first retry=1, …). |
| `prior_verification_artifact_url`     | GHA Actions API URL of the previous attempt's `verification` artifact. The retry workflow pulls it via its own `GITHUB_TOKEN`; redirect resolves to a short-lived presigned blob. |

### Decision engine

Pure decision logic lives in
[`internal/domain/decision/decision.go`](internal/domain/decision/decision.go).
Side effects live at the server call sites, primarily
[`internal/server/completion_api.go`](internal/server/completion_api.go) and
[`internal/server/replay_api.go`](internal/server/replay_api.go). Outputs:

- `advance` - verification passed; consumer's PR step runs.
- `retry` - dispatch retry workflow with `prior_verification_artifact_url`.
- `abort_budget_attempts` - `len(attempts) >= max_attempts`.
- `abort_budget_cost` - `cumulative_cost_usd >= max_cost_usd` (checked first; harder cap).
- `abort_malformed` - verification artifact missing or schema-invalid.

Decision logic and edge-case coverage live in
[`internal/domain/decision/decision_test.go`](internal/domain/decision/decision_test.go).

## Lock primitive (W1 substrate)

Generic mutual-exclusion claims are stored in the `locks` Cosmos container and
keyed by `(scope, key)`. Active Go callers use the primitive for per-issue
dispatch/resume serialization and PR/issue lock release at terminal run states.
The implementation lives in [`internal/store/cosmos`](internal/store/cosmos),
with handler coverage in dispatch, resume, abort, and completion tests.

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
ported to Go store tests before the legacy lock module is deleted.

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

### Triage workflow contract

Triage dispatch sets:

| Input | Description |
|---|---|
| `lease_id`                            | Fresh lease ID for the triage attempt. |
| `host`                                | Host the triage was scheduled onto. |
| `issue_number`                        | Originating issue number. |
| `pr_number`                           | The PR receiving the feedback. |
| `run_id`                              | Glimmung Run ULID. |
| `attempt_index`                       | 0-based attempt index. |
| `feedback`                            | Human-readable feedback text from the reject signal. |
| `prior_verification_artifact_url`     | Empty for triage (the prior attempt PASSED to open the PR; no failure context to feed back). |

The triage workflow contract runs impl + verify with feedback in
context, force-pushes the result, and uploads `verification.json` (same
contract as retry workflows; see verify-loop substrate).

## Historical Platform Phases

These are the original product build phases. The Go-runtime cleanup finished
the app/runtime retirement of the legacy Python tree; final compatibility notes
live in [`docs/go-runtime-cleanup-inventory.md`](docs/go-runtime-cleanup-inventory.md).

1. **Phase 1** ✓ — lease primitive, sweep job, Cosmos backend.
2. **Phase 2** ✓ — GitHub App webhook receiver, `workflow_dispatch` firing, ingress at `glimmung.romaine.life`, Entra ID auth on admin endpoints.
3. **Phase 3** ✓ — Dashboard with SSE, project side pane, workflow as first-class abstraction, MSAL sign-in + admin panel.
4. **Phase 2.5** ✓ — Migrate spirelens `issue-agent.yaml` to consume glimmung leases. (Numbered out of order; see [glimmung issue #2](https://github.com/nelsong6/glimmung/issues/2) for the build order that actually happened.)
5. **Phase 4** — Runner-grounding (verify GHA runner is online before dispatching), dashboard cancel/preempt, migrate ambience + tank-operator agent flows.

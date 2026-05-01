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

### Admin (Entra ID JWKS-validated bearer token, OR cluster SA token; email or `<ns>/<sa>` allowlist gate)

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/projects`                    | Register/upsert a project (`{name, github_repo}`). |
| GET    | `/v1/projects`                    | List projects. |
| POST   | `/v1/workflows`                   | Register/upsert a workflow under a project. |
| GET    | `/v1/workflows`                   | List workflows. |
| POST   | `/v1/hosts`                       | Register/update a host. |
| GET    | `/v1/issues`                      | List open issues across all registered repos (live GH API). |
| GET    | `/v1/issues/{owner}/{repo}/{n}`   | Issue detail (title, body, labels, last-run, lock state). |
| POST   | `/v1/runs/dispatch`               | UI-initiated dispatch (`{repo, issue_number, workflow?}`). Same path as the label-webhook trigger; per-issue lock-serialized. |
| GET    | `/v1/prs`                         | Agent-opened PRs across registered repos (linked Run + state). |
| GET    | `/v1/prs/{owner}/{repo}/{n}`      | PR detail with attempt history + reject feedback surface. |
| POST   | `/v1/signals`                     | Enqueue a Signal (e.g., `{target_type:"pr", target_repo, target_id, source:"glimmung_ui", payload:{kind:"reject", feedback:"…"}}`). UI reject button uses this. |

Admin endpoints accept **either** auth path:

- **Entra ID** — humans + CLI. `az account get-access-token --resource <client-id>` mints a token; backend validates it via JWKS and checks the email claim against `ALLOWED_EMAILS`. The dashboard uses MSAL.js to do the same thing.
- **K8s service-account token** — in-cluster callers (tank-operator, future agents). The pod presents its projected SA token as `Authorization: Bearer <token>`; backend validates it via `TokenReview` against the cluster API server and checks the resolved `system:serviceaccount:<ns>:<name>` against `K8S_SA_ALLOWLIST` (default `tank-operator/tank-operator`). Glimmung's pod SA is bound to `system:auth-delegator` ([k8s/templates/auth-delegator.yaml](k8s/templates/auth-delegator.yaml)) so the review call is permitted. Same RBAC primitive the mcp-* deployments use; the validation runs in-app instead of via a kube-rbac-proxy sidecar because glimmung's listener is publicly exposed.

The two paths are routed by the unverified `iss` claim — Microsoft issuer vs. cluster issuer — and each goes through its own validator. To allowlist additional SAs, set `K8S_SA_ALLOWLIST="ns1/sa1,ns2/sa2"`.

### GitHub webhook

| Method | Path                              | Purpose |
|---|---|---|
| POST   | `/v1/webhook/github`              | Receives `issues` and `workflow_run` events. |

The handler:

1. Verifies `X-Hub-Signature-256` against `GITHUB_WEBHOOK_SECRET`.
2. **`issues`** → match the workflow whose `trigger_label` fires for this event, then route through `dispatch_run` (see below). The label-trigger path is preserved for backward compatibility, but labels are no longer the dispatch primitive — UI dispatch is also a first-class trigger source.
3. **`workflow_run.completed`** → pull lease_id back out of `workflow_run.inputs`, look up project by repo, call `release()`. If the originating workflow opted into the verify-loop substrate (see below), also fetch the verification artifact, run the decision engine, and either dispatch the retry workflow or abort with an issue comment. On terminal Run transitions (PASS / ABORT) or non-Run-tracked completions, also releases the per-issue lock claimed by `dispatch_run`. Belt-and-suspenders alongside any in-workflow release step. Idempotent.
4. Other events → ignore.

### Unified dispatch (`dispatch_run`)

Both the GH webhook and the UI's `POST /v1/runs/dispatch` route through one function in [`src/glimmung/dispatch.py`](src/glimmung/dispatch.py). It:

1. Resolves project (by `github_repo`) and workflow (explicit param, or the project's only registered one).
2. Claims the `("issue", "<repo>#<number>")` lock — concurrent dispatches on the same issue serialize cleanly; the second sees `state="already_running"` without acquiring a lease or firing `workflow_dispatch`.
3. Acquires a lease + fires `workflow_dispatch` (or returns `state="pending"` if no host capacity).
4. Creates a `Run` record if the workflow opts into the verify-loop substrate (`retry_workflow_filename` set), with `trigger_source` recorded for W6 observability.

Lock TTL = `lease_default_ttl_seconds` (4h). Release happens at terminal Run transition (PASS / ABORT) for verify-loop workflows, or at lease release for non-Run-tracked workflows. Lock + lease both expire via TTL/sweep if `workflow_run.completed` never fires.

## Storage

Cosmos DB NoSQL on the shared `infra-cosmos-serverless` account. Database `glimmung`, seven containers (all pre-created by [`tofu/db.tf`](tofu/db.tf)):

- `projects` (partition key `/name`)
- `workflows` (partition key `/project`)
- `hosts` (partition key `/name`)
- `leases` (partition key `/project`)
- `runs` (partition key `/project`) — verify-loop run state, see below
- `locks` (partition key `/scope`) — generic mutual-exclusion primitive, see below
- `signals` (partition key `/target_repo`) — signal bus for triage / re-entry / future automations, see below

Runtime pod auth via the `infra-shared-identity` workload identity, which has `Cosmos DB Built-in Data Contributor` at the account scope (granted in [`infra-bootstrap/tofu/cosmos-serverless.tf`](https://github.com/nelsong6/infra-bootstrap/blob/main/tofu/cosmos-serverless.tf)). Container clients are obtained via `get_*_client` (no API call); reads/writes use the data-plane permissions. CREATE DATABASE / CREATE CONTAINER is control-plane and runs only via tofu under the app SP.

## Lock semantics

Optimistic concurrency on the host doc's `_etag`. Acquire reads matching candidates, sorts by `lastUsedAt` (NULLs first → bin-pack toward unused venues), tries each via `replace_item(match_condition=IfNotModified)`. 412 PreconditionFailed → try the next. Bounded retry; loop terminates after exhausting candidates.

Release paths:
- **Fast**: workflow's own release step (if it has one).
- **Safety net**: `workflow_run.completed` webhook handler. Covers UI-cancellation, runner-died, network blips mid-step.
- **Backstop**: 60-second sweep on stale heartbeat (`ttl_seconds`-driven; default 4h, sized to outlast the longest known caller workflow so heartbeats are optional).

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

Pure function `decide(run) -> RunDecision` lives in [`src/glimmung/decision.py`](src/glimmung/decision.py). Side effects (artifact fetch, Cosmos updates, workflow dispatch, issue comment) live at the call site in `app.py`. Outputs:

- `ADVANCE` — verification passed; consumer's PR step runs.
- `RETRY` — dispatch retry workflow with `prior_verification_artifact_url`.
- `ABORT_BUDGET_ATTEMPTS` — `len(attempts) >= max_attempts`.
- `ABORT_BUDGET_COST` — `cumulative_cost_usd >= max_cost_usd` (checked first; harder cap).
- `ABORT_MALFORMED` — verification artifact missing or schema-invalid.

Decision logic and edge-case coverage live in [`tests/test_decision.py`](tests/test_decision.py).

## Lock primitive (W1 substrate)

Generic mutual-exclusion claim keyed by `(scope, key)`. Used by per-PR triage serialization (#19), per-issue dispatch serialization (#20), signal-drain locks, and any future critical-section need that's mutual exclusion on a logical entity (not host capacity — that's [`leases.py`](src/glimmung/leases.py), a different problem).

API in [`src/glimmung/locks.py`](src/glimmung/locks.py):

| Call | Behavior |
|---|---|
| `claim_lock(scope, key, holder_id, ttl_seconds, metadata?)` | Atomic create or take-over (when prior lock is RELEASED, EXPIRED, or HELD-but-time-expired). Raises `LockBusy(existing)` if currently held. |
| `release_lock(scope, key, holder_id)` | Idempotent. Returns `True` if we transitioned HELD→RELEASED, `False` otherwise (not ours / already released / never existed). |
| `extend_lock(scope, key, holder_id, ttl_seconds)` | Heartbeat. Validates holder. Raises `LockBusy` if the lock is no longer ours. |
| `read_lock(scope, key)` | Diagnostic point-read. Returns `Lock \| None`. Doesn't normalize state for expiry. |
| `sweep_expired_locks()` | Background loop in [`app.py`](src/glimmung/app.py); marks HELD-but-time-expired locks as EXPIRED. Cosmetic — claimers can take over directly without waiting. |

### Doc id

Deterministic: `f"{scope}::{urllib.parse.quote(key, safe='')}"`. Cosmos forbids `/`, `\`, `?`, `#` in ids; URL-encoding handles all four uniformly. Same scope + same key → same doc → Cosmos's `id`-uniqueness constraint enforces "only one active claimer at a time" for free.

### Holder semantics

`holder_id` is opaque to the primitive. Callers pick a stable identifier for their critical section — typically a signal_id, a run_id, or a fresh ULID per claim attempt. `release_lock` and `extend_lock` validate `holder_id` matches before acting.

`claim_lock` is **strict**: a second claim by the same `holder_id` while the lock is held also raises `LockBusy`. Callers wanting refresh-or-claim should use `extend_lock`. Restart-after-crash: pick a new `holder_id`; the previous instance's claim expires via TTL.

### Test coverage

29 unit tests in [`tests/test_locks.py`](tests/test_locks.py), all backed by the in-memory Cosmos fake at [`tests/cosmos_fake.py`](tests/cosmos_fake.py) so `_etag`/`IfNotModified` semantics + TTL behavior are exercised deterministically.

## Signal bus + PR triage (#19)

A `Signal` is a unit of work for the orchestrator. Webhooks (`pull_request_review`, future `issue_comment` / `pull_request_review_comment`) and the glimmung UI (reject button) enqueue Signals; a background drain loop (`_signal_drain_loop` in [`app.py`](src/glimmung/app.py)) processes them through the per-target lock primitive + triage decision engine.

**Per-PR serialization** is built in: signals on a PR whose lock is held stay PENDING and re-evaluate next tick. Two consecutive reject submissions queue cleanly — the first holds the PR lock through its triage cycle, the second waits.

**Triage decision engine** ([`src/glimmung/triage.py`](src/glimmung/triage.py)) is pure: `decide_triage(signal, run) → TriageDecision`. Outputs:

- `DISPATCH_TRIAGE` — re-open the linked Run, append a TRIAGE PhaseAttempt, claim issue + PR locks, dispatch the consumer's `triage_workflow_filename` with `feedback` as input. The PR lock is held through the triage cycle (including any RETRY decisions within); the `workflow_run.completed` terminal handler releases on PASS / ABORT.
- `IGNORE` — non-actionable signal (approved review, empty changes-requested body, etc.).
- `ABORT_NO_RUN` / `ABORT_BUDGET_*` — posts an explanation comment to the PR; lock released immediately.

Decision precedence: no-run → actionability → budget-cost → budget-attempts → DISPATCH_TRIAGE.

**PR↔Run linking** is automatic: `pull_request.opened` / `pull_request.reopened` events whose body matches `Closes #N` / `Fixes #N` / `Resolves #N` link the Run for that issue to the new PR (`Run.pr_number`, `Run.pr_branch`).

### Triage workflow contract

When glimmung dispatches `triage_workflow_filename`, it sets:

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

The triage workflow runs impl + verify with feedback in context, force-pushes the result, and uploads `verification.json` (same contract as retry workflows — see verify-loop substrate).

## Phases

1. **Phase 1** ✓ — lease primitive, sweep job, Cosmos backend.
2. **Phase 2** ✓ — GitHub App webhook receiver, `workflow_dispatch` firing, ingress at `glimmung.romaine.life`, Entra ID auth on admin endpoints.
3. **Phase 3** ✓ — Dashboard with SSE, project side pane, workflow as first-class abstraction, MSAL sign-in + admin panel.
4. **Phase 2.5** ✓ — Migrate spirelens `issue-agent.yaml` to consume glimmung leases. (Numbered out of order; see [glimmung issue #2](https://github.com/nelsong6/glimmung/issues/2) for the build order that actually happened.)
5. **Phase 4** — Runner-grounding (verify GHA runner is online before dispatching), dashboard cancel/preempt, migrate ambience + tank-operator agent flows.

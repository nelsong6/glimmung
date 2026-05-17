# Native Test-Slot Lifecycle Contract

This document is the product and implementation contract for native test-slot
capacity. If implementation behavior disagrees with this contract, the
implementation is wrong and should be migrated.

## Terms

- **Slot**: a named capacity unit for a project, such as
  `tank-operator-slot-5`. Callers do not choose the slot. Glimmung's allocator
  chooses a slot and tells the caller which one was assigned.
- **Queue size** or **slot count**: the desired number of prepared slots for a
  project. This is managed through
  `PATCH /v1/projects/{project}/test-environments/count`.
- **Preliminary resources**: resources that make a slot leasable but do not run
  the project, a session, browser tooling, or validation workload. These may
  include slot metadata, DNS and routing prerequisites, Entra redirect URIs,
  Azure federated identity credentials, namespaces, service accounts, RBAC,
  ExternalSecrets, and other zero-steady-runtime scaffolding.
- **Warm** or **prepared**: all preliminary resources for the slot exist and
  have reconciled successfully.
- **Available**: warm and not currently leased.
- **Leased** or **assigned**: Glimmung has selected the slot for a checkout or
  run request and recorded the lease.
- **Hot** or **active runtime**: lease-scoped resources are running for the
  assigned slot. This includes app deployments, API proxy deployments, session
  pods, Playwright or browser-tooling pods, and any other workload that exists
  to execute or serve the leased test environment.

Warm is not a weaker form of hot. A warm available slot should be cheap to keep
around. It should not contain long-running app, proxy, session, Playwright, or
agent workload pods.

## API Responsibilities

### Queue Size

`PATCH /v1/projects/{project}/test-environments/count` writes the desired slot
count and returns. It does not warm slots synchronously. Preliminary
reconciliation for newly added slots is durable reconciler work — the
test-slot reconciler tick seeds missing `slots[*]` records, runs
`EnsureTestSlotPreliminaries`, and transitions each from `warming` to `ready`.
A handler that blocked here would leave the project doc permanently
inconsistent if it crashed mid-warm, and its `200 OK` would be a lie about
what was actually stored.

This path must not create long-running runtime resources. It must not create or
keep project app deployments, API proxy deployments, session pods, Playwright
servers, or validation jobs as part of making a slot available.

Decreasing the count is the destructive capacity path. It deletes preliminary
resources for slots above the new count after ensuring no active lease still
owns those slots. This is the only destructive scale path — there is no
separate "repair" or "reset" surface that can damage capacity outside the
queue-size handler.

### Checkout

`POST /v1/test-slots/checkout` asks Glimmung for a lease on an available test
environment. The request may identify the project and requester, but it must
not choose the slot. Glimmung chooses an available slot and returns the assigned
slot name, URL, and lease reference.

Runtime materialization belongs after slot assignment. If the checkout response
claims the lease is usable, the required runtime resources for that lease must
have been created and reached readiness.

Checkout may return before runtime activation completes. In that case it must
return `202 Accepted`, `state: "activating"`, `usable: false`, the assigned
slot name, lease reference, URL, and a status URL. Callers must poll the status
URL, or `/v1/state`, until the slot reports `state: "active"` and
`usable: true` before treating the environment as ready. A checkout response
must not hold the public HTTP request open while rendering/applying the
project chart or waiting for runtime deployments.

Activation is durable slot work. When checkout returns `202 Accepted`, Glimmung
has already recorded `activation_attempt`, `activation_state`,
`activation_started_at`, and `activation_job_name` on the slot status. A server
restart must be able to recover any claimed slot left in `activating` and
continue activation from those records. On success Glimmung records
`activation_completed_at`; on failure it records `activation_error`, marks the
slot `error`, and releases the lease after cleanup.

When native Playwright support is enabled, activation must create the
slot-local `slot-playwright` Deployment and Service and wait for the Deployment
to report ready and available replicas before recording the slot as active.

The `slot-playwright` Service is the canonical browser surface for the lease.
Session-side tooling (mcp-glimmung's `inspect_browser_url`, agent-driven
browser scripts) drives this Playwright over its WebSocket protocol instead of
launching a browser elsewhere. Checkout and `/v1/state` responses expose the
endpoint as `playwright_ws_endpoint` on the slot and on the active lease,
shaped `ws://slot-playwright.<slot-name>.svc.cluster.local:<port>`. The field
is omitted on clusters where Playwright support is disabled; tools that need
it must treat absence as "this cluster does not run lease-scoped browsers"
rather than fall back to a shared host.

### MCP Checkout Surface

`nelsong6/mcp-glimmung` exposes `checkout_test_slot` as the session-facing MCP
wrapper for `POST /v1/test-slots/checkout`. Its tool signature must match the
HTTP request contract: project identity, requester/Tank session identity,
optional workflow, and optional TTL only.

The MCP tool must not expose or forward `slot_index`, `mode`, `phase_inputs`,
or any other caller-owned slot identity or cleanup controls. Glimmung chooses
the slot, and destructive cleanup is reserved for return and queue-size changes.

When this API changes, update `mcp-glimmung`'s tool signature, docstring, and
payload tests in the same rollout. A stale MCP tool is a contract bug even when
the Go API correctly rejects the obsolete fields.

### Return

`POST /v1/test-slots/return` starts release of the lease and teardown of hot
runtime resources for that lease. It keeps the slot's preliminary resources so
the slot can become available again without destructive re-provisioning.

Return may be asynchronous. In that case it returns `202 Accepted`,
`state: "cleaning"`, `usable: false`, the lease reference, and a status URL.
Callers must poll the status URL, or `/v1/state`, until the slot reports
`state: "available"` and no active lease. The lease remains claimed while
cleanup runs so the allocator cannot hand the slot to a new caller before hot
runtime is gone and preliminaries are ready again. Glimmung records
`cleanup_state`, `cleanup_started_at`, `cleanup_completed_at`, and
`cleanup_error` on the slot status, and recovery must restart stale `cleaning`
work.

Return is not the scale-down path. It must not delete the slot's baseline
capacity unless the caller is explicitly changing queue size.

Lease callback release for a test-slot lease follows the same runtime cleanup
path as `POST /v1/test-slots/return`. Callback release must not mark the lease
released until hot runtime teardown and preliminary revalidation have
completed.

### Lifecycle Triggers

Test-slot state is event-driven. There is no polling reconciler loop. Every
lifecycle transition responds to an explicit event:

| Event | Trigger | Effect |
|---|---|---|
| count changed | `PATCH /v1/projects/{project}/test-environments/count` | handler writes the new count, fires per-slot warm goroutines for any missing or in-flight-`warming` slot, returns immediately |
| checkout | `POST /v1/test-slots/checkout` | acquires lease, arms a `time.AfterFunc` for `assigned_at + ttl_seconds`, starts activation goroutine |
| return / callback release / admin cancel | `POST /v1/test-slots/return`, `POST /v1/lease-callbacks/.../release`, `POST /v1/leases/cancel` | stops the lease's TTL timer, starts cleanup goroutine |
| TTL deadline | per-lease `time.AfterFunc` fires | starts cleanup goroutine with source `lease.ttl_expiry` |
| activation finished | inline at end of activation goroutine | one-shot installer cleanup, write `active` status |
| cleanup finished | inline at end of cleanup goroutine | release lease, write `ready` status |
| process start | `RecoverInFlightTestSlots` (one-shot, called once from `cmd/glimmung-go/main.go`) | re-arm TTL timers for surviving `claimed` leases, resume in-flight `warming`/`activating`/`cleaning` goroutines, warm missing `slots[*]` entries |

The TTL timer is the design choice that lets the lifecycle stay event-driven
without losing auto-expiry. Polling the lease list every N seconds to ask
"are any leases expired yet?" would burn Cosmos reads forever and is the
lazy version of what a deadline-bound timer expresses directly. A timer
firing at `assigned_at + ttl_seconds` is the same shape as an HTTP request
arriving: it's the event we wanted, delivered when we wanted it.

Timer state is in-process and not durable. Recovery is the responsibility of
`RecoverInFlightTestSlots`: on every process boot it walks Cosmos once and
re-arms an `AfterFunc` for every still-`claimed` test-slot lease, computing
remaining duration from the durable `assigned_at + ttl_seconds`. A deadline
that has already passed fires cleanup immediately. After this one-shot pass
returns, the lifecycle is purely event-driven until the next restart.

### Multi-replica safety

The lifecycle does not require a single replica. During rolling deploys,
node drains, or future horizontal scaling, every running pod independently
arms a TTL timer for every claimed lease and runs its own
`RecoverInFlightTestSlots` sweep on startup. Concurrency safety is layered:

1. **Database CAS on meaningful state transitions.** When two pods both
   attempt the same one-time state transition — `active → cleaning` for
   TTL expiry, or `missing → warming` for first-time warmup — the
   etag-conditional `ReplaceItem` makes the database the synchronization
   point. The first writer wins; every loser gets `412 Precondition
   Failed` (surfaced as `ErrPreconditionFailed`) and no-ops. No leader
   election, no distributed lock.
2. **Retry-on-conflict for cross-slot writes.** A project doc's etag is
   bumped by writes to *any* slot, so two simultaneously-warming slots in
   the same project will trigger 412 on each other even though they're
   not actually racing for the same resource. The warmup claim handles
   this with a bounded retry loop that re-reads, re-checks our slot's
   state, and retries — only giving up if our slot has actually been
   claimed (state moved to `warming` recently or `ready`).
3. **`recoveryMinAge` skip.** Recent in-flight states (warming /
   activating / cleaning with `updated_at` within the last 5 minutes) are
   skipped by the recovery sweep — the assumption is that another live
   pod is still doing the work. Without this, a freshly-booted pod during
   a rolling-update overlap would race the live pod's Helm operation.
4. **Per-process dedup**. Each pod's `testSlotActivations` /
   `testSlotCleanups` / `testSlotWarmups` sync.Maps prevent the same pod
   from spawning two goroutines for the same operation.

Activation and cleanup *resume* (vs initiation) don't have a meaningful
state transition to CAS on — the slot stays in `activating` or `cleaning`
throughout the Helm operation. They rely on layers 3 and 4 plus Helm's
own tolerance of "another operation in progress" errors. The window where
this matters is small (rolling-update overlap during an in-flight Helm
install).

### Graceful shutdown

`cmd/glimmung-go/main.go` intercepts SIGTERM and SIGINT, drains the HTTP
server with a 30s deadline, then waits up to 4 minutes for in-flight
test-slot goroutines (warmup, activation, cleanup) to finish via
`WaitForInflightTestSlots`. The pod's `terminationGracePeriodSeconds` in
the Helm chart is 300s to fit this budget.

The shutdown wait isn't load-bearing for correctness — orphaned in-flight
states get picked up by the next pod's recovery sweep — but it keeps the
orphan rate low so the `recoveryMinAge` gate's "skip recent in-flight
states" heuristic is correct in practice. A pod that gets SIGKILL'd
without draining is a partial-cleanup hazard that the lifecycle can
handle, but graceful shutdown avoids the hazard entirely for normal
evictions (node drains, rolling deploys).

A `PodDisruptionBudget` (`maxUnavailable: 1`) explicitly documents that
the single replica is always evictable — node drains never block on
glimmung, and the correctness story handles the brief unavailability.

There is no admin "repair" endpoint, no periodic reconciler, no scheduled
sweep. A genuinely stuck slot is a code bug to fix, not a button to press.

## Resource Classification

The following resources are preliminary when they are tied to the configured
slot count and do not run a workload:

- slot records in project metadata
- Entra SPA redirect URIs
- Azure managed identity federated identity credentials
- DNS and Gateway API prerequisites
- namespaces
- service accounts
- RBAC bindings
- ExternalSecrets and generated Secrets
- ConfigMaps needed by future runtime activation

The following resources are hot runtime and must be lease-scoped:

- project app Deployments, StatefulSets, Pods, and Services
- API proxy Deployments, Pods, and Services
- session Pods and session namespace workloads
- Playwright or browser-tooling Deployments, Pods, and Services
- validation Jobs that execute the leased environment
- hot-swap helper workloads that keep a process alive

One-shot installer Jobs are acceptable only as an implementation detail of
reconciliation or activation. They should finish quickly, be TTL-cleaned or
explicitly deleted, and must not be treated as the warmed slot itself.

## Naming And Ownership

Project runtime resources belong to the assigned slot and should live in the
slot's namespace or explicitly configured session namespace. Their names should
come from the project/runtime contract, not from Glimmung internals.

Glimmung-owned helper resources may use Glimmung names only when they are
control-plane artifacts, such as short-lived installer Jobs in the native runner
namespace. Slot-owned runtime helpers should use slot-local names. For example,
the Playwright service for a slot is `slot-playwright` in the slot namespace.

## Failure Discovery

Warmup validates only preliminary readiness. It can prove that Glimmung can
prepare capacity, register auth redirect URIs, reconcile workload identities,
and create the required scaffolding.

Warmup cannot validate the PR, branch, session, or hot-swap code that will be
tested later. Runtime boot and application readiness validation belong to the
activation path after Glimmung assigns a lease.

## Implementation Rule

Any code path that makes an available unleased slot keep app, proxy, session,
Playwright, or other steady runtime pods alive violates this contract. Such
resources must move behind lease activation and be deleted on return.

## Slot Status Field Contract

Every field on a slot's status doc describes the slot's **current** state, not
its history. When a field is present, consumers may trust that it describes
something happening or true *right now*.

Concretely:

- `state`, `usable`, `detail`, `updated_at` always reflect the current slot
  state.
- `activation_attempt`, `activation_state`, `activation_started_at`,
  `activation_completed_at`, `activation_job_name`, `activation_error`
  describe the **current** activation. They are populated while a slot is
  `activating` or `active` and cleared by the cleanup pathway when the slot
  returns to the pool (`ready` / `available`). They are deliberately kept on
  the `error` state so an operator repairing the slot has the diagnostic
  context for what failed.
- `cleanup_state`, `cleanup_started_at`, `cleanup_completed_at`,
  `cleanup_error` describe the current or most recent cleanup of the slot.
  These are not cleared because they describe the slot itself (last time it
  was cleaned), not a transient lease's footprint.
- `test_slot_return_history` is the canonical audit trail of who used the
  slot, by which lease number, when, and why each return happened. This is
  the place to look for historical "what lease last used this slot" data;
  the activation_* fields are not.

Consumers (dashboard, mcp-glimmung tooling, operators) must not encode
"this field is only meaningful when the slot is in state X" logic in their
rendering layer. If a field is present, it's current. If it has been
superseded, the producer clears it.

## Completion Checklist

The slot system is not complete until all of these are true:

- Queue size increase warms only preliminary resources.
- Queue size decrease is the only destructive capacity path and refuses to
  remove any slot still owned by an active lease.
- Checkout is allocator-owned. Callers cannot select a slot through request
  fields, lease metadata, or phase inputs.
- Checkout returns quickly. Runtime activation is durable, async, recoverable,
  and pollable through slot status.
- Playwright-enabled slots do not report `usable: true` until the
  lease-scoped `slot-playwright` Deployment is ready.
- Return returns quickly. Runtime cleanup is durable, async, recoverable, and
  keeps the lease claimed until the slot is safe to allocate again.
- Lease callback release follows the same cleanup path as public test-slot
  return for test-slot leases.
- Expired claimed test-slot leases are cleaned by a per-lease
  `time.AfterFunc` armed at checkout. No polling loop scans for expirations.
- Slots in `error`, stale `warming`, or stale `cleaning` are recovered by
  `RecoverInFlightTestSlots` at process startup, not by a periodic tick.
  There is no admin repair endpoint; the only way to remove capacity is
  decreasing queue size.
- Missing `slots[*]` entries are seeded by the PATCH-count handler when the
  count changes and by the startup recovery sweep. There is no background
  job that re-checks count vs `slots[*]` between those two triggers.
- Short-lived installer Jobs and clone Secrets are cleaned once at the end
  of the activation that produced them, and once defensively during the
  startup recovery sweep for any slot found in `active`.
- Dashboard slot rows expose enough activation and cleanup metadata to debug
  stuck work without querying Cosmos directly. State for an unseeded slot is
  empty in the API and labeled "unseeded" in the UI — neither layer
  synthesizes "warming" as a placeholder, which would lie about durable
  state.
- CI or a dispatchable smoke workflow exercises checkout, activation, return,
  cleanup, and no-runtime-after-return against a live configured project.
- Function names, resource names, and documentation use the slot lifecycle
  terms in this document. Retired names are cleanup targets, not supported
  aliases.

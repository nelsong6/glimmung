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

### Reconciliation

Glimmung runs a single test-slot reconciler that owns durable lifecycle work.
On every tick it:

- seeds missing `slots[*]` records (count is set but the index is not in the
  array) and runs preliminary reconciliation to bring them from `warming` to
  `ready`;
- resumes stale `warming` entries whose preliminary reconciliation crashed,
  was rolled back, or never finished;
- restarts stale `activating` work;
- restarts stale `cleaning` work;
- cleans up short-lived installer Jobs and clone Secrets for active slots;
- starts cleanup for expired claimed test-slot leases.

There is no separate admin "repair" endpoint. Any slot in `error`, stale
`warming`, or stale `cleaning` is recovered on the next reconciler tick — an
operator-driven escape hatch is an exception path the migration policy
forbids. A genuinely stuck slot is a reconciler bug to fix, not a
button-to-press; once the reconciler change is shipped, the only way to remove
slot capacity is decreasing queue size.

The reconciler must not re-warm a slot that has a current `claimed` lease;
those slots are driven by the activation/cleaning paths attached to the lease.

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
- Expired or abandoned claimed test-slot leases are cleaned by reconciliation
  without requiring manual intervention.
- Slots in `error`, stale `warming`, or stale `cleaning` are recovered by the
  test-slot reconciler on its next tick. There is no admin repair endpoint;
  the only way to remove capacity is decreasing queue size.
- Missing `slots[*]` entries are seeded by the reconciler. Setting
  `native_standby_dns.count` is sufficient — the queue-size handler does not
  warm synchronously.
- Short-lived installer Jobs and clone Secrets are cleaned after success and
  reconciled after process restarts.
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

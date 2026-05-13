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

`PATCH /v1/projects/{project}/test-environments/count` changes desired slot
capacity. Increasing the count reconciles preliminary resources for the new
slots and marks them available only after that reconciliation succeeds.

This path must not create long-running runtime resources. It must not create or
keep project app deployments, API proxy deployments, session pods, Playwright
servers, or validation jobs as part of making a slot available.

Decreasing the count is the destructive capacity path. It may delete
preliminary resources for slots above the new count after ensuring no active
lease still owns those slots.

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

### Reconciliation And Repair

Glimmung runs a test-slot reconciler for durable lifecycle work. The reconciler
restarts stale `activating` work, restarts stale `cleaning` work, cleans up
short-lived installer Jobs and clone Secrets for active slots, and starts
cleanup for expired claimed test-slot leases.

`POST /v1/projects/{project}/test-environments/{slot_name}/repair` is the
explicit admin repair path for slots left in `error`, stale `warming`, or stale
`cleaning` states. Repair reuses the return cleanup path and then revalidates
preliminary resources. It must refuse to repair a healthy active lease; callers
should return that lease instead.

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
the Playwright service for a slot is `slot-playwright` in the slot namespace,
not a `glim-pw-*` resource in the runner namespace.

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
- Return returns quickly. Runtime cleanup is durable, async, recoverable, and
  keeps the lease claimed until the slot is safe to allocate again.
- Lease callback release follows the same cleanup path as public test-slot
  return for test-slot leases.
- Expired or abandoned claimed test-slot leases are cleaned by reconciliation
  without requiring manual intervention.
- Slots in `error`, stale `warming`, or stale `cleaning` can be repaired by an
  explicit admin operation that does not require queue-size churn.
- Short-lived installer Jobs and clone Secrets are cleaned after success and
  reconciled after process restarts.
- Dashboard slot rows expose enough activation and cleanup metadata to debug
  stuck work without querying Cosmos directly.
- CI or a dispatchable smoke workflow exercises checkout, activation, return,
  cleanup, and no-runtime-after-return against a live configured project.
- Function names, resource names, and documentation use the slot lifecycle
  terms in this document. Legacy names may remain only for compatibility
  cleanup of old resources.

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
have been created and reached readiness. If activation is asynchronous, the API
must expose that explicitly through a non-usable activating state.

### Return

`POST /v1/test-slots/return` releases the lease and tears down hot runtime
resources for that lease. It keeps the slot's preliminary resources so the slot
can become available again without destructive re-provisioning.

Return is not the scale-down path. It must not delete the slot's baseline
capacity unless the caller is explicitly changing queue size.

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

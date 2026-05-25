# Native Test-Slot Lifecycle Contract

This document is the product and implementation contract for native test-slot
capacity. If implementation behavior disagrees with this contract, the
implementation is wrong and should be migrated.

## Storage

Slot state lives in its own Cosmos collection (`slots`), one document
per slot, partition key `project`, document id `<project>:<slot_index>`.
Cross-slot writes do not contend because each slot is its own
document. Audit history lives in a sibling `slot_history` collection.
The project doc carries configuration only (count, github_repo,
helm config, etc.) — *not* slot state.

The slot doc's shape and operations are defined in
[`internal/server/slot.go`](../internal/server/slot.go) and
[`internal/server/slot_store.go`](../internal/server/slot_store.go).
Every write is an etag-conditional `UpdateIfMatch` keyed on the slot
row's own etag — never a read-modify-write of the project doc.

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
- **Provisioning** (formerly *warming*): the slot's preliminary resources
  are being created.
- **Provisioned** (formerly *ready*): all preliminary resources for the
  slot exist and have reconciled successfully.
- **Unseeded**: the slot doc exists (count says it should) but
  provisioning has not yet started. New explicit state — replaces the
  legacy "no record" implicit state.
- **Available**: provisioned and not currently leased. Derived from
  `state=provisioned + active_lease_ref=null` on the wire.
- **Leased** or **assigned**: Glimmung has selected the slot for a checkout or
  run request and recorded the lease. The slot's `active_lease_ref`
  field points at the lease.
- **Running** (formerly *active*): lease-scoped runtime is up and
  serving. App deployments, API proxy deployments, session pods,
  Playwright or browser-tooling pods, validation jobs.

Provisioned is not a weaker form of running. A provisioned available
slot should be cheap to keep around. It should not contain long-running
app, proxy, session, Playwright, or agent workload pods.

## API Responsibilities

### Queue Size

`PATCH /v1/projects/{project}/test-environments/count` writes the desired slot
count and returns. It does not warm slots synchronously. The handler:

1. Writes the new count to the project doc.
2. Creates a slot doc in `unseeded` state for any new index in `1..count`
   (idempotent via `If-None-Match: *`).
3. Deletes any slot doc with index > new count (after verifying no
   active lease references it).
4. Fires per-slot provisioning goroutines for any slot still in
   `unseeded`. Each goroutine writes its own slot doc via per-row CAS;
   no cross-slot contention.

A handler that blocked on provisioning would leave the system
permanently inconsistent if it crashed mid-provision, and its `200 OK`
would be a lie about what was actually stored. The provisioning work
is durable: a process restart between this PATCH and provisioning
completion is covered by `RecoverInFlightTestSlots`, which re-fires
goroutines for slots still in `unseeded` or `provisioning`.

This path must not create long-running runtime resources. It must not create or
keep project app deployments, API proxy deployments, session pods, Playwright
servers, or validation jobs as part of making a slot available.

Decreasing the count is the destructive capacity path. It deletes preliminary
resources for slots above the new count after ensuring no active lease still
owns those slots. This is the only destructive scale path — there is no
separate "repair" or "reset" surface that can delete capacity outside the
queue-size handler.

### Preliminary Repair

`POST /v1/projects/{project}/test-environments/{slot_name}/repair` is an
admin revalidation path for one configured slot. It is deliberately
non-destructive:

1. The slot name must be inside the project's current `1..count` configured
   capacity.
2. No claimed test-slot lease may reference the slot.
3. The handler marks the slot `provisioning` with per-row CAS, then runs
   preliminary reconciliation.
4. If the project has `metadata.test_slot_helm.enabled=true`, repair also
   reconciles the project chart with `renderMode=warm` only.
5. On success the slot returns to `provisioned`; on failure it returns to
   `error` with the failure detail.

Repair may re-enter `provisioning` from `unseeded`, stale `provisioning`,
`provisioned`, or a preliminary-resource `error`. It rejects `activating`,
`running`, `cleaning`, any slot with an active lease reference, and any
`error` carrying `cleanup_error`; those states belong to the runtime cleanup
path, not preliminary repair.

This path must not create long-running runtime resources. In particular it
must not install the `renderMode=hot` Helm release, create Playwright runtime,
or run validation jobs.

### Checkout

`POST /v1/test-slots/checkout` asks Glimmung for a lease on an available test
environment. The request may identify the project and requester, but it must
not choose the slot. Glimmung chooses an available slot and returns the assigned
slot name, URL, and lease reference.

Checkout capacity is project-local. A project's configured slot count and
per-project checkout concurrency decide whether it can receive another native
lease. Claimed native leases in other projects must not block checkout for this
project.

Runtime materialization belongs after slot assignment. If the checkout response
claims the lease is usable, the required runtime resources for that lease must
have been created and reached readiness.

Checkout may return before runtime activation completes. In that case it must
return `202 Accepted`, `state: "activating"`, `usable: false`, the assigned
slot name, lease reference, URL, and a status URL. Callers must poll the status
URL, or `/v1/state`, until the slot reports `state: "running"` and
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

The slot-local Playwright server image is version-coupled to remote clients
such as `mcp-glimmung`: Playwright rejects WebSocket clients with a different
major/minor protocol version. Keep `slot-playwright/Dockerfile` aligned with
the `playwright` package version in `mcp-glimmung` before changing either side.

### MCP Checkout Surface

`nelsong6/mcp-glimmung` exposes `checkout_test_slot` as the session-facing MCP
wrapper for `POST /v1/test-slots/checkout`. Its tool signature must match the
HTTP request contract: project identity, requester/Tank session identity,
optional workflow, and optional TTL only.

The checkout MCP tool must not expose or forward `slot_index`, `mode`,
`phase_inputs`, or any other caller-owned slot identity or cleanup controls.
Glimmung chooses the slot, and destructive cleanup is reserved for return and
queue-size changes.

`extend_test_slot_lease` wraps `POST /v1/test-slots/extend`. It updates the
claimed lease's durable `ttl_seconds` and re-arms the in-process expiry timer
from the Cosmos lease row. The endpoint requires `extend_seconds` and either a
slot selector (`slot_index` or `slot_name`) or the Tank session id that owns the
checkout. When both are present, the session id must match the lease requester
metadata. Extension is rejected after the current durable deadline has passed or
once slot cleanup has started.

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
`state: "available"` (derived: `provisioned` + no `active_lease_ref`) and no active lease. The lease remains claimed while
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
| count changed | `PATCH /v1/projects/{project}/test-environments/count` | handler writes the new count, seeds missing slot docs as `unseeded`, deletes slot docs above the new count, fires per-slot provisioning goroutines for any in `unseeded`, returns immediately |
| preliminary repair requested | `POST /v1/projects/{project}/test-environments/{slot_name}/repair` | validates the named slot is configured and unleased, marks it `provisioning`, reruns preliminary reconciliation plus the warm Helm pass, then marks it `provisioned` or `error` |
| default TTL changed | `PATCH /v1/test-slots/default-ttl` | updates the global generated test-slot lease TTL, or a project's override; future checkouts use the new default unless they pass `ttl_seconds` explicitly |
| hot-swap minimum TTL changed | `PATCH /v1/test-slots/hot-swap-min-ttl` | updates the global minimum lease duration after a hot-swap, or a project's override; hot-swap recording extends shorter active leases to this remaining duration |
| checkout | `POST /v1/test-slots/checkout` | acquires lease, arms a `time.AfterFunc` for `assigned_at + ttl_seconds`, starts activation goroutine |
| TTL changed | `PATCH /v1/leases/ttl` | updates the claimed lease document and re-arms this replica's timer from the durable deadline |
| extend | `POST /v1/test-slots/extend` | validates Tank-session ownership, adds `extend_seconds` to the claimed lease TTL, and uses the durable TTL update path |
| return / callback release / admin cancel | `POST /v1/test-slots/return`, `POST /v1/lease-callbacks/.../release`, `POST /v1/leases/cancel` | stops the lease's TTL timer, starts cleanup goroutine |
| TTL deadline | per-lease `time.AfterFunc` fires | starts cleanup goroutine with source `lease.ttl_expiry` |
| activation finished | inline at end of activation goroutine | one-shot installer cleanup, mark slot `running` |
| cleanup finished | inline at end of cleanup goroutine | release lease, mark slot `provisioned` |
| process start | `RecoverInFlightTestSlots` (one-shot, called once from `cmd/glimmung-go/main.go`) | re-arm TTL timers for surviving `claimed` leases, resume in-flight `provisioning`/`activating`/`cleaning` goroutines, provision missing slot docs |
| process start | `MigrateProjectSlotsIntoCollection` (one-shot, called once from `cmd/glimmung-go/main.go`) | one-time migration of any legacy `metadata.native_standby_dns.slots[]` arrays into the `slots` collection. Idempotent on subsequent boots. |

Generated test-slot leases resolve their TTL with this precedence:
explicit checkout `ttl_seconds`, project `metadata.test_lease_default_ttl_seconds`,
global `test_lease_defaults.global_ttl_seconds`, then the built-in one-hour
fallback. The `/v1/state` snapshot includes the global default, and project
metadata carries any project override so the Test leases UI can edit both
scopes without a separate read path.

After a test-slot hot-swap, Glimmung also ensures the active lease has at
least the configured hot-swap minimum TTL remaining. That value resolves from
project `metadata.test_lease_hot_swap_min_ttl_seconds`, global
`test_lease_defaults.hot_swap_min_ttl_seconds`, then the built-in 30-minute
fallback.

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

When a claimed lease's TTL is changed, only the handling replica can replace
its in-process timer immediately. Other replicas may still hold the previous
timer, so the timer callback re-reads the current lease document before
claiming cleanup. If the durable deadline has moved later, the stale callback
re-arms itself from the current document instead of cleaning the slot early.

### Cleanup interrupts activation

Activation goroutines spawned by `beginTestSlotActivation` are cancellable.
Every cleanup-entry path (return, callback release, TTL timer, startup
recovery) cancels any in-flight activation goroutine for the lease AND
awaits its unwind before issuing K8s deletes. The contract is necessary
because activation directly creates the lease-scoped `slot-playwright`
Deployment (and waits on the installer Job that helm-installs the
project's runtime workloads); without the cancel-await, cleanup races
those creates and `waitForNoPodsInNamespaces` spins until its 5-minute
timeout fires.

The implementation: `testSlotActivations` maps each in-flight activation
to a `*testSlotActivation` with a `cancel` func and a `done` channel
the goroutine closes from a defer. `cancelInflightActivation` looks up
the token, calls cancel, waits on done (bounded by
`activationCancelWait`, currently 30s). Activation goroutines inherit
the cancellable context end-to-end so every K8s API call honors it; the
post-cancel state-machine checks (`testSlotLeaseStillClaimed` /
`testSlotLeaseStillActivating`) catch the case where the slot has
transitioned to `cleaning` while the activation goroutine was unwinding,
and the activation bails without writing an error or invoking the
on-error inline cleanup.

`ReturnTestSlotRuntime` also reorders its K8s deletes for defense in
depth: the helm-install installer Job (the durable K8s-side producer of
slot-namespace workloads) is deleted first, its pods are awaited via
`waitForInstallerPodsTerminated`, and only then are the slot-namespace
workloads deleted. With both the in-process goroutine and the K8s Job
dead, the final `waitForNoPodsInNamespaces` sees a static target.

The cancel-from-cleanup path is observable via
`glimmung_test_slot_activation_cancelled_total{cause=...}`.

### Error recovery

`SlotStateError` is recoverable through cleanup-retry, not terminal in
the dead-end sense. A slot whose previous cleanup attempt landed it in
error with `cleanup_error` set converges to `provisioned` when a new
cleanup trigger fires: a follow-up `returnTestSlot`, callback release,
TTL timer, or the startup recovery sweep. K8s deletes underneath are
idempotent, so the retry either succeeds or re-errors with new
diagnostic context appended to `slot_history`.

`validSlotTransitions[SlotStateError]` allows `SlotStateCleaning`.
`MarkCleaning` and `MarkCleaned` both accept `error` as a prior state
(walking through `cleaning` first when invoked directly without an
intervening `claimTestSlotCleanup`). `claimTestSlotCleanup`'s probe
short-circuits only when the slot is already in `cleaning` (another
replica/caller won the race), not when it's in `error`.

`RecoverInFlightTestSlots` covers two error cases at startup:

- **Claimed lease + slot in error + `cleanup_error` set.** The slot
  belongs to a session whose cleanup goroutine died with the previous
  process. Recovery re-fires cleanup with `releaseLease=true` so a
  successful retry releases the lease and returns the slot to
  `provisioned`.
- **No claimed lease + slot in error + `cleanup_error` set.** The slot
  is orphaned (lease released or never recovered). Recovery re-fires
  cleanup with `releaseLease=false` via a synthetic warmup lease so the
  cleanup pathway is uniform.

Activation-error slots without a `cleanup_error` are intentionally not
re-fired automatically: the activation-error path already runs inline
cleanup before recording error, and re-running activation against a
slot whose lease may have been canceled is not a safe automatic
recovery. The decrease-then-increase count path remains the last-resort
operator action for genuinely stuck error states (cleanup retry also
failed).

### Cleanup-entry CAS contract

All cleanup-entry paths route through `claimTestSlotCleanup`, which
performs the durable `* → cleaning` state transition under the
SlotStore's per-row etag CAS. The probe rejects the claim if the slot
is already in `cleaning` (another caller/replica got there first); the
loser responds with the same 202 the granted-claim path returns
because the durable state is correct from the caller's perspective.
Outcomes are observable via
`glimmung_test_slot_cleanup_claim_total{source,outcome}` where source
is one of `return | callback_release | ttl_expiry | recovery` and
outcome is `granted | lost_race | error`.

This makes simultaneous-trigger races safe by construction: a public
return, a TTL timer firing on the same lease in another replica, and a
callback-release for the same lease all serialize on the slot doc's
etag — exactly one wins, the rest no-op.

### Multi-replica safety

The lifecycle does not require a single replica. During rolling deploys,
node drains, or future horizontal scaling, every running pod independently
arms a TTL timer for every claimed lease and runs its own
`RecoverInFlightTestSlots` sweep on startup. Concurrency safety is
straightforward because each slot is its own Cosmos document:

1. **Per-row etag CAS.** Every slot mutation goes through
   `SlotStore.UpdateIfMatch`, which reads the slot doc with its etag,
   applies the requested transition, and writes with `IfMatch: <etag>`.
   When two replicas attempt the same transition (e.g., both timers fire
   `running → cleaning` for the same lease), exactly one's
   `ReplaceItem` succeeds; the other gets `412 Precondition Failed`
   surfaced as `ErrPreconditionFailed` and no-ops.
2. **Per-process dedup.** Each pod's `testSlotActivations` /
   `testSlotCleanups` sync.Maps prevent the same pod from spawning two
   goroutines for the same operation. With per-row CAS as the durable
   synchronization point, the in-process maps are a soft optimization
   that avoids spawning a goroutine that would lose CAS anyway.

The cross-slot contention layer from the previous design (multiple
warmup goroutines racing on the project doc's shared etag) does not
exist in the new shape — each slot's writes are independent. Retry
budgets, jittered backoff, and the `recoveryMinAge` skip heuristic
became unnecessary at the same time and were removed.

Activation and cleanup *resume* (vs initiation) don't have a meaningful
state transition to CAS on — the slot stays in `activating` or
`cleaning` throughout the Helm operation. Per-process dedup plus Helm's
own tolerance of "another operation in progress" errors covers the
brief rolling-update overlap window.

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

There is no periodic reconciler and no scheduled sweep. Admin repair is an
explicit, one-slot revalidation action for configured unleased capacity; it is
not a reset button and cannot bypass runtime cleanup.

## Resource Classification

The following resources are preliminary when they are tied to the configured
slot count and do not run a workload:

- slot records (docs in the `slots` collection)
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

Every field on a slot doc describes the slot's **current** state, not
its history. When a field is present, consumers may trust that it describes
something happening or true *right now*.

Concretely:

- `state`, `detail`, `updated_at`, `provisioned_at` always reflect the
  current slot state.
- `active_lease_ref` is set while a lease holds the slot
  (`activating` / `running` / `cleaning`) and cleared on return to
  `provisioned`. It is the canonical pointer to the lease doc; the slot
  doc does not duplicate lease metadata.
- `activation_attempt`, `activation_started_at`,
  `activation_completed_at`, `activation_job_name`, `activation_error`
  describe the **current** activation. They are populated while a slot is
  `activating` or `running` and cleared by the cleanup pathway when the
  slot returns to the pool (`provisioned` / available). They are
  deliberately kept on the `error` state so an operator repairing the
  slot has the diagnostic context for what failed.
- `cleanup_started_at`, `cleanup_completed_at`, `cleanup_error` describe
  the current or most recent cleanup of the slot. These are not cleared
  because they describe the slot itself (last time it was cleaned),
  not a transient lease's footprint.
- Slot history (return events, errors over time) lives in the separate
  `slot_history` collection — *not* on the slot doc itself. Look there
  for "what lease last used this slot" and "what happened over time"
  data. The slot doc is current-state-only.

The `/v1/state` snapshot derives wire-compat fields (`activation_state`,
`cleanup_state`, `ready_at`, `test_slot_return_history`) from the new
shape for consumers that haven't migrated. New consumers should read the
slot doc fields directly.

Consumers (dashboard, mcp-glimmung tooling, operators) must not encode
"this field is only meaningful when the slot is in state X" logic in
their rendering layer. If a field is present, it's current. If it has
been superseded, the producer clears it.

## Completion Checklist

The slot system is not complete until all of these are true:

- Queue size increase and preliminary repair create only preliminary resources.
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
- Slots in `error` (with `cleanup_error` set), stale `provisioning`, or
  stale `cleaning` are recovered by `RecoverInFlightTestSlots` at
  process startup, not by a periodic tick. Operator-driven cleanup
  retry via a follow-up `returnTestSlot` against an error slot is also
  supported and goes through the same `error → cleaning` state-machine
  transition; the API-level retry exists so a flaky cleanup doesn't
  require a process restart. Preliminary-resource errors without
  `cleanup_error` may be retried through the admin repair endpoint, which
  goes through `error → provisioning`. The last-resort capacity-removal
  path for a genuinely stuck cleanup-error slot remains decreasing queue size.
- Missing slot docs are seeded by the PATCH-count handler when the count
  changes and by the startup recovery sweep. There is no background
  job that re-checks count vs the `slots` collection between those two
  triggers.
- Slot writes use per-row etag CAS via `SlotStore.UpdateIfMatch`. The
  retired `SetProjectTestEnvironmentSlotStatus`,
  `SetProjectTestEnvironmentSlotStatusIfMatch`,
  `ProjectTestEnvironmentSlotStatusWriter`,
  `ProjectTestEnvironmentSlotStatusClaimer`, and `claimTestSlotWarmup`
  names must not return — they belonged to the embedded-array shape
  this contract replaced.
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

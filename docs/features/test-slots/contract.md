# Test Slots Contract

This contract applies to native test-slot capacity, slot provisioning,
checkout, activation, return, lease TTL, slot-local Playwright, and test-slot
hot swap.

## Product Model

Test slots are prepared capacity for validating agent work without waiting for
full rollout. A slot is not running user workload until Glimmung assigns a
lease. The system must make the difference between provisioned, leased,
running, cleaning, and available explicit.

## Sources Of Truth

- Postgres `slots` owns per-slot lifecycle state.
- Postgres `slot_history` owns slot audit history.
- Postgres `projects` owns slot configuration only, such as count, DNS, Helm,
  workload identity, and hot-swap metadata.
- Postgres `leases` owns active capacity claims and lease TTL.
- Kubernetes owns actual preliminary and lease-scoped resources.
- `docs/test-slot-lifecycle.md` owns slot terms and lifecycle behavior.
- `docs/test-slot-hot-swap.md` owns hot-swap metadata shape and apply behavior.
- `Settings.ControlPlaneLoopsEnabled` (env `CONTROL_PLANE_LOOPS_ENABLED`,
  enforced in `cmd/glimmung-go/main.go`) owns the boundary between processes
  that join the control plane and processes that only serve HTTP handlers.
  The prod glimmung Deployment leaves it at the default `true`; every
  per-issue release rendered by `k8s/issue/` (hot and warm) sets it to
  `false`.

## Migration Rules

- Do not store slot lifecycle state in project metadata.
- Do not treat warmed/provisioned capacity as running application workload.
- Do not let callers choose a slot for checkout; Glimmung chooses.
- Do not expose destructive cleanup controls through the MCP checkout surface.
- Do not keep manual `kubectl cp` plus `kill -HUP` as the documented hot-swap
  path after the apply endpoint supports the project.
- Do not add app/proxy/session/browser pods to unleased preliminary capacity.
- Do not start a background reconciler, recovery sweep, or other goroutine
  that mutates shared runtime state (Postgres rows owned by the run/signal
  loop, `glimmung-runs` Kubernetes Jobs, prod-namespace Secrets) outside the
  `settings.ControlPlaneLoopsEnabled` gate in `cmd/glimmung-go/main.go`. Slot
  processes serve HTTP handlers against the same Postgres and the same
  apiserver as prod; running the control plane in a slot races prod for the
  same rows and Jobs. Add new reconcilers to the gated `switch`, not next to
  it.

## Live Behavior

- Queue-size changes seed or delete slot docs and fire per-slot provisioning
  work without blocking on runtime activation.
- Admin repair revalidates one configured, unleased slot by rerunning
  preliminary reconciliation and the warm Helm pass only; it must reject active
  leases and runtime cleanup states.
- Checkout records a lease before activation and returns either ready runtime
  state or an explicit asynchronous activating state.
- Activation materializes lease-scoped runtime and waits for required runtime
  readiness before `usable=true`.
- Return and callback release tear down lease-scoped runtime before capacity is
  available again.
- TTL updates and extensions update durable lease state and re-arm timers from
  the durable deadline.
- When the originating Issue has `preserve_test_env=true`, the workflow's
  early cleanup phase reports conclusion `skipped` instead of executing.
  The lease stays alive through the human review gate so the reviewer can
  poke at the live build, and the final cleanup phase tears it down after
  approve, reject, or abort releases the gate.
- Hot swap reads `metadata.test_slot_hot_swap`, builds from `git_ref`, copies
  into the selected leased slot, restarts as configured, records history on
  every outcome, and extends short leases to the configured minimum remaining
  TTL.
- A slot process (the binary running inside any `k8s/issue/` release, hot or
  warm) starts the HTTP server, applies database migrations a hot-swap may
  need to land, and serves request-driven code paths. It does not start the
  signal-drain, run-queue, dispatch-timeout, or test-slot recovery
  reconcilers; those run only in the prod glimmung Deployment.
- Lease cleanup is the single retention boundary for free
  (lease-scoped) inspections produced by `POST /v1/inspections`. The
  cleanup goroutine deletes every matching `slot_inspections` row and the
  underlying `report.json` + `screenshot.png` blobs. See
  [Observability And Evidence capabilities → durable-inspections](../observability-and-evidence/capabilities.md).

## Failure And Recovery

- Process start resumes in-flight provisioning, activation, cleaning, and TTL
  timers from durable state.
- Process start also expires every lease whose durable `expires_at` deadline
  has passed but whose state is still `active` or `claimed` (orphaned
  callback releases, AfterFunc timers killed with the previous process,
  pre-test-slot-lifecycle lease shapes). The sweep is gated by
  `Settings.ControlPlaneLoopsEnabled` so slot processes never run it. See
  `server.ExpireStaleLeases`.
- Admin repair may retry preliminary-resource errors, but cleanup-error slots
  remain on the runtime cleanup path.
- Activation failure records error state and releases or cleans up the lease
  through the lifecycle path.
- Cleanup failure leaves the slot unavailable with visible error state rather
  than handing out dirty runtime.
- Hot-swap build, copy, timeout, and health failures are recorded in lease
  metadata or history.
- Slot-local Playwright absence is treated as unsupported cluster capability,
  not as permission to fall back to an unrelated browser host.

## Observability

- Slot state, active lease ref, activation state, cleanup state, error fields,
  and Playwright endpoint should be visible in `/v1/state`.
- Hot-swap history should include operation, status, summary, diagnostics, and
  timings.
- Provisioning, activation, cleanup, TTL expiry, and hot-swap failures should
  be identifiable by project, slot, lease, and source event.

## Acceptance Checks

- Slot lifecycle changes include tests for the durable state transition being
  changed.
- MCP checkout or lease extension changes update `mcp-glimmung` tool signature,
  docstring, and payload tests in the same rollout.
- Hot-swap contract changes update validation tests and the live project
  metadata shape when needed.
- Helm/chart changes run `helm template` or equivalent rendering evidence.
- Changes prove that unleased provisioned slots do not run long-lived runtime
  workload.
- A new background reconciler or recovery sweep adds a test that proves the
  Start… function is unreachable when `Settings.ControlPlaneLoopsEnabled` is
  false (either the gate in `cmd/glimmung-go/main.go` skips the call, or the
  Start… function itself returns early). See
  [Test Slots capabilities → slot-control-plane-isolation](capabilities.md).

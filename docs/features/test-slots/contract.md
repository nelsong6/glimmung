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

## Migration Rules

- Do not store slot lifecycle state in project metadata.
- Do not treat warmed/provisioned capacity as running application workload.
- Do not let callers choose a slot for checkout; Glimmung chooses.
- Do not expose destructive cleanup controls through the MCP checkout surface.
- Do not keep manual `kubectl cp` plus `kill -HUP` as the documented hot-swap
  path after the apply endpoint supports the project.
- Do not add app/proxy/session/browser pods to unleased preliminary capacity.

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

## Failure And Recovery

- Process start resumes in-flight provisioning, activation, cleaning, and TTL
  timers from durable state.
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

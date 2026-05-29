# Test Slots Capabilities

This ledger names user-facing behavior under the test-slots contract. It is
not a backlog. Entries land here when the behavior needs a stable handle for
planning, review, tests, incident follow-up, or retirement.

## slot-control-plane-isolation

Status: shipped

Intent:
A slot process (the binary running inside any `k8s/issue/` release, hot or
warm) serves the HTTP handler surface against the shared Postgres database
and the shared Kubernetes apiserver, and nothing else. It must not start any
background reconciler or recovery sweep that mutates run state, lease state,
signal state, or `glimmung-runs` Kubernetes Jobs. Those belong to the prod
glimmung Deployment, which is the single writer for the control plane.

This is the boundary that lets a hot-swapped binary exercise new code paths
against the real database and the real apiserver without racing the prod
control plane on the same rows and Jobs.

Affected contracts:
- Test Slots (primary — the slot is the isolation boundary)
- Workflow Execution (run-queue, dispatch-timeout, and any future workflow
  reconciler must honor the same gate)

Contract impact:
- `Settings.ControlPlaneLoopsEnabled` (env `CONTROL_PLANE_LOOPS_ENABLED`,
  default `true`) is the canonical gate. The prod Deployment leaves it at
  the default; `k8s/issue/templates/deployment.yaml` sets it to `false` on
  every per-issue release.
- `cmd/glimmung-go/main.go` is the single enforcement point. The
  `switch` that starts `StartSignalDrainReconciler`,
  `StartRunQueueReconciler`, `StartRunDispatchTimeoutReconciler`, and
  `RecoverInFlightTestSlots` is gated on `settings.ControlPlaneLoopsEnabled`
  and emits a startup log line when the gate is closed. Any new reconciler
  or recovery sweep that touches shared runtime state must be added inside
  the same `switch`.
- The slot Deployment in `k8s/issue/templates/deployment.yaml` keeps an
  inline comment naming the gate so a future reader does not strip the
  env var without understanding what it now controls.

Evidence:
- `internal/server/settings_test.go` — `TestSettingsFromEnv_ControlPlaneLoopsEnabled`
  pins default-true, accepted truthy/falsy values, and garbage-falls-back-to-default.
- `cmd/glimmung-go/main.go` — the gated `switch` that wraps every
  background reconciler and the test-slot recovery sweep.
- `internal/server/server.go` — `Settings.ControlPlaneLoopsEnabled` field
  doc explaining the prod-vs-slot invariant.
- `k8s/issue/templates/deployment.yaml` — env-var stanza with an inline
  comment pointing at `Settings.ControlPlaneLoopsEnabled`.

History:
- Before this capability was named, `CONTROL_PLANE_LOOPS_ENABLED` was set on
  the per-issue chart but unread by the Go binary. Slot binaries ran every
  control-plane reconciler against shared Postgres; the omission only became
  visible when a hot-swapped reconciler began calling the apiserver for
  Jobs in `glimmung-runs` and hit 403 against the slot's narrowly-scoped
  ServiceAccount. The fix made the env var real rather than expanding slot
  RBAC.

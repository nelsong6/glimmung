# Test-slot hot-swap

Build new code, place it on a running test slot, step back. One MCP
call. This doc describes the workflow, the contract shape, and the
guarantees the platform makes.

The workflow replaces the manual `kubectl cp` + `kubectl exec` +
`kill -HUP 1` pattern that previous test-slot iteration required (and
which the `/test` skill in agent harnesses currently documents as the
expected dance). That manual pattern is **deprecated** by this endpoint;
the agent skill should migrate to the new MCP tool. Manual ops still
work for one-off debugging, but should not be the documented dev loop.

## The contract — project-side

Each Glimmung project that opts into test-slot hot-swap declares a
`test_slot_hot_swap` block in its `metadata`. The block has four
sub-contracts; a project enables whichever ones it needs.

```json
{
  "test_slot_hot_swap": {
    "enabled": true,

    "static": {
      "enabled": true,
      "source": "frontend/dist",
      "target": "/var/run/orchestrator-static-override"
    },

    "backend": {
      "enabled": true,
      "strategy": "supervisor",
      "build_command": "cd backend-go && go build -o /tmp/app ./cmd/app",
      "artifact": "/tmp/app",
      "target": "/var/run/orchestrator-hot/app",
      "health_path": "/healthz",
      "builder_image": "golang:1.26-alpine"
    },

    "agent_runner": {
      "enabled": true,
      "source": "agent-runner/dist",
      "target": "/var/run/agent-runner-hot/dist",
      "build_command": "cd agent-runner && npm run build",
      "pod_selector": "tank-operator/session-id",
      "container": "agent-runner",
      "restart": "SIGHUP",
      "builder_image": "node:20-alpine"
    },

    "codex_runner": {
      "enabled": true,
      "source": "codex-runner/dist",
      "target": "/var/run/codex-runner-hot/dist",
      "build_command": "cd codex-runner && npm run build",
      "pod_selector": "tank-operator/session-id",
      "container": "codex-runner",
      "restart": "SIGHUP",
      "builder_image": "node:20-alpine"
    }
  }
}
```

### `builder_image` per artifact kind

Each app declares its own build environment. The build runs in a one-off
Kubernetes Job's init container using exactly the image named here. No
language heuristics, no hardcoded defaults — the contract owns this so
the project's build environment is explicit and reproducible.

For `agent_runner` and `codex_runner`, `builder_image` is **required at
contract validation time**: there is no legacy CLI path for these
kinds, so a missing image is unambiguous misconfiguration. For `backend`,
`builder_image` is **optional at validation time** (existing registered
contracts predate the field) but **required at request time** when the
apply endpoint is invoked with `artifact_kind=backend`.

## The endpoint

`POST /v1/test-slots/apply-hot-swap` (admin-authenticated).

```json
{
  "project": "tank-operator",
  "slot_name": "tank-operator-slot-1",
  "artifact_kind": "agent_runner",
  "git_ref": "feat/durable-stop-request",
  "timeout_seconds": 120
}
```

### Sync UX, ArgoCD pattern

The endpoint **blocks until done or timeout**. Researched against
Google AIP-151 (which prescribes async-only operations for >10s); we
chose ArgoCD's `app sync` pattern instead because the caller is a
developer iterating, not a platform service that fans out. The dev
wants one call to return done/failed/diagnostics, mirroring `kubectl`
and `helm install`'s blocking-first design.

Timeout defaults to 120s (covers the 30-90s expected build-and-swap
range plus buffer for cold image pulls); caller can specify
`timeout_seconds` in the request, clamped server-side to a hard max of
600s. A caller asking for 8 hours can't hold a connection open beyond
the cap; the underlying Job runs to its own deadline.

### What the endpoint does

1. Resolves the active test-slot lease for `project + slot`.
2. Reads the project's `test_slot_hot_swap` contract from metadata.
3. Validates `artifact_kind` is supported and (for `backend`) the
   request-time `builder_image` is present.
4. Dispatches a one-off Kubernetes Job:
   - **Init container** uses `contract.<kind>.builder_image`. Clones
     the repo at `git_ref`, runs `contract.<kind>.build_command`,
     leaves the resulting source dir at `/work/source`.
   - **Main container** uses a kubectl-only image. Reads `/work/source`,
     tar-streams its contents into `contract.<kind>.target` inside the
     target pod, sends `contract.<kind>.restart` to PID 1.
5. Watches the Job to completion via `kubectl wait`.
6. Collects build + swap logs (last 4000 chars each).
7. Appends a hot-swap history entry to the lease — **always**, success
   or failure. Durable state in the system, not in the response.
8. Returns a structured result.

### What's deliberately out of scope (v1)

- **Async / fire-and-forget mode.** Sync is the documented shape;
  async will be additive if we ever need it (the Job's history record
  already exists in the lease, so the platform shape supports both).
- **Streaming progress.** The Job's logs are available via
  `kubectl logs job/<name>` while the swap is in flight, for live
  inspection. The final result includes the last 4000 chars.
- **`artifact_kind=static` and `artifact_kind=backend`.** Today these
  route to `ops.TestSlotHotSwap` via the `glimmung-agent` CLI in the
  verify-loop infrastructure. The developer-driven apply endpoint
  covers `agent_runner` today; static and backend land in v2 when
  their consumers explicitly opt in.

## The outcome

The response has an `Outcome` field with four bounded values:

- `persisted` — Job's "complete" condition fired; new code is running
  in the target pod(s).
- `build_failed` — init container exited non-zero. Build logs in the
  response surface the failure.
- `swap_failed` — main container exited non-zero. Swap logs surface
  the failure.
- `timeout` — Job didn't complete within the request's timeout. The
  Job continues running on the cluster (its own deadline is set higher
  than the request's), so the final result lands in the lease history
  even though the request returned early.

## Migrating from the manual kubectl pattern

If you're an agent harness or developer following the previous `/test`
skill instructions:

| Old | New |
|---|---|
| `cd backend-go && go build -o /tmp/app ./cmd/app` | (Glimmung does this in the Job's init container) |
| `kubectl exec -i $pod -- sh -c 'cat > /var/run/.../app' < /tmp/app` | (Glimmung's main container does kubectl-stream) |
| `kubectl exec $pod -- kill -HUP 1` | (Glimmung's main container sends the restart signal) |
| Manual log inspection | Build + swap logs in the response |

The manual pattern remains technically possible — but no platform
documentation should recommend it for routine iteration. The apply
endpoint is the supported developer dev loop.

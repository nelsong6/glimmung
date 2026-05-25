# Go runtime contracts

This document is the active contract for the Go-first Glimmung runtime. The
older migration pilot is complete: production images start `cmd/glimmung-go`,
and the retired Python app/test tree has been removed.

The detailed cleanup inventory lives in
[`docs/go-runtime-cleanup-inventory.md`](go-runtime-cleanup-inventory.md).

## Current service shape

- Runtime entrypoint: `cmd/glimmung-go`.
- HTTP surface: `internal/server`.
- Persistence boundary: `internal/store/cosmos`.
- Auth boundary: `internal/auth` for Entra JWKS auth and Kubernetes
  service-account TokenReview auth.
- GitHub App client: `internal/github` for token minting, upstream workflow
  fetch, and workflow-run cancellation.
- Domain helpers: `internal/domain/*` for budget, decision, paths, phase refs,
  and public IDs.

## Documentation authority

- `README.md` is the operator/developer overview for the active Go service.
- `.github/agent/prompt.md` is the default in-repo agent contract and must keep
  the app validation gate on Go plus the Vite dashboard.
- `docs/workflow-shape.md` owns the workflow model and native job conventions.
- `docs/test-slot-lifecycle.md` owns the native test-slot terms, lifecycle
  states, and warm-versus-hot resource boundary.
- `docs/features/README.md` owns the review-facing contract index for
  substantial feature work.
- `docs/go-runtime-cleanup-inventory.md` records the final cleanup notes for
  the Python retirement.
- `CLAUDE.md` owns architecture direction for human and agent contributors.

## API authority

- Go route registration is canonical. `internal/server/route_inventory_test.go`
  verifies the active route list from `internal/server/server.go`.
- Do not add, remove, or rename MCP-used routes without an explicit rollout
  issue in `docs/mcp-surface-rollout.md`.
- Keep callback-token routes stable. Native runners and lease clients call
  those endpoints directly.
- Keep `/healthz`, `/v1/config`, `/v1/auth/me`, `/v1/state`, and `/v1/events`
  stable for operations, dashboard bootstrap, and automation clients.
- Retired route families must stay unregistered. `route_inventory_test.go`
  rejects storage-ID, GitHub Issue-coordinate, Report alias, PR-coordinate
  Touchpoint, and retired native callback/proxy routes.
- Canonical graph routes are Go-owned: `/v1/issues/by-number/{project}/{issue_number}/graph`
  and `/v1/graph`.

## Data contract

- The active Cosmos containers are `projects`, `workflows`, `leases`, `runs`,
  `run_events`, `issues`, `locks`, `reports`, `playbooks`, and `signals`.
- Workflow phases must use `k8s_job`. Blank workflow phase `kind` values
  normalize to `k8s_job`; any other executor kind is rejected before it can
  become the project runtime contract.
- Lease acquisition is native-only. Lease requests must identify native
  Kubernetes capacity and cannot fall back to registered host allocation.

## Hot-swap rules

- Keep a single writer service active. The Go service is the only app process
  supported against the production Cosmos database.
- Keep the service port and in-cluster DNS expectations stable for dashboard,
  MCP, and runner clients.
- Serve frontend assets through Go when `GLIMMUNG_STATIC_DIR` points at a built
  frontend directory.
- Do not require local Docker as the agent validation gate. Use repo checks
  locally and PR CI for image packaging.

## Go dev loop

Run the Go service locally with:

```sh
PORT=8001 \
TANK_OPERATOR_BASE_URL=https://tank.romaine.life \
GLIMMUNG_STATIC_DIR=frontend/dist \
go run ./cmd/glimmung-go
```

The backend delegates session validation to auth.romaine.life on every
admin request (cookie-forward to `get-session`, cached briefly), so no
per-deploy OAuth configuration is needed.

Static frontend assets and SPA fallback are served when `GLIMMUNG_STATIC_DIR`
points at a built frontend directory.

Run the default backend gate with:

```sh
go test ./...
go vet ./...
```

Run frontend checks from `frontend/` when dashboard code changes:

```sh
npm run test:run
npm run build
```

Pull-request app CI runs the Go gate and frontend gate. It does not install
root Python dependencies or run a Python app test suite. Pushes to `main` also
run a Go-native live Cosmos smoke for the lock lifecycle.

The repository root has no Python package metadata. Repo-local agent workflow
operations live in the Go CLI under `cmd/glimmung-agent`. The old one-shot
Python migration scripts under `scripts/` have been retired because they
imported the retired app package or encoded pre-Go workflow shapes.

## Cleanup contract

The Python app tree is gone. New app/runtime behavior belongs in Go under
`cmd/`, `internal/server`, `internal/store`, or `internal/domain`. Any future
Python must be explicitly scoped as separate non-app tooling and must not
become part of the production image, deploy path, or default CI authority.

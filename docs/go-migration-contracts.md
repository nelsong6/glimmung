# Go runtime contracts

This document is the active contract for the Go-first Glimmung runtime. The
older migration pilot is complete: production images start `cmd/glimmung-go`,
and cleanup work now removes or isolates the legacy Python app.

The detailed cleanup inventory lives in
[`docs/go-runtime-cleanup-inventory.md`](go-runtime-cleanup-inventory.md).

## Current service shape

- Runtime entrypoint: `cmd/glimmung-go`.
- HTTP surface: `internal/server`.
- Persistence boundary: `internal/store/cosmos`.
- Auth boundary: `internal/auth` for Entra JWKS auth and Kubernetes
  service-account TokenReview auth.
- GitHub App client: `internal/github` for token minting, upstream workflow
  fetch, workflow dispatch, and workflow-run cancellation.
- Domain helpers: `internal/domain/*` for budget, decision, paths, phase refs,
  and public IDs.
- Legacy Python app: `src/glimmung` is cleanup/reference material only until
  remaining route and tooling decisions are resolved.

## API authority

- Go route registration is canonical. `internal/server/route_inventory_test.go`
  verifies the active route list from `internal/server/server.go`.
- Do not add, remove, or rename MCP-used routes without an explicit
  compatibility window in `docs/mcp-surface-rollout.md`.
- Keep callback-token routes stable. Native runners and lease clients call
  those endpoints directly.
- Keep `/healthz`, `/v1/config`, `/v1/auth/me`, `/v1/state`, and `/v1/events`
  stable for operations, dashboard bootstrap, and automation clients.
- Storage-ID routes that remain registered as `410 Gone` are intentional
  tombstones, not unfinished handlers.
- Graph routes currently return `410 Gone`; porting or retiring them is still
  an explicit product decision.

## Data compatibility

- Preserve JSON field names and enum values for documents already stored in
  Cosmos until a migration window exists.
- Preserve document shapes for `projects`, `workflows`, `hosts`, `leases`,
  `runs`, `run_events`, `issues`, `locks`, `reports`, `playbooks`, and
  `signals`.
- Keep `gha_dispatch` readable as a workflow phase kind. It is legacy support,
  not the default for new native web work.
- Empty workflow phase `kind` still normalizes to `gha_dispatch` for backward
  compatibility.
- Native `k8s_job` workflows are the default direction for new web-native work.

## Hot-swap rules

- Keep a single writer service active. Do not run the legacy Python process
  against the same Cosmos database alongside the Go service.
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
ENTRA_CLIENT_ID=local-client \
TANK_OPERATOR_BASE_URL=https://tank.romaine.life \
GLIMMUNG_STATIC_DIR=frontend/dist \
go run ./cmd/glimmung-go
```

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
root Python dependencies or run the legacy FastAPI test suite. Pushes to `main`
also run a Go-native live Cosmos smoke for the lock lifecycle.

## Cleanup gates

The Python app tree can be deleted only after:

- Route gaps in `docs/go-runtime-cleanup-inventory.md` are ported, tombstoned,
  or formally retired.
- Active behavior is covered by Go tests or language-neutral checks.
- Remaining root Python packaging is no longer needed for manual legacy tests
  or migration tooling.
- Root Python packaging is removed or moved under the specific non-app tooling
  that still needs it.
- Docs and agent prompts no longer point new contributors at FastAPI,
  `python -m glimmung`, `uv run pytest`, or Python app modules.
- PR CI has verified the production image through
  `.github/workflows/docker-build-check.yml`.

# Go migration contracts

This note defines the compatibility baseline for the Glimmung backend and
control-plane as production traffic moves from Python to Go. Migration PRs
should preserve the runtime surface and prove that with focused tests before
removing legacy Python-only surfaces.

## Current service shape

- Runtime entrypoint: `cmd/glimmung-go` runs the Go HTTP service used by the
  production Docker image.
- Legacy entrypoint: `src/glimmung/__main__.py` remains only for cleanup and
  parity work while Python-only tests and scripts are retired.
- Persistence boundary: `internal/store/cosmos` owns Cosmos containers and
  document access helpers.
- Auth boundary: `internal/auth` handles Entra JWKS auth and Kubernetes
  service-account TokenReview auth.
- Dispatch and native-runner surfaces live under `internal/server` and related
  Go store/domain packages.

## Non-negotiable API contracts

- Preserve route method, path, and registration order for the public API. Some
  routes overlap, so order is part of compatibility. See
  `tests/test_api_contract_inventory.py`.
- Preserve JSON field names and enum values emitted by Pydantic models until
  clients have been migrated intentionally.
- Preserve callback-token routes and semantics. Native runners and lease clients
  call token-scoped endpoints directly.
- Preserve auth behavior for browser users, MCP tools, and Kubernetes service
  accounts. A Go service must keep the Entra and TokenReview behavior equivalent
  before it serves traffic.
- Preserve `/healthz`, `/v1/config`, `/v1/state`, and `/v1/events`. These are
  operational and UI/MCP integration points, not internal implementation
  details.
- Do not rename MCP-used routes without a compatibility window documented in
  `docs/mcp-surface-rollout.md`.

## Module boundaries for Go

Start with modules that are deterministic and have low runtime coupling:

- Domain helpers: public ID generation, path normalization, budget accounting,
  phase input reference parsing, and decision summarization.
- Contract models: Go structs that mirror selected Pydantic JSON payloads and
  golden tests against the Python outputs.
- Read-only API handlers: project/run/issue/touchpoint list and detail views
  after their query shapes have coverage.
- Callback and native-runner handlers: migrate only after token auth, event
  persistence, and runner lifecycle tests are in place.
- Writers and loops: leave registration, dispatch, signals, webhook handling,
  leases, and native job creation in Python until the Go service is proven on
  read-only and callback-compatible surfaces.

## Hot-swap and dev-loop rules

- Keep a single writer service active. The production image now starts the Go
  service; do not also run the legacy Python process against the same Cosmos
  database.
- Keep the service port and in-cluster DNS expectations stable for UI and MCP
  clients. MCP currently targets the Glimmung service URL and reads a projected
  service-account token per request.
- Keep frontend assets and API routing separable. The Go backend should be able
  to serve the same static assets, but initial pilots should not require moving
  the frontend build pipeline.
- Make local development use the same Go entrypoint as production:
  `go run ./cmd/glimmung-go`.

## Go Dev Loop

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

## Remaining Cleanup

The pure-domain and server pilots now cover path normalization, public ID
generation, budget parsing, phase input references, decision routing, abort
explanations, health/config endpoints, static frontend serving, and the current
HTTP API surface. Remaining migration work should retire legacy Python tests,
scripts, and docs once their Go replacements are the sole source of truth.

# Go migration contracts

This note defines the compatibility baseline for moving the Glimmung backend and
control-plane from Python to Go. The first migration PRs should preserve the
runtime surface exactly and prove that with focused tests before replacing any
large backend path.

## Current service shape

- HTTP entrypoint: `src/glimmung/app.py` creates the FastAPI app and owns the
  `/v1/*` API surface.
- Runtime entrypoint: `src/glimmung/__main__.py` runs uvicorn on the configured
  host and port.
- Shadow Go entrypoint: `cmd/glimmung-go` runs a local/dev HTTP server for
  migrated surfaces. It is not wired into Docker, Helm, or production traffic.
- Persistence boundary: `src/glimmung/db.py` owns Cosmos containers and document
  access helpers.
- Auth boundary: `src/glimmung/auth.py` handles Entra JWKS auth and Kubernetes
  service-account TokenReview auth.
- Dispatch boundary: `src/glimmung/dispatch.py` provides the shared
  `dispatch_run` path used by API, playbooks, signals, and portfolio workflows.
- Native runner boundary: `src/glimmung/native_k8s.py` creates and tracks native
  Kubernetes runner jobs.

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

- Keep the Python service as the only writer while Go is introduced.
- Run Go initially as a shadow or sidecar service with no background loops
  enabled. Do not run duplicate lease, signal, dispatch, or native-runner loops.
- Keep the service port and in-cluster DNS expectations stable for UI and MCP
  clients. MCP currently targets the Glimmung service URL and reads a projected
  service-account token per request.
- Keep frontend assets and API routing separable. The Go backend should be able
  to serve the same static assets, but initial pilots should not require moving
  the frontend build pipeline.
- Make local development support both processes: Python remains the default
  `python -m glimmung` path, while Go pilots expose explicit commands and tests.

## Shadow Go dev loop

The Go server can be run locally without replacing the Python service:

```sh
PORT=8001 \
ENTRA_CLIENT_ID=local-client \
TANK_OPERATOR_BASE_URL=https://tank.romaine.life \
GLIMMUNG_STATIC_DIR=frontend/dist \
go run ./cmd/glimmung-go
```

The shadow server currently owns only:

- `GET /healthz`
- `GET /v1/config`
- Static frontend assets and SPA fallback when `GLIMMUNG_STATIC_DIR` points at
  a built frontend directory.

Do not route MCP, native-runner callbacks, lease lifecycle, dispatch, webhooks,
or write endpoints to the Go server until the matching auth, storage, and
runtime parity tests exist.

## Recommended first slice

The first PR should be a contract baseline only:

- Document these boundaries.
- Add an API route inventory test that freezes method, path, route name, and
  order.
- Run the existing pure-domain tests so later Go ports have a known Python
  reference.

The initial pure-domain and shadow-server pilots now cover path normalization,
public ID generation, budget parsing, phase input references, decision routing,
abort explanations, health/config endpoints, and static frontend serving. The
next migration slices should move toward read-only API handlers behind explicit
interfaces, starting with JSON model parity and fake-store tests before any
Cosmos-backed production traffic is served by Go.

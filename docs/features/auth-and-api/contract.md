# Auth And API Surface Contract

This contract applies to human auth, service-account auth, public bootstrap
routes, admin HTTP routes, GitHub webhooks, and MCP-facing API shape.

## Product Model

Glimmung is a control plane. Auth answers who may mutate it, and the API shape
is the contract that browsers, MCP servers, native runners, and in-cluster
callers rely on. A route that accepts stale identity, advertises a stale schema,
or silently drifts from its MCP wrapper can make the system appear controllable
when it is not.

## Sources Of Truth

- auth.romaine.life owns browser session identity and role.
- Kubernetes TokenReview owns in-cluster service-account identity.
- `internal/server/server.go` owns registered HTTP routes.
- `internal/server/route_inventory_test.go` owns the explicit route inventory.
- `docs/mcp-surface-rollout.md` owns rollout sequencing for MCP-used route
  changes.
- `nelsong6/mcp-glimmung` owns the session-facing MCP tool schema for HTTP
  actions exposed to agents.
- GitHub webhook delivery is an input only; canonical run-state transitions
  flow through Glimmung issues, runs, callbacks, signals, and reports.

## Migration Rules

- Do not add local browser JWTs, local JWKS handling, or app-specific auth
  secrets when auth.romaine.life delegation is the browser path.
- Do not add unauthenticated admin fallbacks or hidden operator bypass routes.
- Do not add, remove, or rename MCP-used HTTP routes without updating
  `mcp-glimmung` in the same rollout or following `docs/mcp-surface-rollout.md`.
- Do not preserve retired route aliases for unknown callers.
- Do not treat GitHub webhook side effects as canonical PR review decisions;
  PR decisions enter through the signals contract unless a future contract
  explicitly changes that.

## Live Behavior

- Browser admin requests delegate cookie validation to auth.romaine.life and
  gate on accepted roles before mutation.
- In-cluster callers present projected service-account tokens, pass TokenReview,
  and match `K8S_SA_ALLOWLIST` before mutation.
- Public bootstrap routes stay intentionally public: `/healthz`, `/v1/config`,
  and routes explicitly documented as public.
- Route registration, route inventory tests, frontend affordances, and MCP tool
  signatures move together when an operator-facing API changes.
- HTTP request schema changes are reflected in MCP docstrings and payload tests
  before running sessions can advertise stale tools.

## Failure And Recovery

- Auth delegation failure returns explicit unauthorized or forbidden results,
  not partial admin UI state or accepted writes.
- TokenReview failure is distinguishable from allowlist rejection.
- Stale MCP clients should fail clearly at the API boundary; supported rollout
  paths should keep old tools from being advertised after the server contract
  changes.
- Webhook signature failures must acknowledge no mutation and leave enough log
  detail to identify the event kind and route without leaking secrets.

## Observability

- Logs for rejected auth should include route, auth path, and rejection class
  without logging token material.
- Route inventory failures should identify added, removed, or renamed routes.
- MCP rollout PRs should include evidence from `mcp-glimmung` tests or an
  explicit rollout issue.
- GitHub webhook failures should be diagnosable by delivery id, event kind, and
  signature/parse/processing class.

## Acceptance Checks

- API changes update route inventory tests.
- Operator-facing route changes update the dashboard or explain why the route
  is system-only.
- MCP-facing route changes update `mcp-glimmung` tool signatures, docstrings,
  and payload tests in the same rollout or name the staged rollout issue.
- Auth changes include tests or runtime evidence for the human path and the
  service-account path when both can reach the route.
- Retired routes are removed from registration, tests, docs, and MCP wrappers.

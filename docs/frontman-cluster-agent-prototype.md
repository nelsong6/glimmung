# Frontman Cluster Agent Prototype

This is the first Glimmung-owned prototype contract for a Frontman-style
review/editing flow with cluster-native agent tools.

Upstream verification: checked
[`frontman-ai/frontman`](https://github.com/frontman-ai/frontman) `main` at
`a154ad8ef819ae4de68ff983d48c2dc4a461a71e` on 2026-05-06. At that revision:

- the README describes Next.js, Astro, and Vite integrations
- the framework integration exposes browser and dev-server context as MCP tools
- Frontman runs in development mode and is stripped from production builds
- `apps/frontman_server` is an AGPL-3.0 Phoenix server
- `FrontmanServer.Tools.MCP` parses external MCP tool definitions
- `FrontmanServer.Tools` separates backend tools from MCP-routed tools

## Prototype Choice

Use a hybrid path first:

```text
Frontman-style overlay and selected-element context
  -> Glimmung-owned agent backend
  -> existing in-cluster MCP servers
  -> authenticated branch/PR writer
```

Do not expose a public write surface from the browser. The browser may submit
selected DOM/component context and chat messages. Repository writes happen only
through a scoped backend path that mints or receives an explicit GitHub token.

This keeps the useful Frontman shape while preserving Glimmung's current
security model: agents run in the cluster, tool access is service-account
scoped, and code changes land through branches and PRs.

## Components

### Frontend Overlay

The target app loads a Frontman-like overlay in a validation environment. The
overlay must capture:

- selected DOM node identity and accessible text
- component or source hints when the integration can provide them
- computed styles and layout bounds
- screenshot or viewport reference
- the current route and validation URL

The overlay should work against a validation host, not production. It can use
Frontman client pieces if licensing and packaging are acceptable, or a thin
Glimmung-native overlay if that is faster to secure.

### Agent Backend

The Glimmung-owned backend receives overlay context and starts or resumes an
agent session in Kubernetes. The backend owns:

- user authentication and authorization
- session/run identity
- MCP server allowlist
- selected DOM/context handoff
- token minting for branch/PR operations
- audit trail back to Issue, Run, and Touchpoint

For v1, the backend should expose only one operation: start a review/edit
session for a validation URL plus selected element context.

### MCP Tool Bridge

The session must be able to call at least one in-cluster MCP server from the
same conversation, preferably GitHub first because the prototype must create or
update a PR.

Minimum tool set:

- GitHub: create/update branch and PR, read diff, comment
- Glimmung: attach session/run/touchpoint metadata
- Kubernetes or ArgoCD: read validation environment status

Browser-aware tools and cluster tools should stay distinguishable in the tool
metadata. A selected DOM node is not the same trust domain as a Kubernetes or
GitHub action.

### Branch And PR Writer

The agent may create or update a feature branch and PR. It must not push to
`main`.

Enforce this in the backend, not just in the prompt:

- token scope excludes direct `main` writes where possible
- backend rejects writes whose target ref is `main`
- PR creation requires an explicit base branch
- every write records Issue/Run/Touchpoint metadata

## Argo Boundary

The prototype deployment must be GitOps-managed.

Required deployment properties:

- ArgoCD Application owns the server/overlay test deployment
- namespace and service account are explicit
- ingress/HTTPRoute host is stable and non-production
- secrets come from ExternalSecret or an equivalent managed path
- no direct long-lived `kubectl apply` state is required to keep the prototype
  running

The existing `frontman-selfhost` deployment in `infra-bootstrap` can be the
test bed, but Glimmung should treat the agent bridge contract as its own
interface. If we replace Frontman server internals later, the browser and
Glimmung backend contract should remain stable.

## Security Gates

Before a prototype is considered real:

1. A Glimmung frontend or validation app loads the overlay in a test
   environment.
2. The side panel can send selected DOM/component context to the backend.
3. The same conversation can call an in-cluster MCP tool.
4. The agent can create or update a branch and PR.
5. Direct `main` writes are blocked by backend policy.
6. The write path is authenticated and scoped.
7. The deployment is managed by ArgoCD.

## Recommended First Slice

Start with Glimmung itself:

1. Add the overlay only to a validation/live-design host.
2. On selection, send route, selector-ish context, bounds, text, and screenshot
   reference to a Glimmung backend endpoint.
3. Start a cluster session with GitHub and Glimmung MCP tools only.
4. Let the agent make a trivial frontend copy/style change on a feature branch.
5. Open a PR and attach the validation URL, selected-element context, and
   screenshot evidence to the Touchpoint.

This proves the core loop without coupling Glimmung to Frontman's full server
internals too early.

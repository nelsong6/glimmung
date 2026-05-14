# MCP Surface Rollout

Glimmung depends on MCP tools owned by sibling services such as
`mcp-github`, `mcp-k8s`, `mcp-glimmung`, and `azure-personal`. Session pods
cache MCP tool metadata when they start, so server renames, removals, and
permission changes need an explicit rollout path. Otherwise an already-running
session can still advertise a removed namespace and fail only when the caller
uses it.

Use this sequence for MCP server rename/removal work.

## Request Schema Drift

MCP tool signatures are part of Glimmung's operator-facing API surface. When a
Glimmung HTTP endpoint changes its accepted request fields, update the matching
MCP wrapper at the same time:

- remove obsolete parameters from the tool signature
- update the tool docstring so sessions see the current contract
- update payload tests so the tool cannot keep forwarding removed fields
- call out any necessary fresh-session requirement in the PR

Do not rely on the backend rejecting stale fields as the only protection. A
stale MCP schema still misleads agents before the request reaches Glimmung.

## Rename Or Removal

When a server is renamed, remove the old name in the same change that adds the
new one. Do not keep an alias for already-running sessions. Stale sessions are
unsupported and should be restarted.

Recommended sequence:

1. Add the new server name and remove the old name.
2. Deploy the MCP server and session config together.
3. Start a fresh session and verify only the expected names appear healthy.
4. Record the fresh-session requirement in the PR.

If the upstream service is being removed with no replacement, do not leave a
dead namespace in discovery. Remove the session config entry in the same
change that removes the deployment.

## Stale Session Signal

A session has stale MCP config when tool discovery shows a server namespace
that no longer exists, or when the namespace exists but every call fails before
reaching the intended backend.

Operators should treat these as stale-config indicators:

- tool discovery lists the old namespace after the rename has deployed
- calls fail with connection, DNS, or upstream-not-found errors for a removed
  server
- a fresh session has different MCP server names than an older session

The user-facing response should say that the session was started before the
MCP surface change and needs a refresh or restart. Avoid debugging the removed
backend from inside the stale session unless a fresh session reproduces the
failure.

## Health Contract

Removed or renamed MCP surfaces must not look healthy.

Each MCP server entry should satisfy one of these states:

- `active`: listed in discovery and routes calls to a live backend
- `removed`: absent from discovery for new sessions
- `stale`: visible only in old sessions, with failures that clearly indicate
  stale session config instead of a healthy-but-broken tool

For future session config changes, include a metadata version or config
revision in the session bootstrap output when that plumbing exists. Until then,
compare a fresh session's discovered server names against the stale session's
server names.

## Refresh And Restart

When a stale session is suspected:

1. Start a new session and inspect MCP discovery there.
2. If the new session is correct, restart or replace the stale session.
3. If the new session is also wrong, roll back the MCP config change or finish
   removing the old surface before continuing the migration.
4. Record the old name, new name, deployment revision, and observed stale
   behavior in the PR or issue so the next migration has an audit trail.

The desired operator outcome is simple: new sessions get the new config; old
sessions fail with an obvious stale-session explanation.

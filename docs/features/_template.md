# Feature Name Contract

This contract translates the repo-wide policy docs into feature-specific rules
for this feature.

If the feature area contains named user-facing behavior that is still evolving,
track it in a sibling `capabilities.md` file. The contract owns permanent
invariants; the capability ledger owns named behavior, status, intent, and
evidence history.

## Product Model

Describe what the feature is for, what user trust depends on, and which product
or operational needs matter most for this feature.

## Sources Of Truth

Name the durable tables, event types, external systems, Kubernetes resources,
or GitHub objects that own visible state. Also name any live transports that
are delivery mechanisms only.

## Migration Rules

Name old paths, compatibility layers, browser-local state, fallback reads, and
obsolete tests that must not survive a migration.

## Live Behavior

Define what must happen without refresh, reconnect, manual retry, or operator
guesswork during normal operation.

## Failure And Recovery

Define expected behavior across reloads, reconnects, stale credentials, service
rollouts, runner restarts, pod termination, callback loss, and external service
failures. Be explicit about the boundary where durability stops.

## Observability

Name the metrics, logs, durable events, dashboard state, alerts, or diagnostic
queries that must exist when this feature misbehaves.

## Acceptance Checks

List the concrete checks required before a PR touching this feature can be
called complete. Prefer checks that prove the product invariant instead of the
implementation detail.

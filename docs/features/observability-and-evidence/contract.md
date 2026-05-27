# Observability And Evidence Contract

This contract applies to native event logs, structured server logs, route and
workflow diagnostics, run reports, screenshot/evidence capture, live smoke
checks, metrics, and operator-facing debug paths.

## Product Model

Evidence is how humans trust agent work. Observability is how operators trust
Glimmung itself. A feature is incomplete if its bad states can only be
understood by rerunning work, reading browser devtools, or guessing from a
half-updated dashboard.

## Sources Of Truth

- RunReports own factual per-run evidence surfaced to reviewers.
- Native run events own hot phase/job/step telemetry and log references.
- Structured server logs own per-event diagnostic detail.
- Route inventory tests, workflow validation tests, and live smoke tests own
  executable contract checks.
- `docs/observability.md` owns observability design notes.
- Evidence files produced by agent runs live outside the repo and are attached
  to PR/review surfaces by the runner.

## Migration Rules

- Do not remove a diagnostic field, event, metric, or log path without an
  equivalent way to diagnose the same failure mode.
- Do not accept "look in browser devtools" as the default diagnostic workflow
  for a contracted feature.
- Do not let screenshots substitute for backend evidence when the changed
  behavior is not visible.
- Do not add high-cardinality metric labels for project-specific or run-specific
  detail that belongs in logs, events, reports, or debug endpoints.
- Do not keep stale evidence capture routes after screenshot page ownership
  moves.

## Live Behavior

- Every contracted feature has evidence that maps to the invariant it could
  break.
- Native jobs emit enough ordered events to reconstruct what ran and why it
  passed, failed, waited, or timed out.
- RunReports surface validation URL, screenshots, attempt summaries, cost, and
  terminal state when those facts exist.
- Frontend-visible changes have browser or screenshot evidence; invisible
  backend changes have tests, logs, API output, or notes that prove the data
  path.
- CI and live-smoke checks fail loudly on contract violations rather than
  producing ambiguous green builds.

## Failure And Recovery

- Missing evidence should be visible as missing evidence, not silently treated
  as success.
- Event pruning or archiving should leave an archive reference or explicit
  unavailable state.
- Screenshot failure should not hide backend test results, and backend test
  failure should not be masked by screenshots.
- Live-smoke failures should name the route, object, or external dependency
  that failed.

## Observability

- Logs and events should include stable project, issue, run, cycle, phase, job,
  step, lease, slot, or workflow identifiers where relevant.
- Failure classes should distinguish auth, validation, no capacity, callback,
  Kubernetes, Postgres, GitHub, and renderer failures when the feature crosses
  those boundaries.
- Operator dashboards or API responses should expose stale lock, stale run,
  and missing evidence conditions when those states affect user trust.

## Acceptance Checks

- PR evidence names the exact test, route, log, metric, screenshot, run report,
  or runtime observation that proves each affected contract.
- New native event shapes include tests or fixture evidence for projection.
- New metrics or debug paths avoid unbounded labels and document their failure
  mode.
- Screenshot-page changes update `frontend/screenshot-pages.json`.
- Evidence capture changes are exercised against a validation URL or by focused
  unit/integration tests when a live environment is not appropriate.

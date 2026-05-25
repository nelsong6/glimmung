# Dashboard And Styleguide Contract

This contract applies to the Vite + React dashboard, SSE state following,
admin controls, route navigation, and the platform `/_styleguide` review
surface.

## Product Model

The dashboard is the operator's live view of Glimmung state. It should make
leases, issues, workflows, runs, touchpoints, playbooks, and locks legible
without inventing browser-only truth. The styleguide is the visual review
surface for dashboard components and for projects running through Glimmung's
agent pipeline.

## Sources Of Truth

- `/v1/state` owns the dashboard snapshot for projects, workflows, leases,
  test environments, and lock state.
- `/v1/events` delivers state snapshots over SSE; it is a delivery mechanism,
  not a separate source of truth.
- Feature-specific detail routes own their own detail state, such as issue
  graphs, run reports, playbooks, and touchpoints.
- `docs/styleguide-contract.md` owns the platform `/_styleguide` requirement.
- `frontend/src/StyleguideView.tsx` owns Glimmung's dashboard catalog.
- `frontend/screenshot-pages.json` owns screenshot coverage for review routes.

## Migration Rules

- Do not use browser-local state as the durable source for leases, runs,
  workflow definitions, locks, or admin mutations.
- Do not keep old UI controls, labels, or routes after the backing route or
  concept has migrated.
- Do not add a new reusable component without updating `/_styleguide` in the
  same change.
- Do not add top-level review routes without deciding whether they belong in
  `frontend/screenshot-pages.json`.
- Do not keep an unauthenticated admin affordance that can appear to work.

## Live Behavior

- SSE updates converge visible snapshot state without requiring page refresh.
- A reconnect or reload rehydrates from durable API state and does not revive
  stale browser-only state.
- Admin controls move through requested, confirmed, and failed states that
  match API outcomes.
- `/_styleguide` renders inside the same app bundle and route table as the live
  dashboard.
- Dashboard copy and component shape follow `design-system/SKILL.md` and token
  usage from `design-system/colors_and_type.css`.

## Failure And Recovery

- SSE disconnect should be visible as connection state and should recover by
  reconnecting or by the next snapshot read.
- Failed mutations must leave clear failure state and must not optimistically
  rewrite durable-looking UI state.
- Auth expiration should route to sign-in or forbidden state rather than
  leaving a stale admin panel.
- A route refresh on a detail page should land back on the same durable entity
  or show an explicit not-found/error state.

## Observability

- Browser-visible failures should be paired with server logs or route-level
  test evidence so operators can distinguish auth, API, stream, and render
  failures.
- Styleguide breakage should fail the validation gate with
  `frontend_contract_violation`.
- Screenshot coverage should include `/_styleguide` and any new top-level
  review page reviewers need to inspect.

## Acceptance Checks

- Frontend component changes update `frontend/src/StyleguideView.tsx`.
- Dashboard changes run `npm run test:run` and `npm run build` from
  `frontend/`, unless the PR explains why they are irrelevant.
- New or changed admin controls are backed by durable API outcomes, not
  browser-only success state.
- SSE or snapshot changes prove reconnect/reload convergence.
- Route additions decide whether to update `frontend/screenshot-pages.json`.

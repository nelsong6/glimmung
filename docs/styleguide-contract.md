# Styleguide contract

> Every frontend project that runs through glimmung's agent pipeline
> MUST expose `/_styleguide` on its live env. The validation step in
> `agent-run.yml` enforces this with a `curl -sf` check; a 404 fails the
> run with the typed abort reason `frontend_contract_violation` and
> links back to this document.

## Why

Glimmung exists to make agent-driven SDLC reviewable. The PR diff is
not the review surface — the diff says *what changed*, not *what it
looks like*. A live env per run gives reviewers the running app; a
styleguide route gives them a single page that catalogs every component
in its variants. Together they replace "I can't interact with your UI
changes" with one URL the reviewer can scan in 30 seconds.

This is a platform-level contract, not a project preference. The
validation step is hard-fail by design: a project without `/_styleguide`
is a project where review is harder, and the failure mode should be
loud at run time, not silent at review time.

## What you must expose

A route at `/_styleguide` on the live env. The page renders every
component the project ships, in every variant.

- Same React app, same router, same deploy. **No separate process.**
- Hand-rolled HTML/JSX continuing the project's existing component
  vocabulary. The reference style is `design-system/ui_kits/dashboard/`
  in this repo. **No Storybook.**
- v1 is a visual catalog only — no knobs, no toggles, no search. Render
  each component in each state and label it.
- Pulls from `design-system/colors_and_type.css` for tokens. Don't
  hand-pick hex; `scripts/check-design-tokens.sh` will fail you if you
  do.

See `design-system/SKILL.md` for the canonical component list and
design system rules (pills, console-plate buttons, KPI strips, the
two-step inline confirm pattern, etc.).

## Minimal starter

For a project that has no styleguide yet, this is the smallest thing
that passes the contract — drop it in and grow it as you add
components:

```tsx
// frontend/src/StyleguideView.tsx
export function StyleguideView() {
  return (
    <>
      <h2>styleguide</h2>
      <p className="dim">
        Visual catalog of components shipped by this project. See
        <code> design-system/SKILL.md </code> for the rules these follow.
      </p>

      <h2>pills</h2>
      <div style={{ display: "flex", gap: "0.5rem" }}>
        <span className="pill free">free</span>
        <span className="pill busy">busy</span>
        <span className="pill drain">drain</span>
        <span className="pill info">info</span>
      </div>

      <h2>buttons</h2>
      <div style={{ display: "flex", gap: "0.5rem" }}>
        <button type="button" className="gb primary">
          <span className="label">dispatch</span>
        </button>
        <button type="button" className="gb">
          <span className="label">cancel</span>
        </button>
      </div>

      <h2>empty</h2>
      <div className="empty">No items.</div>
    </>
  );
}
```

Wire it into the router at `/_styleguide`. The leading underscore
marks it as a platform route — keeps it out of the way of
product routes and signals to humans that it's not a feature.

## Maintenance contract

**When you change a component, you must update the styleguide entry
for it in the same change.** This is in the agent prompt for projects
that run through glimmung's pipeline; it's also the rule for humans.

A stale styleguide silently degrades the review surface. There's no
automated drift check yet (out of scope for v1) — the contract is
social, with the validation step as the floor.

When you add a new component:

1. Build the component.
2. Add a section to `/_styleguide` rendering it in every state it
   supports.
3. Confirm `/_styleguide` still renders without errors locally.
4. Open the PR. The screenshot pass will capture the new variant
   automatically (it's in the page list — see
   `frontend/screenshot-pages.json`).

## Where the route lives

A static route inside the existing frontend app. For glimmung, that's
`frontend/src/StyleguideView.tsx` wired into `App.tsx`'s router. No
separate deploy, no separate Dockerfile, no separate domain. The live
env URL with `/_styleguide` appended is the styleguide URL — the PR
body composer surfaces both alongside inline screenshots.

## How the contract is enforced

1. `agent-run.yml`'s validation step deploys the live env.
2. It curls `<env>/_styleguide`. 200 → continue; 404 (or any non-2xx)
   → abort the run with reason `frontend_contract_violation` and a
   message linking to this file.
3. The abort reason flows into glimmung's run record; the dashboard
   surfaces it in the run header.

This check is intentionally upstream of the screenshot pass. Without
`/_styleguide` the screenshots aren't useful and shouldn't be taken.

# Design Portfolio Bootstrap

Use this process when Glimmung needs to retrofit a design portfolio into an
existing frontend repository. The goal is a reviewable in-app route with real
components, stable fixture data, screenshot capture, and passive review state.

The reusable PlaybookCreate payload lives at
[`docs/playbooks/design-portfolio-bootstrap.json`](playbooks/design-portfolio-bootstrap.json).
Create it with `POST /v1/playbooks`, inspect it in the dashboard, then run it
when the target project/workflow is ready.

## Target Contract

A bootstrapped repo should expose one of these routes inside the real app
bundle:

- `/_design-portfolio`, preferred for grouped product/screen review
- `/_styleguide`, acceptable when the repo already uses the Glimmung
  styleguide contract

The route should render live DOM, not a screenshot gallery. Screenshots are
evidence produced from the route after it exists.

Each portfolio row or specimen should have passive review state:

- `unreviewed`
- `needs_review`
- `approved`
- `needs_work`

Changing review state does not dispatch agent work by itself. A later operator
request such as "inspect the rows marked needs_review" can use
`/v1/portfolio/elements` as the queue source.

## Standard Workflow

1. Detect the frontend shape. Identify the framework, router, build command,
   package manager, screenshot tooling, and where fixture data belongs.
2. Inventory review surfaces by user workflow, not by file tree:
   primitives, composites, full screens, loading/empty/error states, and danger
   flows.
3. Propose a grouped specimen plan in the issue or PR body before writing a
   large route. Keep groups small enough for a reviewer to scan.
4. Add or extend the portfolio route in the existing app bundle. Reuse real
   components where possible and provide stable fixture data where live data is
   unavailable.
5. Register specimens through the portfolio element API when Glimmung is the
   orchestrator. Use stable element ids such as `sidebar.nav` or
   `settings.billing.empty`.
6. Add screenshot coverage, usually by updating `frontend/screenshot-pages.json`
   with `/_design-portfolio` or `/_styleguide`.
7. Run the repo's normal build/checks and capture screenshot evidence from the
   validation environment.
8. Open a PR with the portfolio URL, screenshot evidence, and the initial list
   of specimens marked `needs_review` or `unreviewed`.

## Framework Notes

React/Vite:

- add a route in the existing router
- keep fixtures in `src/` near the portfolio view or in an existing mock-data
  module
- update `frontend/screenshot-pages.json`

Static HTML or Go-served frontend:

- add a static route served by the existing binary/server
- keep fixture JSON or embedded data deterministic
- add a screenshot page entry if the repo has the Glimmung screenshot pass

Next.js:

- add an app/pages route with static fixture data
- avoid server-only dependencies in specimens unless the validation environment
  provides them
- add screenshot coverage for the generated route

## Agent Prompt Slice

Give agents this bounded instruction when using the reusable playbook:

```text
Bootstrap a design portfolio route for this frontend repo. Detect the frontend
shape, inventory primitives/composites/screens/states by review group, then add
or extend /_design-portfolio or /_styleguide inside the existing app bundle.
Render real components with stable fixture data. Add screenshot capture config
for the route. Review state must be passive and must not auto-dispatch work.
Run the repo's relevant build/checks and open a PR with the portfolio URL,
screenshot evidence, and any rows that need review.
```

## Dogfood Target

For Glimmung itself, the current route is
[`frontend/src/StyleguideView.tsx`](../frontend/src/StyleguideView.tsx), wired
to both `/_styleguide` and `/_design-portfolio`. The screenshot pass already
captures `/_styleguide`; add `/_design-portfolio` to
[`frontend/screenshot-pages.json`](../frontend/screenshot-pages.json) when the
review route diverges from the styleguide route.

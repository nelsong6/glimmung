# Repo UI Packages And Design Portfolios

Glimmung should not depend on a shared visual-editing host for frontend review.
Each repo owns its UI package and exposes reviewable specimens through its app
bundle, usually at `/_design-portfolio` or the existing `/_styleguide` route.

## Contract

The repo UI package is the source of truth for reusable components, fixtures,
tokens, and screen states. The design portfolio route is the operator surface:

- render real package components where possible
- group rows by workflow or review area, not by file tree
- keep row state passive: `unreviewed`, `needs_review`, `approved`,
  `needs_work`
- register rows through `POST /v1/portfolio/elements` when Glimmung is
  orchestrating the work
- dispatch follow-up only from `/portfolio` or
  `POST /v1/portfolio/elements/dispatch`

This keeps repo-local UI ownership intact while still giving Glimmung a common
review queue and explicit dispatch path.

## Disposable Preview Hosts

Sign-in for both production and disposable review hosts is delegated to
auth.romaine.life. Glimmung holds no per-host Entra app registration of its
own; the auth service owns the single org-wide app reg, and slot hosts work
by passing their own URL as `callbackURL` when redirecting to
`auth.romaine.life/sign-in/microsoft`.

`/v1/config` returns `{auth_url, tank_operator_base_url}` — no host-specific
branching. Admin gating still happens at the backend, but it's now driven by
the upstream `role` claim (`admin`/`user`/`pending`) rather than a per-app
email allowlist.

## Hostnames

auth.romaine.life's `trustedOrigins` covers glimmung's slot pool via the
wildcard `https://*.glimmung.dev.romaine.life` (see nelsong6/auth#20), so new
slots don't need any Entra-side registration — just route the hostname at the
repo-specific validation environment.

## Wiring Checklist

1. Build the repo's UI package and app bundle with stable review fixtures.
2. Expose `/_design-portfolio` or `/_styleguide` in the real app bundle.
3. Use one of the Glimmung-managed `glimmung-slot-N.glimmung.dev.romaine.life`
   preview hostnames.
4. Confirm `/v1/config` on that hostname returns `auth_url` pointing at auth.romaine.life.
5. Confirm Microsoft sign-in completes via auth.romaine.life and lands back on the slot URL.
6. Add screenshot capture for the portfolio route.
7. Register rows with Glimmung and dispatch follow-up only from the operator
   portfolio surface.

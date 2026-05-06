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

Production continues to use `glimmung-oauth`, whose client ID is exposed as
`ENTRA_CLIENT_ID`.

Disposable frontend review hosts use `glimmung-oauth-test`, exposed to the pod
as `ENTRA_TEST_CLIENT_ID`. `/v1/config` returns the test client ID when the
request host is `glimmung.dev.romaine.life` or any subdomain of
`glimmung.dev.romaine.life`. For production hosts it returns the production
client ID.

The backend accepts ID tokens for either audience and still enforces the same
`ALLOWED_EMAILS` allowlist. The test app changes only the SPA redirect URI set;
it does not relax user authorization.

## Hostnames

Microsoft Entra SPA redirect URIs do not support wildcards. Use a small stable
pool of UI review hostnames and register each one on `glimmung-oauth-test`.
The stable hosts can be shared across repos as long as the route points at the
repo-specific validation environment.

Add more entries through `tofu/variables.tf` `test_redirect_uris`; do not use a
runtime-generated hostname unless it has been pre-registered in Entra.

## Wiring Checklist

1. Build the repo's UI package and app bundle with stable review fixtures.
2. Expose `/_design-portfolio` or `/_styleguide` in the real app bundle.
3. Use one of the stable `*.glimmung.dev.romaine.life` preview hostnames.
4. Confirm `/v1/config` on that hostname returns `ENTRA_TEST_CLIENT_ID`.
5. Confirm Microsoft login completes without a redirect URI error.
6. Add screenshot capture for the portfolio route.
7. Register rows with Glimmung and dispatch follow-up only from the operator
   portfolio surface.

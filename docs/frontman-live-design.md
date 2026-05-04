# Frontman live-design environments

Frontman environments should run the real Glimmung frontend against the normal
Glimmung API. They must not bypass authentication or grant service-account
access to anyone who has the URL.

## Auth model

Production continues to use `glimmung-oauth`, whose client ID is exposed as
`ENTRA_CLIENT_ID`.

Disposable live-design hosts use `glimmung-oauth-test`, exposed to the pod as
`ENTRA_TEST_CLIENT_ID`. `/v1/config` returns the test client ID when the request
host is `glimmung.dev.romaine.life` or any subdomain of
`glimmung.dev.romaine.life`. For production hosts it returns the production
client ID.

The backend accepts ID tokens for either audience and still enforces the same
`ALLOWED_EMAILS` allowlist. The test app changes only the SPA redirect URI set;
it does not relax user authorization.

## Hostnames

Microsoft Entra SPA redirect URIs do not support wildcards. Use a small stable
pool of Frontman hostnames and register each one on `glimmung-oauth-test`.
The default OpenTofu list is:

- `https://glimmung.dev.romaine.life/`
- `https://frontman.glimmung.dev.romaine.life/`
- `https://frontman-1.glimmung.dev.romaine.life/`
- `https://frontman-2.glimmung.dev.romaine.life/`
- `https://frontman-3.glimmung.dev.romaine.life/`

Add more entries through `tofu/variables.tf` `test_redirect_uris`; do not use a
runtime-generated hostname unless it has been pre-registered in Entra.

## Wiring Checklist

1. Point the Frontman environment at the built Glimmung frontend, not the stock
   Vite sample app.
2. Use one of the stable `*.glimmung.dev.romaine.life` hostnames.
3. Confirm `/v1/config` on that hostname returns `ENTRA_TEST_CLIENT_ID`.
4. Confirm Microsoft login completes without a redirect URI error.
5. Clean up the Frontman workload/route after the live-design session; leave
   the stable hostname and redirect URI in place for reuse.


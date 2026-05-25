# glimmung

Read `CLAUDE.md` for the project architecture and design-system guidance.
Read `docs/quality-timeframes.md` before planning substantial work.
Read `docs/migration-policy.md` before any migration or cleanup work.
Read `docs/features/README.md` and the relevant feature contract before
substantial work on a contracted surface.

## Migration Policy

When migrating off an old system, the old system must be deleted end to end.
Compatibility is prohibited.

A migrated path must have no live routes, no UI controls or links, no
allocator or executor branches, no fallback defaults, no old behavior tests, no
docs saying the old path is supported, and no runtime reads whose purpose is to
keep old behavior working.

Unknown callers are unsupported. Known old callers are unsupported. Old data
does not justify runtime support.

If removal exposes another dependency on the old system, delete that dependency
too. If the task cannot be completed, stop with a blocker report naming the
exact remaining old dependency. Do not add a compatibility layer. Do not add a
fallback. Do not keep a read-only runtime path.

When asked to complete a migration, search for the old system's names, routes,
types, feature flags, tests, docs, UI labels, and storage behavior. Remove every
live path. Treat `legacy`, `compatibility`, `fallback`, `temporary`, and
`exception` as deletion targets, not design options.

## Container Build Verification

Agent pods are not expected to have Docker. Do not report missing local Docker
as a blocker. Run available repo checks first, then use PR CI as the normal
container build gate: `.github/workflows/docker-build-check.yaml` performs
throwaway builds for the app image with `push: false`. If
image-packaging feedback is needed before a PR is ready, manually dispatch that
workflow with `git_ref`. Release/deploy workflows are the only path that
publishes images.

# glimmung

Read `CLAUDE.md` for the project architecture and design-system guidance.

## Container Build Verification

Agent pods are not expected to have Docker. Do not report missing local Docker
as a blocker. Run available repo checks first, then use PR CI as the normal
container build gate: `.github/workflows/docker-build-check.yml` performs
throwaway builds for the app image with `push: false`. If
image-packaging feedback is needed before a PR is ready, manually dispatch that
workflow with `git_ref`. Release/deploy workflows are the only path that
publishes images.

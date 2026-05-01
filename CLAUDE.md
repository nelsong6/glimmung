# glimmung

Agent dispatcher with verify-loop substrate. See [README.md](README.md) for what it is, the storage layout, the API surface, and the verify-loop / signal-bus / lock primitives.

This file is the operating context an AI session needs that *isn't* in the README.

## Persistence model for AI sessions

Auto-memory at `/home/node/.claude/projects/-workspace/memory/` does **not** persist across sessions in this devcontainer setup — pods are ephemeral, the home dir is wiped. **CLAUDE.md is the durable surface for repo-scoped context** (this file). Session-end handoff state goes in a GitHub issue (e.g. #45-style session-log) so the next session can pick up.

## Where this devcontainer can and can't reach

This devcontainer is a `claude-session` pod in `tank-operator-sessions` namespace.

- **No Cosmos auth from here.** `AZURE_TENANT_ID` + the federated token file are present, but `AZURE_CLIENT_ID` is unset and no UAMI accepts our SA's subject. Direct `azure-cosmos` SDK calls fail at the auth step.
- **Use the Azure MCP for live-Cosmos inspection** (`mcp__azure__cosmos` etc.). The Azure MCP server has its own auth and is the right tool for "what's actually in the `runs` container right now".
- **Don't run `pytest` expecting live data.** Tests run against `tests/cosmos_fake.py` (in-memory `ContainerProxy` + hand-rolled SQL evaluator). New query shapes may need the fake extended; #37 tracks the open question of migrating tests to live Cosmos as the verification mechanism for glimmung-on-glimmung agent runs.
- **No CI gate runs pytest** today. `agent-run.yml` doesn't either. "Tests pass locally" is the only pre-merge signal.

## Glimmung-on-glimmung dispatch wiring

When glimmung dispatches an agent against a glimmung issue (`.github/workflows/agent-run.yml`):

- **Per-issue ephemeral glimmung instance.** Helm release from `k8s/issue/` installs into the **prod `glimmung` namespace**, reusing prod's SA `infra-shared` + `glimmung-identity` UAMI. Has Cosmos auth. Reachable on `<slug>.glimmung.dev.romaine.life`. **Same Cosmos database as prod** — no test DB; isolation is by partition-key convention if at all.
- **Agent Job.** Runs in a *separate* `glimmung-issue-<N>-<run>-<sha>` namespace (NOT the glimmung ns). The federated credential on `glimmung-identity` only matches `system:serviceaccount:glimmung:infra-shared`, so **the agent pod's namespace is not wired for Cosmos auth**. If a future change needs the agent pod to talk to Cosmos directly, that's a tofu PR (federated credential or a dedicated UAMI). Until then, the agent's interactions with data go through the per-issue glimmung instance's HTTP API, not direct Cosmos.

The split + the workload-identity story is summarized in `agent-run.yml` lines 5-22; that comment is load-bearing — read it before making changes that touch namespace or SA wiring.

## Substrate-first PR decomposition

When adding a new domain primitive (issues, PRs, signals, …), the established pattern is:

1. **Substrate PR** — Cosmos container in `tofu/db.tf`, model in `models.py`, CRUD primitives in `glimmung.<domain>`, tests via cosmos_fake. No consumers wired; existing code paths untouched.
2. **Consumer PR(s)** — webhook mirror, read-path cutover, cross-primitive linkage, denormalization cleanup. Each can land independently.

Examples landed: #28 (Issue substrate → consumers in #33/#34/#38/#44), #41 (PR substrate in #46 → consumers pending).

Don't bundle substrate + consumer changes in one PR. Keeps the diff per PR small enough to review, and keeps the test-suite signal meaningful at each step.

## Writes go via the GitHub MCP, not `git push`

Read-only `git clone` / `fetch` / `pull` via the `mint_clone_token` MCP tool (returns a read-only-scoped token). All writes (branches, commits, PRs, label creation, issue updates) via the corresponding MCP tools (`commit_to_branch`, `create_pull_request`, `create_label`, `update_issue`, etc.).

The MCP write tools resolve refs and blob shas server-side at call time; a caller-cached SHA can't introduce staleness. A prior session reverted a merged PR by branching off a cached SHA — going through MCP prevents the same class of bug.

## Build / deploy chain

`main` push → `build.yaml` builds the image + pushes to `romainecr.azurecr.io/glimmung:<sha>` → automated chart bump commit `[skip ci]` updates `k8s/values.yaml` → ArgoCD auto-syncs the cluster. End-to-end is usually <5 min. Don't pause to ask "did the deploy fire" — it did; check the chart-bump commit on `main` to confirm the new image tag.

`tofu.yaml` runs on PRs that touch `tofu/` — plan output appears as a PR comment, apply runs on merge to `main`.

## Open architectural questions (don't trust without re-verifying)

- **#37** — how tests become the verification mechanism for glimmung-on-glimmung agent runs. Where pytest runs (agent pod vs ephemeral glimmung instance), how its output produces `verification.json`, and what auth wiring that implies. Active design as of 2026-05-01.
- **#41 consumer chain** — substrate landed (#46); webhook mirror, read-path cutover, and `Run.pr_id` linkage + denormalization cleanup are pending consumer PRs.
- **#43** — system-wide live graph view; depends on #41 consumers.

When acting on any of the above, re-read the issue + the relevant code; don't act from this section's summary.

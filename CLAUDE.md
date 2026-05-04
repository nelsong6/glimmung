# glimmung

A platform for managing SDLC by leveraging agents — giving them precise lanes, heavily automating every element around them, and providing guard rails that protect token spend.

> *The Glimmung scanned the assembled list of beings he had summoned. From a thousand worlds they had come, each with a craft to contribute.* — paraphrased from Philip K. Dick, *Galactic Pot-Healer*; the metaphor is exact.

## The model

**Issue-driven agentic development.** A human writes an issue. An agent implements it. Glimmung is the substrate between: it queues, dispatches, paces, verifies, and surfaces.

Glimmung embraces the shift in the human's role from *coder* to *curator and manager of a software team*. The human writes specs, steers, reviews evidence, and merges. They don't type code anymore. Every affordance in glimmung assumes that.

## Principles

- **Issues are the canonical trigger.** Agents respond to them.
- **The verify loop is the one cycle in the system.** Agent implements, tests run, the decision engine returns RETRY or ADVANCE up to a bounded budget (typically 3 attempts; cost-ceiling on top of that). GitHub Actions can't model this cycle as a first-class construct — it gets buried in a script, which is boring and invisible. Glimmung makes the loop explicit: attempt history, decision logs, visible state. Everything else in the system (dispatch, PR open, merge, signal handling) is acyclic.
- **Make review easy; trust through test evidence.** Humans still review every PR — that's load-bearing. The job glimmung does is making review easy: the agent's test artifacts (`verification.json` + evidence refs) sit alongside the diff, the predictable fields are surfaced where you expect them, and the human can trust the agent knew it had to prove its work. The reject button exists for when something's still off; rejecting re-enters the verify loop with the human's feedback as additional context.
- **Elaborate queuing on stateful, host-pinned, scarce resources.** The agent-run model GitHub Actions doesn't have a clean shape for, and that kept getting rebuilt badly across spirelens, ambience, tank-operator. Glimmung owns this primitive — leases against capability-matched hosts, with a real queue and a real dashboard.
- **Per-issue test environments**, when the workflow opts in. Spin up an ephemeral environment per agent run on a wildcard hostname so the verify step has somewhere isolated to exercise the change. Some workflows don't need this and have no opt-in; the support is there when it is.
- **Absorbs GitHub's tech debt for this workload.** GHA's queueing and state model. Markdown rendering that can't carry rich evidence cleanly (no external links, awkward at the predictable structured fields glimmung tracks like cost / tokens / attempt count / test evidence). Issue and PR formats that weren't designed for the agent-driven shape. Where GitHub falls short for agentic SDLC, glimmung covers.

## The four levers

Every feature glimmung adds is one or more of:

- **Precise lanes** for the agent — bounded scope, contained changes, isolated environments where useful.
- **Heavy automation around the agent** — the orchestrator, decision engine, signal bus, dispatch, lock primitives. Meta-work the agent shouldn't have to think about.
- **Guard rails** — verify loop, retry budget, cost ceiling, attempt cap, lock-based serialization. Freedom inside the lane; the lane is real.
- **Token-spend protection** — every retry costs money. Budgets are frozen at run-creation time so mid-run relabeling can't move the goalposts.

## Strategic arc

Glimmung is the platform a small SDLC operates on top of, across multiple projects. The lease primitive was the wedge that got us in the door; the reason to stay is everything purpose-curated around it — a single canonical surface for issues, PRs, runs, attempts, evidence, costs, and signals across every project that opts in.

At its best: humans set direction, agents execute in lanes, and the system gracefully handles everything around the work — queue, retry, escalate, abort, surface.

## Current runner direction

GitHub Issues are not part of the live Glimmung issue/run loop. GitHub Issues may still be used as temporary backlog/tracker notes until Glimmung is self-hosting that surface.

For web-native apps, Glimmung is moving off GitHub Actions as the runner source and onto Glimmung-managed Kubernetes Jobs. Keep `gha_dispatch` supported for legacy/exception flows, especially Spirelens for now, but do not route new Ambience-native work through GitHub Actions. GitHub PRs remain a syndication/review target; the canonical Glimmung review object is Report.

## Container build verification

Agent pods are not expected to have Docker. Do not report missing local Docker
as a blocker. Run available repo checks first, then use PR CI as the normal
container build gate: `.github/workflows/docker-build-check.yml` performs
throwaway builds for the app and mcp-glimmung images with `push: false`. If
image-packaging feedback is needed before a PR is ready, manually dispatch that
workflow with `git_ref`. Release/deploy workflows are the only path that
publishes images.

## Frontend / design

Anything that touches the dashboard UI (`frontend/src/`) — new views, new components, copy changes, color tweaks — should follow the design system at `design-system/`.

- **Read `design-system/SKILL.md` first.** It's the checklist: voice (lowercase, terse, technical), pills only from `{free, busy, drain, info}`, two-step inline cancel for destructive actions, KPI strip when a table has > 3 row states, no emoji, no icons-in-buttons, etc.
- **Tokens are at `design-system/colors_and_type.css`.** `frontend/src/index.css` imports it; do not redefine `--color-*`, `--bg-*`, `--fg-*`, `--state-*`, or `--font-*` in the live frontend. The check at `scripts/check-design-tokens.sh` (run in CI) fails if you do.
- **Reference visuals live at `design-system/preview/*.html`.** Open them in a browser to see what each component / pill / state should look like before re-implementing.
- **`design-system/ui_kits/dashboard/`** is a runnable React UI kit (UMD + Babel-standalone, no build step) with fixture data — useful as a sandbox for trying a layout change before touching the live frontend.

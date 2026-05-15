# Quality Timeframes

This is a long-term learning, craft, and portfolio operating mode. The default
engineering timeframe is years, not a sprint, a demo, or a quick production
patch. When a decision can be made at short, medium, or heavy weight, choose
the heavy long-term solution unless the user explicitly asks for a temporary
experiment.

This document is a decision policy for agents and humans working on substantial
technical projects. It sets the quality bar and the expected time horizon.
When a project also has migration, architecture, inspiration, or design-system
guidance, those documents are part of the definition of done.

## Default Timeframe

Assume the user wants the robust version:

- Prefer complete architecture over quick relief.
- Prefer settled contracts over compatibility layers.
- Prefer durable state over process memory.
- Prefer observable systems over "it probably works."
- Prefer finishing the hardening now over creating a follow-up list.
- Prefer learning the right technology over avoiding it to save time.

Short-term fixes create long-term work when the work is already committed to
the full solution. Do not choose a light or medium implementation because it is
faster unless the user explicitly says speed is the priority for this task.

## Done Standard

A feature is not done when the happy path works. It is done when the user can
step away from that feature and focus on another complete feature without
worrying about hidden hardening debt.

For substantial work, include these before calling it complete:

- The durable data model or state ownership is explicit.
- Runtime behavior survives realistic disconnects, retries, restarts, and
  rollouts within the stated product boundary.
- User-visible state is not inferred from transient local optimism when a
  durable source exists.
- Failure, timeout, cancellation, and retry states are designed and visible.
- Observability exists for the bugs a user would otherwise have to guess about.
- Tests cover the contract, not only the implementation detail.
- Docs describe the final behavior and remove obsolete behavior.
- Migration guards prevent old paths from returning.
- Cost and scaling implications are understood, measured, or deliberately
  bounded.

If any of these are missing, either finish them in the same work or stop with a
blocker report that names the exact remaining dependency. Do not merge first
and leave the user to ask what remains.

## Planning Rule

When planning, do not present a minimal fix as the default. Start from the
long-term design and break it into chunks only when the chunks are independently
safe.

Acceptable chunking:

- A sequence of PRs where each PR leaves the system in a coherent state.
- A preparatory refactor that has its own verification and removes risk for the
  full solution.
- A schema or infrastructure step that is inactive until the complete path is
  ready, provided it does not preserve old behavior or add compatibility.

Unacceptable chunking:

- Shipping the easy UI change and leaving the durable model for later.
- Keeping polling, sockets, fallback reads, or old routes as a safety blanket
  after a migration.
- Adding logs while deferring the metrics needed to operate the feature.
- Marking a control action successful before the durable system confirms it.
- Leaving "future hardening" as an unowned list after merge.

If the full solution is too large for one PR, write the full plan first. The
plan must name every stage needed for the final state and identify which stages
are required before the feature can be considered done.

## Communication Rule

Be explicit about quality tradeoffs before implementation and before merge.

Before implementation:

- State the long-term endpoint, not only the immediate patch.
- Identify any tempting short-term shortcut and whether it is being rejected.
- Call out operational risks such as cost, observability, rollout behavior, and
  stuck states.

Before merge:

- Say whether the feature is complete by this document's standard.
- If it is not complete, do not frame the remaining work as optional
  robustness. Name it as unfinished scope.
- Do not make the user discover a 5-10 item hardening list after merge.

## Relationship To Urgency

Urgency changes sequencing, not the quality bar. If there is a production
incident, it is acceptable to make a small stabilizing change only when it is
clearly labeled as incident containment and paired with the complete fix plan.
Outside that explicit emergency mode, optimize for the full solution.

## Review Heuristics

Challenge work that has these smells:

- "Just in case" fallback paths.
- Compatibility for unknown callers.
- Runtime reads whose purpose is to keep an old behavior alive.
- Local UI state that can contradict durable state.
- Polling loops without a cost and scale story.
- Missing counters for user-trust failures.
- A feature that only works until reload, reconnect, restart, or rollout.
- A PR description that says what changed but not what is now complete.

The preferred outcome is fewer, more complete features. Heavy is the default.

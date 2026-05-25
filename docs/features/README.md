# Feature Contracts

The repo-wide policy docs describe the quality bar:

- [migration-policy.md](../migration-policy.md)
- [quality-timeframes.md](../quality-timeframes.md)
- [go-migration-contracts.md](../go-migration-contracts.md)

Feature contracts translate those rules into concrete, reviewable invariants
for the parts of Glimmung that can visibly lie to users, reviewers, agents, or
operators.

Read the relevant contract before planning or implementing substantial work in
that area. A PR that touches a contracted feature should name the contract and
show evidence that the contract still holds. Evidence can be a unit test,
integration test, browser observation, route inventory test, workflow
registration rejection, metric, log, run report, or direct runtime observation,
depending on the risk.

## Contracts

- [Auth And API Surface](auth-and-api/contract.md)
- [Dashboard And Styleguide](dashboard-and-styleguide/contract.md)
- [Issues And Runs](issues-and-runs/contract.md)
- [Observability And Evidence](observability-and-evidence/contract.md)
- [Review Surfaces](review-surfaces/contract.md)
- [Test Slots](test-slots/contract.md)
- [Workflow Execution](workflow-execution/contract.md)

## When To Add A Contract

Add or expand a feature contract when a feature:

- can show state that contradicts Cosmos, Kubernetes, GitHub, or callback
  state;
- crosses browser, MCP, workflow runner, database, Kubernetes, or GitHub
  boundaries;
- depends on live streams, reconnect, retry, rollout, or async recovery;
- has controls where "appeared to work" is different from "durably worked";
- has already produced a user-trust or operator-trust bug.

Small static UI, isolated copy changes, and one-off internal helpers usually do
not need their own contract. They inherit the global policy docs.

## Capability Ledgers

Contracts describe durable invariants for a feature area. Capability ledgers
give named product behavior a stable handle before and after it ships.

Use a `capabilities.md` file inside the relevant feature folder when a new
behavior is complex enough that future agents should not have to reconstruct
the intent from chat history or PR archaeology. Each capability entry should
include:

- a stable name;
- status such as `proposed`, `in progress`, `active`, `shipped`, or `retired`;
- intent;
- affected contracts;
- required or shipped evidence.

Do not use capability ledgers as a backlog for every small idea. Add an entry
when the behavior affects user trust, navigation, durability, lifecycle,
cross-feature coordination, or a top-level product surface. When a capability
settles into a permanent invariant, fold the invariant into `contract.md` and
keep the capability entry as the named history of why it exists.

## PR Review Rule

For any contracted feature, reviewers should ask:

- Which contract does this PR touch?
- Does this PR add or change a named capability?
- Which invariant could the PR break?
- What evidence proves the invariant still holds?
- Does refresh, reconnect, retry, restart, or rollout reveal state the live UI
  missed?
- Is any old path, fallback, or browser-local source of truth being kept alive?

If the answer depends on "it should probably work," the PR is not complete by
the repo quality standard.

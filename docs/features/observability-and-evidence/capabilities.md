# Observability And Evidence Capabilities

This ledger names user-facing behavior under the observability and evidence
contract. It is not a backlog. Entries land here when the behavior needs a
stable handle for planning, review, tests, incident follow-up, or retirement.

## durable-inspections

Status: in progress

Intent:
Make every `inspect_browser_url` invocation a durable artifact record so the
screenshot and inspection report survive the calling MCP tool response,
stay referenceable by `/v1/artifacts/...`, and compose with the existing
`pr_touchpoint` finalize machinery rather than burning agent context as
inline base64.

Affected contracts:
- Observability And Evidence (primary)
- Auth And API Surface (new MCP-used route + tool schema reshape)
- Test Slots (lease-cleanup goroutine gains an artifact sweep step)
- Review Surfaces (Run-bound inspections flow into Touchpoint evidence
  through the existing `pr_touchpoint` primitive — no new caller-facing
  promotion API)

Contract impact:
- Glimmung becomes the first writer into the artifact store. Existing run
  evidence is still uploaded by agent runners via the stdout base64-tar
  side-channel; convergence onto a single write surface is a documented
  follow-up.
- `slot_inspections` is the durable Postgres ledger for lease-scoped
  inspections. Run-bound inspections (caller in a Run context) are not
  tracked here in V1 because lease metadata does not carry a stable
  `run_id` — that path is the documented follow-up alongside the
  convergence above.
- Lease-cleanup is the single retention boundary for free inspections:
  every `slot_inspections` row plus its `report.json` + `screenshot.png`
  is deleted as part of `cleanupTestSlotRuntime`. No wall-clock TTL, no
  "unless referenced" branch.
- Artifact-path whitelist grows by one prefix (`inspections/`).
  `touchpoint_evidence` resolver canonicalizes `inspections/` refs into
  the standard `blob://artifacts/...` shape so a testing job that emits
  an inspection ref in `verification.evidence` is normalized into
  Touchpoint evidence at finalize, exactly like screenshots / videos
  /evidence / refs.
- New metric family `glimmung_inspections_*` with closed-enum labels
  (`scope`, `phase`, `piece`, `outcome`). No project/lease/session
  identifiers in labels.

Evidence:
- `internal/server/inspection_api_test.go` — write-contract, idempotent
  retry, ledger rollback, missing-part rejection, invalid-JSON, missing
  lease, GET-by-id detail shape, GET-by-id 404 for missing, list with
  no filter / lease filter / project filter / invalid-limit rejection.
- `internal/server/inspection_sweep_test.go` — lease-cleanup deletes
  rows and blobs; no-rows no-op; nil-store no-crash.
- `internal/server/artifact_api_test.go` — `inspections/` prefix served;
  `inspections/../escape` rejected.
- `internal/server/route_inventory_test.go` — `POST /v1/inspections`,
  `GET /v1/inspections`, and `GET /v1/inspections/{inspection_id}`
  routes registered in the expected order.
- Follow-up: run-scoped inspection prefix (`runs/<project>/<run>/inspections/...`)
  once leases carry a stable `run_id`. Tracked under glimmung#143.

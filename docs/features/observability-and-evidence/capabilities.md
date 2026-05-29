# Observability And Evidence Capabilities

This ledger names user-facing behavior under the observability and evidence
contract. It is not a backlog. Entries land here when the behavior needs a
stable handle for planning, review, tests, incident follow-up, or retirement.

## durable-inspections

Status: active

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
- `slot_inspections` is the durable Postgres ledger for every
  inspection record. Lease-scoped (`scope='lease'`, `lease_id` set) and
  run-scoped (`scope='run'`, `run_id` set) rows live in the same table;
  the `scope` column distinguishes them at query time.
- Run binding is **caller-declared** at POST time, not derived from
  lease metadata. Test-slot leases intentionally live across multiple
  runs (a slot is a session-owned reservation, not a run-owned
  reservation), so the test-slot lease has no stable `run_id`. The
  POST `/v1/inspections` handler accepts an optional `run_id` form
  field; when supplied, glimmung validates the run exists under the
  lease's project and writes the bytes under
  `runs/<project>/<run_id>/inspections/<id>/...`. When absent, bytes
  land under `inspections/<lease_id>/<id>/...` (the default).
- Lease-cleanup is the retention boundary for **lease-scoped**
  inspections: every `scope='lease'` row matching the lease + its
  `report.json` + `screenshot.png` is deleted as part of
  `cleanupTestSlotRuntime`. **Run-scoped rows survive lease cleanup**
  and follow Run evidence retention semantics (the same as Run videos
  and screenshots): no per-row sweep, governed by whatever global
  retention policy the artifact store implements.
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
- Runner-evidence convergence: the original glimmung#143 plan named a
  "convergence of the agent-runner stdout-base64-tar evidence path onto
  `POST /v1/inspections`" follow-up. Investigation while doing the
  follow-up showed the framing was wrong: the stdout-base64-tar
  emission in `internal/ops/agentops/job.go` (the developer-CLI
  `glimmung-agent apply-agent-job` script) had **no consumer anywhere
  in the repo**. The production runner (`glimmung-native-runner`)
  uses a different completion-file ref path and does not currently
  upload evidence bytes. The honest action per migration-policy
  ("vestigial code is a deletion target") was deletion of the dead
  emission, not migration. A real evidence-upload pipeline (runner
  picks up evidence files → POSTs to glimmung with run context →
  glimmung stores under `runs/<project>/<run>/<kind>/<name>`) is a
  separately-scoped feature, not a follow-up — explicitly out of
  scope here.
- Run-scoped path evidence:
  `internal/server/inspection_api_test.go::TestCreateInspectionRunScopedWritesUnderRunPrefix`
  asserts run-bound bytes land under `runs/<project>/<run>/inspections/...`
  and the ledger row carries `scope='run'` + `run_id`.
  `TestCreateInspectionRunScopedRejectsMissingRun` and
  `TestCreateInspectionRunScopedRejectsCrossProject` pin the
  validation contract.
  `internal/server/inspection_sweep_test.go::TestSweepLeaseInspectionsLeavesRunScopedRows`
  pins the retention boundary: lease cleanup deletes lease-scoped rows
  and blobs while run-scoped rows persist.

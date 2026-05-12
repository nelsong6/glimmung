# Go runtime cleanup inventory

Issue #446 tracks the final cleanup after the production image moved to
`cmd/glimmung-go`. This inventory is the Phase 0 baseline for deleting or
isolating the old Python app without breaking active Glimmung behavior.

## Repo-level target

The cleanup target is **no Python in the app runtime, production image, deploy
path, or default app CI gate**.

Python may remain only as legacy cleanup/reference material under
`src/glimmung` and `tests` until the remaining keep/port/retire decisions are
resolved. Repo-local workflow and preview operations should be Go code under
`cmd/` plus testable functions under `internal/`.

The root `pyproject.toml` and the old repo-local `mcp/pyproject.toml` have
been removed. Do not reintroduce repo-root app dependencies for Python.

The old one-shot Python migration scripts under `scripts/` and the old
`mcp/glimmung_agent` Python ops helper have been retired. New data fixes should
be written against the Go store/API contracts or isolated behind an explicit
tooling decision.

## Route authority

The active HTTP route inventory is now Go-owned by
`internal/server/route_inventory_test.go`. That test reads the registrations in
`internal/server/server.go` and fails when the Go route surface changes without
an intentional inventory update.

`tests/test_api_contract_inventory.py` has been removed because it imported the
legacy FastAPI app and made the Python app route table look canonical.

## App CI authority

The default app CI gate is now Go and frontend-native:

- `go test ./...`
- `go vet ./...`
- `npm run test:run` from `frontend/`
- `npm run build` from `frontend/`

Pull-request CI does not install root Python dependencies or run the legacy
FastAPI test suite. The push-only live Cosmos smoke has been moved to
`internal/store/cosmos` and exercises the Go lock lifecycle with
`GLIMMUNG_TEST_COSMOS=live`.

The root Python suite remains as cleanup/reference material until individual
behaviors are ported, retired, or moved, but it no longer has repo-root
packaging or app CI authority. The former MCP helper tests are now Go tests
under `internal/ops/agentops`.

## Route parity notes

Most active routes are registered by the Go server. The rows below call out the
routes whose behavior is intentionally different from the legacy FastAPI app or
still needs a keep/port/delete decision.

| Route surface | Go state | Cleanup decision |
|---|---|---|
| `/healthz`, `/v1/config`, `/v1/auth/me` | Implemented in Go | Go is canonical. |
| `/v1/artifacts/{blob_path}` | Implemented in Go as `/v1/artifacts/{blob_path...}` | Same public path shape; Go ServeMux uses `{name...}` for catch-all segments. |
| Lease callback routes | Implemented in Go | Go is canonical for token-scoped lease clients. |
| `POST /v1/lease`, `POST /v1/leases/cancel` | Implemented in Go | Go is canonical; admin-auth guarded. |
| Project, workflow, host registration routes | Implemented in Go | Go is canonical; `gha_dispatch` stays valid only as explicit legacy/exception support. Native web app projects default blank phase kinds to `k8s_job` and reject explicit `gha_dispatch`. |
| Workflow upstream/sync routes | Implemented in Go | Keep as import convenience for older desired-state flows, not runtime source of truth. |
| Issue number routes | Implemented in Go | Project plus issue-number routes are canonical. |
| Issue storage-ID routes under `/v1/issues/by-id/...` | Registered in Go and return `410 Gone` | Keep only as explicit compatibility tombstones until clients have migrated. |
| GitHub issue-coordinate detail and graph routes `/v1/issues/{owner}/{repo}/{n}` | Registered in Go and return `410 Gone` | Project plus issue-number lookup is canonical; GitHub Issue coordinates remain explicit compatibility tombstones. |
| Canonical issue graph route `/v1/issues/by-number/{project}/{issue_number}/graph` and system graph `/v1/graph` | Implemented in Go | Go now owns the dashboard graph projection for issue/run/attempt/touchpoint/signal nodes. |
| Run report, abort, callback, native event/status/failure/completion routes | Implemented in Go | Callback-token routes are canonical for runners. |
| `POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/completed` | Python-only | Prefer callback-token completion unless a live caller is found. |
| `GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/pod-logs` | Python-only | Decide whether Go needs direct pod log proxying or whether event/log archive evidence replaces it. |
| Native GitHub token routes | Python-only | Identify live native runners before porting; otherwise retire. |
| Run replay, resume, and completion forward-dispatch routes | Implemented in Go | Go is canonical. Completion dispatches ready downstream workflow phases instead of sealing multi-phase runs after the first advance. |
| Playbook list/get/create/gate routes | Implemented in Go | Go is canonical for current Playbook control-plane operations. |
| `POST /v1/playbooks/{project}/{playbook_ref}/run` | Python-only | Decide whether Playbook execution is still product scope before porting. |
| Portfolio element CRUD routes | Implemented in Go | Go is canonical. |
| `POST /v1/portfolio/elements/dispatch` | Python-only | Port only if portfolio-to-issue dispatch remains required. |
| Test-slot checkout/return routes | Python-only | Likely obsolete if native Kubernetes jobs own ephemeral test capacity; confirm before deletion. |
| Touchpoint/report list/detail/create/update/version routes | Implemented in Go, storage-ID routes return `410 Gone` | GitHub PR coordinate and project issue-number routes are canonical. |
| `POST /v1/signals` | Implemented in Go as enqueue-only | Triage/drain behavior still needs a keep/port/retire decision. |
| `POST /v1/webhook/github` | Implemented in Go for signature validation and acknowledgement | Event-specific issue/run processing from Python still needs a live-consumer decision. |
| Static asset and SPA fallback routes | Implemented in Go when `GLIMMUNG_STATIC_DIR` is set | Go production image serves built frontend assets. |

## Legacy Python module classification

| Module | Classification | Notes |
|---|---|---|
| `src/glimmung/__main__.py` | Reference-only pending deletion | Old app entrypoint; docs should not send operators here. |
| `src/glimmung/app.py` | Reference-only pending deletion | Legacy FastAPI route surface and background loops. Keep only while resolving route gaps above. |
| `src/glimmung/artifacts.py` | Already ported | Go artifact route and blob store own app behavior. |
| `src/glimmung/auth.py` | Already ported | `internal/auth` owns Entra and Kubernetes service-account auth. |
| `src/glimmung/budget.py` | Already ported | `internal/domain/budget` has golden parity coverage. |
| `src/glimmung/decision.py` | Already ported | `internal/domain/decision` is the active decision engine. |
| `src/glimmung/dispatch.py` | Still needed for parity decisions | Go owns dispatch/resume paths, but this remains reference material for native token routes, test-slot routes, and legacy behavior checks. |
| `src/glimmung/github_app.py` | Partially ported | `internal/github` covers token minting, workflow dispatch, cancel, and upstream fetch. PR/comment helpers need a product decision. |
| `src/glimmung/issues.py` | Already ported for canonical routes | Storage-ID and GitHub-coordinate behavior is intentionally disabled in Go. |
| `src/glimmung/leases.py` | Already ported for active lifecycle | Go store owns lease acquire/callback/cancel behavior. |
| `src/glimmung/locks.py` | Already ported for issue/PR lock usage | Go store owns lock document compatibility for dispatch, resume, completion, abort, and read-model enrichment. |
| `src/glimmung/models.py` | Reference-only until all tests/docs move | Go structs now mirror active public and Cosmos shapes. Keep as schema reference during cleanup. |
| `src/glimmung/native_events.py` | Already ported for active event/status paths | Go owns native event write/list/status/failure routes. |
| `src/glimmung/native_k8s.py` | Still needed for parity decisions | Direct native job launch/log/token behavior needs a keep/port/retire decision. |
| `src/glimmung/paths.py` | Already ported | `internal/domain/paths` has golden parity coverage. |
| `src/glimmung/playbooks.py` | Partially ported | Current CRUD/gate routes are in Go; `run` endpoint remains undecided. |
| `src/glimmung/public_ids.py` | Already ported | `internal/domain/publicids` has golden parity coverage. |
| `src/glimmung/replay.py` | Already ported for active route | `internal/server/replay_api.go` owns run replay. |
| `src/glimmung/reports.py` | Already ported | Go touchpoint/report store owns active report shape. |
| `src/glimmung/runs.py` | Partially ported | Go owns run reports, mutation callbacks, resume, replay, completion, retry, and multi-phase forward dispatch. Keep as reference for remaining native token/log decisions. |
| `src/glimmung/settings.py` | Already ported for app runtime | `internal/server.SettingsFromEnv` owns production settings. |
| `src/glimmung/signals.py` | Still needed for parity decisions | Go enqueues signals; legacy drain/triage behavior is not fully ported. |
| `src/glimmung/touchpoints.py` | Already ported | Go touchpoint/report APIs are canonical except storage-ID tombstones. |
| `src/glimmung/triage.py` | Still needed for parity decisions | No Go triage decision engine exists yet. |
| `src/glimmung/verification.py` | Already ported for decision input | Active verification interpretation lives in `internal/domain/decision` and completion/replay handlers. |
| `src/glimmung/workflow_sync.py` | Already ported | `internal/server/workflow_sync_api.go` and `internal/github` own upstream fetch/sync. |
| `src/glimmung/db.py` | Reference-only pending deletion | Go Cosmos store owns production persistence. |

## Python test disposition

| Test file | Disposition |
|---|---|
| `tests/test_api_contract_inventory.py` | Replaced by `internal/server/route_inventory_test.go`; deleted. |
| `tests/test_abort_run.py` | Replace with existing or new Go run mutation coverage. |
| `tests/test_always_run_teardown.py` | Port if always-run phase teardown remains product behavior. |
| `tests/test_artifacts.py` | Replace with `internal/server/artifact_api_test.go` and blob-store tests. |
| `tests/test_budget.py` | Replace with `internal/domain/budget` golden tests. |
| `tests/test_cancel_lease.py` | Replace with `internal/server/lease_api_test.go`. |
| `tests/test_decision.py` | Replace with `internal/domain/decision` tests. |
| `tests/test_dispatch.py` | Replace with `internal/server/dispatch_api_test.go` and completion/resume tests. |
| `tests/test_dispatch_failure_rollback.py` | Replace with Go dispatch rollback coverage. |
| `tests/test_dispatch_inputs_filter.py` | Port to Go dispatch input tests if still required. |
| `tests/test_disposable_frontend_auth_config.py` | Replace with `internal/server/server_test.go` config tests. |
| `tests/test_evidence_verification_gate.py` | Replace with `internal/domain/decision` and completion API tests. |
| `tests/test_glimmung_agent_ops.py` | Replaced by `internal/ops/agentops` Go tests; deleted from the legacy app test suite. |
| `tests/test_issue_endpoints.py` | Replace with `internal/server/issue_api_test.go`. |
| `tests/test_issue_graph.py` | Keep as reference until graph endpoints are ported or retired. |
| `tests/test_issues.py` | Replace with Go issue store/API tests. |
| `tests/test_job_level_dispatch.py` | Port if native job dispatch behavior remains active. |
| `tests/test_leases_sweep.py` | Port only if Go keeps a sweep loop; otherwise delete as obsolete. |
| `tests/test_live_cosmos_smoke.py` | Replaced by `internal/store/cosmos.TestLiveCosmosLockLifecycle` for the live lock smoke. |
| `tests/test_locks.py` | Port remaining generic lock edge cases to Go store tests. |
| `tests/test_mandatory_phases.py` | Replace with Go workflow validation tests. |
| `tests/test_native_events.py` | Replace with Go native event/status tests. |
| `tests/test_native_k8s.py` | Keep as reference until native Kubernetes launch/log/token decision is made. |
| `tests/test_paths.py` | Replace with `internal/domain/paths` golden tests. |
| `tests/test_phase_depends_on.py` | Replace with Go workflow parse/validation tests. |
| `tests/test_phase_input_refs.py` | Replace with `internal/domain/phaserefs` tests. |
| `tests/test_playbook_templates.py` | Port if Playbook templates stay in app scope; otherwise move to docs/tooling. |
| `tests/test_playbooks.py` | Replace with `internal/server/playbook_api_test.go`. |
| `tests/test_portfolio_elements.py` | Replace with `internal/server/portfolio_api_test.go`; dispatch-specific coverage remains undecided. |
| `tests/test_pr_marker.py` | Port only if PR body marker parsing remains in Glimmung. |
| `tests/test_pr_webhook.py` | Port only if GitHub webhook PR handling remains active. |
| `tests/test_public_ids.py` | Replace with `internal/domain/publicids` golden tests. |
| `tests/test_public_read_auth.py` | Replace with Go auth/admin/public endpoint tests. |
| `tests/test_public_signal_refs.py` | Port if signal ref projections remain active. |
| `tests/test_replay.py` | Replace with `internal/server/replay_api_test.go`. |
| `tests/test_report_endpoints.py` | Replace with `internal/server/touchpoint_api_test.go`. |
| `tests/test_reports.py` | Replace with Go touchpoint/report store/API tests. |
| `tests/test_resume.py` | Replace with `internal/server/resume_api_test.go`. |
| `tests/test_run_callbacks.py` | Replace with Go run mutation/completion tests. |
| `tests/test_run_report_api.py` | Replace with `internal/server/run_api_test.go`. |
| `tests/test_signals.py` | Keep as reference until signal drain/triage decision is made. |
| `tests/test_state_snapshot.py` | Replace with `internal/server/state_api_test.go`. |
| `tests/test_test_slot_checkout.py` | Delete if test-slot routes are retired; otherwise port. |
| `tests/test_touchpoint_aliases.py` | Replace with Go touchpoint alias/tombstone tests. |
| `tests/test_triage.py` | Keep as reference until triage engine is ported or retired. |
| `tests/test_verification.py` | Replace with Go decision/completion verification tests. |
| `tests/test_workflow_endpoints.py` | Replace with Go workflow write/read tests. |
| `tests/test_workflow_sync.py` | Replace with `internal/server/workflow_sync_api_test.go`. |
| `tests/cosmos_fake.py` | Delete after Python app tests are gone; use Go fakes and focused Cosmos store fixtures. |

## Cosmos compatibility baseline

The Go store must continue reading existing documents in these containers until
an explicit migration window exists:

| Container | Compatibility notes |
|---|---|
| `projects` | Preserve `id`, `name`, `githubRepo`, `metadata`, `createdAt`; `argocdApp` may still appear on old docs. |
| `workflows` | Preserve `project`, `name`, `phases`, `pr`, `budget`, `triggerLabel`, `defaultRequirements`, `metadata`, `createdAt`; empty phase `kind` still defaults to `gha_dispatch` for legacy/non-native projects, while native web app projects default blank phase kinds to `k8s_job`. |
| `hosts` | Preserve host capability and lease fields for legacy/self-hosted-runner visibility. |
| `leases` | Preserve lease numbers, state values, `metadata.lease_callback_token`, requester metadata, test-slot fields, and TTL timestamps. |
| `runs` | Preserve issue refs, public run-number fields, attempts, verification, phase outputs, callback tokens, lock-holder fields, PR links, and native attempt fields. |
| `run_events` | Preserve native event sequence fields for runner status/log replay. |
| `issues` | Preserve project issue numbers, state values, metadata workflow link, comments, and archive/discard timestamps. |
| `locks` | Preserve `scope`, `key`, `state`, `held_by`, timestamps, and URL-escaped doc IDs. |
| `reports` | Preserve Touchpoint/Report documents and portfolio element documents currently stored in this container. |
| `playbooks` | Preserve Playbook entry state, gates, issue specs, and integration strategy fields. |
| `signals` | Preserve queued signal documents until signal drain/triage behavior is ported or retired. |

## Phase 6 workflow execution decisions

| Surface | Decision | Go coverage |
|---|---|---|
| New web-native workflow kind default | Projects marked `native_webapp`, `native_web_app`, or `app_type=native_web_app` default omitted phase `kind` to `k8s_job`. | `internal/server/workflow_write_api_test.go`, `internal/server/workflow_sync_api_test.go`, `internal/store/cosmos/cosmos_test.go`. |
| GitHub Actions dispatch | Keep `gha_dispatch` readable and dispatchable only when the workflow phase explicitly asks for it. Native web app project registration rejects explicit `gha_dispatch`. | Workflow write/sync validation plus dispatch/completion tests for legacy GHA forwarding. |
| Native dispatch and callbacks | `POST /v1/runs/dispatch` creates the run, lease, callback token, and initial `k8s_job` attempt without firing `workflow_dispatch`; callback-token native status/events/completion routes are canonical for runners. | `internal/server/dispatch_api_test.go`, `internal/server/run_mutation_api_test.go`, `internal/server/completion_api_test.go`, `internal/server/lease_callback_api_test.go`. |
| Lease lifecycle and cancel/abort | Lease read, heartbeat, release, admin cancel, run abort, terminal completion, retry, resume, and teardown paths are Go-owned. Host-backed leases remain for legacy GHA pools; native leases use callback-token APIs and native metadata. | `internal/server/lease_api_test.go`, `internal/server/lease_callback_api_test.go`, `internal/server/run_mutation_api_test.go`, `internal/server/resume_api_test.go`, `internal/server/completion_api_test.go`. |
| Evidence and native logs | Completion payloads stamp summaries, screenshots, verification, phase outputs, and cost. Native event/log archive projection is Go-owned; direct pod-log proxying remains undecided. | `internal/server/completion_api_test.go`, `internal/server/run_api_test.go`, `internal/server/graph_api_test.go`, `internal/store/cosmos/cosmos_test.go`. |
| `.glimmung/workflows/<name>.yaml` | Keep upstream/sync endpoints as optional import helpers. The registered workflow document in Cosmos is runtime authority for dispatch. | `internal/server/workflow_sync_api_test.go`; docs in `README.md` and `docs/workflow-shape.md`. |
| Dashboard host UX | Keep host registration and host tables for legacy/self-hosted GHA capacity, but label them as legacy surfaces. Native web app projects do not surface host pools as the normal path. | Frontend build plus `frontend/src/StyleguideView.tsx` catalog update. |

## Phase 7 workflow ops tooling decisions

| Surface | Decision |
|---|---|
| `cmd/glimmung-agent` | New Go CLI for validation-preview and agent-job orchestration. Commands stay thin and delegate to `internal/ops/agentops`. |
| `internal/ops/agentops` | Owns reusable functions and command-runner boundaries for ACR image checks/builds, Helm preview deploy/destroy, Kubernetes labels, public preview waiting, and agent Job apply/wait behavior. |
| `mcp/glimmung_agent` | Retired and deleted. The repo no longer has a Python package for agent workflow ops. The dedicated `mcp-glimmung` repo remains the MCP server surface. |
| `.github/workflows/agent-pr-cleanup.yml` | Builds `./cmd/glimmung-agent` and invokes the binary for validation-preview cleanup; it no longer installs Python packaging. |
| `scripts/migrate-prs-to-reports.py` | Retired and deleted. It imported `src/glimmung` models/store helpers and represented an old one-shot PR backfill path. |
| `scripts/migrate_workflows_to_phases.py` | Retired and deleted. The phase-shape migration is complete; Cosmos Workflow documents are now runtime authority. |
| `scripts/seed-from-github.py` | Retired and deleted. It imported legacy issue/report/run modules and seeded historical GitHub PR state through the old Python app package. |
| README browser-inspection command | Kept as optional external `mcp-glimmung` tooling because it belongs to the dedicated MCP repo, not this repo's workflow ops helper. |

## Live-consumer checks still required

Before deleting the Python app tree, identify live consumers for:

- `gha_dispatch` workflows in legacy or exception projects.
- Storage-ID routes and GitHub issue-coordinate routes.
- Native pod-log and GitHub-token routes.
- Test-slot checkout/return routes.
- Playbook run and portfolio dispatch routes.
- Signal drain and triage behavior.

Those checks determine which remaining Python-only behavior must be ported,
which compatibility tombstones stay registered, and which routes can be deleted.

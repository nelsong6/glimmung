# Go runtime cleanup inventory

Issue #455 is the active cleanup record for the Python retirement that
followed #446. The cleanup target is now met for the app path: Glimmung's
runtime, production image, deploy path, repo-local ops CLI, and default CI gate
are Go plus the Vite dashboard.

## Final State

- The production entrypoint is `cmd/glimmung-go`.
- The active HTTP surface is registered in `internal/server/server.go` and
  guarded by `internal/server/route_inventory_test.go`.
- The Cosmos persistence boundary is `internal/store/cosmos`.
- Repo-local workflow operations live in `cmd/glimmung-agent` and
  `internal/ops/agentops`.
- Root Python packaging is absent.
- The retired Python app tree under `src/glimmung/` has been deleted.
- The retired root Python test suite under `tests/` and `tests/cosmos_fake.py`
  has been deleted.

## Ported Surfaces

| Surface | Final state |
|---|---|
| Native runner callback, event, status, failure, replay, resume, completion, retry, forward-dispatch, and Kubernetes launch paths | Go-owned. |
| Native pod-log proxy | Deleted; use native events and archived evidence. |
| Native HTTP GitHub token routes | Go-owned surface for native runner callbacks. |
| Test-slot checkout/return routes | Go-owned surface for MCP/test skill callers; project test-environment scaling remains active for capacity management. |
| Storage-ID, GitHub issue-coordinate, Report alias, and PR-coordinate Touchpoint routes | Deleted from the live route table; route inventory tests reject reintroduction. |
| `POST /v1/portfolio/elements/dispatch` | Go-owned; creates a portfolio review Issue and dispatches through the canonical run path. |
| `POST /v1/playbooks/{project}/{playbook_ref}/run` | Go-owned; advances ready Playbook entries by creating Issues and dispatching Runs. |
| Signal drain and request-changes triage | Go-owned; queued PR feedback signals drain in the Go service, reopen linked Runs through the PR recycle policy, and hold PR locks until terminal release. |
| GitHub webhook event processing beyond signature acknowledgement | Retired unless a future product issue restores a specific syndication behavior. |
| GitHub Actions executor dispatch | Retired; managed workflow phases must use the native `k8s_job` path. |

## Stored Data Notes

The Go store owns these active Cosmos containers:

| Container | Data notes |
|---|---|
| `projects` | Preserve `id`, `name`, `githubRepo`, `metadata`, and `createdAt`; `argocdApp` may still appear on old docs. |
| `workflows` | Preserve `project`, `name`, `phases`, `pr`, `budget`, `defaultRequirements`, `metadata`, and `createdAt`. |
| `leases` | Preserve lease numbers, state values, callback-token metadata, requester metadata, test-slot fields, and TTL timestamps. |
| `runs` | Preserve issue refs, public run-number fields, attempts, verification, phase outputs, callback tokens, lock-holder fields, PR links, and native attempt fields. |
| `run_events` | Preserve native event sequence fields for runner status/log replay. |
| `issues` | Preserve project issue numbers, state values, metadata workflow link, comments, and archive/discard timestamps. |
| `locks` | Preserve `scope`, `key`, `state`, `held_by`, timestamps, and URL-escaped doc IDs. |
| `reports` | Preserve Touchpoint/Report documents and portfolio element documents currently stored in this container. |
| `playbooks` | Preserve Playbook entry state, gates, issue specs, created issue/run refs, and integration strategy fields. |
| `signals` | Preserve queued and processed signal documents, decisions, and failure reasons. |

## Validation Authority

The default verification set is:

- `go test ./...`
- `go vet ./...`
- `npm run test:run` from `frontend/`
- `npm run build` from `frontend/`
- PR CI Docker Build Check for app-image packaging

Do not reintroduce root Python packaging or a Python app test suite for the app
path. Any future Python must be isolated non-app tooling with its own explicit
owner and validation story.

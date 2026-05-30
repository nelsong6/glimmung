# Durable Project Configuration

Status: design + staged implementation. Stage 1 in progress.

Owner surface: `internal/store/pg/projects.go`, `internal/store/store/postgres.go`,
`internal/server/project_write_api.go`, and a new project sync surface mirroring
`internal/server/workflow_sync_api.go`.

## Problem

Project rows are durable Postgres state, but the only write path is the
imperative `register_project` upsert (`POST /v1/projects` →
`ProjectWriter.UpsertProject` → `pg.ProjectsStore.Upsert`). That upsert does a
**wholesale payload replace**:

```sql
ON CONFLICT (name) DO UPDATE
  SET payload = EXCLUDED.payload, github_repo = EXCLUDED.github_repo
```

and the payload is rebuilt from only `{name, github_repo, metadata}` taken from
the request body. Two structural defects fall out of this:

1. **Authored config has no complete source.** Callers hand-assemble a partial
   `metadata` map. Any field a caller omits is silently destroyed on the next
   register. This is how the live `glimmung` project lost its
   `test_slot_hot_swap` block — a later partial re-register dropped it — which
   forced a manual `kubectl cp` instead of the documented
   `glimmung-agent test-slot-hot-swap` path (`docs/test-slot-hot-swap.md`).

2. **Server-reconciled status is interleaved with authored config.** Reconciler
   outputs (`managed_auth_origin_status`,
   `native_standby_workload_identity_status`) live *inside* the same `metadata`
   blob that a human/agent overwrites on register. So a config write clobbers
   status, and a status write has to read-modify-write the whole authored blob
   (`mutateProject`). The two concerns are tangled and neither is safe to write
   independently.

There is no version history, no drift detection, and no declarative source —
project config is process-memory that any partial write corrupts invisibly.

This violates `docs/quality-timeframes.md` ("prefer durable state over process
memory", "settled contracts over compatibility layers", "the durable data model
is explicit") and leaves a class of silent-corruption bugs live.

## The model already exists for workflows

Glimmung solved the identical problem for **workflows** and codified the stance
in `docs/workflow-inspiration.md`: *"Postgres registrations remain the runtime
contract. Repository files are import/sync inputs, not what dispatch reads."*

Workflows have, and projects lack:

| Concern | Workflows | Projects (today) |
| --- | --- | --- |
| Durable runtime row | `workflows (project, name)` | `projects (name)` |
| Immutable version history | `workflow_schemas (project, schema_ref)`, content-hash `schema_ref` | none |
| Transactional versioned write | `WorkflowsStore.Upsert` mints schema + moves pointer | `Upsert` blind replace |
| Config vs status separation | status lives in `runs`, never in the workflow payload | status tangled into `metadata` |
| Declarative source | `.glimmung/workflows/<name>.yaml` in the project repo | none |
| Drift detection | `GET …/workflows/{name}/upstream` → `workflowsInSync` | none |
| Apply route | `POST …/workflows/{name}/sync` | none |

The fix is to bring projects up to the workflow object's durability bar. The
end state: **authored project config is a complete declarative document
(`.glimmung/project.yaml`), versioned immutably, reconciled into Postgres;
server-reconciled status is a separate, reconciler-owned column.** A full
config write then replaces authored config cleanly — exactly like a workflow
sync — without ever touching status.

## Target data model

`projects` table gains:

- `config_schema_ref text NOT NULL DEFAULT ''` — pointer to the current
  authored-config version (content hash).
- `status jsonb NOT NULL DEFAULT '{}'` — reconciler-owned. Holds the
  `*_status` blobs that used to live in `payload.metadata`.

New table, mirroring `workflow_schemas`:

```
project_config_schemas (
  name        text NOT NULL,
  schema_ref  text NOT NULL,           -- "pcs_<sha256[:8]>" of canonical authored config
  payload     jsonb NOT NULL,          -- the full authored-config document at this version
  created_at  timestamptz NOT NULL,
  PRIMARY KEY (name, schema_ref)
)
```

Authored config = `{name, githubRepo, metadata}` with the server-managed
`*_status` keys removed. The content hash is computed over a canonicalized form
of that document (stable key order), identical in spirit to
`workflowSchemaRef`.

### Config vs status split

Server-managed (reconciler-owned, lives in `status` column, never in an
authored register/file):

- `managed_auth_origin_status` (written by `SetManagedAuthOriginStatus`)
- `native_standby_workload_identity_status` (written by
  `SetNativeWorkloadIdentityStatus`)

Everything else in `metadata` is authored config — including
`native_standby_dns` (count + config), `native_standby_workload_identity`
(config), `test_slot_helm`, `test_slot_hot_swap`, and the per-project TTL
fields. Operator actions that set authored config through dedicated APIs
(`SetTestEnvironmentCount`, `SetTestLeaseDefaultTTL`, …) mutate authored config
and mint a new config version; they never write the `status` column.

### Read compatibility

`projectFromRecord` / `scanProjectRow` merge the `status` column back under
`Metadata` before returning, so the `server.Project` shape and every API
response (and the frontend, which renders `managed_auth_origin_status`) are
**unchanged**. The split is a storage-layer concern; the read contract is
stable. This is a clean migration, not a compatibility shim: nothing reads the
old interleaved layout after the backfill.

## Stages

Each stage leaves the system coherent on its own.

### Stage 1 — Durable substrate (this PR)

1. Migration (idempotent, `IF NOT EXISTS`, non-destructive to the existing row):
   - add `projects.config_schema_ref`, `projects.status`;
   - create `project_config_schemas`;
   - one-time backfill: move `payload.metadata.managed_auth_origin_status` and
     `payload.metadata.native_standby_workload_identity_status` into `status`,
     delete them from `metadata`, and seed `config_schema_ref` +
     `project_config_schemas` from the current authored config.
2. `projectConfigSchemaRef(payload)` content hash.
3. Rework `ProjectsStore.Upsert`: transactional — mint the immutable
   `project_config_schemas` row (`ON CONFLICT DO NOTHING`), move the
   `config_schema_ref` pointer, replace authored `payload`, and **leave the
   `status` column untouched**.
4. Reconciler status setters write the `status` column only. Authored-config
   setters (`SetTestEnvironmentCount`, TTL setters, slot-array strip) mutate
   `payload` and re-version.
5. Read merge in `scanProjectRow` so API/Frontend shape is unchanged.
6. Observability: structured log + counter on each config-version transition
   (`project`, prev→new `config_schema_ref`), per `docs/observability.md`.
7. Guard tests: a register that omits status preserves status; a full register
   replaces authored config and mints exactly one new version; re-registering
   identical config is a no-op version (same `schema_ref`).

After Stage 1 a config write can no longer destroy reconciled status, every
config write is recoverable/auditable, and the substrate for declarative sync
exists. It does **not** yet prevent an authored-config field (like
`test_slot_hot_swap`) from being dropped by a partial register — that requires a
complete authored source, which is Stage 2.

### Stage 2 — Declarative project config as import/sync input

1. `.glimmung/project.yaml` in each project's own repo (for glimmung, the
   glimmung repo). The complete authored-config document; the README "dogfood
   metadata" becomes real and checked-in.
2. `GET /v1/projects/{project}/upstream` (drift) and
   `POST /v1/projects/{project}/sync` (apply), mirroring the workflow routes and
   `workflowsInSync` / `fetchUpstreamResult`. Sync replaces authored config from
   the file (safe — status is a separate column) and mints a version.
3. The repo file becomes the reviewable source of truth; Postgres stays the
   runtime contract. A partial register can no longer be the source of authored
   config drift because the file is complete by construction.

### Stage 3 — Reconcile + restore

1. Add `.glimmung/project.yaml` to the glimmung repo carrying the
   `test_slot_hot_swap` block (static `frontend/dist →
   /var/run/glimmung-static-override`; backend supervisor `go build … →
   /var/run/glimmung-hot/glimmung`, health `/healthz`) and sync it in through
   the new durable path.
2. CI reconcile on merge to main so the file and the durable row cannot drift,
   with drift surfaced via the upstream endpoint.

## Cross-repo note

The agent-facing MCP tools (`register_workflow`, `sync_workflow`,
`check_workflow_updates`, …) live in the tank-operator repo and wrap the
glimmung HTTP routes. Full parity for projects means adding matching
`*_project` sync/upstream MCP tools there in Stage 2/3. The glimmung-side HTTP
routes are usable without the MCP wrappers.

## Migration safety

`project_config_schemas`, the new columns, and the backfill are additive and
idempotent. The existing `projects` row is never dropped or replaced by the
migration — it is read, split, and rewritten in place under the same advisory
lock the rest of `RunMigrations` uses. No old interleaved-layout read path
survives the backfill (migration-policy compliant).
</content>
</invoke>

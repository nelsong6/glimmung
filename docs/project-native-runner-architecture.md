# Project-Native Runner Architecture

Status: initial prototype implemented in Glimmung.

This document records the product boundary for Glimmung-managed project
workflows. It follows the repository policies in
[`migration-policy.md`](migration-policy.md),
[`workflow-inspiration.md`](workflow-inspiration.md), and
[`quality-timeframes.md`](quality-timeframes.md).

## Decision

Glimmung owns project workflow schemas, native runner execution, callback
protocol, durable state, output storage, cleanup semantics, and run history.
Consumer repositories remain normal source repositories. They do not need
`.glimmung/workflows/*` files, repo-owned callback libraries, or app-specific
Glimmung runner images by default.

Project-specific work is expressed as Glimmung project configuration and inline
step commands. Those commands may run ordinary repository scripts, tests,
builds, Helm operations, or agent clients, but they must not implement the
Glimmung callback protocol themselves.

The first implementation should favor a coherent central runner over a broad
abstraction layer. Profiles, UI editors, and richer step types can come later,
but the v1 runtime contract should already be owned by Glimmung end to end.

## Non-Goals

- Do not make repo YAML the workflow source of truth.
- Do not require projects to add `.glimmung/workflows` files.
- Do not restore GitHub Actions, GitLab CI, or Azure DevOps as the executor.
- Do not make each repo maintain its own native callback shell library.
- Do not use git as the runtime workflow database.
- Do not reintroduce retired callback routes such as `/native/failed`.

## Core Model

The workflow hierarchy remains:

```text
Project -> Workflow -> Run -> Phase -> Job -> Step
```

Phases and jobs remain durable graph entities. Steps are the visible execution
surface. A step has its own command and the shared runner wraps that command
with start, log, failure, completion, output, and artifact behavior.

The initial project-specific behavior model is:

1. Glimmung supplies lifecycle primitives.
2. Project workflows store inline hook commands in the database.
3. Hook commands can call normal repo commands or scripts.
4. Hook commands do not call Glimmung callback APIs directly.

Inline command text is acceptable for the prototype. Better editing surfaces
can be added later through the dashboard, MCP tools, or admin APIs.

## Runner

Use one broad Glimmung-owned runner image for now. It should include the common
tooling current project workflows need, such as:

- shell utilities, `git`, `gh`, `curl`, `jq`
- Go, Node, Python
- `kubectl`, Helm, Azure CLI
- browser/Playwright dependencies
- Codex and Claude clients
- the Glimmung runner CLI/helper

The runner owns:

- callback authentication
- event emission
- step wrapping
- completion callbacks
- failure reporting through `/native/completed`
- output persistence
- artifact upload
- checkout token handling
- standard labels/metadata

Per-project runner images may exist later as explicit profiles, but they are
not the default architecture.

## Steps

For v1, use a minimal typed schema:

- each step has a `type`
- only `type: "run"` needs to execute initially
- reserve future step types such as `agent`, `artifact_upload`, `output_set`,
  and `helm_deploy`

Each `run` step has inline command text. The runner executes each step in a
fresh shell/process with a shared job workspace/filesystem. The default working
directory is `/workspace`; job-level and step-level overrides are allowed.

Default failure behavior is fail-fast. A non-zero step exits the job, records
the failed step, and reports the job through the normal native completion
contract. `allow_failure` can be added later when a concrete workflow needs it.

Shell default should be pragmatic: `bash` with strict settings. The exact
invocation can be refined in implementation, but shell choice must be explicit
and overrideable.

## Checkout

Checkout is a built-in Glimmung job capability. Individual workflow commands
should not mint GitHub tokens and run ad hoc clone logic.

Each job declares a source policy. A job may:

- check out the project default branch resolved at run start
- check out a fixed ref or SHA
- check out a ref from prior phase output, such as an implementation branch
- skip checkout entirely

The default workspace layout is repo-named:

```text
/workspace/<repo-name>
```

Examples:

```text
/workspace/ambience
/workspace/tank-operator
/workspace/glimmung
```

A job has one primary checkout plus optional extra checkouts for future
multi-repo workflows.

## Outputs

Outputs are first-class durable data in Glimmung. Artifacts are files in
artifact storage. The database stores structured output values and artifact
references, not large blobs.

Steps set phase outputs directly. There is no separate job-output or
phase-output mapping layer in v1. A step can set:

```text
validation_url
image_tag
branch_name
verification
screenshots[]
```

Those values are persisted in the run state immediately with provenance:

- project
- run
- cycle/attempt
- phase
- job
- step
- timestamp
- phase/job/step status

Duplicate output keys in the same phase attempt are rejected by default.

Phase outputs are visible as soon as a step sets them. Visibility is not the
same as downstream eligibility: normal later phases still run only after their
dependency phase succeeds. Outputs from failed phases remain visible for humans
and diagnostics, with provenance/status metadata.

Later non-cleanup phases consume only successful dependency phase outputs.
Cleanup should not depend on unique phase outputs by default.

## Artifacts

Large files belong in artifact storage. Examples:

- screenshots
- Playwright traces
- logs too large for event chunks
- generated reports
- build artifacts that need review

Steps can upload artifacts through the Glimmung runner helper. The helper
stores bytes in artifact storage and writes structured artifact refs into the
run state. Later UI and report surfaces should render artifact refs from the
database rather than scraping logs.

## Cleanup

Cleanup should rely on Glimmung-owned context, labels, and metadata, not on
project-specific phase outputs.

Resources that Glimmung may clean up or display must carry required
Glimmung-owned labels/metadata, such as:

```text
glimmung.project
glimmung.run_id
glimmung.run_ref
glimmung.workflow
glimmung.phase
glimmung.job_id
glimmung.lease_ref
```

Project-specific names are allowed, but required selectors make cleanup
independent of whether an earlier step finished and emitted outputs.

## Agent Steps

For v1, agent execution can be an inline command that invokes Codex or Claude.
The schema should reserve a future `type: "agent"` step so agent execution can
become first-class once the runner contract is stable.

The broad shared runner should include both Codex and Claude clients for now.
Workflow/job config selects which one to invoke.

## Current Drift To Remove

The current registered workflows use three different runner patterns:

- Ambience uses an app-specific runner image with callback shell scripts baked
  into the image.
- Tank uses a generic image, clones the repo at runtime, and runs repo callback
  scripts.
- Glimmung embeds callback shell directly in the database workflow row.

The prototype should migrate away from all three as settled patterns. The
target is one Glimmung-owned runner contract with project-native step commands.

## Prototype Target

The first coherent prototype should:

1. Define the v1 job/step schema fields for checkout, working directory, shell,
   run command, and output declarations.
2. Build a shared Glimmung runner image/entrypoint.
3. Move callback handling into the runner.
4. Execute each step command under runner control.
5. Persist step events, logs, outputs, artifacts, and job completion through
   Glimmung-owned APIs.
6. Validate duplicate output rejection.
7. Validate failure completion through `/native/completed`.
8. Migrate one project workflow, preferably Ambience, onto the shared runner.
9. Re-run a real issue workflow and verify the run graph, outputs, cleanup, and
   report surfaces from durable state.

## Implemented Slice

The current prototype adds a managed `k8s_job` path owned by Glimmung:

- `jobs[].managed: true` selects the shared runner entrypoint.
- `jobs[].checkout` and `jobs[].extra_checkouts[]` describe built-in GitHub
  checkout.
- `jobs[].working_directory` and `steps[].working_directory` control command
  cwd.
- `jobs[].shell` and `steps[].shell` override the default strict Bash shell.
- `steps[].type` defaults to `run`; only `run` executes initially.
- `steps[].run` stores the inline command text.
- `steps[].env` adds step-local environment.

The launcher passes the durable job spec to the runner in
`GLIMMUNG_RUNNER_JOB_SPEC`, and the runner posts step events, log events,
`phase_output_set` events, and terminal job completion through Glimmung native
callback APIs.

Step commands set phase outputs by appending either `key=value` lines or JSON
objects to `$GLIMMUNG_OUTPUT_FILE`. The runner rejects duplicate keys locally
and the server persists `phase_output_set` events into the attempt's
`phase_outputs` immediately. The current persisted output value shape remains
`map[string]string`; richer typed output records and artifact refs are the next
state-model increment.

## Done Standard

The prototype is not done when a happy-path job runs. It is done when:

- no repo callback library is needed for the migrated project
- no retired callback route is required
- failure before the first step is visible as a job failure, not a dispatch
  timeout
- outputs are visible immediately with provenance
- duplicate phase output writes fail clearly
- cleanup can find resources by Glimmung labels/metadata
- the workflow schema and runner version used by a run are durable
- tests cover the runner contract at the Glimmung boundary

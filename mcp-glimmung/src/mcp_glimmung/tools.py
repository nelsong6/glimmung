"""mcp-glimmung tools — typed wrappers over glimmung's HTTP API.

Read surface plus session-safe mutations. Lease and webhook endpoints stay
unexposed — those are runner / orchestrator concerns, not session concerns.
"""

from typing import Any

from mcp.server.fastmcp import FastMCP

from .browser_inspector import inspect_url
from .glimmung_client import GlimmungClient


def register_tools(mcp: FastMCP, client: GlimmungClient) -> None:
    @mcp.tool()
    def get_issue(repo_owner: str, repo_name: str, issue_number: int) -> dict[str, Any]:
        """Get a Glimmung issue by GitHub repository owner/name and GitHub issue number.

        Use to inspect a GitHub-backed Glimmung Issue before patching it,
        dispatching a run, reviewing locks, or finding its Glimmung-native id.
        Returns title, body, state, labels, last_run_id, last_run_state,
        issue_lock_held, plus the glimmung `id` and `project` (use those
        for patch_issue if you intend to mutate)."""
        return client.get(f"/v1/issues/{repo_owner}/{repo_name}/{issue_number}")

    @mcp.tool()
    def get_issue_by_id(project: str, issue_id: str) -> dict[str, Any]:
        """Get a Glimmung issue by project and Glimmung issue id.

        Use this for glimmung-native issues that have no GitHub counterpart."""
        return client.get(f"/v1/issues/by-id/{project}/{issue_id}")

    @mcp.tool()
    def get_issue_graph(repo_owner: str, repo_name: str, issue_number: int) -> dict[str, Any]:
        """Get the Glimmung lineage graph for one issue, including runs, phases, reports, and signals.

        Lineage graph for one Issue: every Run dispatched against it,
        every PhaseAttempt inside each Run, the Report(s) opened, and the
        Signals fed back."""
        return client.get(f"/v1/issues/{repo_owner}/{repo_name}/{issue_number}/graph")

    @mcp.tool()
    def get_native_run_events(
        project: str,
        run_id: str,
        attempt_index: int | None = None,
        job_id: str | None = None,
        limit: int | None = 200,
    ) -> dict[str, Any]:
        """Read hot native k8s_job step/log events for a Glimmung run.

        Use with graph attempt metadata (`phase_kind == "k8s_job"`) to inspect
        the ordered runner event stream. `attempt_index` narrows to one
        PhaseAttempt, `job_id` narrows to one native job, and `limit` caps the
        returned hot rows. Older archived attempts expose `archive_url` in the
        response when hot rows have been pruned or archived.
        """
        params = {
            "attempt_index": attempt_index,
            "job_id": job_id,
            "limit": limit,
        }
        return client.get(
            f"/v1/runs/{project}/{run_id}/native/events",
            params={k: v for k, v in params.items() if v is not None},
        )

    @mcp.tool()
    def list_issues(
        project: str | None = None,
        repo: str | None = None,
        limit: int | None = 50,
    ) -> list[dict[str, Any]]:
        """List Glimmung issues across projects, optionally filtered.

        Use to discover issue ids, project names, GitHub-backed issues, and
        glimmung-native issues before dispatching or patching. `project`
        filters by Glimmung project name, `repo` filters GitHub-backed
        issues by owner/name, and `limit` caps returned rows.
        """
        params = {
            "project": project,
            "repo": repo,
            "limit": limit,
        }
        return client.get(
            "/v1/issues",
            params={k: v for k, v in params.items() if v is not None},
        )

    @mcp.tool()
    def get_report(repo_owner: str, repo_name: str, pr_number: int) -> dict[str, Any]:
        """Get a Glimmung report by GitHub repository owner/name and pull request (PR) number.

        Use to inspect PR-backed report state, linked issue/run ids, branch,
        merge state, and report metadata.
        """
        return client.get(f"/v1/reports/{repo_owner}/{repo_name}/{pr_number}")

    @mcp.tool()
    def get_report_by_id(project: str, report_id: str) -> dict[str, Any]:
        """Get a Glimmung report by project and canonical Glimmung report id."""
        return client.get(f"/v1/reports/by-id/{project}/{report_id}")

    @mcp.tool()
    def list_report_versions(
        project: str,
        report_id: str,
        limit: int | None = 50,
    ) -> list[dict[str, Any]]:
        """List immutable Glimmung report snapshots for one report, newest first.

        `limit` caps returned snapshots.
        """
        params = {"limit": limit}
        return client.get(
            f"/v1/reports/by-id/{project}/{report_id}/versions",
            params={k: v for k, v in params.items() if v is not None},
        )

    @mcp.tool()
    def get_report_version(project: str, report_id: str, version: int) -> dict[str, Any]:
        """Get one immutable Glimmung report snapshot by integer version."""
        return client.get(f"/v1/reports/by-id/{project}/{report_id}/versions/{version}")

    @mcp.tool()
    def list_reports(
        project: str | None = None,
        repo: str | None = None,
        state: str | None = None,
        limit: int | None = 50,
    ) -> list[dict[str, Any]]:
        """List Glimmung reports across projects, optionally filtered.

        Use to find reports associated with GitHub pull requests, branches,
        issues, or runs. `project` filters by Glimmung project name, `repo`
        filters by GitHub owner/name, `state` filters by report state
        (ready, needs_review, failed, closed, merged), and `limit` caps
        returned rows.
        """
        params = {
            "project": project,
            "repo": repo,
            "state": state,
            "limit": limit,
        }
        return client.get(
            "/v1/reports",
            params={k: v for k, v in params.items() if v is not None},
        )

    @mcp.tool()
    def get_state() -> dict[str, Any]:
        """Get Glimmung control-plane state: hosts, leases, locks, and recent runs.

        Snapshot of hosts, leases, and recent runs. Same shape the
        /v1/events SSE feed pushes; this returns the latest snapshot
        point-in-time."""
        return client.get("/v1/state")

    @mcp.tool()
    def list_projects(
        name: str | None = None,
        github_repo: str | None = None,
        limit: int | None = 50,
    ) -> list[dict[str, Any]]:
        """List configured Glimmung projects and their GitHub repository bindings.

        `name`, `github_repo`, and `limit` narrow large project lists.
        """
        params = {
            "name": name,
            "github_repo": github_repo,
            "limit": limit,
        }
        return client.get(
            "/v1/projects",
            params={k: v for k, v in params.items() if v is not None},
        )

    @mcp.tool()
    def register_project(
        name: str,
        github_repo: str,
        metadata: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Create or update a Glimmung project mapped to a GitHub repository.

        Upsert a glimmung Project. Use this when standing up a new
        repository in the control plane before registering workflows or
        native issues. `github_repo` is the canonical "owner/repo" slug;
        `metadata` is an optional free-form bag preserved on the Project."""
        return client.post(
            "/v1/projects",
            json={
                "name": name,
                "github_repo": github_repo,
                "metadata": metadata or {},
            },
        )

    @mcp.tool()
    def register_host(
        name: str,
        capabilities: dict[str, Any] | None = None,
        drained: bool = False,
    ) -> dict[str, Any]:
        """Create or update a Glimmung runner host and its dispatch capabilities.

        Admin/bootstrap tool: use it
        to advertise a worker slot and its dispatch `capabilities`.
        `drained=True` keeps the host registered but ineligible for new
        leases."""
        return client.post(
            "/v1/hosts",
            json={
                "name": name,
                "capabilities": capabilities or {},
                "drained": drained,
            },
        )

    @mcp.tool()
    def list_workflows(
        project: str | None = None,
        name: str | None = None,
        trigger_label: str | None = None,
        limit: int | None = 50,
    ) -> list[dict[str, Any]]:
        """List Glimmung workflow definitions across projects, optionally filtered.

        Use to discover workflow names, phase shapes, trigger labels, PR
        settings, budgets, and requirements before patching or registering.
        `project`, `name`, `trigger_label`, and `limit` narrow large lists.
        """
        params = {
            "project": project,
            "name": name,
            "trigger_label": trigger_label,
            "limit": limit,
        }
        return client.get(
            "/v1/workflows",
            params={k: v for k, v in params.items() if v is not None},
        )

    @mcp.tool()
    def create_playbook(
        project: str,
        title: str,
        description: str = "",
        entries: list[dict[str, Any]] | None = None,
        concurrency_limit: int | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Create a draft Glimmung Playbook for coordinating multiple issues.

        Storage-only surface for preserving operator intent across a batch
        of related issues. Entries are not enqueued or dispatched by this
        v1 call; future playbook run semantics will consume the stored
        entry specs. Each entry should include `id` and an `issue` object
        with `title`, optional `body`, `labels`, `workflow`, and metadata.
        """
        payload: dict[str, Any] = {
            "project": project,
            "title": title,
            "description": description,
            "entries": entries or [],
            "metadata": metadata or {},
        }
        if concurrency_limit is not None:
            payload["concurrency_limit"] = concurrency_limit
        return client.post("/v1/playbooks", json=payload)

    @mcp.tool()
    def list_playbooks(
        project: str | None = None,
        state: str | None = None,
        limit: int | None = 50,
    ) -> list[dict[str, Any]]:
        """List Glimmung Playbooks, optionally filtered by project or state."""
        params = {
            "project": project,
            "state": state,
            "limit": limit,
        }
        params = {k: v for k, v in params.items() if v is not None}
        return client.get("/v1/playbooks", params=params)

    @mcp.tool()
    def get_playbook(project: str, playbook_id: str) -> dict[str, Any]:
        """Get one Glimmung Playbook by project and playbook id."""
        return client.get(f"/v1/playbooks/{project}/{playbook_id}")

    @mcp.tool()
    def run_playbook(project: str, playbook_id: str) -> dict[str, Any]:
        """Start or advance a Glimmung Playbook.

        The server mints and dispatches ready entries up to the playbook's
        concurrency limit, refreshes linked run outcomes, and records created
        issue/run ids. Re-run this after entries complete to advance
        dependency-gated work.
        """
        return client.post(f"/v1/playbooks/{project}/{playbook_id}/run")

    @mcp.tool()
    def inspect_browser_url(
        url: str,
        viewport: dict[str, int] | None = None,
        wait_ms: int = 2000,
        timeout_ms: int = 30000,
        screenshot: bool = True,
        full_page: bool = True,
        capture_accessibility: bool = False,
        capture_console: bool = True,
        capture_network: bool = True,
        max_elements: int = 80,
        body_text_limit: int = 4000,
    ) -> dict[str, Any]:
        """Inspect a live URL with Chromium and return browser-state JSON.

        Use for validation URLs when a static screenshot is not enough:
        the tool waits for the rendered page, captures final URL/status,
        title/body summary, interesting DOM elements with selectors and
        bounds, console/page errors, failed requests and HTTP >= 400
        responses, optional accessibility tree, optional screenshot path,
        and canvas nonblank sampling data.
        """
        return inspect_url(
            url=url,
            viewport=viewport,
            wait_ms=wait_ms,
            timeout_ms=timeout_ms,
            screenshot=screenshot,
            full_page=full_page,
            capture_accessibility=capture_accessibility,
            capture_console=capture_console,
            capture_network=capture_network,
            max_elements=max_elements,
            body_text_limit=body_text_limit,
        )

    @mcp.tool()
    def register_workflow(
        project: str,
        name: str,
        phases: list[dict[str, Any]],
        pr: dict[str, Any] | None = None,
        budget: dict[str, Any] | None = None,
        trigger_label: str = "issue-agent",
        default_requirements: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Create or replace a Glimmung workflow registration, including phases, PR policy, budget, and triggers.

        Upsert a Workflow (create or replace). Use this for the
        structural fields `patch_workflow` won't touch: phase shape,
        declared inputs/outputs, recycle policy, trigger label, default
        requirements. Idempotent — re-registering the same shape is a
        no-op replace, so consumer-migration scripts can run repeatedly
        without piling up state. The server preserves `createdAt` on
        replace and validates cross-phase input refs at registration
        time, so a typo in `${{ phases.NAME.outputs.KEY }}` surfaces
        before it can corrupt a run.

        `phases` is a list of PhaseSpec dicts; `gha_dispatch` phases
        declare `workflow_filename`, while `k8s_job` phases declare
        `jobs` with app-owned `steps`. Optional fields: `kind` (default
        "gha_dispatch"), `workflow_ref`, `inputs`, `outputs`,
        `requirements`, `verify`, `recycle_policy`. `pr` is a
        PrPrimitiveSpec dict (`enabled`, `recycle_policy`); omit for the
        default disabled primitive. `budget` is `{"total": float}`
        (default 25.0). Pair with `patch_workflow` for live rollout-knob
        flips that don't need a full re-register."""
        payload: dict[str, Any] = {
            "project": project,
            "name": name,
            "phases": phases,
            "trigger_label": trigger_label,
        }
        if pr is not None:
            payload["pr"] = pr
        if budget is not None:
            payload["budget"] = budget
        if default_requirements is not None:
            payload["default_requirements"] = default_requirements
        return client.post("/v1/workflows", json=payload)

    @mcp.tool()
    def patch_workflow(
        project: str,
        name: str,
        pr_enabled: bool | None = None,
        budget_total: float | None = None,
    ) -> dict[str, Any]:
        """Patch Glimmung workflow rollout knobs such as PR creation and budget.

        Patch a Workflow's live rollout knobs (`pr.enabled`, `budget.total`).
        All fields optional — None means "don't change". Structural fields
        (phases, recycle policy) are not patchable here; re-run
        register_workflow for those.

        `name` is the workflow's canonical handle (e.g. "agent-run"); pair
        it with `project` (the partition key)."""
        payload: dict[str, Any] = {}
        if pr_enabled is not None:
            payload["pr_enabled"] = pr_enabled
        if budget_total is not None:
            payload["budget_total"] = budget_total
        return client.patch(f"/v1/workflows/{project}/{name}", json=payload)

    @mcp.tool()
    def patch_issue(
        project: str,
        issue_id: str,
        title: str | None = None,
        body: str | None = None,
        labels: list[str] | None = None,
        state: str | None = None,
    ) -> dict[str, Any]:
        """Patch or update a Glimmung issue title, body, labels, or state.

        All fields optional — None means \"don't change\".
        Pass an empty string to actually clear `body`, or an empty list to
        clear `labels`. `state` is \"open\" or \"closed\"; transitions route
        through close_issue / reopen_issue so closed_at is stamped
        consistently."""
        payload: dict[str, Any] = {}
        if title is not None:
            payload["title"] = title
        if body is not None:
            payload["body"] = body
        if labels is not None:
            payload["labels"] = labels
        if state is not None:
            payload["state"] = state
        return client.patch(f"/v1/issues/by-id/{project}/{issue_id}", json=payload)

    @mcp.tool()
    def create_issue(
        project: str,
        title: str,
        body: str = "",
        labels: list[str] | None = None,
    ) -> dict[str, Any]:
        """Create a Glimmung-native issue without creating a GitHub issue.

        Mint a glimmung-native Issue. No GitHub issue is created; the
        returned `id` is the canonical handle for detail, comments, and
        dispatch APIs."""
        return client.post(
            "/v1/issues",
            json={
                "project": project,
                "title": title,
                "body": body,
                "labels": labels or [],
            },
        )

    @mcp.tool()
    def enqueue_signal(
        target_type: str,
        target_repo: str,
        target_id: str,
        source: str = "glimmung_ui",
        payload: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Enqueue a Glimmung signal for an issue, pull request (PR), or run.

        Use to feed actionable feedback or trigger details into the drain loop.
        Common values:
        `target_type` is `pr`, `issue`, or `run`; `target_repo` is the
        repository slug / partition key; `target_id` is a PR number,
        issue number, or run id. Put the actionable feedback or trigger
        detail in `payload`."""
        return client.post(
            "/v1/signals",
            json={
                "target_type": target_type,
                "target_repo": target_repo,
                "target_id": target_id,
                "source": source,
                "payload": payload or {},
            },
        )

    @mcp.tool()
    def replay_run_decision(
        project: str,
        run_id: str,
        synthetic_completion: dict[str, Any],
        override_workflow: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Replay the Glimmung run decision engine without writes or dispatch.

        Use to debug workflow verification, phase outputs, retry/recycle
        decisions, PR opening decisions, and registration fixes. This is a
        pure-function replay with no Cosmos writes and no GHA dispatch.
        Returns the decision the
        engine *would* make for `synthetic_completion`, plus a next-action
        hint (which phase would advance, which recycle target would fire,
        what abort comment would be posted).

        Smoke-test substrate from glimmung#111: catches verify=true→false-
        class registration bugs at zero cost. The classic case — registered
        verify=true, /completed callback omits the verification field —
        used to cost ~20 min of agent runtime per iteration to surface;
        replay returns ABORT_MALFORMED in milliseconds.

        `synthetic_completion` mirrors the live `/completed` callback body:
        `{conclusion: "success"|"failure"|..., verification: dict|null,
        phase_outputs: dict|null}`. Copy-paste a real completion and tweak
        fields to ask "what if?".

        `override_workflow` is optional. When set, the replay uses the
        provided shape instead of the live registration — useful for
        previewing a registration fix before applying it. Shape:
        `{phases: [...PhaseSpec...], pr: {...}, budget: {...}}`. Cross-
        phase input refs are validated; a typo in
        `${{ phases.X.outputs.Y }}` 422s with the same error
        register_workflow returns.

        Returns: `{decision, applied_to_phase, applied_to_attempt_index,
        abort_reason?, would_advance_to_phase?, would_open_pr,
        would_retry_target_phase?, cumulative_cost_usd_after,
        attempts_in_phase_after, workflow_source}`. `workflow_source` is
        "registered" or "override" so the verdict's basis is unambiguous.
        """
        payload: dict[str, Any] = {"synthetic_completion": synthetic_completion}
        if override_workflow is not None:
            payload["override_workflow"] = override_workflow
        return client.post(
            f"/v1/runs/{project}/{run_id}/replay",
            json=payload,
        )

    @mcp.tool()
    def resume_run(
        project: str,
        run_id: str,
        entrypoint_phase: str,
        entrypoint_job_id: str | None = None,
        entrypoint_step_slug: str | None = None,
        input_overrides: dict[str, str] | None = None,
        artifact_refs: dict[str, str] | None = None,
        context: dict[str, Any] | None = None,
        trigger_source: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Resume a Glimmung run by spawning a new run from a terminal prior run at a chosen phase.

        Use to re-drive failed or aborted multi-phase workflows without
        re-running successful earlier phases. Picks up at `entrypoint_phase`.
        All phases declared earlier in the workflow
        order are auto-skipped — each gets a synthesized PhaseAttempt
        with `phase_outputs` carried forward from the prior Run's same-
        named phase, and the multi-phase substitution path feeds those
        outputs into the entrypoint phase's `workflow_dispatch.inputs`.

        The motivating case from glimmung#111: an `agent-execute`
        attempt aborted on a `verify=true→false` registration mismatch.
        After fixing the registration, `resume_run(... entrypoint_phase=
        "agent-execute")` re-uses `env-prep`'s captured outputs and
        dispatches a fresh `agent-execute` attempt without re-running
        env-prep — saves ~20 minutes of agent runtime per iteration.

        Refuses with state=`prior_in_progress` if the prior Run is
        still IN_PROGRESS (would race the in-flight dispatch's lock).
        Refuses with state=`already_running` if the issue's lock is
        currently held by a different Run (caller must abort the
        conflicting run first).

        For native `k8s_job` phases, set `entrypoint_job_id` and
        `entrypoint_step_slug` to restart at a specific app-owned step
        boundary. Earlier jobs/steps are pre-marked skipped on the new
        run, and the boundary plus `artifact_refs` / `context` are exposed
        to the native pod via GLIMMUNG_* env vars. `input_overrides`
        replaces substituted phase input values for the resumed attempt.

        `trigger_source` is recorded on the new Run for observability;
        the server adds `kind: resume_via_mcp` and `resumed_from_run_id`
        if not provided.

        Returns: `{state, new_run_id, prior_run_id, lease_id?, host?,
        issue_lock_holder_id, detail?}`. State values include
        `dispatched`, `pending`, `dispatch_failed`, `prior_in_progress`,
        `already_running`, `phase_invalid`, `outputs_missing`,
        `prior_missing`, `workflow_missing`. The HTTP layer maps the
        validation states to 4xx; happy paths return state in the body.
        """
        ts: dict[str, Any] = {"kind": "resume_via_mcp", "resumed_from_run_id": run_id}
        if trigger_source:
            ts.update(trigger_source)
        payload: dict[str, Any] = {
            "entrypoint_phase": entrypoint_phase,
            "trigger_source": ts,
        }
        for k, v in {
            "entrypoint_job_id": entrypoint_job_id,
            "entrypoint_step_slug": entrypoint_step_slug,
            "input_overrides": input_overrides,
            "artifact_refs": artifact_refs,
            "context": context,
        }.items():
            if v:
                payload[k] = v
        return client.post(
            f"/v1/runs/{project}/{run_id}/resume",
            json=payload,
        )

    @mcp.tool()
    def abort_run(
        project: str,
        run_id: str,
        reason: str = "aborted_via_mcp",
    ) -> dict[str, Any]:
        """Abort a Glimmung run and release issue locks or run locks.

        Use for orphaned, stuck, or intentionally cancelled runs. Flips a Run
        from in_progress to aborted and releases any locks it was holding.
        Use when a Run has no lease or workflow_run_id and `cancel_lease`
        can't grip onto it.

        Idempotent — calling twice returns `state: already_terminal` the
        second time. If the Run has a workflow_run_id, a GH cancel is
        POSTed best-effort; `gh_run_cancelled` records the outcome
        (`None` if no GH dispatch was attempted)."""
        return client.post(
            f"/v1/runs/{project}/{run_id}/abort",
            params={"reason": reason},
        )

    @mcp.tool()
    def dispatch_run(
        issue_id: str,
        project: str | None = None,
        workflow: str | None = None,
    ) -> dict[str, Any]:
        """Dispatch a Glimmung agent run for an issue and workflow.

        Use to manually start or re-drive a run after a fix lands. Same path
        the dashboard's re-dispatch button takes: claims a host that matches
        the workflow's requirements, creates a Run, and fires the
        workflow_dispatch event or the first phase of a multi-phase workflow.

        `issue_id` is the glimmung ULID (find via `get_issue` →
        `id`). `project` is optional — the server resolves it from
        the Issue doc when omitted. `workflow` is optional and only
        needed if the project has more than one workflow registered.

        Returns the dispatch result: created Run id, claimed lease id,
        host, and the GHA workflow_dispatch outcome."""
        payload: dict[str, Any] = {"issue_id": issue_id}
        if project is not None:
            payload["project"] = project
        if workflow is not None:
            payload["workflow"] = workflow
        return client.post("/v1/runs/dispatch", json=payload)

    @mcp.tool()
    def create_report(
        project: str,
        repo: str,
        number: int,
        title: str,
        branch: str,
        body: str = "",
        base_ref: str = "main",
        head_sha: str = "",
        html_url: str = "",
        linked_issue_id: str | None = None,
        linked_run_id: str | None = None,
    ) -> dict[str, Any]:
        """Create or register a Glimmung report for an existing GitHub pull request (PR).

        Use after creating a GitHub PR to link the PR back to Glimmung issue/run
        state. Idempotent on `(repo, number)` and can attach `linked_issue_id` /
        `linked_run_id` during either create or re-registration."""
        payload: dict[str, Any] = {
            "project": project,
            "repo": repo,
            "number": number,
            "title": title,
            "branch": branch,
            "body": body,
            "base_ref": base_ref,
            "head_sha": head_sha,
            "html_url": html_url,
        }
        if linked_issue_id is not None:
            payload["linked_issue_id"] = linked_issue_id
        if linked_run_id is not None:
            payload["linked_run_id"] = linked_run_id
        return client.post("/v1/reports", json=payload)

    @mcp.tool()
    def create_report_version(
        project: str,
        report_id: str,
        title: str,
        body: str = "",
        state: str = "ready",
        linked_run_id: str | None = None,
        github_repo: str | None = None,
        github_pr_number: int | None = None,
        github_html_url: str | None = None,
        version: int | None = None,
    ) -> dict[str, Any]:
        """Create an immutable snapshot for a Glimmung report.

        Use after materially changing or syndicating a report to preserve the
        exact title/body/state and GitHub linkage observed at that point in
        time. If `version` is omitted, the server assigns the next integer.
        """
        payload: dict[str, Any] = {
            "title": title,
            "body": body,
            "state": state,
        }
        for k, v in {
            "linked_run_id": linked_run_id,
            "github_repo": github_repo,
            "github_pr_number": github_pr_number,
            "github_html_url": github_html_url,
            "version": version,
        }.items():
            if v is not None:
                payload[k] = v
        return client.post(
            f"/v1/reports/by-id/{project}/{report_id}/versions",
            json=payload,
        )

    @mcp.tool()
    def patch_report(
        project: str,
        report_id: str,
        title: str | None = None,
        body: str | None = None,
        branch: str | None = None,
        base_ref: str | None = None,
        head_sha: str | None = None,
        html_url: str | None = None,
        linked_issue_id: str | None = None,
        linked_run_id: str | None = None,
        state: str | None = None,
        merged_by: str | None = None,
    ) -> dict[str, Any]:
        """Patch or update a Glimmung report linked to a GitHub pull request (PR).

        Use to update report title, body, branch, base ref, head SHA, URL,
        linked issue/run ids, state, or merged_by. All fields optional; None
        means don't change.
        """
        payload: dict[str, Any] = {}
        for k, v in {
            "title": title,
            "body": body,
            "branch": branch,
            "base_ref": base_ref,
            "head_sha": head_sha,
            "html_url": html_url,
            "linked_issue_id": linked_issue_id,
            "linked_run_id": linked_run_id,
            "state": state,
            "merged_by": merged_by,
        }.items():
            if v is not None:
                payload[k] = v
        return client.patch(f"/v1/reports/by-id/{project}/{report_id}", json=payload)

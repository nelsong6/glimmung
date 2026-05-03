"""mcp-glimmung tools — typed wrappers over glimmung's HTTP API.

Read surface plus session-safe mutations. Lease and webhook endpoints stay
unexposed — those are runner / orchestrator concerns, not session concerns.
"""

from typing import Any

from mcp.server.fastmcp import FastMCP

from .glimmung_client import GlimmungClient


def register_tools(mcp: FastMCP, client: GlimmungClient) -> None:
    @mcp.tool()
    def get_issue(repo_owner: str, repo_name: str, issue_number: int) -> dict[str, Any]:
        """Detail view of a glimmung Issue keyed by GitHub repo coords.
        Returns title, body, state, labels, last_run_id, last_run_state,
        issue_lock_held, plus the glimmung `id` and `project` (use those
        for patch_issue if you intend to mutate)."""
        return client.get(f"/v1/issues/{repo_owner}/{repo_name}/{issue_number}")

    @mcp.tool()
    def get_issue_by_id(project: str, issue_id: str) -> dict[str, Any]:
        """Detail view of a glimmung Issue keyed by its glimmung id. Use
        this for glimmung-native issues that have no GitHub counterpart."""
        return client.get(f"/v1/issues/by-id/{project}/{issue_id}")

    @mcp.tool()
    def get_issue_graph(repo_owner: str, repo_name: str, issue_number: int) -> dict[str, Any]:
        """Lineage graph for one Issue: every Run dispatched against it,
        every PhaseAttempt inside each Run, the PR(s) opened, and the
        Signals fed back."""
        return client.get(f"/v1/issues/{repo_owner}/{repo_name}/{issue_number}/graph")

    @mcp.tool()
    def list_issues() -> list[dict[str, Any]]:
        """List all glimmung Issues across projects."""
        return client.get("/v1/issues")

    @mcp.tool()
    def get_pr(repo_owner: str, repo_name: str, pr_number: int) -> dict[str, Any]:
        """Detail view of a glimmung PR keyed by GitHub repo coords."""
        return client.get(f"/v1/prs/{repo_owner}/{repo_name}/{pr_number}")

    @mcp.tool()
    def get_pr_by_id(project: str, pr_id: str) -> dict[str, Any]:
        """Detail view of a glimmung PR keyed by its glimmung id."""
        return client.get(f"/v1/prs/by-id/{project}/{pr_id}")

    @mcp.tool()
    def list_prs() -> list[dict[str, Any]]:
        """List all glimmung PRs across projects."""
        return client.get("/v1/prs")

    @mcp.tool()
    def get_state() -> dict[str, Any]:
        """Snapshot of hosts, leases, and recent runs. Same shape the
        /v1/events SSE feed pushes; this returns the latest snapshot
        point-in-time."""
        return client.get("/v1/state")

    @mcp.tool()
    def list_projects() -> list[dict[str, Any]]:
        """List configured glimmung projects."""
        return client.get("/v1/projects")

    @mcp.tool()
    def register_project(
        name: str,
        github_repo: str,
        metadata: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Upsert a glimmung Project. Use this when standing up a new
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
        """Upsert a runner Host. This is an admin/bootstrap tool: use it
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
    def list_workflows() -> list[dict[str, Any]]:
        """List workflow definitions across projects."""
        return client.get("/v1/workflows")

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
        """Upsert a Workflow (create or replace). Use this for the
        structural fields `patch_workflow` won't touch: phase shape,
        declared inputs/outputs, recycle policy, trigger label, default
        requirements. Idempotent — re-registering the same shape is a
        no-op replace, so consumer-migration scripts can run repeatedly
        without piling up state. The server preserves `createdAt` on
        replace and validates cross-phase input refs at registration
        time, so a typo in `${{ phases.NAME.outputs.KEY }}` surfaces
        before it can corrupt a run.

        `phases` is a list of PhaseSpec dicts; each must declare `name`
        and `workflow_filename`. Optional fields: `kind` (default
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
        """Patch a Workflow's live rollout knobs (`pr.enabled`, `budget.total`).
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
        """Patch an Issue. All fields optional — None means \"don't change\".
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
        """Mint a glimmung-native Issue. No GitHub issue is created; the
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
        """Enqueue a Signal for the drain loop. Common values:
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
        """Pure-function replay of the decision engine against a Run, with
        no Cosmos writes and no GHA dispatch. Returns the decision the
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
        trigger_source: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Spawn a new Run from a terminal prior Run, picking up at
        `entrypoint_phase`. All phases declared earlier in the workflow
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
        """Flip a Run from in_progress to aborted and release any locks
        it was holding. Use when a Run is orphaned (no lease, no
        workflow_run_id) and `cancel_lease` can't grip onto it.

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
        """Manually dispatch an agent run for a glimmung Issue. Same path
        the dashboard's re-dispatch button takes: claims a host that
        matches the workflow's requirements, creates a Run, and fires
        the workflow_dispatch (or the first phase of a multi-phase
        workflow). Useful for re-driving a run after a fix lands when
        the original webhook trigger has already been consumed.

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
    def create_pr(
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
        """Register a glimmung PR after the GitHub PR exists. Idempotent
        on `(repo, number)` and can attach `linked_issue_id` /
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
        return client.post("/v1/prs", json=payload)

    @mcp.tool()
    def patch_pr(
        project: str,
        pr_id: str,
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
        """Patch a PR. All fields optional — None means \"don't change\".
        `state` is \"open\", \"closed\", or \"merged\"; \"merged\" requires
        `merged_by`. Closed-vs-merged route to close_pr vs merge_pr
        (different timestamp invariants)."""
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
        return client.patch(f"/v1/prs/by-id/{project}/{pr_id}", json=payload)

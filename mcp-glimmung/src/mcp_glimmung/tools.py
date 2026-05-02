"""mcp-glimmung tools — typed wrappers over glimmung's HTTP API.

Read surface plus the by-id PATCH endpoints and selectively-exposed admin
mutations (`abort_run`). Lease / dispatch / signal / hosts / webhook
endpoints stay unexposed — those are runner / orchestrator concerns, not
session concerns.
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

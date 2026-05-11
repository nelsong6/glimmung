from __future__ import annotations

from fastapi.routing import APIRoute

from glimmung.app import app

Route = tuple[str, str, str]


EXPECTED_API_ROUTES: tuple[Route, ...] = (
    ("GET", "/healthz", "healthz"),
    ("GET", "/v1/config", "public_config"),
    ("GET", "/v1/auth/me", "auth_me"),
    ("GET", "/v1/artifacts/{blob_path:path}", "read_artifact"),
    ("POST", "/v1/lease", "create_lease"),
    ("GET", "/v1/lease-callbacks/{callback_token}", "read_lease_by_callback_token"),
    (
        "POST",
        "/v1/lease-callbacks/{callback_token}/heartbeat",
        "heartbeat_lease_by_callback_token",
    ),
    (
        "POST",
        "/v1/lease-callbacks/{callback_token}/release",
        "release_lease_by_callback_token",
    ),
    ("POST", "/v1/leases/cancel", "cancel_lease_by_ref"),
    ("GET", "/v1/projects/{project}/runs", "list_project_runs"),
    (
        "GET",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/report",
        "get_run_report_by_number",
    ),
    (
        "POST",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/abort",
        "abort_run_by_number",
    ),
    ("POST", "/v1/run-callbacks/{callback_token}/started", "run_started_by_callback_token"),
    ("POST", "/v1/run-callbacks/{callback_token}/aborted", "run_aborted_by_callback_token"),
    (
        "POST",
        "/v1/run-callbacks/{callback_token}/completed",
        "run_completed_by_callback_token",
    ),
    (
        "GET",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events",
        "native_run_events_by_number",
    ),
    (
        "POST",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events",
        "native_run_event_by_number",
    ),
    (
        "POST",
        "/v1/run-callbacks/{callback_token}/native/events",
        "native_run_event_by_callback_token",
    ),
    (
        "GET",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/pod-logs",
        "native_run_pod_logs_by_number",
    ),
    (
        "GET",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/status",
        "native_run_status_by_number",
    ),
    (
        "GET",
        "/v1/run-callbacks/{callback_token}/native/status",
        "native_run_status_by_callback_token",
    ),
    (
        "POST",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/completed",
        "native_run_completed_by_number",
    ),
    (
        "POST",
        "/v1/run-callbacks/{callback_token}/native/completed",
        "native_run_completed_by_callback_token",
    ),
    (
        "POST",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/failed",
        "native_run_failed_by_number",
    ),
    (
        "POST",
        "/v1/run-callbacks/{callback_token}/native/failed",
        "native_run_failed_by_callback_token",
    ),
    (
        "POST",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/github-token",
        "native_github_token_by_number",
    ),
    (
        "POST",
        "/v1/run-callbacks/{callback_token}/native/github-token",
        "native_github_token_by_callback_token",
    ),
    (
        "POST",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/replay",
        "replay_run_decision_by_number",
    ),
    (
        "POST",
        "/v1/projects/{project}/issues/{issue_number}/runs/{run_number}/resume",
        "resume_run_by_number",
    ),
    ("GET", "/v1/state", "state"),
    ("GET", "/v1/events", "events"),
    ("POST", "/v1/projects", "register_project"),
    ("GET", "/v1/projects", "list_projects"),
    (
        "PATCH",
        "/v1/projects/{project}/test-environments/count",
        "scale_project_test_environments",
    ),
    ("POST", "/v1/workflows", "register_workflow"),
    (
        "GET",
        "/v1/projects/{project}/workflows/{name}/upstream",
        "get_workflow_upstream",
    ),
    ("POST", "/v1/projects/{project}/workflows/{name}/sync", "sync_workflow"),
    ("GET", "/v1/workflows", "list_workflows"),
    ("DELETE", "/v1/workflows/{project}/{name}", "delete_workflow"),
    ("POST", "/v1/playbooks", "create_playbook_endpoint"),
    ("GET", "/v1/playbooks", "list_playbooks_endpoint"),
    ("GET", "/v1/playbooks/{project}/{playbook_ref}", "get_playbook_endpoint"),
    ("POST", "/v1/playbooks/{project}/{playbook_ref}/run", "run_playbook_endpoint"),
    (
        "POST",
        "/v1/playbooks/{project}/{playbook_ref}/entries/{entry_id}/gate",
        "set_playbook_entry_gate_endpoint",
    ),
    ("PATCH", "/v1/workflows/{project}/{name}", "patch_workflow_endpoint"),
    ("POST", "/v1/hosts", "register_host"),
    ("POST", "/v1/webhook/github", "github_webhook"),
    ("GET", "/v1/issues", "list_issues"),
    ("GET", "/v1/issues/by-id/{project}/{issue_id}", "issue_detail_by_id"),
    ("GET", "/v1/issues/by-number/{project}/{issue_number}", "issue_detail_by_number"),
    ("GET", "/v1/issues/{repo_owner}/{repo_name}/{issue_number}", "issue_detail"),
    ("POST", "/v1/issues", "create_issue_endpoint"),
    (
        "PATCH",
        "/v1/issues/by-number/{project}/{issue_number}",
        "patch_issue_by_number_endpoint",
    ),
    ("PATCH", "/v1/issues/by-id/{project}/{issue_id}", "patch_issue_endpoint"),
    (
        "POST",
        "/v1/issues/by-number/{project}/{issue_number}/archive",
        "archive_issue_by_number_endpoint",
    ),
    ("POST", "/v1/issues/by-id/{project}/{issue_id}/archive", "archive_issue_endpoint"),
    (
        "POST",
        "/v1/issues/by-number/{project}/{issue_number}/discard",
        "discard_issue_by_number_endpoint",
    ),
    ("POST", "/v1/issues/by-id/{project}/{issue_id}/discard", "discard_issue_endpoint"),
    (
        "POST",
        "/v1/issues/by-number/{project}/{issue_number}/comments",
        "create_issue_comment_by_number_endpoint",
    ),
    (
        "POST",
        "/v1/issues/by-id/{project}/{issue_id}/comments",
        "create_issue_comment_endpoint",
    ),
    (
        "PATCH",
        "/v1/issues/by-number/{project}/{issue_number}/comments/{comment_id}",
        "update_issue_comment_by_number_endpoint",
    ),
    (
        "DELETE",
        "/v1/issues/by-number/{project}/{issue_number}/comments/{comment_id}",
        "delete_issue_comment_by_number_endpoint",
    ),
    (
        "PATCH",
        "/v1/issues/by-id/{project}/{issue_id}/comments/{comment_id}",
        "update_issue_comment_endpoint",
    ),
    (
        "DELETE",
        "/v1/issues/by-id/{project}/{issue_id}/comments/{comment_id}",
        "delete_issue_comment_endpoint",
    ),
    ("GET", "/v1/issues/by-number/{project}/{issue_number}/graph", "issue_graph_by_number"),
    ("GET", "/v1/issues/{repo_owner}/{repo_name}/{issue_number}/graph", "issue_graph"),
    ("GET", "/v1/graph", "system_graph"),
    ("POST", "/v1/runs/dispatch", "dispatch_run_endpoint"),
    ("POST", "/v1/test-slots/checkout", "checkout_test_slot"),
    ("POST", "/v1/test-slots/return", "return_test_slot"),
    ("GET", "/v1/portfolio/elements", "list_portfolio_elements"),
    ("POST", "/v1/portfolio/elements", "upsert_portfolio_element"),
    (
        "PATCH",
        "/v1/portfolio/elements/{project}/{element_ref}",
        "patch_portfolio_element",
    ),
    ("POST", "/v1/portfolio/elements/dispatch", "dispatch_portfolio_elements"),
    ("GET", "/v1/reports", "list_touchpoints"),
    ("GET", "/v1/touchpoints", "list_touchpoints"),
    ("GET", "/v1/reports/by-id/{project}/{report_id}", "touchpoint_detail_by_id"),
    ("GET", "/v1/touchpoints/by-id/{project}/{report_id}", "touchpoint_detail_by_id"),
    ("GET", "/v1/reports/{repo_owner}/{repo_name}/{pr_number}", "touchpoint_detail"),
    ("GET", "/v1/touchpoints/{repo_owner}/{repo_name}/{pr_number}", "touchpoint_detail"),
    (
        "GET",
        "/v1/projects/{project}/issues/{issue_number}/touchpoint",
        "issue_touchpoint_detail",
    ),
    (
        "GET",
        "/v1/reports/by-id/{project}/{report_id}/versions",
        "list_touchpoint_versions_endpoint",
    ),
    (
        "GET",
        "/v1/touchpoints/by-id/{project}/{report_id}/versions",
        "list_touchpoint_versions_endpoint",
    ),
    (
        "GET",
        "/v1/reports/by-id/{project}/{report_id}/versions/{version}",
        "touchpoint_version_detail_endpoint",
    ),
    (
        "GET",
        "/v1/touchpoints/by-id/{project}/{report_id}/versions/{version}",
        "touchpoint_version_detail_endpoint",
    ),
    ("POST", "/v1/reports", "create_touchpoint_endpoint"),
    ("POST", "/v1/touchpoints", "create_touchpoint_endpoint"),
    (
        "POST",
        "/v1/reports/by-id/{project}/{report_id}/versions",
        "create_touchpoint_version_endpoint",
    ),
    (
        "POST",
        "/v1/touchpoints/by-id/{project}/{report_id}/versions",
        "create_touchpoint_version_endpoint",
    ),
    (
        "PATCH",
        "/v1/reports/by-id/{project}/{report_id}",
        "patch_touchpoint_endpoint",
    ),
    (
        "PATCH",
        "/v1/touchpoints/by-id/{project}/{report_id}",
        "patch_touchpoint_endpoint",
    ),
    ("POST", "/v1/signals", "enqueue_signal_endpoint"),
)


def _route_inventory() -> tuple[Route, ...]:
    routes: list[Route] = []
    for route in app.routes:
        if not isinstance(route, APIRoute):
            continue
        if route.path.startswith("/docs") or route.path.startswith("/openapi"):
            continue
        for method in sorted(route.methods or ()):
            if method in {"HEAD", "OPTIONS"}:
                continue
            routes.append((method, route.path, route.name))
    return tuple(routes)


def test_api_route_inventory_matches_go_migration_contract() -> None:
    assert _route_inventory() == EXPECTED_API_ROUTES

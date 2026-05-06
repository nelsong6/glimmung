from fastapi.routing import APIRoute

from glimmung.app import app


PUBLIC_READ_ROUTES = {
    ("GET", "/v1/projects"),
    ("GET", "/v1/workflows"),
    ("GET", "/v1/issues"),
    ("GET", "/v1/issues/{repo_owner}/{repo_name}/{issue_number}"),
    ("GET", "/v1/issues/by-id/{project}/{issue_id}"),
    ("GET", "/v1/issues/{repo_owner}/{repo_name}/{issue_number}/graph"),
    ("GET", "/v1/graph"),
    ("GET", "/v1/runs/{project}/{run_id}/native/events"),
    ("GET", "/v1/reports"),
    ("GET", "/v1/touchpoints"),
    ("GET", "/v1/reports/{repo_owner}/{repo_name}/{pr_number}"),
    ("GET", "/v1/touchpoints/{repo_owner}/{repo_name}/{pr_number}"),
    ("GET", "/v1/reports/by-id/{project}/{report_id}"),
    ("GET", "/v1/touchpoints/by-id/{project}/{report_id}"),
}

MUTATING_ADMIN_ROUTES = {
    ("POST", "/v1/projects"),
    ("POST", "/v1/workflows"),
    ("PATCH", "/v1/workflows/{project}/{name}"),
    ("POST", "/v1/issues"),
    ("PATCH", "/v1/issues/by-id/{project}/{issue_id}"),
    ("POST", "/v1/runs/dispatch"),
    ("POST", "/v1/reports"),
    ("POST", "/v1/touchpoints"),
    ("PATCH", "/v1/reports/by-id/{project}/{report_id}"),
    ("PATCH", "/v1/touchpoints/by-id/{project}/{report_id}"),
    ("POST", "/v1/signals"),
}


def _route(method: str, path: str) -> APIRoute:
    for route in app.routes:
        if isinstance(route, APIRoute) and route.path == path and method in route.methods:
            return route
    raise AssertionError(f"route not found: {method} {path}")


def test_read_routes_are_public() -> None:
    for method, path in PUBLIC_READ_ROUTES:
        route = _route(method, path)
        assert route.dependencies == []
        assert route.dependant.dependencies == []


def test_mutating_routes_remain_admin_gated() -> None:
    for method, path in MUTATING_ADMIN_ROUTES:
        route = _route(method, path)
        assert route.dependant.dependencies, f"{method} {path} has no admin dependency"


def test_native_issue_by_id_route_precedes_legacy_issue_route() -> None:
    """FastAPI route order matters here: if the legacy three-segment GH
    route is first, `/v1/issues/by-id/{project}/{issue_id}` is parsed as
    `{owner=by-id}/{repo=project}/{issue_number=issue_id}` and returns
    422 before the native route can run."""
    issue_route_paths = [
        route.path for route in app.routes
        if isinstance(route, APIRoute) and "GET" in route.methods
    ]
    assert issue_route_paths.index("/v1/issues/by-id/{project}/{issue_id}") < (
        issue_route_paths.index("/v1/issues/{repo_owner}/{repo_name}/{issue_number}")
    )


def test_report_by_id_route_precedes_repo_number_route() -> None:
    """Same route-order trap as native Issue detail."""
    pr_route_paths = [
        route.path for route in app.routes
        if isinstance(route, APIRoute) and "GET" in route.methods
    ]
    assert pr_route_paths.index("/v1/reports/by-id/{project}/{report_id}") < (
        pr_route_paths.index("/v1/reports/{repo_owner}/{repo_name}/{pr_number}")
    )


def test_touchpoint_by_id_route_precedes_repo_number_route() -> None:
    """Same route-order trap for the product-facing touchpoint aliases."""
    pr_route_paths = [
        route.path for route in app.routes
        if isinstance(route, APIRoute) and "GET" in route.methods
    ]
    assert pr_route_paths.index("/v1/touchpoints/by-id/{project}/{report_id}") < (
        pr_route_paths.index("/v1/touchpoints/{repo_owner}/{repo_name}/{pr_number}")
    )

package server

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var expectedGoRoutes = []string{
	"GET /healthz",
	"GET /v1/config",
	"GET /v1/auth/me",
	"GET /v1/artifacts/{blob_path...}",
	"GET /v1/issues/by-id/{project}/{issue_id}",
	"GET /v1/issues",
	"PATCH /v1/issues/by-id/{project}/{issue_id}",
	"POST /v1/issues/by-id/{project}/{issue_id}/archive",
	"POST /v1/issues/by-id/{project}/{issue_id}/discard",
	"POST /v1/issues/by-id/{project}/{issue_id}/comments",
	"PATCH /v1/issues/by-id/{project}/{issue_id}/comments/{comment_id}",
	"DELETE /v1/issues/by-id/{project}/{issue_id}/comments/{comment_id}",
	"GET /v1/reports/by-id/{project}/{report_id}",
	"GET /v1/touchpoints/by-id/{project}/{report_id}",
	"GET /v1/reports/by-id/{project}/{report_id}/versions",
	"GET /v1/touchpoints/by-id/{project}/{report_id}/versions",
	"GET /v1/reports/by-id/{project}/{report_id}/versions/{version}",
	"GET /v1/touchpoints/by-id/{project}/{report_id}/versions/{version}",
	"POST /v1/reports/by-id/{project}/{report_id}/versions",
	"POST /v1/touchpoints/by-id/{project}/{report_id}/versions",
	"PATCH /v1/reports/by-id/{project}/{report_id}",
	"PATCH /v1/touchpoints/by-id/{project}/{report_id}",
	"GET /v1/projects/{project}/runs",
	"GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/report",
	"GET /v1/issues/by-number/{project}/{issue_number}",
	"GET /v1/issues/{repo_owner}/{repo_name}/{issue_number}",
	"GET /v1/issues/by-number/{project}/{issue_number}/graph",
	"GET /v1/issues/{repo_owner}/{repo_name}/{issue_number}/graph",
	"GET /v1/graph",
	"GET /v1/playbooks",
	"POST /v1/playbooks",
	"GET /v1/playbooks/{project}/{playbook_ref}",
	"GET /v1/touchpoints",
	"GET /v1/reports",
	"GET /v1/touchpoints/{repo_owner}/{repo_name}/{pr_number}",
	"GET /v1/reports/{repo_owner}/{repo_name}/{pr_number}",
	"GET /v1/projects/{project}/issues/{issue_number}/touchpoint",
	"POST /v1/touchpoints",
	"POST /v1/reports",
	"GET /v1/projects",
	"POST /v1/projects",
	"POST /v1/issues",
	"PATCH /v1/issues/by-number/{project}/{issue_number}",
	"POST /v1/issues/by-number/{project}/{issue_number}/archive",
	"POST /v1/issues/by-number/{project}/{issue_number}/discard",
	"POST /v1/issues/by-number/{project}/{issue_number}/comments",
	"PATCH /v1/issues/by-number/{project}/{issue_number}/comments/{comment_id}",
	"DELETE /v1/issues/by-number/{project}/{issue_number}/comments/{comment_id}",
	"PATCH /v1/projects/{project}/test-environments/count",
	"GET /v1/workflows",
	"POST /v1/workflows",
	"PATCH /v1/workflows/{project}/{name}",
	"DELETE /v1/workflows/{project}/{name}",
	"GET /v1/lease-callbacks/{callback_token}",
	"POST /v1/lease-callbacks/{callback_token}/heartbeat",
	"POST /v1/lease-callbacks/{callback_token}/release",
	"GET /v1/state",
	"GET /v1/events",
	"POST /v1/signals",
	"GET /v1/portfolio/elements",
	"POST /v1/portfolio/elements",
	"PATCH /v1/portfolio/elements/{project}/{element_ref}",
	"POST /v1/playbooks/{project}/{playbook_ref}/entries/{entry_id}/gate",
	"POST /v1/hosts",
	"POST /v1/lease",
	"POST /v1/leases/cancel",
	"GET /v1/projects/{project}/workflows/{name}/upstream",
	"POST /v1/projects/{project}/workflows/{name}/sync",
	"POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/abort",
	"POST /v1/run-callbacks/{callback_token}/started",
	"POST /v1/run-callbacks/{callback_token}/aborted",
	"GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events",
	"POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events",
	"POST /v1/run-callbacks/{callback_token}/native/events",
	"GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/status",
	"GET /v1/run-callbacks/{callback_token}/native/status",
	"POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/failed",
	"POST /v1/run-callbacks/{callback_token}/native/failed",
	"POST /v1/run-callbacks/{callback_token}/completed",
	"POST /v1/run-callbacks/{callback_token}/native/completed",
	"POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/replay",
	"POST /v1/runs/dispatch",
	"POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/resume",
	"POST /v1/webhook/github",
	"GET /assets/",
	"GET /",
}

func TestGoRouteInventoryMatchesCleanupContract(t *testing.T) {
	got := registeredRoutesInServerSource(t)
	if len(got) != len(expectedGoRoutes) {
		t.Fatalf("route count=%d, want %d\n\ngot:\n%s\n\nwant:\n%s", len(got), len(expectedGoRoutes), formatRoutes(got), formatRoutes(expectedGoRoutes))
	}
	for i := range expectedGoRoutes {
		if got[i] != expectedGoRoutes[i] {
			t.Fatalf("route[%d]=%q, want %q\n\ngot:\n%s\n\nwant:\n%s", i, got[i], expectedGoRoutes[i], formatRoutes(got), formatRoutes(expectedGoRoutes))
		}
	}
}

func registeredRoutesInServerSource(t *testing.T) []string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test filename")
	}
	sourcePath := filepath.Join(filepath.Dir(filename), "server.go")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}

	pattern := regexp.MustCompile(`mux\.Handle(?:Func)?\(\s*"([^"]+)"`)
	matches := pattern.FindAllSubmatch(source, -1)
	routes := make([]string, 0, len(matches))
	for _, match := range matches {
		routes = append(routes, string(match[1]))
	}
	return routes
}

func formatRoutes(routes []string) string {
	return strings.Join(routes, "\n")
}

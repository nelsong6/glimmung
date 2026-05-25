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
	"GET /metrics",
	"GET /v1/config",
	"GET /v1/auth/me",
	"GET /v1/artifacts/{blob_path...}",
	"GET /v1/issues",
	"GET /v1/projects/{project}/runs",
	"GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/report",
	"GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/cycles/{cycle_number}/graph",
	"GET /v1/issues/by-number/{project}/{issue_number}",
	"GET /v1/issues/by-number/{project}/{issue_number}/graph",
	"GET /v1/graph",
	"GET /v1/playbooks",
	"POST /v1/playbooks",
	"GET /v1/playbooks/{project}/{playbook_ref}",
	"GET /v1/touchpoints",
	"GET /v1/projects/{project}/issues/{issue_number}/touchpoint",
	"POST /v1/touchpoints",
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
	"POST /v1/projects/{project}/test-environments/{slot_name}/repair",
	"GET /v1/workflows",
	"POST /v1/workflows",
	"PATCH /v1/workflows/{project}/{name}",
	"DELETE /v1/workflows/{project}/{name}",
	"GET /v1/lease-callbacks/{callback_token}",
	"POST /v1/lease-callbacks/{callback_token}/heartbeat",
	"POST /v1/lease-callbacks/{callback_token}/release",
	"GET /v1/state",
	"GET /v1/projects/{project}/test-environments/{slot_name}",
	"GET /v1/events",
	"POST /v1/signals",
	"POST /v1/signals/drain",
	"GET /v1/portfolio/elements",
	"POST /v1/portfolio/elements",
	"POST /v1/portfolio/elements/dispatch",
	"PATCH /v1/portfolio/elements/{project}/{element_ref}",
	"POST /v1/playbooks/{project}/{playbook_ref}/run",
	"POST /v1/playbooks/{project}/{playbook_ref}/entries/{entry_id}/gate",
	"POST /v1/leases/cancel",
	"PATCH /v1/leases/ttl",
	"PATCH /v1/test-slots/default-ttl",
	"PATCH /v1/test-slots/hot-swap-min-ttl",
	"GET /v1/projects/{project}/workflows/{name}/upstream",
	"POST /v1/projects/{project}/workflows/{name}/sync",
	"POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/abort",
	"GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events",
	"POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events",
	"POST /v1/run-callbacks/{callback_token}/native/events",
	"GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/status",
	"GET /v1/run-callbacks/{callback_token}/native/status",
	"POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/github-token",
	"POST /v1/run-callbacks/{callback_token}/native/github-token",
	"POST /v1/run-callbacks/{callback_token}/native/completed",
	"POST /v1/test-slots/checkout",
	"POST /v1/test-slots/return",
	"POST /v1/test-slots/extend",
	"POST /v1/test-slots/hot-swap-history",
	"POST /v1/test-slots/apply-hot-swap",
	"POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/replay",
	"POST /v1/runs/dispatch",
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

func TestRetiredRouteFamiliesStayDeleted(t *testing.T) {
	got := formatRoutes(registeredRoutesInServerSource(t))
	for _, forbidden := range []string{
		"/by-id/",
		"/v1/reports",
		"/native/pod-logs",
		"/native/failed",
		"/runs/{run_number}/native/completed",
		"/aborted",
		"/v1/issues/{repo_owner}",
		"/v1/touchpoints/{repo_owner}",
		"/resume",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("retired route family %q is registered:\n%s", forbidden, got)
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

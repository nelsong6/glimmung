package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeJobManifestIncludesRunnerCallbackEnv(t *testing.T) {
	runNumber := 7
	runDisplay := "7"
	callback := "callback-token"
	timeout := 120
	leaseNumber := 3
	req := NativeLaunchRequest{
		Lease: Lease{
			Project:     "tank-operator",
			LeaseNumber: &leaseNumber,
			State:       "claimed",
			Metadata: map[string]any{
				"native_slot_name":  "tank-operator-slot-1",
				"native_slot_index": "1",
				"phase_inputs": map[string]any{
					"target": "provision",
				},
			},
		},
		Workflow: Workflow{Name: "agent-run"},
		Phase:    PhaseSpec{Name: "verify"},
		Run: RunReplayData{
			ID:               "run-123",
			Project:          "tank-operator",
			IssueNumber:      42,
			RunNumber:        &runNumber,
			RunDisplayNumber: &runDisplay,
			CallbackToken:    &callback,
			Attempts:         []RunAttemptData{{AttemptIndex: 1, Phase: "verify"}},
		},
	}

	manifest := nativeJobManifest(Settings{
		NativeRunnerNamespace:         "glimmung-runs",
		NativeRunnerServiceAccount:    "glimmung-native-runner",
		NativeRunnerCallbackBaseURL:   "http://glimmung.glimmung.svc.cluster.local",
		NativeRunnerCodexSecret:       "codex-credentials",
		NativeRunnerCodexMountPath:    "/etc/codex-creds",
		NativeRunnerPlaywrightEnabled: true,
		NativeRunnerPlaywrightPort:    "3000",
	}, req, NativeJobSpec{ID: "test", Image: "runner:latest", TimeoutSeconds: &timeout}, "job", "secret", "attempt")

	env := nativeManifestEnv(manifest)
	if env["GLIMMUNG_COMPLETED_URL"] != "http://glimmung.glimmung.svc.cluster.local/v1/run-callbacks/callback-token/native/completed" {
		t.Fatalf("completed url=%q", env["GLIMMUNG_COMPLETED_URL"])
	}
	if env["GLIMMUNG_GITHUB_TOKEN_URL"] == "" {
		t.Fatal("expected GitHub token URL")
	}
	if env["GLIMMUNG_ATTEMPT_INDEX"] != "1" {
		t.Fatalf("attempt index=%q", env["GLIMMUNG_ATTEMPT_INDEX"])
	}
	if env["GLIMMUNG_INPUT_TARGET"] != "provision" {
		t.Fatalf("phase input env=%q", env["GLIMMUNG_INPUT_TARGET"])
	}
	if env["PLAYWRIGHT_WS_ENDPOINT"] != "ws://slot-playwright.tank-operator-slot-1.svc.cluster.local:3000" {
		t.Fatalf("Playwright endpoint=%q", env["PLAYWRIGHT_WS_ENDPOINT"])
	}
}

func TestReturnTestSlotRuntimeDoesNotDeleteNamespaces(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesNativeLauncher{
		Settings: Settings{
			K8sAPIHost:                "https://kube.test",
			K8sSATokenPath:            tokenPath,
			NativeRunnerNamespace:     "glimmung-runs",
			NativeRunnerJobTTLSeconds: 3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := ""
			if req.Method == http.MethodGet {
				body = `{"items":[]}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := Lease{
		Project:     "tank",
		LeaseNumber: intPtr(2),
		Metadata: map[string]any{
			"native_slot_name":          "tank-slot-1",
			"native_slot_index":         "1",
			"native_sessions_namespace": "tank-slot-1-sessions",
		},
	}

	if err := launcher.ReturnTestSlotRuntime(context.Background(), lease, Project{Name: "tank"}); err != nil {
		t.Fatalf("ReturnTestSlotRuntime: %v", err)
	}
	for _, path := range paths {
		if path == "DELETE /api/v1/namespaces/tank-slot-1" || path == "DELETE /api/v1/namespaces/tank-slot-1-sessions" {
			t.Fatalf("return should not delete slot namespaces, saw %s in %#v", path, paths)
		}
	}
	if !containsPath(paths, "DELETE /apis/apps/v1/namespaces/tank-slot-1/deployments/slot-playwright") {
		t.Fatalf("return should delete slot Playwright deployment, paths=%#v", paths)
	}
	if !containsPath(paths, "DELETE /api/v1/namespaces/tank-slot-1/services/slot-playwright") {
		t.Fatalf("return should delete slot Playwright service, paths=%#v", paths)
	}
}

func TestReturnTestSlotRuntimeDeletesSteadyRuntimeResources(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesNativeLauncher{
		Settings: Settings{
			K8sAPIHost:            "https://kube.test",
			K8sSATokenPath:        tokenPath,
			NativeRunnerNamespace: "glimmung-runs",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := runtimeListResponse(req.URL.Path)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := Lease{
		Project:     "tank",
		LeaseNumber: intPtr(2),
		Metadata: map[string]any{
			"native_slot_name":          "tank-slot-1",
			"native_slot_index":         "1",
			"native_sessions_namespace": "tank-slot-1-sessions",
		},
	}

	if err := launcher.ReturnTestSlotRuntime(context.Background(), lease, Project{Name: "tank"}); err != nil {
		t.Fatalf("ReturnTestSlotRuntime: %v", err)
	}
	for _, want := range []string{
		"DELETE /apis/apps/v1/namespaces/tank-slot-1/deployments/tank-operator",
		"DELETE /apis/apps/v1/namespaces/tank-slot-1/deployments/claude-api-proxy",
		"DELETE /api/v1/namespaces/tank-slot-1/services/tank-operator",
		"DELETE /api/v1/namespaces/tank-slot-1-sessions/pods/session-4",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("missing runtime delete %s, paths=%#v", want, paths)
		}
	}
}

func TestEnsureTestSlotPreliminariesDoesNotCreatePlaywrightRuntime(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesNativeLauncher{
		Settings: Settings{
			K8sAPIHost:                     "https://kube.test",
			K8sSATokenPath:                 tokenPath,
			NativeRunnerNamespace:          "glimmung-runs",
			NativeRunnerPlaywrightEnabled:  true,
			NativeRunnerPlaywrightImage:    "playwright:latest",
			NativeRunnerPlaywrightPort:     "3000",
			NativeRunnerServiceAccount:     "glimmung-native-runner",
			NativeRunnerCallbackBaseURL:    "http://glimmung.glimmung.svc.cluster.local",
			NativeRunnerCodexSecret:        "codex-credentials",
			NativeRunnerCodexMountPath:     "/etc/codex-creds",
			NativeRunnerJobTTLSeconds:      3600,
			NativeRunnerNamespaceRole:      "cluster-admin",
			NativeRunnerProjectConcurrency: 1,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		})},
	}
	lease := Lease{
		Project:     "tank",
		LeaseNumber: intPtr(2),
		Metadata: map[string]any{
			"native_slot_name":  "tank-slot-1",
			"native_slot_index": "1",
		},
	}

	if err := launcher.EnsureTestSlotPreliminaries(context.Background(), lease, Project{Name: "tank"}); err != nil {
		t.Fatalf("EnsureTestSlotPreliminaries: %v", err)
	}
	for _, path := range paths {
		if strings.Contains(path, "/deployments") || strings.Contains(path, "/services") {
			t.Fatalf("baseline warm should not create Playwright runtime resources, paths=%#v", paths)
		}
	}
}

func TestActivateTestSlotRuntimeRunsHelmInstallerAfterLeaseAssignment(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesNativeLauncher{
		Settings: Settings{
			K8sAPIHost:                 "https://kube.test",
			K8sSATokenPath:             tokenPath,
			NativeRunnerNamespace:      "glimmung-runs",
			NativeRunnerServiceAccount: "glimmung-native-runner",
			NativeRunnerNamespaceRole:  "cluster-admin",
			NativeRunnerJobTTLSeconds:  3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := `{}`
			if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/jobs/glim-slot-apply-") {
				body = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	leaseNumber := 12
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: &leaseNumber,
		State:       "claimed",
		Metadata: map[string]any{
			"native_slot_name":          "tank-operator-slot-2",
			"native_slot_index":         "2",
			"native_sessions_namespace": "tank-operator-slot-2-sessions",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "nelsong6/tank-operator",
		Metadata:   map[string]any{"test_slot_helm": map[string]any{"enabled": true}},
	}

	if err := launcher.ActivateTestSlotRuntime(context.Background(), lease, project, fakeNativeGitHubTokenMinter{token: "ghs_test"}); err != nil {
		t.Fatalf("ActivateTestSlotRuntime: %v", err)
	}
	if !containsPath(paths, "POST /apis/batch/v1/namespaces/glimmung-runs/jobs") {
		t.Fatalf("activation should create Helm installer job, paths=%#v", paths)
	}
	if !containsPath(paths, "GET /apis/batch/v1/namespaces/glimmung-runs/jobs/glim-slot-apply-tank-operator-slot-2-12") {
		t.Fatalf("activation should wait for Helm installer job completion, paths=%#v", paths)
	}
	if containsPath(paths, "POST /apis/apps/v1/namespaces/tank-operator-slot-2/deployments") {
		t.Fatalf("activation should delegate app runtime creation to installer job, paths=%#v", paths)
	}
}

func TestLaunchNativePhaseCreatesSlotPlaywrightRuntime(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesNativeLauncher{
		Settings: Settings{
			K8sAPIHost:                    "https://kube.test",
			K8sSATokenPath:                tokenPath,
			NativeRunnerNamespace:         "glimmung-runs",
			NativeRunnerServiceAccount:    "glimmung-native-runner",
			NativeRunnerCallbackBaseURL:   "http://glimmung.glimmung.svc.cluster.local",
			NativeRunnerCodexSecret:       "codex-credentials",
			NativeRunnerCodexMountPath:    "/etc/codex-creds",
			NativeRunnerPlaywrightEnabled: true,
			NativeRunnerPlaywrightImage:   "playwright:latest",
			NativeRunnerPlaywrightPort:    "3000",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		})},
	}
	runNumber := 7
	callback := "callback-token"
	leaseNumber := 3
	req := NativeLaunchRequest{
		Lease: Lease{
			Project:     "tank-operator",
			LeaseNumber: &leaseNumber,
			State:       "claimed",
			Metadata: map[string]any{
				"native_slot_name":  "tank-operator-slot-1",
				"native_slot_index": "1",
			},
		},
		Workflow: Workflow{Name: "agent-run"},
		Phase:    PhaseSpec{Name: "verify", Jobs: []NativeJobSpec{{ID: "test", Image: "runner:latest"}}},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "tank-operator",
			IssueNumber:   42,
			RunNumber:     &runNumber,
			CallbackToken: &callback,
		},
	}

	if _, err := launcher.LaunchNativePhase(context.Background(), req); err != nil {
		t.Fatalf("LaunchNativePhase: %v", err)
	}
	if !containsPath(paths, "POST /apis/apps/v1/namespaces/tank-operator-slot-1/deployments") {
		t.Fatalf("launch should create slot Playwright deployment, paths=%#v", paths)
	}
	if !containsPath(paths, "POST /api/v1/namespaces/tank-operator-slot-1/services") {
		t.Fatalf("launch should create slot Playwright service, paths=%#v", paths)
	}
	if containsPath(paths, "POST /apis/apps/v1/namespaces/glimmung-runs/deployments") {
		t.Fatalf("launch should not create Playwright in glimmung-runs, paths=%#v", paths)
	}
}

func TestTestSlotHelmConfigDefaultsTankChart(t *testing.T) {
	config, ok := testSlotHelmConfig(Project{
		ID:         "tank-operator",
		Name:       "tank-operator",
		GitHubRepo: "nelsong6/tank-operator",
		Metadata: map[string]any{
			"test_slot_helm": map[string]any{"enabled": true},
		},
	})
	if !ok {
		t.Fatal("expected helm config")
	}
	if config.ChartPath != "k8s" {
		t.Fatalf("chart path=%q", config.ChartPath)
	}
	if config.InstallerImage != "alpine/k8s:1.30.0" {
		t.Fatalf("installer image=%q", config.InstallerImage)
	}
	if config.Values["testEnv.enabled"] != "true" {
		t.Fatalf("test env value=%q", config.Values["testEnv.enabled"])
	}
	if len(config.ClusterRoleBindings) != 2 {
		t.Fatalf("cluster role binding templates=%d", len(config.ClusterRoleBindings))
	}
}

func TestTestSlotInstallJobManifestRendersHelmApplyJob(t *testing.T) {
	leaseNumber := 12
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: &leaseNumber,
		Metadata: map[string]any{
			"native_slot_name":  "tank-operator-slot-2",
			"native_slot_index": "2",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "nelsong6/tank-operator",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{
				"record_base": "tank.dev.romaine.life",
			},
			"test_slot_helm": map[string]any{"enabled": true},
		},
	}
	config, ok := testSlotHelmConfig(project)
	if !ok {
		t.Fatal("expected helm config")
	}
	manifest := testSlotInstallJobManifest(
		Settings{NativeRunnerNamespace: "glimmung-runs", NativeRunnerServiceAccount: "glimmung-native-runner", NativeRunnerJobTTLSeconds: 3600},
		config,
		lease,
		project,
		testSlotSubstitutions(lease, project, "tank-operator-slot-2", "tank-operator-slot-2-sessions"),
	)
	if manifest["metadata"].(map[string]any)["name"] != "glim-slot-apply-tank-operator-slot-2-12" {
		t.Fatalf("job name=%q", manifest["metadata"].(map[string]any)["name"])
	}
	spec := manifest["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	initScript := spec["initContainers"].([]any)[0].(map[string]any)["command"].([]string)[2]
	if strings.Contains(initScript, "ghs_") {
		t.Fatalf("clone script should not contain token: %s", initScript)
	}
	if !strings.Contains(initScript, "nelsong6/tank-operator") {
		t.Fatalf("clone script missing repo: %s", initScript)
	}
	installScript := spec["containers"].([]any)[0].(map[string]any)["command"].([]string)[2]
	for _, want := range []string{
		"helm template 'tank-operator-slot-2' 'k8s'",
		"--set 'testEnv.enabled=true'",
		"ClusterRoleBinding",
		"kubectl apply -f -",
		"kubectl -n 'tank-operator-slot-2' wait --for=condition=available --timeout=180s deployment --all",
	} {
		if !strings.Contains(installScript, want) {
			t.Fatalf("install script missing %q: %s", want, installScript)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type fakeNativeGitHubTokenMinter struct {
	token string
	err   error
}

func (m fakeNativeGitHubTokenMinter) InstallationToken(context.Context) (string, error) {
	return m.token, m.err
}

func tempTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("token"), 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func runtimeListResponse(path string) string {
	switch path {
	case "/apis/apps/v1/namespaces/tank-slot-1/deployments":
		return `{"items":[{"metadata":{"name":"tank-operator"}},{"metadata":{"name":"claude-api-proxy"}}]}`
	case "/api/v1/namespaces/tank-slot-1/services":
		return `{"items":[{"metadata":{"name":"tank-operator"}}]}`
	case "/api/v1/namespaces/tank-slot-1-sessions/pods":
		return `{"items":[{"metadata":{"name":"session-4"}}]}`
	default:
		if strings.Contains(path, "/jobs/glim-slot-apply-") {
			return `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
		}
		return `{"items":[]}`
	}
}

func nativeManifestEnv(manifest map[string]any) map[string]string {
	spec := manifest["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	podSpec := template["spec"].(map[string]any)
	containers := podSpec["containers"].([]any)
	container := containers[0].(map[string]any)
	envRows := container["env"].([]map[string]any)
	env := map[string]string{}
	for _, row := range envRows {
		if value, ok := row["value"].(string); ok {
			env[row["name"].(string)] = value
		}
	}
	return env
}

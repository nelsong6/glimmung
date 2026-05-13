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
	if env["PLAYWRIGHT_WS_ENDPOINT"] == "" {
		t.Fatal("expected Playwright endpoint")
	}
}

func TestReturnTestSlotDoesNotDeleteNamespaces(t *testing.T) {
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

	if err := launcher.ReturnTestSlot(context.Background(), lease); err != nil {
		t.Fatalf("ReturnTestSlot: %v", err)
	}
	for _, path := range paths {
		if strings.Contains(path, "/api/v1/namespaces/tank-slot-1") {
			t.Fatalf("return should not delete slot namespaces, saw %s in %#v", path, paths)
		}
		if strings.Contains(path, "/apis/apps/v1/namespaces/glimmung-runs/deployments/") {
			t.Fatalf("return should not delete warmed Playwright resources, saw %s in %#v", path, paths)
		}
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
	if manifest["metadata"].(map[string]any)["name"] != "glim-helm-install-tank-operator-slot-2-12" {
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

func tempTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("token"), 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
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

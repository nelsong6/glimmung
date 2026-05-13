package server

import (
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
					"test_slot_mode": "provision",
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
	if env["GLIMMUNG_INPUT_TEST_SLOT_MODE"] != "provision" {
		t.Fatalf("phase input env=%q", env["GLIMMUNG_INPUT_TEST_SLOT_MODE"])
	}
	if env["PLAYWRIGHT_WS_ENDPOINT"] == "" {
		t.Fatal("expected Playwright endpoint")
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
		"kubectl apply -n 'tank-operator-slot-2' -f -",
	} {
		if !strings.Contains(installScript, want) {
			t.Fatalf("install script missing %q: %s", want, installScript)
		}
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

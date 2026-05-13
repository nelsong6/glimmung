package server

import "testing"

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

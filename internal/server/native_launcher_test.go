package server

import (
	"context"
	"encoding/json"
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
				"native_slot_name":     "tank-operator-slot-1",
				"native_slot_index":    "1",
				"entrypoint_job_id":    "test",
				"entrypoint_step_slug": "verify-ui",
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
	}, req, NativeJobSpec{
		ID:             "test",
		Image:          "runner:latest",
		TimeoutSeconds: &timeout,
		Env: map[string]string{
			"AZURE_SUBSCRIPTION_ID": "sub-123",
			"GLIMMUNG_PROJECT":      "must-not-override",
		},
	}, "job", "secret", "attempt")

	env := nativeManifestEnv(manifest)
	if env["GLIMMUNG_COMPLETED_URL"] != "http://glimmung.glimmung.svc.cluster.local/v1/run-callbacks/callback-token/native/completed" {
		t.Fatalf("completed url=%q", env["GLIMMUNG_COMPLETED_URL"])
	}
	if _, ok := env["GLIMMUNG_FAILED_URL"]; ok {
		t.Fatal("failed callback URL should not be injected")
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
	if env["GLIMMUNG_VALIDATION_NAMESPACE"] != "tank-operator-slot-1" {
		t.Fatalf("validation namespace=%q", env["GLIMMUNG_VALIDATION_NAMESPACE"])
	}
	if env["GLIMMUNG_ENTRYPOINT_JOB_ID"] != "test" || env["GLIMMUNG_ENTRYPOINT_STEP_SLUG"] != "verify-ui" {
		t.Fatalf("entrypoint env job=%q step=%q", env["GLIMMUNG_ENTRYPOINT_JOB_ID"], env["GLIMMUNG_ENTRYPOINT_STEP_SLUG"])
	}
	if env["AZURE_SUBSCRIPTION_ID"] != "sub-123" {
		t.Fatalf("job env=%q", env["AZURE_SUBSCRIPTION_ID"])
	}
	if env["GLIMMUNG_PROJECT"] != "tank-operator" {
		t.Fatalf("system env was overridden: %q", env["GLIMMUNG_PROJECT"])
	}
	if env["PLAYWRIGHT_WS_ENDPOINT"] != "ws://slot-playwright.tank-operator-slot-1.svc.cluster.local:3000" {
		t.Fatalf("Playwright endpoint=%q", env["PLAYWRIGHT_WS_ENDPOINT"])
	}
}

func TestNativeJobManifestIncludesStringMapPhaseInputs(t *testing.T) {
	req := NativeLaunchRequest{
		Lease: Lease{
			Project: "ambience",
			State:   "claimed",
			Metadata: map[string]any{
				"phase_inputs": map[string]string{
					"namespace":      "ambience-slot-1",
					"validation_url": "https://ambience-slot-1.ambience.dev.romaine.life",
				},
			},
		},
		Workflow: Workflow{Name: "default"},
		Phase:    PhaseSpec{Name: "llm-work"},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "ambience",
			CallbackToken: stringPtr("callback-token"),
			Attempts:      []RunAttemptData{{AttemptIndex: 1, Phase: "llm-work"}},
		},
	}

	manifest := nativeJobManifest(Settings{
		NativeRunnerNamespace:       "glimmung-runs",
		NativeRunnerServiceAccount:  "glimmung-native-runner",
		NativeRunnerCallbackBaseURL: "http://glimmung.glimmung.svc.cluster.local",
		NativeRunnerCodexSecret:     "codex-credentials",
		NativeRunnerCodexMountPath:  "/etc/codex-creds",
	}, req, NativeJobSpec{ID: "llm-test-plan", Image: "runner:latest"}, "job", "secret", "attempt")

	env := nativeManifestEnv(manifest)
	if env["GLIMMUNG_INPUT_NAMESPACE"] != "ambience-slot-1" {
		t.Fatalf("namespace input env=%q", env["GLIMMUNG_INPUT_NAMESPACE"])
	}
	if env["GLIMMUNG_INPUT_VALIDATION_URL"] != "https://ambience-slot-1.ambience.dev.romaine.life" {
		t.Fatalf("validation_url input env=%q", env["GLIMMUNG_INPUT_VALIDATION_URL"])
	}
}

func TestNativeJobManifestManagedJobUsesSharedRunnerEntrypoint(t *testing.T) {
	req := NativeLaunchRequest{
		Lease:    Lease{Project: "ambience"},
		Workflow: Workflow{Name: "agent-run"},
		Phase:    PhaseSpec{Name: "env-prep"},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "ambience",
			CallbackToken: stringPtr("callback-token"),
			Attempts:      []RunAttemptData{{AttemptIndex: 1, Phase: "env-prep"}},
		},
	}
	job := NativeJobSpec{
		ID:               "prepare",
		Managed:          true,
		WorkingDirectory: "/workspace/ambience",
		Steps: []NativeStepSpec{{
			Slug: "unit",
			Run:  "go test ./...",
		}},
	}

	manifest := nativeJobManifest(Settings{
		NativeRunnerNamespace:       "glimmung-runs",
		NativeRunnerServiceAccount:  "glimmung-native-runner",
		NativeRunnerCallbackBaseURL: "http://glimmung.glimmung.svc.cluster.local",
		NativeRunnerImage:           "romainecr.azurecr.io/glimmung-native-runner:test",
		NativeRunnerEntrypoint:      "/runner/glimmung-native-runner",
		NativeRunnerCodexSecret:     "codex-credentials",
		NativeRunnerCodexMountPath:  "/etc/codex-creds",
	}, req, job, "job", "secret", "attempt")

	container := nativeManifestContainer(manifest)
	command, ok := container["command"].([]string)
	if !ok || len(command) != 1 || command[0] != "/runner/glimmung-native-runner" {
		t.Fatalf("command=%#v", container["command"])
	}
	if container["image"] != "romainecr.azurecr.io/glimmung-native-runner:test" {
		t.Fatalf("image=%#v", container["image"])
	}
	if _, ok := container["args"]; ok {
		t.Fatalf("managed runner should not receive legacy args: %#v", container["args"])
	}
	env := nativeManifestEnv(manifest)
	var got NativeJobSpec
	if err := json.Unmarshal([]byte(env["GLIMMUNG_RUNNER_JOB_SPEC"]), &got); err != nil {
		t.Fatalf("runner spec JSON: %v", err)
	}
	if !got.Managed || got.ID != "prepare" || got.WorkingDirectory != "/workspace/ambience" {
		t.Fatalf("runner spec=%#v", got)
	}
	if len(got.Steps) != 1 || got.Steps[0].Run != "go test ./..." {
		t.Fatalf("runner steps=%#v", got.Steps)
	}
}

func TestNativeJobManifestEvidenceGateUsesManagedRunner(t *testing.T) {
	req := NativeLaunchRequest{
		Lease:    Lease{Project: "ambience"},
		Workflow: Workflow{Name: "default"},
		Phase: PhaseSpec{
			Name:                     "evidence-gate",
			EvidenceVerificationGate: true,
			Jobs: []NativeJobSpec{{
				ID:      "legacy-gate",
				Image:   "python:3.12-slim",
				Command: []string{"python", "-c"},
				Args:    []string{"exit(1)"},
			}},
		},
		Run: RunReplayData{
			ID:            "run-123",
			Project:       "ambience",
			CallbackToken: stringPtr("callback-token"),
			Attempts:      []RunAttemptData{{AttemptIndex: 3, Phase: "evidence-gate"}},
		},
	}

	manifest := nativeJobManifest(Settings{
		NativeRunnerNamespace:       "glimmung-runs",
		NativeRunnerServiceAccount:  "glimmung-native-runner",
		NativeRunnerCallbackBaseURL: "http://glimmung.glimmung.svc.cluster.local",
		NativeRunnerImage:           "romainecr.azurecr.io/glimmung-native-runner:test",
		NativeRunnerEntrypoint:      "/app/glimmung-native-runner",
		NativeRunnerCodexSecret:     "codex-credentials",
		NativeRunnerCodexMountPath:  "/etc/codex-creds",
	}, req, req.Phase.Jobs[0], "job", "secret", "attempt")

	container := nativeManifestContainer(manifest)
	if container["image"] != "romainecr.azurecr.io/glimmung-native-runner:test" {
		t.Fatalf("image=%#v", container["image"])
	}
	command, ok := container["command"].([]string)
	if !ok || len(command) != 1 || command[0] != "/app/glimmung-native-runner" {
		t.Fatalf("command=%#v", container["command"])
	}
	if _, ok := container["args"]; ok {
		t.Fatalf("evidence gate should not receive legacy args: %#v", container["args"])
	}
	env := nativeManifestEnv(manifest)
	var got NativeJobSpec
	if err := json.Unmarshal([]byte(env["GLIMMUNG_RUNNER_JOB_SPEC"]), &got); err != nil {
		t.Fatalf("runner spec JSON: %v", err)
	}
	if got.ID != "legacy-gate" || !got.Managed || len(got.Steps) != 1 || got.Steps[0].Slug != EvidenceGateStepSlug {
		t.Fatalf("runner spec=%#v", got)
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
	deleted := map[string]bool{}
	launcher := &KubernetesNativeLauncher{
		Settings: Settings{
			K8sAPIHost:            "https://kube.test",
			K8sSATokenPath:        tokenPath,
			NativeRunnerNamespace: "glimmung-runs",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			if req.Method == http.MethodDelete {
				deleted[req.URL.Path] = true
			}
			body := runtimeListResponse(req.URL.Path, deleted)
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
	if countPath(paths, "GET /api/v1/namespaces/tank-slot-1-sessions/pods") < 2 {
		t.Fatalf("return should re-check session pods before marking cleanup complete, paths=%#v", paths)
	}
}

func TestReturnTestSlotRuntimeUninstallsHelmRuntimeRelease(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesNativeLauncher{
		Settings: Settings{
			K8sAPIHost:                 "https://kube.test",
			K8sSATokenPath:             tokenPath,
			NativeRunnerNamespace:      "glimmung-runs",
			NativeRunnerServiceAccount: "glimmung-native-runner",
			NativeRunnerJobTTLSeconds:  3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := `{"items":[]}`
			if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/jobs/glim-slot-uninstall-hot-") {
				body = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: intPtr(2),
		Metadata: map[string]any{
			"native_slot_name":          "tank-operator-slot-1",
			"native_slot_index":         "1",
			"native_sessions_namespace": "tank-operator-slot-1-sessions",
		},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "nelsong6/tank-operator",
		Metadata:   map[string]any{"test_slot_helm": map[string]any{"enabled": true}},
	}

	if err := launcher.ReturnTestSlotRuntime(context.Background(), lease, project); err != nil {
		t.Fatalf("ReturnTestSlotRuntime: %v", err)
	}
	for _, want := range []string{
		"POST /apis/batch/v1/namespaces/glimmung-runs/jobs",
		"GET /apis/batch/v1/namespaces/glimmung-runs/jobs/glim-slot-uninstall-hot-tank-operator-slot-1-2",
		"DELETE /apis/apps/v1/namespaces/tank-operator-slot-1/deployments/slot-playwright",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("missing %s in paths=%#v", want, paths)
		}
	}
	for _, path := range paths {
		if strings.Contains(path, "/services/tank-operator") || strings.Contains(path, "/services/claude-api-proxy") || strings.Contains(path, "/services/codex-api-proxy") {
			t.Fatalf("helm cleanup should not hand-delete runtime services, paths=%#v", paths)
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
	for _, want := range []string{
		"GET /apis/batch/v1/namespaces/glimmung-runs/jobs/glim-slot-apply-warm-tank-operator-slot-2-12",
		"GET /apis/batch/v1/namespaces/glimmung-runs/jobs/glim-slot-apply-hot-tank-operator-slot-2-12",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("activation should wait for Helm installer job completion %s, paths=%#v", want, paths)
		}
	}
	if containsPath(paths, "POST /apis/apps/v1/namespaces/tank-operator-slot-2/deployments") {
		t.Fatalf("activation should delegate app runtime creation to installer job, paths=%#v", paths)
	}
}

func TestActivateTestSlotRuntimeCreatesReadyPlaywrightRuntime(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	launcher := &KubernetesNativeLauncher{
		Settings: Settings{
			K8sAPIHost:                    "https://kube.test",
			K8sSATokenPath:                tokenPath,
			NativeRunnerNamespace:         "glimmung-runs",
			NativeRunnerServiceAccount:    "glimmung-native-runner",
			NativeRunnerNamespaceRole:     "cluster-admin",
			NativeRunnerJobTTLSeconds:     3600,
			NativeRunnerPlaywrightEnabled: true,
			NativeRunnerPlaywrightImage:   "playwright:latest",
			NativeRunnerPlaywrightPort:    "3000",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			body := `{}`
			if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/jobs/glim-slot-apply-") {
				body = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
			}
			if req.Method == http.MethodGet && req.URL.Path == "/apis/apps/v1/namespaces/tank-operator-slot-2/deployments/slot-playwright" {
				body = `{"status":{"readyReplicas":1,"availableReplicas":1}}`
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
	for _, want := range []string{
		"POST /apis/apps/v1/namespaces/tank-operator-slot-2/deployments",
		"POST /api/v1/namespaces/tank-operator-slot-2/services",
		"GET /apis/apps/v1/namespaces/tank-operator-slot-2/deployments/slot-playwright",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("missing Playwright activation path %s, paths=%#v", want, paths)
		}
	}
}

func TestLaunchNativePhaseCreatesSlotPlaywrightRuntimeWithoutReadinessWait(t *testing.T) {
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
	if containsPath(paths, "GET /apis/apps/v1/namespaces/tank-operator-slot-1/deployments/slot-playwright") {
		t.Fatalf("launch should not wait for slot Playwright readiness, paths=%#v", paths)
	}
	if containsPath(paths, "POST /apis/apps/v1/namespaces/glimmung-runs/deployments") {
		t.Fatalf("launch should not create Playwright in glimmung-runs, paths=%#v", paths)
	}
}

func TestDeprovisionTestSlotDeletesInstallerAndNamespaces(t *testing.T) {
	tokenPath := tempTokenFile(t)
	var paths []string
	namespaces := map[string]bool{
		"tank-operator-slot-11":          true,
		"tank-operator-slot-11-sessions": true,
	}
	launcher := &KubernetesNativeLauncher{
		Settings: Settings{
			K8sAPIHost:                 "https://kube.test",
			K8sSATokenPath:             tokenPath,
			NativeRunnerNamespace:      "glimmung-runs",
			NativeRunnerServiceAccount: "glimmung-native-runner",
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.Method+" "+req.URL.Path)
			status := http.StatusOK
			body := `{}`
			if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v1/namespaces/") {
				namespace := strings.TrimPrefix(req.URL.Path, "/api/v1/namespaces/")
				if !namespaces[namespace] {
					status = http.StatusNotFound
				}
			}
			if req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/api/v1/namespaces/") {
				namespace := strings.TrimPrefix(req.URL.Path, "/api/v1/namespaces/")
				namespaces[namespace] = false
			}
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
	lease := testEnvironmentWarmupLease(Project{Name: "tank-operator"}, 11, "tank-operator-slot-11")

	if err := launcher.DeprovisionTestSlot(context.Background(), lease, Project{Name: "tank-operator"}); err != nil {
		t.Fatalf("DeprovisionTestSlot: %v", err)
	}
	for _, want := range []string{
		"DELETE /api/v1/namespaces/glimmung-runs/secrets/glim-helm-clone-tank-operator-slot-11-0",
		"DELETE /api/v1/namespaces/tank-operator-slot-11-sessions",
		"GET /api/v1/namespaces/tank-operator-slot-11-sessions",
		"DELETE /api/v1/namespaces/tank-operator-slot-11",
		"GET /api/v1/namespaces/tank-operator-slot-11",
	} {
		if !containsPath(paths, want) {
			t.Fatalf("missing %s in paths=%#v", want, paths)
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
	if _, ok := config.Values["testEnv.enabled"]; ok {
		t.Fatalf("testEnv.enabled should not be injected into helm values")
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
		testSlotRenderModeHot,
	)
	if manifest["metadata"].(map[string]any)["name"] != "glim-slot-apply-hot-tank-operator-slot-2-12" {
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
		"helm template 'tank-operator-slot-2-hot' 'k8s'",
		"helm upgrade --install 'tank-operator-slot-2-hot' 'k8s'",
		"--set 'testEnv.slotName=tank-operator-slot-2'",
		"--set 'renderMode=hot'",
		"kubectl delete --ignore-not-found=true -f -",
		"--wait --wait-for-jobs --timeout 180s",
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

func countPath(paths []string, want string) int {
	count := 0
	for _, path := range paths {
		if path == want {
			count++
		}
	}
	return count
}

func runtimeListResponse(path string, deleted map[string]bool) string {
	item := func(deletePath, name string) string {
		if deleted[deletePath] {
			return ""
		}
		return `{"metadata":{"name":"` + name + `"}}`
	}
	items := func(values ...string) string {
		filtered := make([]string, 0, len(values))
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				filtered = append(filtered, value)
			}
		}
		return `{"items":[` + strings.Join(filtered, ",") + `]}`
	}
	switch path {
	case "/apis/apps/v1/namespaces/tank-slot-1/deployments":
		return items(
			item("/apis/apps/v1/namespaces/tank-slot-1/deployments/tank-operator", "tank-operator"),
			item("/apis/apps/v1/namespaces/tank-slot-1/deployments/claude-api-proxy", "claude-api-proxy"),
		)
	case "/api/v1/namespaces/tank-slot-1/services":
		return items(item("/api/v1/namespaces/tank-slot-1/services/tank-operator", "tank-operator"))
	case "/api/v1/namespaces/tank-slot-1-sessions/pods":
		return items(item("/api/v1/namespaces/tank-slot-1-sessions/pods/session-4", "session-4"))
	default:
		if strings.Contains(path, "/jobs/glim-slot-apply-") {
			return `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
		}
		return `{"items":[]}`
	}
}

func nativeManifestEnv(manifest map[string]any) map[string]string {
	container := nativeManifestContainer(manifest)
	envRows := container["env"].([]map[string]any)
	env := map[string]string{}
	for _, row := range envRows {
		if value, ok := row["value"].(string); ok {
			env[row["name"].(string)] = value
		}
	}
	return env
}

func nativeManifestContainer(manifest map[string]any) map[string]any {
	spec := manifest["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	podSpec := template["spec"].(map[string]any)
	containers := podSpec["containers"].([]any)
	return containers[0].(map[string]any)
}

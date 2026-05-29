package agentops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/domain/hotswap"
)

type recordedCommand struct {
	Command
}

type fakeRunner struct {
	commands []recordedCommand
	outputs  []string
	results  []Result
	errors   []error
}

func (f *fakeRunner) Run(_ context.Context, command Command) (Result, error) {
	f.commands = append(f.commands, recordedCommand{Command: command})
	idx := len(f.commands) - 1
	result := Result{}
	if idx < len(f.results) {
		result = f.results[idx]
	}
	if idx < len(f.outputs) {
		result.Stdout = f.outputs[idx]
	}
	var err error
	if idx < len(f.errors) {
		err = f.errors[idx]
	}
	if err != nil && !command.AllowFailure {
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		return result, err
	}
	return result, nil
}

func TestBuildPreviewImageSkipsExistingACRTag(t *testing.T) {
	runner := &fakeRunner{outputs: []string{"issue-1", "issue-1"}}
	ops := &Ops{Runner: runner, RepoRoot: "/repo"}

	result, err := ops.BuildPreviewImage(context.Background(), "issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Image != "romainecr.azurecr.io/glimmung:issue-1" {
		t.Fatalf("unexpected image %q", result.Image)
	}
	if !result.SkippedBuild {
		t.Fatal("expected existing image tag to skip build")
	}
	for _, command := range runner.commands {
		if len(command.Args) >= 2 && reflect.DeepEqual(append([]string{command.Name}, command.Args[:2]...), []string{"az", "acr", "build"}) {
			t.Fatalf("unexpected build command: %#v", command)
		}
	}
}

func TestBuildPreviewImageBuildsMissingACRTag(t *testing.T) {
	runner := &fakeRunner{outputs: []string{"", "", "issue-1"}}
	ops := &Ops{Runner: runner, RepoRoot: "/repo"}

	result, err := ops.BuildPreviewImage(context.Background(), "issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.SkippedBuild {
		t.Fatal("expected missing image tag to build")
	}
	assertCommand(t, runner.commands[1].Command, "az", []string{
		"acr", "build",
		"--registry", "romainecr",
		"--image", "glimmung:issue-1",
		"/repo",
	})
}

func TestRebuildValidationImageUsesKubectlRolloutAndSkipsExistingBuild(t *testing.T) {
	runner := &fakeRunner{outputs: []string{"issue-1-r2", "", ""}}
	ops := &Ops{Runner: runner, RepoRoot: "/repo"}

	result, err := ops.RebuildValidationImage(context.Background(), RebuildValidationImageOptions{
		Release:   "issue-1",
		Namespace: "glimmung",
		Branch:    "glimmung/run-1",
		ImageTag:  "issue-1-r2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.SkippedBuild {
		t.Fatal("expected existing rebuild tag to skip build")
	}
	assertCommand(t, runner.commands[1].Command, "kubectl", []string{
		"-n", "glimmung", "set", "image",
		"deployment/issue-1", "glimmung=romainecr.azurecr.io/glimmung:issue-1-r2",
	})
	assertCommand(t, runner.commands[2].Command, "kubectl", []string{
		"-n", "glimmung", "rollout", "status",
		"deployment/issue-1", "--timeout=5m",
	})
}

func TestDeployPreviewCommandIncludesChartAndLabels(t *testing.T) {
	runner := &fakeRunner{outputs: []string{""}}
	ops := &Ops{Runner: runner, RepoRoot: "/repo"}

	result, err := ops.DeployPreview(context.Background(), DeployPreviewOptions{
		Release:    "issue-1",
		Namespace:  "glimmung",
		ImageTag:   "issue-1",
		PublicHost: "issue-1.example.test",
		PRNumber:   "123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://issue-1.example.test" {
		t.Fatalf("unexpected URL %q", result.URL)
	}
	assertCommand(t, runner.commands[0].Command, "helm", []string{
		"upgrade", "--install", "issue-1", "/repo/k8s/issue",
		"--namespace", "glimmung",
		"--set-string", "image.tag=issue-1",
		"--set-string", "hostname=issue-1.example.test",
		"--wait",
		"--timeout", "5m",
		"--set-string", "prNumber=123",
	})
}

func TestDestroyPreviewIgnoresHelmUninstallFailure(t *testing.T) {
	runner := &fakeRunner{errors: []error{errors.New("not found")}}
	ops := &Ops{Runner: runner, RepoRoot: "/repo"}

	result, err := ops.DestroyPreview(context.Background(), "issue-1", "glimmung")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Destroyed {
		t.Fatal("expected destroyed result even when helm uninstall fails")
	}
	if !runner.commands[0].AllowFailure {
		t.Fatal("expected helm uninstall to allow failure")
	}
}

func TestApplyAgentJobAppliesRenderedJobJSON(t *testing.T) {
	runner := &fakeRunner{outputs: []string{"job.batch/agent-1 created"}}
	ops := &Ops{Runner: runner, RepoRoot: "/repo"}

	result, err := ops.ApplyAgentJob(context.Background(), ApplyAgentJobOptions{
		Namespace:         "agent-ns",
		JobName:           "agent-1",
		IssueNumber:       "42",
		IssueTitle:        "do work",
		IssueURL:          "https://github.com/nelsong6/glimmung/issues/42",
		ValidationURL:     "https://issue-42.example.test",
		BranchName:        "glimmung/run-1",
		ProxyIP:           "10.0.0.4",
		AgentContainerTag: "agent-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != "job.batch/agent-1 created" {
		t.Fatalf("unexpected apply output %q", result.Applied)
	}
	assertCommand(t, runner.commands[0].Command, "kubectl", []string{"apply", "-f", "-"})

	var spec map[string]any
	if err := json.Unmarshal([]byte(runner.commands[0].Input), &spec); err != nil {
		t.Fatal(err)
	}
	if spec["kind"] != "Job" {
		t.Fatalf("unexpected spec kind %#v", spec["kind"])
	}
	metadata := spec["metadata"].(map[string]any)
	if metadata["name"] != "agent-1" || metadata["namespace"] != "agent-ns" {
		t.Fatalf("unexpected metadata %#v", metadata)
	}
	template := spec["spec"].(map[string]any)["template"].(map[string]any)
	podSpec := template["spec"].(map[string]any)
	containers := podSpec["containers"].([]any)
	container := containers[0].(map[string]any)
	if container["image"] != "romainecr.azurecr.io/agent-container:agent-v1" {
		t.Fatalf("unexpected image %#v", container["image"])
	}
	command := container["command"].([]any)
	if strings.Contains(command[2].(string), "===EVIDENCE-TAR-START===") {
		t.Fatal("agent script still contains the retired stdout-base64-tar evidence emission — see glimmung#143 follow-up")
	}
}

func TestTestSlotHotSwapBuildsCopiesAndRestartsBackend(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "app")
	if err := os.WriteFile(artifact, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: []string{"slot-pod", "build ok", "", "", "", "restarted"}}
	ops := &Ops{Runner: runner, RepoRoot: root}

	result, err := ops.TestSlotHotSwap(context.Background(), HotSwapOptions{
		Namespace: "tank-slot-1",
		Selector:  "app=tank-operator",
		Container: "tank-operator",
		Contract: hotswap.Contract{
			Enabled: true,
			Backend: hotswap.BackendContract{
				Enabled:      true,
				Strategy:     "supervisor",
				BuildCommand: "go build -o " + artifact + " ./backend-go/cmd/tank-operator",
				Artifact:     artifact,
				Target:       "/var/run/app-hot/app",
				HealthPath:   "/healthz",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pod != "slot-pod" || result.Backend == nil {
		t.Fatalf("result=%#v", result)
	}
	assertCommand(t, runner.commands[0].Command, "kubectl", []string{"-n", "tank-slot-1", "get", "pods", "-l", "app=tank-operator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}"})
	assertCommand(t, runner.commands[1].Command, "sh", []string{"-c", "go build -o " + artifact + " ./backend-go/cmd/tank-operator"})
	assertCommand(t, runner.commands[2].Command, "kubectl", []string{"-n", "tank-slot-1", "cp", artifact, "slot-pod:/var/run/app-hot/app.next", "-c", "tank-operator"})
	assertCommand(t, runner.commands[3].Command, "kubectl", []string{"-n", "tank-slot-1", "exec", "slot-pod", "-c", "tank-operator", "--", "sh", "-c", "chmod +x '/var/run/app-hot/app.next' && mv -f '/var/run/app-hot/app.next' '/var/run/app-hot/app'"})
	assertCommand(t, runner.commands[4].Command, "kubectl", []string{"-n", "tank-slot-1", "exec", "slot-pod", "-c", "tank-operator", "--", "sh", "-c", "kill -HUP 1"})
	assertCommand(t, runner.commands[5].Command, "kubectl", []string{"-n", "tank-slot-1", "logs", "slot-pod", "--tail=120", "-c", "tank-operator"})
}

func TestTestSlotHotSwapCanCopyAndRestartDifferentContainers(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "app")
	if err := os.WriteFile(artifact, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: []string{"slot-pod", "build ok", "", "", "", "restarted"}}
	ops := &Ops{Runner: runner, RepoRoot: root}

	_, err := ops.TestSlotHotSwap(context.Background(), HotSwapOptions{
		Namespace: "ambience-slot-1",
		Selector:  "app=ambience,component=authority",
		Container: "ambience",
		Contract: hotswap.Contract{
			Enabled: true,
			Backend: hotswap.BackendContract{
				Enabled:          true,
				Strategy:         "supervisor",
				BuildCommand:     "go build -o " + artifact + " ./cmd/ambience",
				Artifact:         artifact,
				Target:           "/var/run/ambience-hot/ambience",
				HealthPath:       "/healthz",
				CopyContainer:    "hot-writer",
				RestartContainer: "ambience",
				RestartCommand:   []string{"/ambience-supervisor", "signal"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCommand(t, runner.commands[2].Command, "kubectl", []string{"-n", "ambience-slot-1", "cp", artifact, "slot-pod:/var/run/ambience-hot/ambience.next", "-c", "hot-writer"})
	assertCommand(t, runner.commands[3].Command, "kubectl", []string{"-n", "ambience-slot-1", "exec", "slot-pod", "-c", "hot-writer", "--", "sh", "-c", "chmod +x '/var/run/ambience-hot/ambience.next' && mv -f '/var/run/ambience-hot/ambience.next' '/var/run/ambience-hot/ambience'"})
	assertCommand(t, runner.commands[4].Command, "kubectl", []string{"-n", "ambience-slot-1", "exec", "slot-pod", "-c", "ambience", "--", "/ambience-supervisor", "signal"})
	assertCommand(t, runner.commands[5].Command, "kubectl", []string{"-n", "ambience-slot-1", "logs", "slot-pod", "--tail=120", "-c", "ambience"})
}

func TestTestSlotHotSwapCopiesStaticOnly(t *testing.T) {
	root := t.TempDir()
	staticDir := filepath.Join(root, "frontend", "dist")
	if err := os.MkdirAll(staticDir, 0755); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: []string{""}}
	ops := &Ops{Runner: runner, RepoRoot: root}

	result, err := ops.TestSlotHotSwap(context.Background(), HotSwapOptions{
		Namespace:  "ambience-slot-1",
		Pod:        "ambience-pod",
		StaticOnly: true,
		Contract: hotswap.Contract{
			Enabled: true,
			Static: hotswap.StaticContract{
				Enabled: true,
				Source:  "frontend/dist",
				Target:  "/var/run/app-static-override",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Static == nil || result.Backend != nil {
		t.Fatalf("result=%#v", result)
	}
	assertCommand(t, runner.commands[0].Command, "kubectl", []string{"-n", "ambience-slot-1", "cp", staticDir + string(filepath.Separator) + ".", "ambience-pod:/var/run/app-static-override"})
}

func TestTestSlotHotSwapCopiesStaticToAllSelectedPods(t *testing.T) {
	root := t.TempDir()
	staticDir := filepath.Join(root, "frontend", "dist")
	if err := os.MkdirAll(staticDir, 0755); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: []string{"pod-a\npod-b", "", ""}}
	ops := &Ops{Runner: runner, RepoRoot: root}

	result, err := ops.TestSlotHotSwap(context.Background(), HotSwapOptions{
		Namespace: "tank-slot-1",
		Selector:  "app=tank-operator",
		Contract: hotswap.Contract{
			Enabled: true,
			Static:  hotswap.StaticContract{Enabled: true, Source: "frontend/dist", Target: "/var/run/static"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Pods, []string{"pod-a", "pod-b"}) {
		t.Fatalf("pods=%#v", result.Pods)
	}
	assertCommand(t, runner.commands[1].Command, "kubectl", []string{"-n", "tank-slot-1", "cp", staticDir + string(filepath.Separator) + ".", "pod-a:/var/run/static"})
	assertCommand(t, runner.commands[2].Command, "kubectl", []string{"-n", "tank-slot-1", "cp", staticDir + string(filepath.Separator) + ".", "pod-b:/var/run/static"})
}

func TestTestSlotHotSwapRejectsImageRequiredChanges(t *testing.T) {
	ops := &Ops{Runner: &fakeRunner{}, RepoRoot: t.TempDir()}

	result, err := ops.TestSlotHotSwap(context.Background(), HotSwapOptions{
		Namespace:    "tank-slot-1",
		Pod:          "tank-pod",
		ChangedFiles: []string{"Dockerfile"},
		Contract: hotswap.Contract{
			Enabled: true,
			Static:  hotswap.StaticContract{Enabled: true, Source: "frontend/dist", Target: "/var/run/static"},
		},
	})
	if err == nil {
		t.Fatal("expected image-required rejection")
	}
	if !result.ChangeClassification.NeedsImage {
		t.Fatalf("classification=%#v", result.ChangeClassification)
	}
}

func assertCommand(t *testing.T, command Command, name string, args []string) {
	t.Helper()
	if command.Name != name {
		t.Fatalf("command name = %q, want %q", command.Name, name)
	}
	if !reflect.DeepEqual(command.Args, args) {
		t.Fatalf("command args = %#v, want %#v", command.Args, args)
	}
}

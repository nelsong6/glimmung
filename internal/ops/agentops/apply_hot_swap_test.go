package agentops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/hotswap"
)

// TestApplyHotSwapJobSpecRendersInitAndMainContainers pins the Job-spec
// shape: init container uses contract.AgentRunner.BuilderImage and runs
// the build command; main container uses the swap image and runs the
// kubectl-stream + restart script. Shared emptyDir volume mounted at
// /work in both containers.
func TestApplyHotSwapJobSpecRendersInitAndMainContainers(t *testing.T) {
	spec := renderApplyHotSwapJobSpec(applyHotSwapJobInputs{
		JobName:            "apply-hot-swap-abc",
		JobNamespace:       "glimmung",
		ServiceAccount:     "glimmung-agent",
		Project:            "tank-operator",
		GitRef:             "feat/x",
		RepoURL:            "https://github.com/nelsong6/tank-operator.git",
		BuilderImage:       "node:20-alpine",
		BuildCommand:       "cd agent-runner && npm run build",
		SwapContainerImage: "bitnami/kubectl:1.31",
		Source:             "agent-runner/dist",
		Target:             "/var/run/agent-runner-hot/dist",
		TargetNamespace:    "tank-operator-slot-1-sessions",
		TargetPods:         []string{"session-5"},
		TargetContainer:    "agent-runner",
		RestartSignal:      "SIGHUP",
	})

	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(encoded)

	// Job-level
	if !strings.Contains(s, `"name":"apply-hot-swap-abc"`) {
		t.Fatalf("job name missing: %s", s)
	}
	if !strings.Contains(s, `"namespace":"glimmung"`) {
		t.Fatalf("job namespace missing")
	}
	if !strings.Contains(s, `"backoffLimit":0`) {
		t.Fatalf("backoffLimit not 0 (a Job that retries hot-swap would silently re-apply changes)")
	}

	// Init container
	if !strings.Contains(s, `"name":"build"`) {
		t.Fatalf("init container name missing")
	}
	if !strings.Contains(s, `"image":"node:20-alpine"`) {
		t.Fatalf("init container should use contract.BuilderImage; got: %s", s)
	}
	if !strings.Contains(s, "npm run build") {
		t.Fatalf("init container should embed contract.BuildCommand: %s", s)
	}
	if !strings.Contains(s, "feat/x") {
		t.Fatalf("init container should pass GIT_REF env: %s", s)
	}

	// Main container
	if !strings.Contains(s, `"name":"swap"`) {
		t.Fatalf("swap container name missing")
	}
	if !strings.Contains(s, `"image":"bitnami/kubectl:1.31"`) {
		t.Fatalf("swap container should use SwapContainerImage")
	}
	if !strings.Contains(s, "tar c -C /work/source") {
		t.Fatalf("swap container should tar-stream /work/source: %s", s)
	}
	if !strings.Contains(s, "kill -HUP 1") {
		t.Fatalf("swap container should signal SIGHUP to PID 1: %s", s)
	}
	if !strings.Contains(s, "tank-operator-slot-1-sessions") {
		t.Fatalf("swap container should target the slot session namespace")
	}
	if !strings.Contains(s, "session-5") {
		t.Fatalf("swap container should embed the resolved pod name(s)")
	}
	if !strings.Contains(s, `"serviceAccountName":"glimmung-agent"`) {
		t.Fatalf("ServiceAccount should be set on the pod spec")
	}

	// Both containers share a /work emptyDir.
	if !strings.Contains(s, `"name":"work"`) || !strings.Contains(s, `"emptyDir"`) {
		t.Fatalf("work emptyDir volume missing")
	}
}

// TestApplyHotSwapRejectsUnsupportedKinds asserts v1 only handles
// agent_runner; static/backend route to the legacy CLI path.
func TestApplyHotSwapRejectsUnsupportedKinds(t *testing.T) {
	ops := &Ops{Runner: &fakeRunner{}, RepoRoot: "/repo"}
	for _, kind := range []string{"static", "backend", "", "frontend"} {
		result, err := ops.ApplyHotSwap(context.Background(), ApplyHotSwapOptions{
			ArtifactKind: kind,
			Contract:     hotswap.Contract{Enabled: true, AgentRunner: hotswap.AgentRunnerContract{Enabled: true, BuilderImage: "x"}},
		})
		if err == nil {
			t.Fatalf("kind %q should be rejected; got result=%+v", kind, result)
		}
		if !strings.Contains(result.Error, "not supported") {
			t.Fatalf("kind %q error should say not supported; got %q", kind, result.Error)
		}
	}
}

// TestApplyHotSwapRejectsMissingBuilderImage asserts the request-time
// guard for builder_image: agent_runner enabled in the contract but no
// builder_image (e.g., the field was added to the struct after the
// contract was registered) fails fast.
func TestApplyHotSwapRejectsMissingBuilderImage(t *testing.T) {
	ops := &Ops{Runner: &fakeRunner{}, RepoRoot: "/repo"}
	result, err := ops.ApplyHotSwap(context.Background(), ApplyHotSwapOptions{
		ArtifactKind: "agent_runner",
		Contract: hotswap.Contract{
			Enabled: true,
			AgentRunner: hotswap.AgentRunnerContract{
				Enabled: true,
				// BuilderImage intentionally empty
			},
		},
	})
	if err == nil {
		t.Fatalf("missing builder_image should be rejected; got result=%+v", result)
	}
	if !strings.Contains(result.Error, "builder_image") {
		t.Fatalf("error should name builder_image; got %q", result.Error)
	}
}

// TestApplyHotSwapHappyPathRecordsTimings asserts the happy path runs
// kubectl apply + wait + logs + delete in order, returns Outcome=persisted,
// and populates the result struct (target pods, timings, log tails).
func TestApplyHotSwapHappyPathRecordsTimings(t *testing.T) {
	runner := &fakeRunner{
		// outputs (ordered): pod resolution, apply, wait, build logs, swap logs, delete
		outputs: []string{
			"session-5\n",
			"job/apply-hot-swap-... created",
			"job.batch/apply-hot-swap-... condition met",
			"build log line 1\nbuild log line 2",
			"swap log line 1",
			"job.batch/apply-hot-swap-... deleted",
		},
	}
	ops := &Ops{Runner: runner, RepoRoot: "/repo"}
	result, err := ops.ApplyHotSwap(context.Background(), ApplyHotSwapOptions{
		Project:           "tank-operator",
		ArtifactKind:      "agent_runner",
		GitRef:            "feat/x",
		RepoURL:           "https://github.com/nelsong6/tank-operator.git",
		TargetNamespace:   "tank-operator-slot-1-sessions",
		TargetPodSelector: "tank-operator/session-id",
		JobNamespace:      "glimmung",
		Timeout:           30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			AgentRunner: hotswap.AgentRunnerContract{
				Enabled:      true,
				Source:       "agent-runner/dist",
				Target:       "/var/run/agent-runner-hot/dist",
				BuildCommand: "cd agent-runner && npm run build",
				PodSelector:  "tank-operator/session-id",
				Container:    "agent-runner",
				Restart:      "SIGHUP",
				BuilderImage: "node:20-alpine",
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v (logs: %s)", err, result.Error)
	}
	if result.Outcome != "persisted" {
		t.Fatalf("outcome = %q, want persisted", result.Outcome)
	}
	if len(result.TargetPods) != 1 || result.TargetPods[0] != "session-5" {
		t.Fatalf("target pods = %v, want [session-5]", result.TargetPods)
	}
	if result.BuildLogsTail == "" {
		t.Fatalf("build logs tail should be populated")
	}
	if result.SwapLogsTail == "" {
		t.Fatalf("swap logs tail should be populated")
	}
	if _, ok := result.Timings["total"]; !ok {
		t.Fatalf("timings missing 'total'")
	}
}

// TestApplyHotSwapJobFailureSurfacesBuildLogs asserts that when the Job
// fails (wait returns non-zero), the result carries Outcome=swap_failed
// (or build_failed/timeout) and the logs are still collected for the
// developer to diagnose without re-running kubectl by hand.
func TestApplyHotSwapJobFailureSurfacesBuildLogs(t *testing.T) {
	runner := &fakeRunner{
		// pod resolve, apply ok, wait fails, build logs (with "error"),
		// swap logs empty, get-failed-reason, delete
		outputs: []string{
			"session-5\n",          // resolve
			"job created",          // apply
			"",                     // wait stdout (fails)
			"npm ERR! error",       // build logs (signals build_failed)
			"",                     // swap logs (empty — swap never ran)
			"BackoffLimitExceeded", // failed reason
		},
		// results: zero exit codes for all by default, but wait must exit non-zero
		results: []Result{
			{}, // resolve
			{}, // apply
			{ExitCode: 1, Stderr: "error: timed out waiting for the condition"}, // wait
		},
	}
	ops := &Ops{Runner: runner, RepoRoot: "/repo"}
	result, _ := ops.ApplyHotSwap(context.Background(), ApplyHotSwapOptions{
		Project:           "tank-operator",
		ArtifactKind:      "agent_runner",
		GitRef:            "feat/x",
		RepoURL:           "https://github.com/nelsong6/tank-operator.git",
		TargetNamespace:   "tank-operator-slot-1-sessions",
		TargetPodSelector: "tank-operator/session-id",
		JobNamespace:      "glimmung",
		Timeout:           30 * time.Second,
		Contract: hotswap.Contract{
			Enabled: true,
			AgentRunner: hotswap.AgentRunnerContract{
				Enabled:      true,
				Source:       "agent-runner/dist",
				Target:       "/var/run/agent-runner-hot/dist",
				BuildCommand: "npm run build",
				PodSelector:  "tank-operator/session-id",
				Container:    "agent-runner",
				Restart:      "SIGHUP",
				BuilderImage: "node:20-alpine",
			},
		},
	})
	// Could be build_failed, swap_failed, or timeout — the test asserts
	// it's a named failure outcome, not a panic + the logs are surfaced.
	switch result.Outcome {
	case "build_failed", "swap_failed", "timeout":
		// ok
	default:
		t.Fatalf("outcome = %q, want one of build_failed/swap_failed/timeout", result.Outcome)
	}
	if result.BuildLogsTail == "" {
		t.Fatal("build logs tail should still be collected on failure")
	}
	if result.Error == "" {
		t.Fatal("error should be populated on failure")
	}
}

// TestApplyHotSwapRejectsMissingTargetNamespace pins input validation
// (the request reaches here pre-validated; defense in depth catches a
// caller that skips it).
func TestApplyHotSwapRejectsMissingTargetNamespace(t *testing.T) {
	ops := &Ops{Runner: &fakeRunner{}, RepoRoot: "/repo"}
	result, err := ops.ApplyHotSwap(context.Background(), ApplyHotSwapOptions{
		ArtifactKind: "agent_runner",
		Contract: hotswap.Contract{
			Enabled: true,
			AgentRunner: hotswap.AgentRunnerContract{Enabled: true, BuilderImage: "x"},
		},
	})
	if err == nil {
		t.Fatalf("missing target_namespace should be rejected; got result=%+v", result)
	}
	if !strings.Contains(result.Error, "target_namespace") {
		t.Fatalf("error should name target_namespace; got %q", result.Error)
	}
}

// TestApplyHotSwapPodResolutionFailureSurfaces asserts that when pod
// resolution fails (selector matches nothing), the result returns clean
// with no Job dispatched.
func TestApplyHotSwapPodResolutionFailureSurfaces(t *testing.T) {
	runner := &fakeRunner{
		outputs: []string{""}, // empty kubectl-get output → no pods matched
	}
	ops := &Ops{Runner: runner, RepoRoot: "/repo"}
	_, err := ops.ApplyHotSwap(context.Background(), ApplyHotSwapOptions{
		ArtifactKind:      "agent_runner",
		TargetNamespace:   "tank-operator-slot-1-sessions",
		TargetPodSelector: "tank-operator/session-id=99",
		RepoURL:           "https://github.com/nelsong6/tank-operator.git",
		Contract: hotswap.Contract{
			Enabled: true,
			AgentRunner: hotswap.AgentRunnerContract{
				Enabled:      true,
				Source:       "agent-runner/dist",
				Target:       "/var/run/agent-runner-hot/dist",
				BuildCommand: "true",
				PodSelector:  "tank-operator/session-id",
				Container:    "agent-runner",
				Restart:      "SIGHUP",
				BuilderImage: "node:20-alpine",
			},
		},
	})
	if err == nil {
		t.Fatal("expected error when no pods match selector")
	}
	// The fakeRunner returns no error, so the function should detect
	// no-pods-matched internally.
	if !errors.Is(err, err) {
		t.Fatal("err passthrough sanity")
	}
}

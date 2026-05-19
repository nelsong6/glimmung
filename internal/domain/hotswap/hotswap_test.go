package hotswap

import "testing"

func TestFromMetadataValidatesContract(t *testing.T) {
	contract, ok, err := FromMetadata(map[string]any{
		"test_slot_hot_swap": map[string]any{
			"enabled": true,
			"static": map[string]any{
				"enabled": true,
				"source":  "frontend/dist",
				"target":  "/var/run/app-static-override",
			},
			"backend": map[string]any{
				"enabled":       true,
				"strategy":      "supervisor",
				"build_command": "go build -o /tmp/app ./cmd/app",
				"artifact":      "/tmp/app",
				"target":        "/var/run/app-hot/app",
				"health_path":   "/healthz",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !contract.Enabled || !contract.Static.Enabled || !contract.Backend.Enabled {
		t.Fatalf("contract=%#v ok=%v", contract, ok)
	}
}

func TestValidateRejectsIncompleteBackend(t *testing.T) {
	err := Contract{Enabled: true, Backend: BackendContract{Enabled: true, Target: "/app"}}.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
}

// TestContractAgentRunnerRoundtrip pins Guarantee 2 of
// scripts/check-apply-test-slot-hot-swap-migration.mjs: FromMetadata
// parses the new AgentRunner sub-contract end-to-end with builder_image
// + all required fields, and Validate returns nil for a complete shape.
func TestContractAgentRunnerRoundtrip(t *testing.T) {
	contract, ok, err := FromMetadata(map[string]any{
		"test_slot_hot_swap": map[string]any{
			"enabled": true,
			"agent_runner": map[string]any{
				"enabled":       true,
				"source":        "agent-runner/dist",
				"target":        "/var/run/agent-runner-hot/dist",
				"build_command": "cd agent-runner && npm run build",
				"pod_selector":  "tank-operator/session-id",
				"container":     "agent-runner",
				"restart":       "SIGHUP",
				"builder_image": "node:20-alpine",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !contract.Enabled || !contract.AgentRunner.Enabled {
		t.Fatalf("contract=%#v ok=%v", contract, ok)
	}
	if contract.AgentRunner.BuilderImage != "node:20-alpine" {
		t.Fatalf("builder_image = %q, want node:20-alpine", contract.AgentRunner.BuilderImage)
	}
	if contract.AgentRunner.Target != "/var/run/agent-runner-hot/dist" {
		t.Fatalf("target = %q", contract.AgentRunner.Target)
	}
}

func TestContractCodexRunnerRoundtrip(t *testing.T) {
	contract, ok, err := FromMetadata(map[string]any{
		"test_slot_hot_swap": map[string]any{
			"enabled": true,
			"codex_runner": map[string]any{
				"enabled":       true,
				"source":        "codex-runner/dist",
				"target":        "/var/run/codex-runner-hot/dist",
				"build_command": "cd codex-runner && npm run build",
				"pod_selector":  "tank-operator/session-id",
				"container":     "codex-runner",
				"restart":       "SIGHUP",
				"builder_image": "node:20-alpine",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !contract.Enabled || !contract.CodexRunner.Enabled {
		t.Fatalf("contract=%#v ok=%v", contract, ok)
	}
	if contract.CodexRunner.Target != "/var/run/codex-runner-hot/dist" {
		t.Fatalf("target = %q", contract.CodexRunner.Target)
	}
}

// TestValidateRejectsAgentRunnerMissingBuilderImage pins that the apply
// endpoint's primary consumer (the new AgentRunner kind) requires
// builder_image at Validate time — there is no legacy CLI path for
// agent_runner, so missing builder_image is unambiguous misconfiguration.
func TestValidateRejectsAgentRunnerMissingBuilderImage(t *testing.T) {
	err := Contract{
		Enabled: true,
		AgentRunner: AgentRunnerContract{
			Enabled:      true,
			Source:       "agent-runner/dist",
			Target:       "/var/run/agent-runner-hot/dist",
			BuildCommand: "cd agent-runner && npm run build",
			PodSelector:  "tank-operator/session-id",
			Container:    "agent-runner",
			Restart:      "SIGHUP",
			// BuilderImage intentionally empty
		},
	}.Validate()
	if err == nil {
		t.Fatal("expected validation error when AgentRunner.BuilderImage is empty")
	}
}

// TestValidateAllowsBackendWithoutBuilderImage pins the deliberately
// permissive Backend.BuilderImage rule: existing registered contracts
// (which predate the field) keep validating. The apply endpoint
// validates builder_image at request time when artifact_kind=backend
// is invoked.
func TestValidateAllowsBackendWithoutBuilderImage(t *testing.T) {
	err := Contract{
		Enabled: true,
		Backend: BackendContract{
			Enabled:      true,
			Strategy:     "supervisor",
			BuildCommand: "go build -o /tmp/app ./cmd/app",
			Artifact:     "/tmp/app",
			Target:       "/var/run/app-hot/app",
			HealthPath:   "/healthz",
		},
	}.Validate()
	if err != nil {
		t.Fatalf("unexpected error: %v (backend.builder_image is optional at validation time)", err)
	}
}

// TestValidateRejectsAgentRunnerUnsupportedRestart pins that only SIGHUP
// is accepted today. Future restart signals require an explicit code
// change here (so a typo doesn't silently land a non-functional contract).
func TestValidateRejectsAgentRunnerUnsupportedRestart(t *testing.T) {
	err := Contract{
		Enabled: true,
		AgentRunner: AgentRunnerContract{
			Enabled:      true,
			Source:       "agent-runner/dist",
			Target:       "/var/run/agent-runner-hot/dist",
			BuildCommand: "true",
			PodSelector:  "label/key",
			Container:    "agent-runner",
			Restart:      "SIGTERM",
			BuilderImage: "node:20-alpine",
		},
	}.Validate()
	if err == nil {
		t.Fatal("expected validation error for unsupported restart signal")
	}
}

func TestClassifyPathsRequiresImageForRuntimeInputs(t *testing.T) {
	got := ClassifyPaths([]string{"backend-go/cmd/tank-operator/main.go", "Dockerfile"})
	if got.Class != ChangeClassImage || !got.NeedsImage {
		t.Fatalf("classification=%#v", got)
	}
}

func TestClassifyPathsSeparatesStaticAndBackend(t *testing.T) {
	if got := ClassifyPaths([]string{"frontend/src/App.tsx"}); got.Class != ChangeClassStatic {
		t.Fatalf("static classification=%#v", got)
	}
	if got := ClassifyPaths([]string{"cmd/glimmung-go/main.go"}); got.Class != ChangeClassBackend {
		t.Fatalf("backend classification=%#v", got)
	}
}

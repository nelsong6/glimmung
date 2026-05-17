package hotswap

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

type ChangeClass string

const (
	ChangeClassNone    ChangeClass = "none"
	ChangeClassStatic  ChangeClass = "static"
	ChangeClassBackend ChangeClass = "backend"
	ChangeClassImage   ChangeClass = "image"
)

type ChangeClassification struct {
	Class      ChangeClass `json:"class"`
	NeedsImage bool        `json:"needs_image"`
	Reasons    []string    `json:"reasons"`
}

type Contract struct {
	Enabled     bool                `json:"enabled"`
	Static      StaticContract      `json:"static"`
	Backend     BackendContract     `json:"backend"`
	AgentRunner AgentRunnerContract `json:"agent_runner"`
}

type StaticContract struct {
	Enabled      bool   `json:"enabled"`
	Source       string `json:"source"`
	Target       string `json:"target"`
	// BuilderImage names the OCI image used as the init container in the
	// apply_test_slot_hot_swap Job. The project owns this choice — see
	// docs/test-slot-hot-swap.md. Required when the project consumes the
	// apply endpoint, not by the legacy glimmung-agent CLI path.
	BuilderImage string `json:"builder_image,omitempty"`
}

type BackendContract struct {
	Enabled          bool     `json:"enabled"`
	Strategy         string   `json:"strategy"`
	BuildCommand     string   `json:"build_command"`
	Artifact         string   `json:"artifact"`
	Target           string   `json:"target"`
	HealthPath       string   `json:"health_path"`
	CopyContainer    string   `json:"copy_container"`
	RestartContainer string   `json:"restart_container"`
	RestartCommand   []string `json:"restart_command"`
	// BuilderImage: see StaticContract.BuilderImage.
	BuilderImage string `json:"builder_image,omitempty"`
}

// AgentRunnerContract describes the test-slot hot-swap shape for a
// runner container inside a session pod (the analog of BackendContract
// for the orchestrator pod). The runner's code lives on a writable
// volume inside the session pod; the apply endpoint copies a freshly
// built source directory into that volume and signals a supervisor to
// re-exec. Distinguished from BackendContract by: (a) the artifact is a
// directory of files (e.g., agent-runner/dist/), not a single binary;
// (b) the target pod is selected by a pod_selector label, not by being
// the orchestrator's own pod.
//
// Lands alongside Static and Backend; existing sub-contracts unchanged.
type AgentRunnerContract struct {
	Enabled bool   `json:"enabled"`
	// Source is the directory inside the cloned repo whose contents
	// (recursively) are streamed into Target inside the session pod.
	Source string `json:"source"`
	// Target is the absolute container path receiving Source's contents.
	// Typically a writable volume the supervisor's hot-artifact resolution
	// points at (e.g., /var/run/agent-runner-hot/dist).
	Target string `json:"target"`
	// BuildCommand runs in the init container's working directory (the
	// cloned repo root) before the copy step. Empty means "no build,
	// source is consumed as-is."
	BuildCommand string `json:"build_command"`
	// PodSelector is the label selector that resolves the target session
	// pod (e.g., "tank-operator/session-id"). The apply endpoint expects
	// the caller to constrain further (e.g., a specific session_id) — the
	// selector is the field naming the dimension, not the value.
	PodSelector string `json:"pod_selector"`
	// Container names the pod container that owns the writable target
	// volume (e.g., "agent-runner"). Required.
	Container string `json:"container"`
	// Restart is the signal-string sent to PID 1 of Container after the
	// copy step. Today only "SIGHUP" is supported; future signals can be
	// added without a contract break.
	Restart string `json:"restart"`
	// BuilderImage: see StaticContract.BuilderImage.
	BuilderImage string `json:"builder_image,omitempty"`
}

func FromMetadata(metadata map[string]any) (Contract, bool, error) {
	raw, ok := metadata["test_slot_hot_swap"]
	if !ok {
		raw, ok = metadata["testSlotHotSwap"]
	}
	if !ok {
		return Contract{}, false, nil
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return Contract{}, true, err
	}
	var contract Contract
	if err := json.Unmarshal(payload, &contract); err != nil {
		return Contract{}, true, err
	}
	return contract, true, contract.Validate()
}

func ParseJSON(raw string) (Contract, error) {
	var contract Contract
	if err := json.Unmarshal([]byte(raw), &contract); err != nil {
		return Contract{}, err
	}
	return contract, contract.Validate()
}

func (c Contract) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !c.Static.Enabled && !c.Backend.Enabled && !c.AgentRunner.Enabled {
		return errors.New("test_slot_hot_swap must enable static, backend, or agent_runner")
	}
	if c.Static.Enabled {
		if strings.TrimSpace(c.Static.Source) == "" {
			return errors.New("test_slot_hot_swap.static.source is required")
		}
		if strings.TrimSpace(c.Static.Target) == "" {
			return errors.New("test_slot_hot_swap.static.target is required")
		}
		if !strings.HasPrefix(strings.TrimSpace(c.Static.Target), "/") {
			return errors.New("test_slot_hot_swap.static.target must be an absolute container path")
		}
	}
	if c.Backend.Enabled {
		if strategy := strings.TrimSpace(c.Backend.Strategy); strategy != "" && strategy != "supervisor" {
			return fmt.Errorf("test_slot_hot_swap.backend.strategy %q is not supported", strategy)
		}
		required := []struct {
			name  string
			value string
		}{
			{name: "build_command", value: c.Backend.BuildCommand},
			{name: "artifact", value: c.Backend.Artifact},
			{name: "target", value: c.Backend.Target},
			{name: "health_path", value: c.Backend.HealthPath},
		}
		for _, field := range required {
			if strings.TrimSpace(field.value) == "" {
				return fmt.Errorf("test_slot_hot_swap.backend.%s is required", field.name)
			}
		}
		if !isLocalAbsolutePath(strings.TrimSpace(c.Backend.Artifact)) {
			return errors.New("test_slot_hot_swap.backend.artifact must be an absolute local path")
		}
		if !strings.HasPrefix(strings.TrimSpace(c.Backend.Target), "/") {
			return errors.New("test_slot_hot_swap.backend.target must be an absolute container path")
		}
		if !strings.HasPrefix(strings.TrimSpace(c.Backend.HealthPath), "/") {
			return errors.New("test_slot_hot_swap.backend.health_path must start with /")
		}
	}
	if c.Static.Enabled {
		// Static doesn't always need a build step (frontends with prebuilt
		// dists do; pure static asset trees don't), so builder_image is
		// not required here. The apply endpoint will reject a build-required
		// static swap at request time if builder_image is unset.
		_ = c.Static.BuilderImage
	}
	if c.AgentRunner.Enabled {
		required := []struct {
			name  string
			value string
		}{
			{name: "source", value: c.AgentRunner.Source},
			{name: "target", value: c.AgentRunner.Target},
			{name: "build_command", value: c.AgentRunner.BuildCommand},
			{name: "pod_selector", value: c.AgentRunner.PodSelector},
			{name: "container", value: c.AgentRunner.Container},
			{name: "restart", value: c.AgentRunner.Restart},
			// builder_image is required for AgentRunner because there's
			// no legacy CLI path that runs without it — the only consumer
			// of agent_runner is the apply endpoint, which needs the
			// image to provision the init container. We require it at
			// validation time so the registration call fails fast on
			// missing config. Backend's builder_image stays optional at
			// validation time because the existing glimmung-agent CLI
			// path doesn't need it; the apply endpoint validates it at
			// request time. See ApplyTestSlotHotSwap.
			{name: "builder_image", value: c.AgentRunner.BuilderImage},
		}
		for _, field := range required {
			if strings.TrimSpace(field.value) == "" {
				return fmt.Errorf("test_slot_hot_swap.agent_runner.%s is required", field.name)
			}
		}
		if !strings.HasPrefix(strings.TrimSpace(c.AgentRunner.Target), "/") {
			return errors.New("test_slot_hot_swap.agent_runner.target must be an absolute container path")
		}
		if restart := strings.TrimSpace(c.AgentRunner.Restart); restart != "SIGHUP" {
			return fmt.Errorf("test_slot_hot_swap.agent_runner.restart %q is not supported (only SIGHUP today)", restart)
		}
	}
	return nil
}

func isLocalAbsolutePath(value string) bool {
	return filepath.IsAbs(value) || strings.HasPrefix(value, "/")
}

func ClassifyPaths(paths []string) ChangeClassification {
	out := ChangeClassification{Class: ChangeClassNone}
	for _, raw := range paths {
		path := strings.Trim(strings.TrimSpace(raw), "/")
		if path == "" {
			continue
		}
		switch {
		case requiresImage(path):
			out.Class = ChangeClassImage
			out.NeedsImage = true
			out.Reasons = append(out.Reasons, path+": image/runtime input changed")
		case out.Class != ChangeClassImage && isBackend(path):
			out.Class = ChangeClassBackend
			out.Reasons = append(out.Reasons, path+": compiled backend change")
		case out.Class == ChangeClassNone && isStatic(path):
			out.Class = ChangeClassStatic
			out.Reasons = append(out.Reasons, path+": static asset change")
		}
	}
	return out
}

func requiresImage(path string) bool {
	base := path[strings.LastIndex(path, "/")+1:]
	if base == "Dockerfile" || strings.HasPrefix(path, ".github/workflows/") {
		return true
	}
	for _, suffix := range []string{
		"go.mod", "go.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock",
		"requirements.txt", "pyproject.toml", "uv.lock",
	} {
		if base == suffix {
			return true
		}
	}
	return strings.HasPrefix(path, "k8s/") || strings.HasPrefix(path, "chart/")
}

func isBackend(path string) bool {
	return strings.HasSuffix(path, ".go") ||
		strings.HasPrefix(path, "backend-go/") ||
		strings.HasPrefix(path, "cmd/")
}

func isStatic(path string) bool {
	return strings.HasPrefix(path, "frontend/") ||
		strings.HasPrefix(path, "cmd/ambience/web/") ||
		strings.HasSuffix(path, ".html") ||
		strings.HasSuffix(path, ".css") ||
		strings.HasSuffix(path, ".js") ||
		strings.HasSuffix(path, ".ts") ||
		strings.HasSuffix(path, ".tsx")
}

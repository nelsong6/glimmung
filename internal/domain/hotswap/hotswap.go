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
	Enabled bool            `json:"enabled"`
	Static  StaticContract  `json:"static"`
	Backend BackendContract `json:"backend"`
}

type StaticContract struct {
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"`
	Target  string `json:"target"`
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
	if !c.Static.Enabled && !c.Backend.Enabled {
		return errors.New("test_slot_hot_swap must enable static or backend")
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

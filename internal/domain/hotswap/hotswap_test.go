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

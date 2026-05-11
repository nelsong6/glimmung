package phaserefs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type goldenCase struct {
	Name       string `json:"name"`
	Value      string `json:"value"`
	WantParsed bool   `json:"want_parsed"`
	WantPhase  string `json:"want_phase"`
	WantKey    string `json:"want_key"`
}

func TestPhaseRefGoldenParity(t *testing.T) {
	for _, tc := range loadGoldenCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			got, ok := Parse(tc.Value)
			if ok != tc.WantParsed {
				t.Fatalf("parsed=%v, want %v", ok, tc.WantParsed)
			}
			if !ok {
				return
			}
			if got.Phase != tc.WantPhase || got.Key != tc.WantKey {
				t.Fatalf("got (%q, %q), want (%q, %q)", got.Phase, got.Key, tc.WantPhase, tc.WantKey)
			}
		})
	}
}

func TestValidateAcceptsEarlierDeclaredOutput(t *testing.T) {
	phases := []Phase{
		{Name: "env-prep", Outputs: []string{"validation_url", "image_tag"}},
		{
			Name: "agent-execute",
			Inputs: map[string]string{
				"validation_url": "${{ phases.env-prep.outputs.validation_url }}",
			},
		},
	}
	if err := Validate(phases); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestValidateRejectsSelfForwardUnknownMalformedAndUndeclaredRefs(t *testing.T) {
	tests := []struct {
		name   string
		phases []Phase
		want   string
	}{
		{
			name: "self ref",
			phases: []Phase{
				{Name: "agent", Inputs: map[string]string{"x": "${{ phases.agent.outputs.x }}"}, Outputs: []string{"x"}},
			},
			want: "refs itself",
		},
		{
			name: "forward ref",
			phases: []Phase{
				{Name: "agent", Inputs: map[string]string{"x": "${{ phases.cleanup.outputs.x }}"}},
				{Name: "cleanup", Outputs: []string{"x"}},
			},
			want: "doesn't appear earlier",
		},
		{
			name: "unknown phase",
			phases: []Phase{
				{Name: "a", Outputs: []string{"x"}},
				{Name: "b", Inputs: map[string]string{"x": "${{ phases.nonexistent.outputs.x }}"}},
			},
			want: "doesn't appear earlier",
		},
		{
			name: "undeclared output",
			phases: []Phase{
				{Name: "a", Outputs: []string{"validation_url"}},
				{Name: "b", Inputs: map[string]string{"image_tag": "${{ phases.a.outputs.image_tag }}"}},
			},
			want: "declared:",
		},
		{
			name: "malformed",
			phases: []Phase{
				{Name: "a", Outputs: []string{"x"}},
				{Name: "b", Inputs: map[string]string{"x": "literal-not-a-ref"}},
			},
			want: "not a valid phase ref",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.phases)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestSubstituteResolvesInputs(t *testing.T) {
	phase := Phase{
		Name: "agent-execute",
		Inputs: map[string]string{
			"validation_url": "${{ phases.env-prep.outputs.validation_url }}",
			"image_tag":      "${{ phases.env-prep.outputs.image_tag }}",
		},
	}
	prior := map[string]map[string]string{
		"env-prep": {
			"validation_url": "https://x.glimmung.dev",
			"image_tag":      "issue-123-abc",
		},
	}

	got, err := Substitute(phase, prior)
	if err != nil {
		t.Fatalf("Substitute returned error: %v", err)
	}
	if got["validation_url"] != "https://x.glimmung.dev" || got["image_tag"] != "issue-123-abc" {
		t.Fatalf("unexpected substitution result: %#v", got)
	}
}

func TestSubstituteEmptyInputsReturnsEmpty(t *testing.T) {
	got, err := Substitute(Phase{Name: "a"}, map[string]map[string]string{})
	if err != nil {
		t.Fatalf("Substitute returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %#v, want empty map", got)
	}
}

func TestSubstituteRejectsRuntimeDrift(t *testing.T) {
	tests := []struct {
		name  string
		phase Phase
		prior map[string]map[string]string
		want  string
	}{
		{
			name:  "malformed",
			phase: Phase{Name: "b", Inputs: map[string]string{"x": "literal-not-a-ref"}},
			prior: map[string]map[string]string{},
			want:  "malformed",
		},
		{
			name:  "missing phase",
			phase: Phase{Name: "b", Inputs: map[string]string{"x": "${{ phases.a.outputs.x }}"}},
			prior: map[string]map[string]string{},
			want:  "no captured outputs",
		},
		{
			name:  "missing key",
			phase: Phase{Name: "b", Inputs: map[string]string{"x": "${{ phases.a.outputs.x }}"}},
			prior: map[string]map[string]string{"a": {"y": "v"}},
			want:  "phase posted outputs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Substitute(tt.phase, tt.prior)
			if err == nil {
				t.Fatal("expected substitution error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.want)
			}
		})
	}
}

func loadGoldenCases(t *testing.T) []goldenCase {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test file")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "..", "testdata", "phase_ref_cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden cases: %v", err)
	}

	var cases []goldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("decode golden cases: %v", err)
	}
	return cases
}

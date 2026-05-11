package phaserefs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

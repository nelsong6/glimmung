package budget

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type goldenCase struct {
	Name            string   `json:"name"`
	Function        string   `json:"function"`
	Label           string   `json:"label"`
	Labels          []string `json:"labels"`
	WorkflowDefault *float64 `json:"workflow_default"`
	WantTotal       *float64 `json:"want_total"`
	WantResolved    float64  `json:"want_resolved"`
	WantParsed      bool     `json:"want_parsed"`
}

func TestBudgetGoldenParity(t *testing.T) {
	for _, tc := range loadGoldenCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			switch tc.Function {
			case "parse_budget_label":
				got, ok := ParseBudgetLabel(tc.Label)
				if ok != tc.WantParsed {
					t.Fatalf("parsed=%v, want %v", ok, tc.WantParsed)
				}
				if !ok {
					return
				}
				if tc.WantTotal == nil {
					t.Fatal("parsed case missing want_total")
				}
				assertFloatEqual(t, got.Total, *tc.WantTotal)
			case "resolve_budget":
				var workflowDefault *Config
				if tc.WorkflowDefault != nil {
					workflowDefault = &Config{Total: *tc.WorkflowDefault}
				}
				got := ResolveBudget(tc.Labels, workflowDefault)
				assertFloatEqual(t, got.Total, tc.WantResolved)
			default:
				t.Fatalf("unknown budget function %q", tc.Function)
			}
		})
	}
}

func assertFloatEqual(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.0000001 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func loadGoldenCases(t *testing.T) []goldenCase {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test file")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "..", "testdata", "budget_cases.json")
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

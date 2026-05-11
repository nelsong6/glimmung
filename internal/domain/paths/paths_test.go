package paths

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type goldenCase struct {
	Name         string `json:"name"`
	Function     string `json:"function"`
	Project      string `json:"project"`
	Workflow     string `json:"workflow"`
	Phase        string `json:"phase"`
	RunID        string `json:"run_id"`
	AttemptIndex int    `json:"attempt_index"`
	JobID        string `json:"job_id"`
	StepSlug     string `json:"step_slug"`
	Want         string `json:"want"`
}

func TestPathGoldenParity(t *testing.T) {
	for _, tc := range loadGoldenCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			got := runPathCase(t, tc)
			if got != tc.Want {
				t.Fatalf("got %q, want %q", got, tc.Want)
			}
		})
	}
}

func runPathCase(t *testing.T, tc goldenCase) string {
	t.Helper()

	switch tc.Function {
	case "project_path":
		return ProjectPath(tc.Project)
	case "workflow_path":
		return WorkflowPath(tc.Project, tc.Workflow)
	case "phase_path":
		return PhasePath(tc.Project, tc.Workflow, tc.Phase)
	case "run_path":
		return RunPath(tc.Project, tc.RunID)
	case "attempt_path":
		return AttemptPath(tc.Project, tc.RunID, tc.AttemptIndex)
	case "job_path":
		return JobPath(tc.Project, tc.RunID, tc.AttemptIndex, tc.JobID)
	case "step_path":
		return StepPath(tc.Project, tc.RunID, tc.AttemptIndex, tc.JobID, tc.StepSlug)
	default:
		t.Fatalf("unknown path function %q", tc.Function)
		return ""
	}
}

func loadGoldenCases(t *testing.T) []goldenCase {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test file")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "..", "testdata", "path_cases.json")
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

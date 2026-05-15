package publicids

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type goldenCase struct {
	Name        string `json:"name"`
	Function    string `json:"function"`
	Project     string `json:"project"`
	Repo        string `json:"repo"`
	Number      *int   `json:"number"`
	IssueNumber *int   `json:"issue_number"`
	RunDisplay  string `json:"run_display"`
	SlotName    string `json:"slot_name"`
	LeaseNumber *int   `json:"lease_number"`
	Want        string `json:"want"`
}

func TestPublicIDGoldenParity(t *testing.T) {
	for _, tc := range loadGoldenCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			got := runPublicIDCase(t, tc)
			if got != tc.Want {
				t.Fatalf("got %q, want %q", got, tc.Want)
			}
		})
	}
}

func runPublicIDCase(t *testing.T, tc goldenCase) string {
	t.Helper()

	switch tc.Function {
	case "issue_ref":
		return IssueRef(tc.Project, tc.Number)
	case "run_ref":
		return RunRef(tc.Project, tc.IssueNumber, tc.RunDisplay)
	case "touchpoint_ref":
		return TouchpointRef(tc.Repo, tc.Number)
	case "lease_ref":
		return LeaseRef(tc.Project, tc.SlotName, tc.LeaseNumber)
	default:
		t.Fatalf("unknown public ID function %q", tc.Function)
		return ""
	}
}

func loadGoldenCases(t *testing.T) []goldenCase {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test file")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "..", "testdata", "public_id_cases.json")
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

package nativecostrepair

import "testing"

func TestRepairRunPayloadCopiesObservedNativeLogCostsToLedger(t *testing.T) {
	payload := map[string]any{
		"cumulative_cost_usd": float64(0),
		"attempts": []any{
			map[string]any{
				"attempt_index": float64(1),
				"cost_usd":      float64(0),
				"job_completions": map[string]any{
					"llm-test-plan": map[string]any{
						"job_id":   "llm-test-plan",
						"cost_usd": float64(0),
					},
					"llm-implement": map[string]any{
						"job_id":   "llm-implement",
						"cost_usd": float64(0),
					},
				},
			},
			map[string]any{
				"attempt_index": float64(2),
				"cost_usd":      float64(0),
				"verification": map[string]any{
					"status":   "pass",
					"cost_usd": float64(0),
				},
				"job_completions": map[string]any{
					"llm-verify": map[string]any{
						"job_id":   "llm-verify",
						"cost_usd": float64(0),
						"verification": map[string]any{
							"status":   "pass",
							"cost_usd": float64(0),
						},
					},
				},
			},
		},
	}

	result, err := RepairRunPayload(payload, []Event{
		{AttemptIndex: 1, JobID: "llm-test-plan", Event: "log", Message: `{"type":"result","total_cost_usd":1.25}`},
		{AttemptIndex: 1, JobID: "llm-implement", Event: "log", Message: `{"type":"result","total_cost_usd":2.5}`},
		{AttemptIndex: 2, JobID: "llm-verify", Event: "log", Message: `{"type":"result","total_cost_usd":3.75}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("Changed=false, want true")
	}
	if result.AttemptPatchCount != 2 {
		t.Fatalf("AttemptPatchCount=%d, want 2", result.AttemptPatchCount)
	}
	if result.JobPatchCount != 3 {
		t.Fatalf("JobPatchCount=%d, want 3", result.JobPatchCount)
	}
	if got := payload["cumulative_cost_usd"]; got != 7.5 {
		t.Fatalf("cumulative_cost_usd=%v, want 7.5", got)
	}

	attempts := payload["attempts"].([]any)
	first := attempts[0].(map[string]any)
	if got := first["cost_usd"]; got != 3.75 {
		t.Fatalf("attempt 1 cost=%v, want 3.75", got)
	}
	firstJobs := first["job_completions"].(map[string]any)
	if got := firstJobs["llm-test-plan"].(map[string]any)["cost_usd"]; got != 1.25 {
		t.Fatalf("llm-test-plan cost=%v, want 1.25", got)
	}
	if got := firstJobs["llm-implement"].(map[string]any)["cost_usd"]; got != 2.5 {
		t.Fatalf("llm-implement cost=%v, want 2.5", got)
	}
	second := attempts[1].(map[string]any)
	if got := second["verification"].(map[string]any)["cost_usd"]; got != 3.75 {
		t.Fatalf("attempt verification cost=%v, want 3.75", got)
	}
	secondJobs := second["job_completions"].(map[string]any)
	if got := secondJobs["llm-verify"].(map[string]any)["verification"].(map[string]any)["cost_usd"]; got != 3.75 {
		t.Fatalf("job verification cost=%v, want 3.75", got)
	}
}

func TestRepairRunPayloadDoesNotOverwritePositiveCost(t *testing.T) {
	payload := map[string]any{
		"cumulative_cost_usd": float64(9),
		"attempts": []any{
			map[string]any{
				"attempt_index": float64(0),
				"cost_usd":      float64(9),
				"job_completions": map[string]any{
					"llm": map[string]any{
						"job_id":   "llm",
						"cost_usd": float64(9),
					},
				},
			},
		},
	}

	result, err := RepairRunPayload(payload, []Event{
		{AttemptIndex: 0, JobID: "llm", Event: "log", Message: `{"type":"result","total_cost_usd":4}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed {
		t.Fatalf("Changed=true, want false; patches=%#v", result.Patches)
	}
	if got := payload["cumulative_cost_usd"]; got != float64(9) {
		t.Fatalf("cumulative_cost_usd=%v, want 9", got)
	}
}

func TestRepairRunPayloadIgnoresNonResultCostShapes(t *testing.T) {
	payload := map[string]any{
		"cumulative_cost_usd": float64(0),
		"attempts": []any{
			map[string]any{
				"attempt_index":   float64(0),
				"job_completions": map[string]any{},
			},
		},
	}

	result, err := RepairRunPayload(payload, []Event{
		{AttemptIndex: 0, JobID: "llm", Event: "log", Message: `{"message":{"total_cost_usd":4}}`},
		{AttemptIndex: 0, JobID: "llm", Event: "log", Message: `{"total_cost_usd":0}`},
		{AttemptIndex: 0, JobID: "llm", Event: "step_completed", Message: `{"total_cost_usd":4}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed {
		t.Fatalf("Changed=true, want false; patches=%#v", result.Patches)
	}
}

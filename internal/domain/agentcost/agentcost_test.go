package agentcost

import "testing"

func TestFromJSONLogLineExtractsTopLevelTotalCost(t *testing.T) {
	cost, ok := FromJSONLogLine(`{"type":"result","total_cost_usd":1.2345,"usage":{"input_tokens":12}}`)
	if !ok {
		t.Fatal("expected cost")
	}
	if cost != 1.2345 {
		t.Fatalf("cost=%v", cost)
	}
}

func TestFromJSONLogLineIgnoresNestedOrZeroCosts(t *testing.T) {
	for _, line := range []string{
		`{"type":"assistant","message":{"total_cost_usd":9.99}}`,
		`{"type":"result","total_cost_usd":0}`,
		`not json`,
	} {
		if cost, ok := FromJSONLogLine(line); ok {
			t.Fatalf("line %q produced cost %v", line, cost)
		}
	}
}

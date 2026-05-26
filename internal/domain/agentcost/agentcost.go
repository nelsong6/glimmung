package agentcost

import (
	"bytes"
	"encoding/json"
	"strings"
)

// FromJSONLogLine extracts the total cost emitted by agent CLIs that stream a
// final JSON result line. It intentionally only trusts a top-level
// total_cost_usd field so nested tool payloads are not double-counted.
func FromJSONLogLine(line string) (float64, bool) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return 0, false
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return 0, false
	}
	value, ok := numberValue(obj["total_cost_usd"])
	if !ok || value <= 0 {
		return 0, false
	}
	return value, true
}

func numberValue(raw any) (float64, bool) {
	switch value := raw.(type) {
	case json.Number:
		parsed, err := value.Float64()
		return parsed, err == nil
	case float64:
		return value, true
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return 0, false
		}
		dec := json.NewDecoder(bytes.NewBufferString(value))
		dec.UseNumber()
		var parsed json.Number
		if err := dec.Decode(&parsed); err != nil {
			return 0, false
		}
		f, err := parsed.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

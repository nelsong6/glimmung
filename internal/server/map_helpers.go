package server

import (
	"strconv"
	"strings"
)

// Helpers shared across project metadata parsers (workload-identity, slot
// state, etc). Previously co-located with the now-deleted Entra redirect
// reconciler; moved here so the surviving consumers compile.

func stringMapValue(values map[string]any, key string) string {
	value, ok := stringFromMap(values, key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func stringSliceFromMap(values map[string]any, keys ...string) []string {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case []string:
			out := make([]string, 0, len(typed))
			for _, value := range typed {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					out = append(out, trimmed)
				}
			}
			return out
		case []any:
			out := make([]string, 0, len(typed))
			for _, value := range typed {
				if s, ok := value.(string); ok {
					if trimmed := strings.TrimSpace(s); trimmed != "" {
						out = append(out, trimmed)
					}
				}
			}
			return out
		}
	}
	return nil
}

func nonNegativeIntMapValue(values map[string]any, key string) int {
	if values == nil {
		return 0
	}
	raw, ok := values[key]
	if !ok {
		return 0
	}
	switch typed := raw.(type) {
	case int:
		if typed > 0 {
			return typed
		}
	case int64:
		if typed > 0 {
			return int(typed)
		}
	case float64:
		if typed > 0 {
			return int(typed)
		}
	case string:
		parsed, err := strconv.Atoi(typed)
		if err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}


package server

import (
	"errors"
	"strconv"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")
var ErrInactive = errors.New("inactive")

type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string {
	return e.Message
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func mapOrEmpty(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	return values
}

func sliceOrEmpty[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

func mapFromMap(values map[string]any, key string) (map[string]any, bool) {
	if values == nil {
		return nil, false
	}
	raw, ok := values[key]
	if !ok {
		return nil, false
	}
	typed, ok := raw.(map[string]any)
	return typed, ok
}

func stringFromMap(values map[string]any, key string) (string, bool) {
	if values == nil {
		return "", false
	}
	value, ok := values[key]
	if !ok {
		return "", false
	}
	typed := stringValue(value)
	return typed, typed != ""
}

func boolFromMap(values map[string]any, key string) bool {
	if values == nil {
		return false
	}
	value, ok := values[key]
	if !ok {
		return false
	}
	typed, ok := value.(bool)
	return ok && typed
}

func positiveIntFromMap(values map[string]any, key string) (int, bool) {
	if values == nil {
		return 0, false
	}
	value, ok := values[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return positiveInt(typed)
	case int64:
		return positiveInt(int(typed))
	case float64:
		return positiveInt(int(typed))
	case string:
		parsed, err := strconv.Atoi(typed)
		if err != nil {
			return 0, false
		}
		return positiveInt(parsed)
	default:
		return 0, false
	}
}

func positiveInt(value int) (int, bool) {
	if value < 1 {
		return 0, false
	}
	return value, true
}

func stringValue(value any) string {
	typed, ok := value.(string)
	if !ok {
		return ""
	}
	return typed
}

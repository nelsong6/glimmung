package budget

import (
	"strconv"
	"strings"
)

const (
	labelPrefix  = "agent-budget:"
	defaultTotal = 25.0
)

// Config is the run-cumulative cost cap.
type Config struct {
	Total float64
}

// DefaultConfig returns the global fallback budget.
func DefaultConfig() Config {
	return Config{Total: defaultTotal}
}

// ParseBudgetLabel parses an agent-budget label.
func ParseBudgetLabel(label string) (Config, bool) {
	if !strings.HasPrefix(label, labelPrefix) {
		return Config{}, false
	}

	spec := strings.TrimPrefix(label, labelPrefix)
	raw := spec
	if _, after, found := strings.Cut(spec, "x"); found {
		raw = after
	}

	total, err := strconv.ParseFloat(raw, 64)
	if err != nil || total <= 0 {
		return Config{}, false
	}
	return Config{Total: total}, true
}

// ResolveBudget returns the first valid label budget, then workflowDefault, then the global default.
func ResolveBudget(labelNames []string, workflowDefault *Config) Config {
	for _, label := range labelNames {
		if cfg, ok := ParseBudgetLabel(label); ok {
			return cfg
		}
	}
	if workflowDefault != nil {
		return *workflowDefault
	}
	return DefaultConfig()
}

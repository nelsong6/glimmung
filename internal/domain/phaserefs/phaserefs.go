package phaserefs

import (
	"fmt"
	"regexp"
	"sort"
)

var phaseRefPattern = regexp.MustCompile(`^\s*\$\{\{\s*phases\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)\s*\}\}\s*$`)

// Ref is a parsed cross-phase input reference.
type Ref struct {
	Phase string
	Key   string
}

// Phase is the minimum workflow phase shape needed for input-ref validation.
type Phase struct {
	Name    string
	Inputs  map[string]string
	Outputs []string
}

// Parse parses a `${{ phases.<name>.outputs.<key> }}` expression.
func Parse(value string) (Ref, bool) {
	matches := phaseRefPattern.FindStringSubmatch(value)
	if matches == nil {
		return Ref{}, false
	}
	return Ref{Phase: matches[1], Key: matches[2]}, true
}

// Validate checks that every phase input references an output from an earlier phase.
func Validate(phases []Phase) error {
	declaredOutputs := map[string]map[string]struct{}{}
	for _, phase := range phases {
		for inputName, value := range phase.Inputs {
			ref, ok := Parse(value)
			if !ok {
				return fmt.Errorf(
					"phase %q input %q=%q is not a valid phase ref (expected `${{ phases.NAME.outputs.KEY }}`)",
					phase.Name,
					inputName,
					value,
				)
			}
			if ref.Phase == phase.Name {
				return fmt.Errorf("phase %q input %q refs itself; self-refs are not allowed", phase.Name, inputName)
			}
			outputs, ok := declaredOutputs[ref.Phase]
			if !ok {
				return fmt.Errorf(
					"phase %q input %q refs phase %q which doesn't appear earlier in the workflow",
					phase.Name,
					inputName,
					ref.Phase,
				)
			}
			if _, ok := outputs[ref.Key]; !ok {
				return fmt.Errorf(
					"phase %q input %q refs %q.outputs.%q but %q doesn't declare that output (declared: %v)",
					phase.Name,
					inputName,
					ref.Phase,
					ref.Key,
					ref.Phase,
					sortedOutputNames(outputs),
				)
			}
		}
		declaredOutputs[phase.Name] = outputSet(phase.Outputs)
	}
	return nil
}

// Substitute resolves a phase's inputs against captured outputs from earlier phases.
func Substitute(phase Phase, priorOutputs map[string]map[string]string) (map[string]string, error) {
	resolved := map[string]string{}
	for inputName, value := range phase.Inputs {
		ref, ok := Parse(value)
		if !ok {
			return nil, fmt.Errorf(
				"phase %q input %q ref %q is malformed (registration validation should have caught this)",
				phase.Name,
				inputName,
				value,
			)
		}
		outputs, ok := priorOutputs[ref.Phase]
		if !ok {
			return nil, fmt.Errorf(
				"phase %q input %q refs phase %q which has no captured outputs on this run",
				phase.Name,
				inputName,
				ref.Phase,
			)
		}
		output, ok := outputs[ref.Key]
		if !ok {
			return nil, fmt.Errorf(
				"phase %q input %q refs %s.outputs.%q; phase posted outputs %v",
				phase.Name,
				inputName,
				ref.Phase,
				ref.Key,
				sortedMapKeys(outputs),
			)
		}
		resolved[inputName] = output
	}
	return resolved, nil
}

func outputSet(outputs []string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, output := range outputs {
		set[output] = struct{}{}
	}
	return set
}

func sortedOutputNames(outputs map[string]struct{}) []string {
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedMapKeys(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

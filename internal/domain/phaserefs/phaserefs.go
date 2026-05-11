package phaserefs

import "regexp"

var phaseRefPattern = regexp.MustCompile(`^\s*\$\{\{\s*phases\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)\s*\}\}\s*$`)

// Ref is a parsed cross-phase input reference.
type Ref struct {
	Phase string
	Key   string
}

// Parse parses a `${{ phases.<name>.outputs.<key> }}` expression.
func Parse(value string) (Ref, bool) {
	matches := phaseRefPattern.FindStringSubmatch(value)
	if matches == nil {
		return Ref{}, false
	}
	return Ref{Phase: matches[1], Key: matches[2]}, true
}

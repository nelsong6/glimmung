package cosmos

import "strings"

// sanitizeLiveSmokeName lowercases a string and replaces every character
// outside [a-z0-9-] with '-', then trims leading/trailing dashes. Used by
// the slot live-smoke test to build deterministic per-run names without
// surprising characters from CI environment metadata.
//
// Previously colocated with the cosmos lock-lifecycle live smoke test
// (TestLiveCosmosLockLifecycle), which Stage 2b deleted along with the
// cosmos-side lock implementation. The contract this helper exercised is
// now covered by internal/store/pg/locks live smoke tests.
func sanitizeLiveSmokeName(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "test"
	}
	return out
}

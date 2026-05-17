package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retiredSlotSymbols names the identifiers that the slot-storage rework
// retired end-to-end. Per docs/migration-policy.md the deletion is
// total: no compatibility shim, no legacy reader, no fallback. This
// test fails if any of these names re-appear in the production source
// tree (test files and migration test fixtures are excluded — they
// retain the names so the legacy compat bridge keeps in-flight tests
// passing through the transition).
//
// Add a name here when retiring a slot-related symbol; the test will
// fail loudly when someone re-introduces it.
var retiredSlotSymbols = []string{
	"SetProjectTestEnvironmentSlotStatus",
	"SetProjectTestEnvironmentSlotStatusIfMatch",
	"ProjectTestEnvironmentSlotStatusWriter",
	"ProjectTestEnvironmentSlotStatusClaimer",
	"claimTestSlotWarmup",
	"warmupRetryJitter",
}

// retiredSymbolAllowlist names files where the retired symbols are
// permitted to appear: documentation references, test files that bridge
// legacy state for compat, and the lifecycle contract doc that names
// the retired symbols as keep-them-deleted markers.
var retiredSymbolAllowlist = map[string]bool{
	// State api keeps a legacy fallback reader for in-memory fakes that
	// don't yet implement SlotStore — references TestEnvironmentSlotStatus
	// (which is allowed) but never any of the retired writer names.
}

func TestRetiredSlotSymbolsStayDeleted(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	type hit struct {
		file   string
		symbol string
	}
	var hits []hit
	walkErr := filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Exclude test files: fakes carry the retired names as part of the
		// legacy-compat bridge that keeps in-flight tests passing through
		// the transition. Those names will be deleted in a follow-up pass
		// once tests have been migrated to use the SlotStore directly.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if retiredSymbolAllowlist[filepath.ToSlash(rel)] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(data)
		for _, symbol := range retiredSlotSymbols {
			if !strings.Contains(body, symbol) {
				continue
			}
			// Comments naming retired symbols as deletion markers are
			// allowed — they're the keep-it-deleted guardrails.
			// Distinguish by checking whether every line containing the
			// symbol starts with `//` (after leading whitespace).
			lines := strings.Split(body, "\n")
			for _, line := range lines {
				if !strings.Contains(line, symbol) {
					continue
				}
				trimmed := strings.TrimLeft(line, " \t")
				if strings.HasPrefix(trimmed, "//") {
					continue
				}
				hits = append(hits, hit{file: filepath.ToSlash(rel), symbol: symbol})
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
	if len(hits) > 0 {
		var b strings.Builder
		b.WriteString("retired slot symbols re-introduced in production source:\n")
		for _, h := range hits {
			b.WriteString("  ")
			b.WriteString(h.file)
			b.WriteString(": ")
			b.WriteString(h.symbol)
			b.WriteString("\n")
		}
		b.WriteString("\nThese names belonged to the embedded `project.metadata.native_standby_dns.slots[]` shape that the slot-storage rework retired. See docs/test-slot-lifecycle.md.")
		t.Fatal(b.String())
	}
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

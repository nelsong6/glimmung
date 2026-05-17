package server

// Typed test helpers for the slot lifecycle subsystem.
//
// Before this file, tests constructed slot fixtures as untyped
// `map[string]any{"native_standby_dns": map[string]any{"slots": []any{...}}}`
// nested-map soup on the project metadata, then relied on a "legacy-
// compat bridge" inside the fake stores (PR #518's `slotFromLegacyEntry`)
// to synthesize Slot rows on demand. That bridge violated
// docs/migration-policy.md ("no parallel path that works for now") and
// meant tests were tied to a wire shape (`native_standby_dns.slots[]`)
// that production code had already abandoned. It also meant every test
// setup was 7+ lines of typed-as-`any` casts that any rename rippled
// through.
//
// This file replaces that pattern with a small typed builder API:
//
//   project := newSlotProject(t, "tank", withSlotCount(3))
//   seedSlot(t, store, "tank", 1, SlotStateProvisioned, withSlotName("tank-slot-1"))
//   waitForSlotState(t, store, "tank", 1, SlotStateRunning)
//
// All helpers go through the SlotStore interface — the same path
// production code uses — so tests and prod can never disagree about
// where slot state lives. The fake stores' SlotStore implementation
// (`fakeLeaseStore.CreateSlot` / `UpdateIfMatch`) is the single point
// of seed + assertion; no embedded-array shim, no state-name translator.

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// slotProjectOption configures one knob on the typed Project builder.
// Each builder returns the modified Project so callers can chain options;
// the helpers themselves accept a variadic slice of options at the call
// site for readability.
type slotProjectOption func(*Project)

// withSlotCount sets `native_standby_dns.count` on the project metadata.
// The count is the only field from the legacy embedded shape that's
// still load-bearing after PR #518 (slot rows live in their own Cosmos
// container; only the configured cap stays on the project doc).
func withSlotCount(count int) slotProjectOption {
	return func(p *Project) {
		standby := projectStandbyDNSMap(p)
		standby["count"] = float64(count)
		p.Metadata["native_standby_dns"] = standby
	}
}

// withSlotPrefix sets `native_standby_dns.slot_prefix`. This is the
// operator-visible naming root for slot DNS records and namespace
// derivations; tests set it when assertions depend on the resulting
// slot name. Default-derived names ("<project>-slot-<n>") suffice for
// most tests, so callers usually omit this option.
func withSlotPrefix(prefix string) slotProjectOption {
	return func(p *Project) {
		standby := projectStandbyDNSMap(p)
		standby["slot_prefix"] = prefix
		p.Metadata["native_standby_dns"] = standby
	}
}

// withProjectMetadata merges arbitrary top-level metadata onto the
// project doc. Used for tests that need additional metadata blocks
// (e.g., `native_standby_workload_identity`) alongside the slot count.
func withProjectMetadata(extra map[string]any) slotProjectOption {
	return func(p *Project) {
		if p.Metadata == nil {
			p.Metadata = map[string]any{}
		}
		for k, v := range extra {
			p.Metadata[k] = v
		}
	}
}

// withGitHubRepo sets the project's `github_repo` field. Some tests
// assert on this for completeness; default is empty.
func withGitHubRepo(repo string) slotProjectOption {
	return func(p *Project) {
		p.GitHubRepo = repo
	}
}

// newSlotProject builds a Project value suitable for handing to a fake
// store via `fakeReadStore{projects: []Project{project}}`. The project
// has `Name == ID == name`, a populated `native_standby_dns.count` (via
// withSlotCount; default 0), and no embedded slots array — slot rows
// are seeded through seedSlot, which goes through the SlotStore.
//
// t is unused today but kept on the signature so future helpers can
// fail-fast if a future option violates an invariant.
func newSlotProject(t *testing.T, name string, opts ...slotProjectOption) Project {
	t.Helper()
	p := Project{
		ID:       name,
		Name:     name,
		Metadata: map[string]any{},
	}
	for _, opt := range opts {
		opt(&p)
	}
	return p
}

// projectStandbyDNSMap returns the project's `native_standby_dns` map,
// creating an empty one (and the surrounding `Metadata`) if absent.
// Helpers compose through this so callers can layer multiple options
// without each one re-deriving the structure.
func projectStandbyDNSMap(p *Project) map[string]any {
	if p.Metadata == nil {
		p.Metadata = map[string]any{}
	}
	standby, ok := p.Metadata["native_standby_dns"].(map[string]any)
	if !ok {
		standby = map[string]any{}
	}
	return standby
}

// slotSeedOption configures one knob on the typed slot seeder. Mirrors
// slotProjectOption but for the slot row itself. Each option targets
// a specific Slot field — there is intentionally no "set arbitrary
// fields" escape hatch; if a test needs a Slot field that has no
// option, add one rather than reaching past the typed surface.
type slotSeedOption func(*Slot)

// withSlotName overrides the default slot name ("<project>-slot-<n>").
// Tests that assert on slot naming or set a custom prefix typically
// pair this with `withSlotPrefix`.
func withSlotName(name string) slotSeedOption {
	return func(s *Slot) {
		s.SlotName = name
	}
}

// withSlotDetail sets the slot's optional `detail` field (the
// human-readable explanation that pairs with non-terminal states or
// errors).
func withSlotDetail(detail string) slotSeedOption {
	return func(s *Slot) {
		s.Detail = stringPtr(detail)
	}
}

// withActiveLeaseRef pins the slot to a specific lease. Required when
// seeding `running` slots whose lease is the focus of the test (e.g.,
// recovery sweeps that resume in-flight activation against a known
// lease).
func withActiveLeaseRef(ref string) slotSeedOption {
	return func(s *Slot) {
		s.ActiveLeaseRef = stringPtr(ref)
	}
}

// withSlotUpdatedAt sets the slot's `updated_at` to a deterministic
// timestamp. Default is `time.Now().UTC()` when omitted. Used by
// recovery-staleness tests that need a slot's last-touch time to be
// older than the recovery min-age threshold.
func withSlotUpdatedAt(t time.Time) slotSeedOption {
	return func(s *Slot) {
		s.UpdatedAt = t.UTC()
	}
}

// withActivationStarted marks the slot as activation-in-flight starting
// at the given time. Used by recovery tests that need to assert resume
// behavior for slots that were mid-activation when the previous pod
// died.
func withActivationStarted(at time.Time, jobName string) slotSeedOption {
	return func(s *Slot) {
		t := at.UTC()
		s.ActivationStartedAt = &t
		s.ActivationJobName = stringPtr(jobName)
		attempt := 1
		s.ActivationAttempt = &attempt
	}
}

// withActivationCompleted marks the slot's activation as finished at
// the given time. Pair with withActivationStarted for full-lifecycle
// fixtures, or use solo for tests asserting on retained activation
// metadata.
func withActivationCompleted(at time.Time) slotSeedOption {
	return func(s *Slot) {
		t := at.UTC()
		s.ActivationCompletedAt = &t
	}
}

// withCleanupStarted marks the slot's cleanup phase as in-flight.
// Used by recovery tests for stuck-cleanup scenarios.
func withCleanupStarted(at time.Time) slotSeedOption {
	return func(s *Slot) {
		t := at.UTC()
		s.CleanupStartedAt = &t
	}
}

// slotSeeder is the optional fake-store hook for "this write is fixture
// setup, not a write under test." Implemented by fakeLeaseStore (and
// the fakes that embed it). Test helpers call beginSeed before issuing
// CreateSlot fixture writes and endSeed after, so the fake's write-log
// (slotStatuses) only contains writes from production code paths.
type slotSeeder interface {
	beginSeed()
	endSeed()
}

// seedSlot writes a slot row directly into the SlotStore. Tests use this
// in place of nested-map soup on the project metadata. The resulting
// slot row is what production code reads via ListSlotsByProject /
// GetSlot — single source of truth, no bridge.
//
// state must be one of the canonical SlotState* constants in slot.go.
// The default slot name is "<project>-slot-<index>"; override with
// withSlotName. All other fields take their Slot{} zero value unless
// configured via opts.
//
// Returns the persisted Slot (with the fake's etag attached) so tests
// that need to chain a CAS-write through UpdateIfMatch have the
// captured etag. Fails the test on any store error — seeding is
// structural test setup, not the unit under test.
func seedSlot(t *testing.T, store SlotStore, project string, slotIndex int, state string, opts ...slotSeedOption) Slot {
	t.Helper()
	now := time.Now().UTC()
	slot := Slot{
		Project:   project,
		SlotIndex: slotIndex,
		SlotName:  project + "-slot-" + strconv.Itoa(slotIndex),
		State:     state,
		UpdatedAt: now,
	}
	if state == SlotStateProvisioned || state == SlotStateRunning || state == SlotStateActivating || state == SlotStateCleaning {
		// Slots that are provisioned-or-beyond have a provisioned_at —
		// they passed through the provisioning state on the way here.
		// Default to the seed time; explicit opts override.
		t := now
		slot.ProvisionedAt = &t
	}
	for _, opt := range opts {
		opt(&slot)
	}
	if seeder, ok := store.(slotSeeder); ok {
		seeder.beginSeed()
		defer seeder.endSeed()
	}
	created, err := store.CreateSlot(context.Background(), slot)
	if err != nil {
		t.Fatalf("seedSlot(%s, %d, %s) failed: %v", project, slotIndex, state, err)
	}
	return created
}

// mustGetSlot returns the slot at (project, slotIndex). Fails the test
// if the slot doesn't exist or the store returns an error. Tests that
// want to assert on a Slot's *current* state should use this; tests
// that want the full sequence of writes should iterate the fake's
// recorded slotStatuses log instead.
func mustGetSlot(t *testing.T, store SlotStore, project string, slotIndex int) Slot {
	t.Helper()
	slot, err := store.GetSlot(context.Background(), project, slotIndex)
	if err != nil {
		t.Fatalf("mustGetSlot(%s, %d) failed: %v", project, slotIndex, err)
	}
	return slot
}

// waitForSlotState polls the SlotStore until the slot at
// (project, slotIndex) reaches `want`, or fails the test if it doesn't
// happen within 5 seconds.
//
// Used by tests that exercise the full async lifecycle — checkout,
// recovery sweep, etc. The background goroutines transitioning a slot
// don't synchronize with the test thread except through SlotStore
// writes, so a poll loop is the right shape.
func waitForSlotState(t *testing.T, store SlotStore, project string, slotIndex int, want string) Slot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		slot, err := store.GetSlot(context.Background(), project, slotIndex)
		if err == nil && slot.State == want {
			return slot
		}
		time.Sleep(10 * time.Millisecond)
	}
	final, _ := store.GetSlot(context.Background(), project, slotIndex)
	t.Fatalf("waitForSlotState(%s, %d): state never reached %q; final=%q", project, slotIndex, want, final.State)
	return Slot{}
}

// stringPtr lives in read_api.go and is reused here for slot pointer
// fields. Kept noted so future helpers don't accidentally redeclare it.

// seedSlotsFromLegacyMetadata reads any
// `project.metadata.native_standby_dns.slots[]` entries on the given
// project and writes them into the SlotStore via `CreateSlot`. This is
// the test-side equivalent of `MigrateProjectSlotsIntoCollection`, the
// one-shot boot migration glimmung runs in production: it mirrors the
// exact translation (state-name remap from `warming`/`ready`/`active`
// to the new vocabulary) and the same call pattern (`store.CreateSlot`).
//
// Use this in tests whose project fixtures still carry the legacy
// `slots[]` setup shape that pre-dated the slot-storage rework.
// Calling it explicitly preserves transparency — the test states what
// it depends on instead of relying on auto-synthesis inside the fake's
// SlotStore methods. The auto-synth bridge that previously lived in
// `fakeLeaseStore.GetSlot` was deleted per docs/migration-policy.md.
//
// The legacy `slots[]` fixture shape is itself a deletion target; new
// tests should call `seedSlot` directly instead of feeding legacy
// data through this helper.
func seedSlotsFromLegacyMetadata(t *testing.T, store SlotStore, projectFromStore interface{ ListProjects(context.Context) ([]Project, error) }, projectName string) {
	t.Helper()
	if seeder, ok := store.(slotSeeder); ok {
		seeder.beginSeed()
		defer seeder.endSeed()
	}
	projects, err := projectFromStore.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("seedSlotsFromLegacyMetadata: ListProjects: %v", err)
	}
	for _, p := range projects {
		if p.Name != projectName && p.ID != projectName {
			continue
		}
		for _, entry := range readLegacyProjectSlots(p) {
			slot := slotFromLegacyEntry(projectName, entry, time.Now().UTC())
			// Preserve the legacy entry's activation/cleanup fields so
			// tests that pre-populated those for diagnostics still see
			// them on the resulting Slot.
			if v, ok := positiveIntFromMap(entry.raw, "activation_attempt"); ok {
				attempt := v
				slot.ActivationAttempt = &attempt
			}
			if v, ok := stringFromMap(entry.raw, "activation_job_name"); ok && v != "" {
				job := v
				slot.ActivationJobName = &job
			}
			if v, ok := stringFromMap(entry.raw, "activation_started_at"); ok {
				if at, err := time.Parse(time.RFC3339Nano, v); err == nil {
					slot.ActivationStartedAt = &at
				}
			}
			if v, ok := stringFromMap(entry.raw, "activation_completed_at"); ok {
				if at, err := time.Parse(time.RFC3339Nano, v); err == nil {
					slot.ActivationCompletedAt = &at
				}
			}
			// If the legacy entry implies activation reached `running`
			// but didn't carry per-phase timestamps, infer them from
			// updated_at so derivedActivationState reports
			// post-activation correctly.
			if slot.ActivationCompletedAt == nil && slot.State == SlotStateRunning {
				at := slot.UpdatedAt
				slot.ActivationCompletedAt = &at
				if slot.ActivationStartedAt == nil {
					slot.ActivationStartedAt = &at
				}
			}
			if _, err := store.CreateSlot(context.Background(), slot); err != nil {
				t.Fatalf("seedSlotsFromLegacyMetadata: CreateSlot(%s, %d): %v", projectName, entry.slotIndex, err)
			}
		}
		return
	}
}

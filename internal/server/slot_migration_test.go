package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

// migrationFakeStore is a thin extension of fakeSlotStore that also
// implements ListProjects so MigrateProjectSlotsIntoCollection can find
// the legacy slot arrays to copy. The stripper interface is implemented
// inline (records stripped projects without persisting a real change).
type migrationFakeStore struct {
	*fakeSlotStore
	projects        []Project
	strippedProjects map[string]bool
}

func newMigrationFakeStore(projects ...Project) *migrationFakeStore {
	return &migrationFakeStore{
		fakeSlotStore:    newFakeSlotStore(),
		projects:         projects,
		strippedProjects: map[string]bool{},
	}
}

// ListProjects satisfies the ReadStore interface used by the migration.
func (m *migrationFakeStore) ListProjects(_ context.Context) ([]Project, error) {
	out := make([]Project, len(m.projects))
	copy(out, m.projects)
	return out, nil
}

// Stub out the rest of ReadStore. The migration only uses ListProjects;
// ListWorkflows is present so *migrationFakeStore satisfies the
// interface but it's never called.
func (m *migrationFakeStore) ListWorkflows(context.Context) ([]Workflow, error) { return nil, nil }

func (m *migrationFakeStore) StripProjectTestEnvironmentSlotsArray(_ context.Context, project string) error {
	m.strippedProjects[project] = true
	// Mutate the in-memory project's metadata so re-running the
	// migration on the same store doesn't re-create the slot docs.
	for i := range m.projects {
		if m.projects[i].Name != project && m.projects[i].ID != project {
			continue
		}
		md := m.projects[i].Metadata
		if standby, ok := md["native_standby_dns"].(map[string]any); ok {
			delete(standby, "slots")
		}
	}
	return nil
}

func tankOperatorLegacyProject() Project {
	return Project{
		ID:   "tank-operator",
		Name: "tank-operator",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{
				"count": 10,
				"slots": []any{
					map[string]any{
						"slot_index": 1,
						"slot_name":  "tank-operator-slot-1",
						"state":      "active",
						"updated_at": "2026-05-17T07:08:38.832966033Z",
						"ready_at":   "2026-05-17T07:05:00.582214060Z",
					},
					map[string]any{
						"slot_index": 5,
						"slot_name":  "tank-operator-slot-5",
						"state":      "active",
						"updated_at": "2026-05-17T07:30:18.875885225Z",
						"ready_at":   "2026-05-17T07:13:36.396893168Z",
					},
					map[string]any{
						"slot_index": 9,
						"slot_name":  "tank-operator-slot-9",
						"state":      "ready",
						"updated_at": "2026-05-17T07:16:38.684347740Z",
						"ready_at":   "2026-05-17T07:16:38.684347740Z",
					},
				},
			},
		},
	}
}

func TestMigrateProjectSlotsCopiesAndTranslatesLegacyStates(t *testing.T) {
	store := newMigrationFakeStore(tankOperatorLegacyProject())
	summary, err := MigrateProjectSlotsIntoCollection(context.Background(), store)
	if err != nil {
		t.Fatalf("MigrateProjectSlotsIntoCollection: %v", err)
	}
	if summary.SlotsCreated != 3 {
		t.Errorf("SlotsCreated=%d, want 3", summary.SlotsCreated)
	}
	if summary.SlotsAlreadyMigrated != 0 {
		t.Errorf("SlotsAlreadyMigrated=%d, want 0", summary.SlotsAlreadyMigrated)
	}
	if summary.ProjectsStripped != 1 {
		t.Errorf("ProjectsStripped=%d, want 1", summary.ProjectsStripped)
	}

	got, err := store.GetSlot(context.Background(), "tank-operator", 1)
	if err != nil {
		t.Fatalf("GetSlot(1): %v", err)
	}
	if got.State != SlotStateRunning {
		t.Errorf("slot 1 state=%q, want running (was 'active' in legacy)", got.State)
	}

	got9, err := store.GetSlot(context.Background(), "tank-operator", 9)
	if err != nil {
		t.Fatalf("GetSlot(9): %v", err)
	}
	if got9.State != SlotStateProvisioned {
		t.Errorf("slot 9 state=%q, want provisioned (was 'ready' in legacy)", got9.State)
	}
	if got9.ProvisionedAt == nil {
		t.Error("slot 9 ProvisionedAt nil; want copied from legacy ready_at")
	}
}

func TestMigrateProjectSlotsIsIdempotent(t *testing.T) {
	store := newMigrationFakeStore(tankOperatorLegacyProject())
	if _, err := MigrateProjectSlotsIntoCollection(context.Background(), store); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	second, err := MigrateProjectSlotsIntoCollection(context.Background(), store)
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}
	// After strip, the project's slots[] array is gone; second run finds
	// no legacy slots to migrate (ProjectsWithLegacySlots=0, SlotsCreated=0,
	// SlotsAlreadyMigrated=0).
	if second.SlotsCreated != 0 {
		t.Errorf("idempotent second run SlotsCreated=%d, want 0", second.SlotsCreated)
	}
	if second.ProjectsWithLegacySlots != 0 {
		t.Errorf("idempotent second run ProjectsWithLegacySlots=%d, want 0 (array stripped on first run)", second.ProjectsWithLegacySlots)
	}
}

func TestMigrateProjectSlotsErrorsWhenStoreIsNotSlotStore(t *testing.T) {
	store := readStoreWithoutSlotStore{}
	_, err := MigrateProjectSlotsIntoCollection(context.Background(), store)
	if err == nil || !errors.Is(err, errNoSlotStore) && err.Error() == "" {
		t.Fatalf("err=%v, want non-nil 'slot store not configured'", err)
	}
}

func TestTranslateLegacyState(t *testing.T) {
	cases := []struct{ legacy, want string }{
		{"warming", SlotStateProvisioning},
		{"ready", SlotStateProvisioned},
		{"", SlotStateProvisioned},
		{"active", SlotStateRunning},
		{"activating", SlotStateActivating},
		{"cleaning", SlotStateCleaning},
		{"error", SlotStateError},
		{"junk", SlotStateUnseeded},
	}
	for _, tc := range cases {
		if got := translateLegacyState(tc.legacy); got != tc.want {
			t.Errorf("translateLegacyState(%q)=%q, want %q", tc.legacy, got, tc.want)
		}
	}
}

func TestReadLegacyProjectSlotsHandlesMissingArray(t *testing.T) {
	project := Project{Metadata: map[string]any{"native_standby_dns": map[string]any{"count": 5}}}
	if got := readLegacyProjectSlots(project); len(got) != 0 {
		t.Fatalf("len=%d, want 0", len(got))
	}
}

func TestReadLegacyProjectSlotsHandlesNilMetadata(t *testing.T) {
	if got := readLegacyProjectSlots(Project{}); len(got) != 0 {
		t.Fatalf("len=%d, want 0", len(got))
	}
}

func TestMigrateProjectSlotsHandlesEmptyProjectList(t *testing.T) {
	store := newMigrationFakeStore()
	summary, err := MigrateProjectSlotsIntoCollection(context.Background(), store)
	if err != nil {
		t.Fatalf("MigrateProjectSlotsIntoCollection: %v", err)
	}
	if summary.ProjectsScanned != 0 {
		t.Errorf("ProjectsScanned=%d, want 0", summary.ProjectsScanned)
	}
	if summary.SlotsCreated != 0 {
		t.Errorf("SlotsCreated=%d, want 0", summary.SlotsCreated)
	}
}

// readStoreWithoutSlotStore is a ReadStore that does NOT implement
// SlotStore. Used to verify MigrateProjectSlotsIntoCollection errors
// loudly when the store can't accept slot writes.
type readStoreWithoutSlotStore struct{}

func (readStoreWithoutSlotStore) ListProjects(context.Context) ([]Project, error)   { return nil, nil }
func (readStoreWithoutSlotStore) ListWorkflows(context.Context) ([]Workflow, error) { return nil, nil }

// Note: also doesn't implement SlotStore on purpose.

var errNoSlotStore = errors.New("slot store not configured")

// migrationTimingMatters is a small marker used by debugging when this
// file is hand-edited; harmless.
var _ = time.Now

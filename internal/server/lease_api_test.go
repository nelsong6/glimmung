package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakeLeaseStore struct {
	fakeReadStore
	lease    Lease
	leases   []Lease
	result   CancelLeaseResult
	leaseReq LeaseAcquireRequest
	// slotStatuses is an append-only log of every Slot write the fake
	// observed (via CreateSlot / UpdateIfMatch). Recorded in canonical
	// state-name vocabulary post-PR-518. Tests inspect it when they
	// care about the *sequence* of writes rather than the slot's
	// current state.
	slotStatuses []TestEnvironmentSlotStatus
	cancelledRef string
	err          error
	// leaseErr, when non-nil, fails ListLeases / AcquireLease only —
	// distinct from `err` which fails reads across the embedded
	// fakeReadStore (ListProjects etc.). Lets tests assert
	// downstream-Cosmos-outage behavior without breaking unrelated
	// reads needed to set up the test.
	leaseErr error
	// mu guards slotStatuses and the project mutations performed by
	// SetProjectTestEnvironmentSlotStatus. The reconciler now warms multiple
	// slots concurrently, so several goroutines may write through this fake at
	// the same time. Without the mutex appends race and the test sees a short
	// count or corrupted slice header.
	mu sync.Mutex
	// etag tracks the optimistic-concurrency cursor for the project doc.
	// Bumped on every write; ReadProject captures the current value;
	// SetProjectTestEnvironmentSlotStatusIfMatch returns ErrPreconditionFailed
	// when the caller's etag is stale. Lets multi-replica cleanup-claim
	// races be exercised in unit tests without a real Cosmos.
	etag int
	// slots is the new per-row slot storage backing the SlotStore interface.
	// Keyed by SlotDocID. Per-slot etag is tracked in slotEtags so
	// UpdateIfMatch's CAS semantics can be exercised.
	slots     map[string]Slot
	slotEtags map[string]int
	// slotHistory backs AppendSlotHistory/ListSlotHistory. One slice per
	// project; entries are appended in arrival order.
	slotHistory map[string][]SlotHistoryEntry
	// slotHistoryNextID is the counter used to assign synthetic ids to
	// SlotHistoryEntry rows that arrive with an empty ID.
	slotHistoryNextID int
	// seedMode skips appending to slotStatuses during CreateSlot /
	// UpdateIfMatch. Test helpers (seedSlot, seedSlotsFromLegacyMetadata)
	// flip this on while priming initial state so the slotStatuses
	// write-log only carries writes from production code paths under
	// test, not from fixture setup.
	seedMode bool
}

// beginSeed and endSeed wrap a block of test-fixture writes that should
// not show up in the slotStatuses write-log. The helpers in
// slot_test_helpers_test.go invoke these around their CreateSlot calls.
func (s *fakeLeaseStore) beginSeed() {
	s.mu.Lock()
	s.seedMode = true
	s.mu.Unlock()
}

func (s *fakeLeaseStore) endSeed() {
	s.mu.Lock()
	s.seedMode = false
	s.mu.Unlock()
}

func (s *fakeLeaseStore) ensureSlotsInitLocked() {
	if s.slots == nil {
		s.slots = map[string]Slot{}
	}
	if s.slotEtags == nil {
		s.slotEtags = map[string]int{}
	}
	if s.slotHistory == nil {
		s.slotHistory = map[string][]SlotHistoryEntry{}
	}
}

func (s *fakeLeaseStore) slotEtagLocked(key string) string {
	return "slot-etag-" + strconv.Itoa(s.slotEtags[key])
}

func (s *fakeLeaseStore) CreateSlot(_ context.Context, slot Slot) (Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSlotsInitLocked()
	key := slot.DocID()
	if existing, ok := s.slots[key]; ok {
		return existing.WithETag(s.slotEtagLocked(key)), nil
	}
	s.slotEtags[key]++
	s.slots[key] = slot
	s.recordLegacySlotStatusLocked(slot)
	return slot.WithETag(s.slotEtagLocked(key)), nil
}

func (s *fakeLeaseStore) GetSlot(_ context.Context, project string, slotIndex int) (Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSlotsInitLocked()
	key := SlotDocID(project, slotIndex)
	if stored, ok := s.slots[key]; ok {
		return stored.WithETag(s.slotEtagLocked(key)), nil
	}
	// Earlier the fake auto-synthesized Slot rows from the project's
	// legacy embedded `native_standby_dns.slots[]` array. That was the
	// PR #518 "legacy-compat bridge" the migration policy forbids:
	// tests had no way to distinguish "I forgot to seed a slot" from
	// "I'm depending on legacy data shape to round-trip." The bridge
	// is gone; tests seed via seedSlot / seedSlotsFromLegacyMetadata.
	return Slot{}, ErrNotFound
}

func (s *fakeLeaseStore) ListSlotsByProject(_ context.Context, project string) ([]Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSlotsInitLocked()
	var out []Slot
	for _, slot := range s.slots {
		if slot.Project == project {
			out = append(out, slot)
		}
	}
	return out, nil
}

func (s *fakeLeaseStore) UpdateIfMatch(_ context.Context, project string, slotIndex int, mutate func(Slot) (Slot, error)) (Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSlotsInitLocked()
	key := SlotDocID(project, slotIndex)
	stored, ok := s.slots[key]
	if !ok {
		return Slot{}, ErrNotFound
	}
	current := stored.WithETag(s.slotEtagLocked(key))
	next, err := mutate(current)
	if err != nil {
		return Slot{}, err
	}
	if next.Project != current.Project || next.SlotIndex != current.SlotIndex {
		return Slot{}, ErrInvalidSlotTransition
	}
	s.slotEtags[key]++
	s.slots[key] = next
	s.recordLegacySlotStatusLocked(next)
	return next.WithETag(s.slotEtagLocked(key)), nil
}

func (s *fakeLeaseStore) DeleteSlot(_ context.Context, project string, slotIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSlotsInitLocked()
	key := SlotDocID(project, slotIndex)
	delete(s.slots, key)
	delete(s.slotEtags, key)
	return nil
}

func (s *fakeLeaseStore) AppendSlotHistory(_ context.Context, entry SlotHistoryEntry) (SlotHistoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSlotsInitLocked()
	if entry.ID == "" {
		s.slotHistoryNextID++
		entry.ID = "history-" + strconv.Itoa(s.slotHistoryNextID)
	}
	s.slotHistory[entry.Project] = append(s.slotHistory[entry.Project], entry)
	// Mirror the history entry into the most recent matching slot status
	// in the legacy slotStatuses slice. Tests written before the slot
	// history was split out into its own collection still inspect
	// `status.ReturnHistory` for return-event payloads.
	legacy := TestSlotReturnHistoryEntry{
		Event:           entry.Event,
		CreatedAt:       entry.CreatedAt,
		Project:         entry.Project,
		SlotIndex:       entry.SlotIndex,
		SlotName:        entry.SlotName,
		LeaseRef:        entry.LeaseRef,
		LeaseNumber:     entry.LeaseNumber,
		LeaseRequester:  entry.LeaseRequester,
		CallerPodIP:     entry.CallerPodIP,
		CallerSessionID: entry.CallerSessionID,
		Source:          entry.Source,
		Reason:          entry.Reason,
		CleanupStarted:  entry.CleanupStarted,
	}
	for i := range s.slotStatuses {
		if entry.SlotIndex != nil && s.slotStatuses[i].SlotIndex == *entry.SlotIndex {
			s.slotStatuses[i].ReturnHistory = append(s.slotStatuses[i].ReturnHistory, legacy)
		}
	}
	return entry, nil
}

func (s *fakeLeaseStore) ListSlotHistory(_ context.Context, project string, slotIndex *int) ([]SlotHistoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSlotsInitLocked()
	entries := s.slotHistory[project]
	if slotIndex == nil {
		out := make([]SlotHistoryEntry, len(entries))
		copy(out, entries)
		return out, nil
	}
	var out []SlotHistoryEntry
	for _, e := range entries {
		if e.SlotIndex != nil && *e.SlotIndex == *slotIndex {
			out = append(out, e)
		}
	}
	return out, nil
}

// recordSlotStatusLocked mirrors a Slot write into the slotStatuses slice
// so existing tests can inspect the sequence of writes via the fake.
//
// After PR #518's storage rework the canonical state lives in the new
// `slots` collection (s.slots). slotStatuses is purely a write-log for
// test convenience; the state names recorded here are the **new**
// canonical names (`provisioning`, `provisioned`, `running`, etc.) — no
// retired-vocabulary translation. The earlier `legacyStateFromSlot`
// translator was deleted per docs/migration-policy.md: tests must assert
// against the canonical state vocabulary, not a parallel one.
func (s *fakeLeaseStore) recordLegacySlotStatusLocked(slot Slot) {
	// Skip the synthetic "unseeded" creates that show up via
	// ensureSlotExists — the slotStatuses log is intended to contain
	// writes that came from the lifecycle helpers, not the seeding
	// path.
	if slot.State == SlotStateUnseeded {
		return
	}
	// Skip writes that happen inside a seed block. Test helpers wrap
	// their fixture writes in beginSeed/endSeed so the slotStatuses
	// log only carries writes from production code paths under test.
	if s.seedMode {
		return
	}
	status := TestEnvironmentSlotStatus{
		SlotIndex: slot.SlotIndex,
		SlotName:  slot.SlotName,
		State:     slot.State,
		UpdatedAt: slot.UpdatedAt,
		Detail:    slot.Detail,
	}
	if slot.ProvisionedAt != nil {
		status.ReadyAt = slot.ProvisionedAt
	}
	if slot.ActivationAttempt != nil {
		status.ActivationAttempt = slot.ActivationAttempt
	}
	// ActivationState mirrors the slot's activation-phase lifecycle.
	// Derived from the activation_* fields rather than slot.State so it
	// persists across the broader state transitions (a slot that
	// reached `running` and is now `cleaning` still records its
	// activation outcome).
	if derived := derivedActivationState(slot); derived != nil {
		status.ActivationState = derived
	}
	if slot.ActivationStartedAt != nil {
		status.ActivationStartedAt = slot.ActivationStartedAt
	}
	if slot.ActivationCompletedAt != nil {
		status.ActivationCompletedAt = slot.ActivationCompletedAt
	}
	if slot.ActivationJobName != nil {
		status.ActivationJobName = slot.ActivationJobName
	}
	if slot.ActivationError != nil {
		status.ActivationError = slot.ActivationError
	}
	if slot.CleanupStartedAt != nil {
		status.CleanupStartedAt = slot.CleanupStartedAt
	}
	if slot.CleanupCompletedAt != nil {
		status.CleanupCompletedAt = slot.CleanupCompletedAt
	}
	if derived := derivedCleanupState(slot); derived != nil {
		status.CleanupState = derived
	}
	if slot.CleanupError != nil {
		status.CleanupError = slot.CleanupError
	}
	s.slotStatuses = append(s.slotStatuses, status)
}

func (s *fakeLeaseStore) AcquireLease(_ context.Context, req LeaseAcquireRequest) (Lease, error) {
	s.leaseReq = req
	if s.err != nil {
		return Lease{}, s.err
	}
	return s.lease, nil
}

func (s *fakeLeaseStore) CancelLeaseByRef(_ context.Context, _, ref string) (CancelLeaseResult, error) {
	s.cancelledRef = ref
	if s.err != nil {
		return CancelLeaseResult{}, s.err
	}
	return s.result, nil
}

func (s *fakeLeaseStore) ListLeases(context.Context) ([]Lease, error) {
	if s.leaseErr != nil {
		return nil, s.leaseErr
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.leases != nil {
		return s.leases, nil
	}
	return []Lease{s.lease}, nil
}

// SetProjectTestEnvironmentSlotStatus and its IfMatch sibling were the
// pre-PR-518 slot-status write path. They were deleted with the
// surrounding production code; no fake-side implementation is needed.
// Slot writes now go through SlotStore.UpdateIfMatch on the embedded
// `slots` map — see CreateSlot, GetSlot, UpdateIfMatch above.

// ReadProject implements ProjectReader for the fake. Returns the project
// with the current etag attached so callers can attempt etag-conditional
// writes.
func (s *fakeLeaseStore) ReadProject(_ context.Context, project string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.projects {
		if p.Name == project || p.ID == project {
			return p.WithETag(s.currentEtagLocked()), nil
		}
	}
	return Project{}, ErrNotFound
}

func (s *fakeLeaseStore) currentEtagLocked() string {
	return "etag-" + strconv.Itoa(s.etag)
}

// snapshotSlotStatuses returns a stable copy of the recorded status writes
// under the same lock that protects them. Tests that read slotStatuses while
// concurrent warmup goroutines are still appending must use this helper.
func (s *fakeLeaseStore) snapshotSlotStatuses() []TestEnvironmentSlotStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TestEnvironmentSlotStatus, len(s.slotStatuses))
	copy(out, s.slotStatuses)
	return out
}

func TestCreateLeaseRouteRetired(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeLeaseStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/lease", strings.NewReader(`{"project":"myproject"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCancelLeaseByRef(t *testing.T) {
	store := &fakeLeaseStore{
		result: CancelLeaseResult{
			State:    "cancelled",
			LeaseRef: "myproject/leases/3",
		},
	}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"project":"myproject","lease_ref":"myproject/leases/3"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/leases/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state":"cancelled"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCancelLeaseByRefNotFound(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeLeaseStore{err: ErrNotFound}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"project":"myproject","lease_ref":"myproject/leases/99"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/leases/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCancelLeaseByRefRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/leases/cancel", strings.NewReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

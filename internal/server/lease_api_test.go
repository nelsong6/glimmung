package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakeLeaseStore struct {
	fakeReadStore
	lease        Lease
	leases       []Lease
	result       CancelLeaseResult
	leaseReq     LeaseAcquireRequest
	slotStatuses []TestEnvironmentSlotStatus
	cancelledRef string
	err          error
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
	// Bridge for tests pre-populated with legacy slot data: if the
	// project's metadata still carries a slots[] entry for this index,
	// synthesize a Slot from it. Mirrors what
	// MigrateProjectSlotsIntoCollection does once per project at boot
	// in production. Without this bridge tests would have to call the
	// migration manually before every lifecycle test.
	for _, p := range s.projects {
		if p.Name != project && p.ID != project {
			continue
		}
		for _, entry := range readLegacyProjectSlots(p) {
			if entry.slotIndex != slotIndex {
				continue
			}
			slot := slotFromLegacyEntry(project, entry, time.Now().UTC())
			// Preserve the legacy entry's activation/cleanup fields
			// so tests that pre-populate those for diagnostics still
			// see them.
			if v, ok := positiveIntFromMap(entry.raw, "activation_attempt"); ok {
				attempt := v
				slot.ActivationAttempt = &attempt
			}
			if v, ok := stringFromMap(entry.raw, "activation_job_name"); ok && strings.TrimSpace(v) != "" {
				job := strings.TrimSpace(v)
				slot.ActivationJobName = &job
			}
			if v, ok := stringFromMap(entry.raw, "activation_started_at"); ok {
				if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
					slot.ActivationStartedAt = &t
				}
			}
			if v, ok := stringFromMap(entry.raw, "activation_completed_at"); ok {
				if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
					slot.ActivationCompletedAt = &t
				}
			}
			// If the legacy entry didn't carry per-phase timestamps
			// but the legacy state implies activation reached `active`,
			// infer ActivationCompletedAt from the updated_at timestamp
			// so the derived ActivationState in recordLegacySlotStatusLocked
			// reports "active" instead of falling through.
			if slot.ActivationCompletedAt == nil && slot.State == SlotStateRunning {
				t := slot.UpdatedAt
				slot.ActivationCompletedAt = &t
				if slot.ActivationStartedAt == nil {
					slot.ActivationStartedAt = &t
				}
			}
			s.slotEtags[key]++
			s.slots[key] = slot
			return slot.WithETag(s.slotEtagLocked(key)), nil
		}
	}
	return Slot{}, ErrNotFound
}

func (s *fakeLeaseStore) ListSlotsByProject(_ context.Context, project string) ([]Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSlotsInitLocked()
	// Bridge: lazy-translate any legacy embedded-array entries into the
	// new collection so tests that only populate project metadata see
	// them through this listing path. Mirrors the GetSlot bridge above —
	// production code now reads slots exclusively from the new
	// collection, so the legacy fixture compat lives entirely in the
	// fake. Idempotent: per-key existence check prevents the bridge
	// from clobbering slots already written via UpdateIfMatch.
	for _, p := range s.projects {
		if p.Name != project && p.ID != project {
			continue
		}
		for _, entry := range readLegacyProjectSlots(p) {
			key := SlotDocID(project, entry.slotIndex)
			if _, exists := s.slots[key]; exists {
				continue
			}
			slot := slotFromLegacyEntry(project, entry, time.Now().UTC())
			if v, ok := positiveIntFromMap(entry.raw, "activation_attempt"); ok {
				attempt := v
				slot.ActivationAttempt = &attempt
			}
			if v, ok := stringFromMap(entry.raw, "activation_job_name"); ok && strings.TrimSpace(v) != "" {
				job := strings.TrimSpace(v)
				slot.ActivationJobName = &job
			}
			if v, ok := stringFromMap(entry.raw, "activation_started_at"); ok {
				if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
					slot.ActivationStartedAt = &t
				}
			}
			if v, ok := stringFromMap(entry.raw, "activation_completed_at"); ok {
				if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
					slot.ActivationCompletedAt = &t
				}
			}
			if slot.ActivationCompletedAt == nil && slot.State == SlotStateRunning {
				t := slot.UpdatedAt
				slot.ActivationCompletedAt = &t
				if slot.ActivationStartedAt == nil {
					slot.ActivationStartedAt = &t
				}
			}
			s.slotEtags[key]++
			s.slots[key] = slot
		}
	}
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

// recordLegacySlotStatusLocked mirrors a Slot write into the legacy
// slotStatuses slice that existing tests still inspect. The state name
// is translated back to the legacy vocabulary (provisioning→warming,
// provisioned→ready, running→active) so test assertions written before
// the rename keep passing during the cutover. After Stage 2 is fully
// landed and tests have been updated to use the new state names, the
// slotStatuses slice and this translator can be deleted.
func (s *fakeLeaseStore) recordLegacySlotStatusLocked(slot Slot) {
	// Skip the synthetic "unseeded" creates that show up via
	// ensureSlotExists — old tests expect the legacy slotStatuses slice
	// to contain only the writes that came from the actual lifecycle
	// helpers (warming/activating/cleaning/etc.). The new SlotStore
	// model creates the slot doc lazily, but that's an implementation
	// detail that doesn't belong in the legacy view.
	if slot.State == SlotStateUnseeded {
		return
	}
	status := TestEnvironmentSlotStatus{
		SlotIndex: slot.SlotIndex,
		SlotName:  slot.SlotName,
		State:     legacyStateFromSlot(slot.State),
		UpdatedAt: slot.UpdatedAt,
		Detail:    slot.Detail,
	}
	if slot.ProvisionedAt != nil {
		status.ReadyAt = slot.ProvisionedAt
	}
	if slot.ActivationAttempt != nil {
		status.ActivationAttempt = slot.ActivationAttempt
	}
	// ActivationState mirrors the slot's activation-phase lifecycle in
	// the legacy shape. It persists across the broader slot State so
	// that operators inspecting an error'd slot can still see what its
	// most recent activation outcome was. Derive it from the
	// activation_* fields rather than the slot.State directly.
	switch {
	case slot.ActivationError != nil:
		state := "error"
		status.ActivationState = &state
	case slot.ActivationCompletedAt != nil:
		state := "active"
		status.ActivationState = &state
	case slot.ActivationStartedAt != nil:
		state := "activating"
		status.ActivationState = &state
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
		cleaningState := "cleaning"
		status.CleanupState = &cleaningState
	}
	if slot.CleanupCompletedAt != nil {
		status.CleanupCompletedAt = slot.CleanupCompletedAt
		switch slot.State {
		case SlotStateProvisioned:
			done := "ready"
			status.CleanupState = &done
		case SlotStateError:
			errored := "error"
			status.CleanupState = &errored
		}
	}
	if slot.CleanupError != nil {
		status.CleanupError = slot.CleanupError
	}
	s.slotStatuses = append(s.slotStatuses, status)
}

func legacyStateFromSlot(state string) string {
	switch state {
	case SlotStateProvisioning:
		return "warming"
	case SlotStateProvisioned:
		return "ready"
	case SlotStateRunning:
		return "active"
	default:
		return state
	}
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

func (s *fakeLeaseStore) ExtendLeaseTTLByRef(_ context.Context, project, ref string, extendSeconds int) (Lease, error) {
	if s.err != nil {
		return Lease{}, s.err
	}
	var leases []Lease
	if s.leases != nil {
		leases = s.leases
	} else {
		leases = []Lease{s.lease}
	}
	for i := range leases {
		if leases[i].Project != project || LeasePublicRefFromLease(leases[i]) != ref {
			continue
		}
		if leases[i].State != "claimed" {
			return Lease{}, ErrConflict
		}
		expiresAt := testSlotLeaseExpiresAt(leases[i])
		if expiresAt == nil || !expiresAt.After(time.Now().UTC()) {
			return Lease{}, ErrConflict
		}
		leases[i].TTLSeconds += extendSeconds
		if s.leases != nil {
			s.leases[i] = leases[i]
		} else {
			s.lease = leases[i]
		}
		return leases[i], nil
	}
	return Lease{}, ErrNotFound
}

func (s *fakeLeaseStore) ListLeases(context.Context) ([]Lease, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.leases != nil {
		return s.leases, nil
	}
	return []Lease{s.lease}, nil
}

// AnyLockHeld satisfies StateStore. Lease tests do not exercise the
// inflight-locks snapshot field, so a no-locks-held stub is the
// honest answer here.
func (s *fakeLeaseStore) AnyLockHeld(context.Context, string) (bool, error) {
	return false, nil
}

func (s *fakeLeaseStore) SetProjectTestEnvironmentSlotStatus(_ context.Context, project string, status TestEnvironmentSlotStatus) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeSlotStatusLocked(project, status, "")
}

// SetProjectTestEnvironmentSlotStatusIfMatch implements
// ProjectTestEnvironmentSlotStatusClaimer for the fake. When `ifMatchEtag`
// is non-empty and disagrees with the store's current etag, returns
// ErrPreconditionFailed without mutating state — simulating the Cosmos
// 412 path that makes the multi-replica cleanup-claim race safe.
func (s *fakeLeaseStore) SetProjectTestEnvironmentSlotStatusIfMatch(_ context.Context, project string, status TestEnvironmentSlotStatus, ifMatchEtag string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeSlotStatusLocked(project, status, ifMatchEtag)
}

func (s *fakeLeaseStore) writeSlotStatusLocked(project string, status TestEnvironmentSlotStatus, ifMatchEtag string) (Project, error) {
	if ifMatchEtag != "" && ifMatchEtag != s.currentEtagLocked() {
		return Project{}, ErrPreconditionFailed
	}
	s.slotStatuses = append(s.slotStatuses, status)
	for i := range s.projects {
		if s.projects[i].Name != project && s.projects[i].ID != project {
			continue
		}
		if s.projects[i].Metadata == nil {
			s.projects[i].Metadata = map[string]any{}
		}
		standby, _ := s.projects[i].Metadata["native_standby_dns"].(map[string]any)
		if standby == nil {
			standby = map[string]any{}
		}
		slots, _ := standby["slots"].([]any)
		replaced := false
		for j, raw := range slots {
			slot, _ := raw.(map[string]any)
			if slot == nil {
				continue
			}
			if index, ok := positiveIntFromMap(slot, "slot_index"); ok && index == status.SlotIndex {
				slots[j] = testSlotStatusMap(status)
				replaced = true
			}
		}
		if !replaced {
			slots = append(slots, testSlotStatusMap(status))
		}
		standby["slots"] = slots
		s.projects[i].Metadata["native_standby_dns"] = standby
		s.etag++
		return s.projects[i].WithETag(s.currentEtagLocked()), nil
	}
	return Project{}, ErrNotFound
}

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

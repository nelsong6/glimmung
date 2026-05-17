package server

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeStateStore is the /v1/state test double. Wraps a *fakeLeaseStore
// so it gets a working SlotStore + SlotHistoryStore implementation —
// production /v1/state reads slot rows via SlotStore.ListSlotsByProject
// after PR #518, so the fake must satisfy that interface for the
// rendered snapshot to contain any slot data.
//
// Pointer field (not embedded value) because fakeLeaseStore carries a
// mutex; value-copying the struct around (state_api tests previously
// passed `store` by value) would unsafely duplicate the mutex.
type fakeStateStore struct {
	fakeReadStore
	leases []Lease
	err    error
	// slotStore is lazily allocated by ensureSlotStore; tests that
	// don't exercise slot rows can leave it nil and a single-shared
	// fakeLeaseStore is created on first seed call.
	slotStore *fakeLeaseStore
}

func (s *fakeStateStore) ensureSlotStore() *fakeLeaseStore {
	if s.slotStore == nil {
		s.slotStore = &fakeLeaseStore{fakeReadStore: s.fakeReadStore}
	}
	return s.slotStore
}

func (s *fakeStateStore) CreateSlot(ctx context.Context, slot Slot) (Slot, error) {
	return s.ensureSlotStore().CreateSlot(ctx, slot)
}

func (s *fakeStateStore) GetSlot(ctx context.Context, project string, slotIndex int) (Slot, error) {
	return s.ensureSlotStore().GetSlot(ctx, project, slotIndex)
}

func (s *fakeStateStore) ListSlotsByProject(ctx context.Context, project string) ([]Slot, error) {
	return s.ensureSlotStore().ListSlotsByProject(ctx, project)
}

func (s *fakeStateStore) UpdateIfMatch(ctx context.Context, project string, slotIndex int, mutate func(Slot) (Slot, error)) (Slot, error) {
	return s.ensureSlotStore().UpdateIfMatch(ctx, project, slotIndex, mutate)
}

func (s *fakeStateStore) DeleteSlot(ctx context.Context, project string, slotIndex int) error {
	return s.ensureSlotStore().DeleteSlot(ctx, project, slotIndex)
}

func (s *fakeStateStore) AppendSlotHistory(ctx context.Context, entry SlotHistoryEntry) (SlotHistoryEntry, error) {
	return s.ensureSlotStore().AppendSlotHistory(ctx, entry)
}

func (s *fakeStateStore) ListSlotHistory(ctx context.Context, project string, slotIndex *int) ([]SlotHistoryEntry, error) {
	return s.ensureSlotStore().ListSlotHistory(ctx, project, slotIndex)
}

func (s *fakeStateStore) beginSeed() { s.ensureSlotStore().beginSeed() }
func (s *fakeStateStore) endSeed()   { s.ensureSlotStore().endSeed() }

func (s *fakeStateStore) ListLeases(context.Context) ([]Lease, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.leases, nil
}

func TestStateSnapshotUsesPublicLeaseRefs(t *testing.T) {
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	leaseID := "01JLEASEBACKING"
	store := &fakeStateStore{
		leases: []Lease{{
			ID:           leaseID,
			LeaseNumber:  intPtr(17),
			Project:      "ambience",
			Workflow:     stringPtr("agent-run"),
			Host:         stringPtr("runner-1"),
			State:        "claimed",
			Requirements: map[string]any{},
			Metadata:     map[string]any{},
			RequestedAt:  now,
			AssignedAt:   &now,
			TTLSeconds:   900,
		}},
	}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/state", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ref":"ambience/leases/17"`) {
		t.Fatalf("body=%s, missing public lease ref", body)
	}
	if strings.Contains(body, leaseID) {
		t.Fatalf("body leaks backing lease id: %s", body)
	}
}

func TestStateSnapshotIncludesTestEnvironmentsAndWaitingRequests(t *testing.T) {
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := &fakeStateStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "glimmung",
			Name: "glimmung",
			Metadata: map[string]any{
				"native_standby_dns": map[string]any{
					"enabled":     true,
					"count":       float64(2),
					"slot_prefix": "glimmung-slot",
					"slots": []any{
						map[string]any{"slot_index": float64(1), "slot_name": "glimmung-slot-1", "state": "ready"},
						map[string]any{"slot_index": float64(2), "slot_name": "glimmung-slot-2", "state": "ready"},
					},
				},
			},
			CreatedAt: now,
		}}},
		leases: []Lease{
			{
				ID:          "lease-1",
				Project:     "glimmung",
				Workflow:    stringPtr("test-slot-checkout"),
				State:       "claimed",
				Metadata:    map[string]any{"test_slot_checkout": true, "native_slot_index": "1", "native_slot_name": "glimmung-slot-1"},
				RequestedAt: now,
			},
			{
				ID:                 "request-2",
				Kind:               "test_slot_request",
				Project:            "glimmung",
				State:              "waiting",
				RequestedSlotIndex: intPtr(2),
				Metadata:           map[string]any{},
				RequestedAt:        now,
			},
		},
	}
	seedSlotsFromLegacyMetadata(t, store, store, "glimmung")
	handler := NewWithStore(Settings{NativeRunnerProjectConcurrency: 5}, store)

	var snapshot StateSnapshot
	getJSON(t, handler, "/v1/state", &snapshot)

	if len(snapshot.TestEnvironments) != 2 {
		t.Fatalf("test_environments=%#v", snapshot.TestEnvironments)
	}
	if snapshot.TestEnvironments[0].State != "claimed" || snapshot.TestEnvironments[0].SlotName != "glimmung-slot-1" {
		t.Fatalf("claimed env=%#v", snapshot.TestEnvironments[0])
	}
	if snapshot.TestEnvironments[1].State != "available" || len(snapshot.TestEnvironments[1].WaitingRequests) != 1 {
		t.Fatalf("waiting env=%#v", snapshot.TestEnvironments[1])
	}
	if snapshot.WaitingTestSlotRequests[0].Ref != "glimmung/test-requests/request-2" {
		t.Fatalf("waiting refs=%#v", snapshot.WaitingTestSlotRequests)
	}
}

func TestTestEnvironmentStatusShowsActivatingSlot(t *testing.T) {
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	detail := "test-slot runtime activation is in progress"
	store := &fakeStateStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "tank",
			Name: "tank",
			Metadata: map[string]any{
				"native_standby_dns": map[string]any{
					"count":       float64(1),
					"slot_prefix": "tank-slot",
					"slots": []any{
						map[string]any{
							"slot_index": float64(1),
							"slot_name":  "tank-slot-1",
							"state":      "activating",
							"detail":     detail,
							"updated_at": now.Format(time.RFC3339Nano),
						},
					},
				},
			},
			CreatedAt: now,
		}}},
		leases: []Lease{{
			ID:          "lease-1",
			Project:     "tank",
			Workflow:    stringPtr("test-slot-checkout"),
			State:       "claimed",
			Metadata:    map[string]any{"test_slot_checkout": true, "native_slot_index": "1", "native_slot_name": "tank-slot-1"},
			RequestedAt: now,
		}},
	}
	seedSlotsFromLegacyMetadata(t, store, store, "tank")
	handler := NewWithStore(Settings{}, store)

	var env TestEnvironmentPublic
	getJSON(t, handler, "/v1/projects/tank/test-environments/tank-slot-1", &env)

	if env.State != "activating" || env.Usable || env.Detail == nil || *env.Detail != detail {
		t.Fatalf("env=%#v", env)
	}
	if env.UpdatedAt == nil || !env.UpdatedAt.Equal(now) {
		t.Fatalf("updated_at=%v, want %v", env.UpdatedAt, now)
	}
	if env.Lease == nil || env.Lease.Ref != "tank-slot-1" {
		t.Fatalf("lease=%#v", env.Lease)
	}
}

func TestStateSnapshotEmitsEmptyStateForUnseededSlots(t *testing.T) {
	// A project with count=3 but no slots[*] records yet (count just set, or
	// reconciler has not ticked) must render state="" — not a synthesized
	// "warming". The dashboard owns the labeling for unseeded slots; the API
	// must mirror durable storage honestly.
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := &fakeStateStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "fresh",
			Name: "fresh",
			Metadata: map[string]any{
				"native_standby_dns": map[string]any{
					"enabled":     true,
					"count":       float64(3),
					"slot_prefix": "fresh-slot",
				},
			},
			CreatedAt: now,
		}}},
	}
	handler := NewWithStore(Settings{}, store)

	var snapshot StateSnapshot
	getJSON(t, handler, "/v1/state", &snapshot)

	if len(snapshot.TestEnvironments) != 3 {
		t.Fatalf("test_environments=%#v, want 3 rows for count=3", snapshot.TestEnvironments)
	}
	for i, env := range snapshot.TestEnvironments {
		if env.State != "" {
			t.Fatalf("env[%d].State=%q, want empty (no synthesized warming for unseeded slots)", i, env.State)
		}
		if env.Usable {
			t.Fatalf("env[%d].Usable=true for unseeded slot", i)
		}
		if env.Lease != nil {
			t.Fatalf("env[%d].Lease=%#v, want nil for unseeded slot", i, env.Lease)
		}
	}
}

func TestStateSnapshotDoesNotInferNativeSlotsFromAppType(t *testing.T) {
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := &fakeStateStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:        "glimmung",
			Name:      "glimmung",
			Metadata:  map[string]any{"app_type": "native_web_app"},
			CreatedAt: now,
		}}},
	}
	handler := NewWithStore(Settings{NativeRunnerProjectConcurrency: 3}, store)

	var snapshot StateSnapshot
	getJSON(t, handler, "/v1/state", &snapshot)

	if len(snapshot.TestEnvironments) != 0 {
		t.Fatalf("test_environments=%#v", snapshot.TestEnvironments)
	}
}

func TestStateSnapshotIgnoresOutOfRangeNativeSlots(t *testing.T) {
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := &fakeStateStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "tank-operator",
			Name: "tank-operator",
			Metadata: map[string]any{
				"native_standby_dns": map[string]any{
					"count":       float64(2),
					"slot_prefix": "tank-operator-slot",
				},
			},
			CreatedAt: now,
		}}},
		leases: []Lease{{
			ID:          "old-slot",
			Project:     "tank-operator",
			Workflow:    stringPtr("test-slot-checkout"),
			State:       "claimed",
			Metadata:    map[string]any{"test_slot_checkout": true, "native_slot_index": "99", "native_slot_name": "tank-operator-slot-99"},
			RequestedAt: now,
		}},
	}
	handler := NewWithStore(Settings{NativeRunnerProjectConcurrency: 10}, store)

	var snapshot StateSnapshot
	getJSON(t, handler, "/v1/state", &snapshot)

	if len(snapshot.TestEnvironments) != 2 {
		t.Fatalf("test_environments=%#v", snapshot.TestEnvironments)
	}
	for _, env := range snapshot.TestEnvironments {
		if strings.Contains(env.SlotName, "99") {
			t.Fatalf("out-of-range slot leaked into queue: %#v", snapshot.TestEnvironments)
		}
	}
}

func TestStateSnapshotRequiresStateStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/state", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestStateSnapshotStoreErrorsReturn500(t *testing.T) {
	handler := NewWithStore(Settings{}, &fakeStateStore{
		fakeReadStore: fakeReadStore{},
		err:           errors.New("boom"),
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/state", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestStateEventsRequiresStateStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestStateEventsStreamsInitialStateEvent(t *testing.T) {
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := &fakeStateStore{
		leases: []Lease{{
			ID:          "lease-1",
			Project:     "ambience",
			State:       "claimed",
			Metadata:    map[string]any{"native_slot_name": "ambience-slot-1"},
			RequestedAt: now,
		}},
	}
	server := httptest.NewServer(NewWithStore(Settings{}, store))
	defer server.Close()

	resp, err := http.Get(server.URL + "/v1/events")
	if err != nil {
		t.Fatalf("GET /v1/events: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if contentType := resp.Header.Get("content-type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("content-type=%q", contentType)
	}

	reader := bufio.NewReader(resp.Body)
	var lines []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE line: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		lines = append(lines, line)
	}
	event := strings.Join(lines, "\n")
	if !strings.Contains(event, "event: state") {
		t.Fatalf("event=%q", event)
	}
	if !strings.Contains(event, `"ref":"ambience-slot-1"`) {
		t.Fatalf("event=%q, missing state payload", event)
	}
}

func intPtr(value int) *int {
	return &value
}

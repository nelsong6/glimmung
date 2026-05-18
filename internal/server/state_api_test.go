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

type fakeStateStore struct {
	fakeReadStore
	leases       []Lease
	err          error
	issuesLocked bool
	prsLocked    bool
}

func (s fakeStateStore) ListLeases(context.Context) ([]Lease, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.leases, nil
}

// SlotStore + SlotHistoryStore satisfied so state_api tests with legacy
// embedded-slot fixtures still render through the production code path
// that now reads exclusively from the SlotStore. The reads bridge fresh
// from the embedded legacy array on every call; writes panic because
// fakeStateStore is used by /v1/state tests that don't mutate slots —
// any future test that needs write paths should use fakeLeaseStore,
// which has the full slot machinery.
func (s fakeStateStore) ListSlotsByProject(_ context.Context, project string) ([]Slot, error) {
	var out []Slot
	for _, p := range s.projects {
		if p.Name != project && p.ID != project {
			continue
		}
		for _, entry := range readLegacyProjectSlots(p) {
			slot := slotFromLegacyEntry(project, entry, time.Now().UTC())
			if v, ok := stringFromMap(entry.raw, "detail"); ok && strings.TrimSpace(v) != "" {
				detail := strings.TrimSpace(v)
				slot.Detail = &detail
			}
			out = append(out, slot)
		}
	}
	return out, nil
}

func (s fakeStateStore) GetSlot(_ context.Context, project string, slotIndex int) (Slot, error) {
	for _, p := range s.projects {
		if p.Name != project && p.ID != project {
			continue
		}
		for _, entry := range readLegacyProjectSlots(p) {
			if entry.slotIndex == slotIndex {
				slot := slotFromLegacyEntry(project, entry, time.Now().UTC())
				if v, ok := stringFromMap(entry.raw, "detail"); ok && strings.TrimSpace(v) != "" {
					detail := strings.TrimSpace(v)
					slot.Detail = &detail
				}
				return slot, nil
			}
		}
	}
	return Slot{}, ErrNotFound
}

func (s fakeStateStore) CreateSlot(context.Context, Slot) (Slot, error) {
	panic("fakeStateStore is read-only; use fakeLeaseStore for slot write paths")
}

func (s fakeStateStore) UpdateIfMatch(context.Context, string, int, func(Slot) (Slot, error)) (Slot, error) {
	panic("fakeStateStore is read-only; use fakeLeaseStore for slot write paths")
}

func (s fakeStateStore) DeleteSlot(context.Context, string, int) error {
	panic("fakeStateStore is read-only; use fakeLeaseStore for slot write paths")
}

// ListSlotHistory returns empty so state_api can render envs without a
// history-store crash. fakeStateStore tests that need history use
// fakeLeaseStore instead.
func (s fakeStateStore) ListSlotHistory(_ context.Context, _ string, _ map[string]any) ([]SlotHistoryEntry, error) {
	return nil, nil
}

func (s fakeStateStore) AppendSlotHistory(context.Context, SlotHistoryEntry) (SlotHistoryEntry, error) {
	panic("fakeStateStore is read-only; use fakeLeaseStore for slot write paths")
}

// AnyLockHeld lets fakeStateStore satisfy StateStore so the state
// snapshot handler can populate InflightLocks without polling. Tests
// that care about the inflight summary set issuesLocked / prsLocked
// directly; the default false/false matches the no-locks-held case.
func (s fakeStateStore) AnyLockHeld(_ context.Context, scope string) (bool, error) {
	switch scope {
	case "issue":
		return s.issuesLocked, nil
	case "pr":
		return s.prsLocked, nil
	}
	return false, nil
}

func TestStateSnapshotUsesPublicLeaseRefs(t *testing.T) {
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	leaseID := "01JLEASEBACKING"
	store := fakeStateStore{
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

// TestStateSnapshotIncludesInflightLocks pins the Stage 3 wire shape.
// The SPA derives its "needs attention" pulse from this field; before
// the migration it polled /v1/issues + /v1/touchpoints every 20s only
// to compute the same booleans. A regression that drops the field
// would silently revert the SPA to no pulse — visible to operators
// but not to CI without this test.
func TestStateSnapshotIncludesInflightLocks(t *testing.T) {
	cases := []struct {
		name         string
		issuesLocked bool
		prsLocked    bool
		wantIssues   bool
		wantPRs      bool
	}{
		{name: "no locks", issuesLocked: false, prsLocked: false, wantIssues: false, wantPRs: false},
		{name: "issue lock only", issuesLocked: true, prsLocked: false, wantIssues: true, wantPRs: false},
		{name: "pr lock only", issuesLocked: false, prsLocked: true, wantIssues: false, wantPRs: true},
		{name: "both", issuesLocked: true, prsLocked: true, wantIssues: true, wantPRs: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := fakeStateStore{
				issuesLocked: tc.issuesLocked,
				prsLocked:    tc.prsLocked,
			}
			handler := NewWithStore(Settings{}, store)
			var snapshot StateSnapshot
			getJSON(t, handler, "/v1/state", &snapshot)
			if snapshot.InflightLocks.Issues != tc.wantIssues {
				t.Errorf("inflight_locks.issues = %v, want %v", snapshot.InflightLocks.Issues, tc.wantIssues)
			}
			if snapshot.InflightLocks.PRs != tc.wantPRs {
				t.Errorf("inflight_locks.prs = %v, want %v", snapshot.InflightLocks.PRs, tc.wantPRs)
			}
		})
	}
}

func TestStateSnapshotIncludesTestEnvironmentsAndWaitingRequests(t *testing.T) {
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := fakeStateStore{
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
	store := fakeStateStore{
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
	store := fakeStateStore{
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
	store := fakeStateStore{
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
	store := fakeStateStore{
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
	handler := NewWithStore(Settings{}, fakeStateStore{
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
	store := fakeStateStore{
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

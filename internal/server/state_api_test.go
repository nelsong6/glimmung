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
	hosts  []Host
	leases []Lease
	err    error
}

func (s fakeStateStore) ListHosts(context.Context) ([]Host, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.hosts, nil
}

func (s fakeStateStore) ListLeases(context.Context) ([]Lease, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.leases, nil
}

func TestStateSnapshotUsesPublicLeaseRefs(t *testing.T) {
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	leaseID := "01JLEASEBACKING"
	store := fakeStateStore{
		hosts: []Host{{
			ID:             "runner-1",
			Name:           "runner-1",
			Capabilities:   map[string]any{},
			CurrentLeaseID: &leaseID,
			LastHeartbeat:  &now,
			LastUsedAt:     &now,
			CreatedAt:      now,
		}},
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
	if !strings.Contains(body, `"current_lease_ref":"ambience/leases/17"`) {
		t.Fatalf("body=%s, missing public host lease ref", body)
	}
	if !strings.Contains(body, `"ref":"ambience/leases/17"`) {
		t.Fatalf("body=%s, missing public lease ref", body)
	}
	if strings.Contains(body, leaseID) {
		t.Fatalf("body leaks backing lease id: %s", body)
	}
}

func TestStateSnapshotIncludesTestEnvironmentsAndWaitingRequests(t *testing.T) {
	now := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := fakeStateStore{
		fakeReadStore: fakeReadStore{projects: []Project{{
			ID:   "glimmung",
			Name: "glimmung",
			Metadata: map[string]any{
				"native_standby_dns": map[string]any{"enabled": true, "count": float64(2), "slot_prefix": "glimmung-slot"},
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

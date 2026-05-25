package cosmos

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/server"
)

// selectReadySlotIndices is the pure-function form of the slot-eligibility
// filter the dispatcher uses. It only returns indices whose state is
// `provisioned` and which fall inside the project's declared 1..count
// bound. The function exists so the contract is testable without a
// live Cosmos round trip; the wrapper nativeReadySlots is the
// Cosmos-backed callsite and is exercised in live_smoke_test.go.
//
// The bug this fix repairs (PR #518 left nativeReadySlots reading
// project metadata that #518 stopped populating) means the production
// observation before the fix was "0 ready slots, every checkout 503s,
// glimmung_unavailable_total ticks". The fixture cases below pin the
// post-fix contract.
func TestSelectReadySlotIndices(t *testing.T) {
	t.Parallel()
	mk := func(idx int, state string) server.Slot {
		return server.Slot{Project: "p", SlotIndex: idx, State: state}
	}
	cases := []struct {
		name  string
		slots []server.Slot
		count int
		want  []int
	}{
		{
			name:  "no slots returns empty",
			slots: nil,
			count: 5,
			want:  []int{},
		},
		{
			name: "only provisioned counts",
			slots: []server.Slot{
				mk(1, server.SlotStateProvisioned),
				mk(2, server.SlotStateUnseeded),
				mk(3, server.SlotStateProvisioning),
				mk(4, server.SlotStateActivating),
				mk(5, server.SlotStateRunning),
				mk(6, server.SlotStateCleaning),
				mk(7, server.SlotStateError),
			},
			count: 10,
			want:  []int{1},
		},
		{
			name: "out-of-range indices are dropped",
			slots: []server.Slot{
				mk(0, server.SlotStateProvisioned),
				mk(1, server.SlotStateProvisioned),
				mk(2, server.SlotStateProvisioned),
				mk(3, server.SlotStateProvisioned),
			},
			count: 2,
			want:  []int{1, 2},
		},
		{
			name: "result is sorted ascending",
			slots: []server.Slot{
				mk(5, server.SlotStateProvisioned),
				mk(1, server.SlotStateProvisioned),
				mk(3, server.SlotStateProvisioned),
			},
			count: 5,
			want:  []int{1, 3, 5},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := selectReadySlotIndices(tc.slots, tc.count)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSelectAvailableNativeSlotUsesProjectLocalClaims(t *testing.T) {
	t.Parallel()
	claimed := func(project string, slot int) leaseDoc {
		return leaseDoc{
			Project:  project,
			State:    "claimed",
			Metadata: map[string]any{"native_slot_index": strconv.Itoa(slot)},
		}
	}
	cases := []struct {
		name       string
		ready      []int
		claimed    []leaseDoc
		projectCap int
		want       *int
	}{
		{
			name:       "first ready slot when project has no claims",
			ready:      []int{1, 2},
			projectCap: 5,
			want:       intPtr(1),
		},
		{
			name:       "skips claimed slot",
			ready:      []int{1, 2, 3},
			claimed:    []leaseDoc{claimed("ambience", 1)},
			projectCap: 5,
			want:       intPtr(2),
		},
		{
			name:       "project cap blocks project only",
			ready:      []int{1, 2, 3},
			claimed:    []leaseDoc{claimed("ambience", 1), claimed("ambience", 2)},
			projectCap: 2,
			want:       nil,
		},
		{
			name:       "all ready slots already claimed",
			ready:      []int{1, 2},
			claimed:    []leaseDoc{claimed("ambience", 1), claimed("ambience", 2)},
			projectCap: 5,
			want:       nil,
		},
		{
			name:  "cross-project claims do not consume capacity",
			ready: []int{1, 2},
			claimed: []leaseDoc{
				claimed("tank-operator", 1),
				claimed("tank-operator", 2),
				claimed("tank-operator", 3),
			},
			projectCap: 2,
			want:       intPtr(1),
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := selectAvailableNativeSlot("ambience", tc.ready, tc.claimed, tc.projectCap)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %v", *tc.want)
			}
			if *got != *tc.want {
				t.Fatalf("got %v, want %v", *got, *tc.want)
			}
		})
	}
}

func TestSlotDocMarshalsCanonicalShape(t *testing.T) {
	now := time.Date(2026, 5, 17, 8, 0, 0, 0, time.UTC)
	slot := server.NewUnseededSlot("tank-operator", 5, "tank-operator-slot-5", now)
	provisioned, err := slot.MarkProvisioning(now)
	if err != nil {
		t.Fatalf("MarkProvisioning: %v", err)
	}

	doc := newSlotDoc(provisioned)
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(payload)

	// Identity fields the cosmos layer needs.
	for _, want := range []string{
		`"id":"tank-operator:5"`,
		`"project":"tank-operator"`,
		`"slot_index":5`,
		`"slot_name":"tank-operator-slot-5"`,
		`"state":"provisioning"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s — got %s", want, s)
		}
	}

	// Pointer fields with nil values must be omitted (omitempty contract).
	for _, mustNotContain := range []string{
		`"detail"`,
		`"provisioned_at"`,
		`"active_lease_ref"`,
		`"activation_attempt"`,
		`"activation_started_at"`,
		`"activation_completed_at"`,
		`"activation_job_name"`,
		`"activation_error"`,
		`"cleanup_started_at"`,
		`"cleanup_completed_at"`,
		`"cleanup_error"`,
	} {
		if strings.Contains(s, mustNotContain) {
			t.Errorf("payload should omit nil %s — got %s", mustNotContain, s)
		}
	}
}

func TestSlotDocRoundTripPreservesAllFields(t *testing.T) {
	now := time.Date(2026, 5, 17, 8, 0, 0, 123456789, time.UTC)
	leaseRef := "tank-operator/leases/83"
	jobName := "tank-operator-slot-5-installer"
	in := server.Slot{
		Project:               "tank-operator",
		SlotIndex:             5,
		SlotName:              "tank-operator-slot-5",
		State:                 server.SlotStateRunning,
		UpdatedAt:             now,
		ProvisionedAt:         timePtr(now.Add(-time.Hour)),
		ActiveLeaseRef:        &leaseRef,
		ActivationAttempt:     intP(2),
		ActivationStartedAt:   timePtr(now.Add(-30 * time.Minute)),
		ActivationCompletedAt: timePtr(now.Add(-15 * time.Minute)),
		ActivationJobName:     &jobName,
	}

	payload, err := json.Marshal(newSlotDoc(in))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var doc slotDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	out := doc.Slot

	if out.Project != in.Project ||
		out.SlotIndex != in.SlotIndex ||
		out.SlotName != in.SlotName ||
		out.State != in.State {
		t.Errorf("identity fields differ: in=%+v out=%+v", in, out)
	}
	if !out.UpdatedAt.Equal(in.UpdatedAt) {
		t.Errorf("UpdatedAt: in=%v out=%v", in.UpdatedAt, out.UpdatedAt)
	}
	if out.ProvisionedAt == nil || !out.ProvisionedAt.Equal(*in.ProvisionedAt) {
		t.Errorf("ProvisionedAt: in=%v out=%v", in.ProvisionedAt, out.ProvisionedAt)
	}
	if out.ActiveLeaseRef == nil || *out.ActiveLeaseRef != *in.ActiveLeaseRef {
		t.Errorf("ActiveLeaseRef: in=%v out=%v", in.ActiveLeaseRef, out.ActiveLeaseRef)
	}
	if out.ActivationAttempt == nil || *out.ActivationAttempt != *in.ActivationAttempt {
		t.Errorf("ActivationAttempt: in=%v out=%v", in.ActivationAttempt, out.ActivationAttempt)
	}
	if out.ActivationJobName == nil || *out.ActivationJobName != *in.ActivationJobName {
		t.Errorf("ActivationJobName: in=%v out=%v", in.ActivationJobName, out.ActivationJobName)
	}
}

func TestSlotDocIDIsDeterministic(t *testing.T) {
	doc := newSlotDoc(server.Slot{Project: "p", SlotIndex: 5})
	if doc.ID != "p:5" {
		t.Fatalf("doc.ID=%q, want p:5", doc.ID)
	}
}

func TestSlotHistoryDocMarshalsCanonicalShape(t *testing.T) {
	now := time.Date(2026, 5, 17, 8, 0, 0, 0, time.UTC)
	idx := 5
	entry := server.SlotHistoryEntry{
		ID:        "deadbeef",
		Event:     "return",
		CreatedAt: now,
		Project:   "tank-operator",
		SlotIndex: &idx,
		LeaseRef:  "tank-operator/leases/83",
		Source:    "test_slot_return",
	}
	payload, err := json.Marshal(slotHistoryDoc{SlotHistoryEntry: entry})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(payload)
	for _, want := range []string{
		`"id":"deadbeef"`,
		`"event":"return"`,
		`"project":"tank-operator"`,
		`"slot_index":5`,
		`"lease_ref":"tank-operator/leases/83"`,
		`"source":"test_slot_return"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s — got %s", want, s)
		}
	}
}

func timePtr(t time.Time) *time.Time { return &t }
func intP(i int) *int                { return &i }

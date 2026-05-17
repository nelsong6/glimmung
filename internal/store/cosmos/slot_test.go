package cosmos

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/server"
)

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

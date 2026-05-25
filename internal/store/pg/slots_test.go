package pg

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizedSlotHistoryIDKeepsUUID(t *testing.T) {
	id := uuid.NewString()
	got := normalizedSlotHistoryID(SlotHistoryRow{ID: " " + id + " "})
	if got != id {
		t.Fatalf("normalized id=%q, want %q", got, id)
	}
}

func TestNormalizedSlotHistoryIDDeterministicForLegacyID(t *testing.T) {
	row := SlotHistoryRow{
		ID:        "admin-cleanup-tank-operator-slot-5-20260522T072033Z",
		Project:   "tank-operator",
		SlotIndex: 5,
		Payload:   []byte(`{"event":"return_requested"}`),
		CreatedAt: time.Date(2026, 5, 22, 7, 20, 33, 0, time.UTC),
	}
	got := normalizedSlotHistoryID(row)
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("normalized id is not a uuid: %q: %v", got, err)
	}
	if again := normalizedSlotHistoryID(row); again != got {
		t.Fatalf("normalized id not deterministic: %q then %q", got, again)
	}
}

package server

import (
	"context"
	"time"
)

// SlotStore is the storage interface for the `slots` table.
//
// Every mutation is a deliberate transition (see Slot.Mark* methods)
// expressed through UpdateIfMatch. There is no "set status" or
// "merge whole slot" primitive — Slot.Mark* + UpdateIfMatch is the only
// write path. Cross-slot writes don't contend because each slot is its own
// row.
type SlotStore interface {
	// CreateSlot writes a new slot doc in `unseeded` state for the given
	// project and slot index. Idempotent: if a doc already exists at
	// that (project, slot_index), CreateSlot returns the existing doc
	// without modifying it. Used by PATCH-count and the boot recovery
	// sweep to seed missing slots.
	CreateSlot(ctx context.Context, slot Slot) (Slot, error)

	// GetSlot returns the slot doc for (project, slot_index). Returns
	// ErrNotFound if no such doc exists. The returned slot carries its
	// current etag (Slot.ETag()) — use UpdateIfMatch with that etag
	// for CAS writes.
	GetSlot(ctx context.Context, project string, slotIndex int) (Slot, error)

	// ListSlotsByProject returns all slot docs for the given project, ordered by
	// slot_index ascending. No etag on returned slots; point-read GetSlot if you
	// need to CAS-write.
	ListSlotsByProject(ctx context.Context, project string) ([]Slot, error)

	// UpdateIfMatch performs a read-modify-write on the slot doc using
	// the etag carried on the slot read at the start of the operation.
	// The mutator is called with the freshly-read slot; whatever it
	// returns is written with `IfMatch: <fresh etag>`.
	//
	// On 412 Precondition Failed (another writer raced us), returns
	// server.ErrPreconditionFailed; the caller can re-read and retry
	// the mutator against the fresh state.
	//
	// On Slot.Mark* errors (invalid state transition), the mutator's
	// error propagates and no write is attempted.
	//
	// Returns ErrNotFound if the slot doc doesn't exist.
	UpdateIfMatch(ctx context.Context, project string, slotIndex int, mutate func(Slot) (Slot, error)) (Slot, error)

	// DeleteSlot removes a slot doc. Used only by the count-decrease path
	// after the caller has verified no active lease references the slot.
	// Returns ErrNotFound if the doc doesn't exist (idempotent for the
	// "already deleted" case).
	DeleteSlot(ctx context.Context, project string, slotIndex int) error
}

// SlotHistoryEntry is one append-only audit record describing something
// that happened to a slot — a return, an error, an admin cancel, etc.
// Replaces the in-doc `test_slot_return_history` array per
// docs/test-slot-storage-rework.md.
//
// One Postgres row per entry. The ID is a uuid assigned at write time.
type SlotHistoryEntry struct {
	ID              string    `json:"id"`
	Event           string    `json:"event"`
	CreatedAt       time.Time `json:"created_at"`
	Project         string    `json:"project"`
	SlotIndex       *int      `json:"slot_index,omitempty"`
	SlotName        *string   `json:"slot_name,omitempty"`
	LeaseRef        string    `json:"lease_ref"`
	LeaseNumber     *int      `json:"lease_number,omitempty"`
	LeaseRequester  *string   `json:"lease_requester,omitempty"`
	CallerPodIP     *string   `json:"caller_pod_ip,omitempty"`
	CallerSessionID *string   `json:"caller_session_id,omitempty"`
	Source          string    `json:"source"`
	Reason          *string   `json:"reason,omitempty"`
	CleanupStarted  bool      `json:"cleanup_started"`
}

// SlotHistoryStore is the storage interface for the `slot_history`
// collection. Append-only; no updates, no deletes. The audit trail is
// the canonical "what has happened to this slot" record — slot doc
// fields describe the slot's *current* state only (see the "Slot Status
// Field Contract" section of docs/test-slot-lifecycle.md).
type SlotHistoryStore interface {
	// AppendSlotHistory writes a new history entry. The entry's ID may
	// be empty on input; the store assigns a uuid when so. Returns the
	// stored entry with the assigned ID.
	AppendSlotHistory(ctx context.Context, entry SlotHistoryEntry) (SlotHistoryEntry, error)

	// ListSlotHistory returns history entries for the given project,
	// ordered by created_at ascending. If slotIndex is non-nil, returns
	// only entries for that slot; otherwise returns all entries for the
	// project. Single-partition query.
	ListSlotHistory(ctx context.Context, project string, slotIndex *int) ([]SlotHistoryEntry, error)
}

package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	pgstore "github.com/nelsong6/glimmung/internal/store/pg"

	"github.com/nelsong6/glimmung/internal/server"
)

// slotDoc is the on-the-wire shape historically stored in the cosmos
// `slots` container. Retained as the unmarshal target so the prior
// payload shape (server.Slot wrapped with a cosmos document id)
// continues to round-trip through pg's jsonb column unchanged.
type slotDoc struct {
	ID string `json:"id"`
	server.Slot
}

func newSlotDoc(s server.Slot) slotDoc {
	return slotDoc{ID: server.SlotDocID(s.Project, s.SlotIndex), Slot: s}
}

// slotETagFromUpdatedAt formats the pg row's updated_at timestamp as
// the slot's ETag string. Callers use Slot.ETag() and pass it to
// UpdateIfMatch unchanged; the pg layer parses it back to a time.Time
// for the CAS predicate.
func slotETagFromUpdatedAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseSlotETag(etag string) (time.Time, error) {
	if etag == "" {
		return time.Time{}, fmt.Errorf("slot etag missing")
	}
	t, err := time.Parse(time.RFC3339Nano, etag)
	if err != nil {
		return time.Time{}, fmt.Errorf("slot etag parse: %w", err)
	}
	return t, nil
}

// slotFromPGRow rebuilds a server.Slot from a pg row, attaching the
// row's updated_at timestamp as the CAS etag.
func slotFromPGRow(row pgstore.SlotRow) (server.Slot, error) {
	var doc slotDoc
	if err := json.Unmarshal(row.Payload, &doc); err != nil {
		return server.Slot{}, fmt.Errorf("slot unmarshal: %w", err)
	}
	return doc.Slot.WithETag(slotETagFromUpdatedAt(row.UpdatedAt)), nil
}

// CreateSlot writes a new slot doc. If a doc already exists at
// (project, slot_index), returns the existing doc rather than an
// error (idempotent — the migration and the PATCH-count handler both
// rely on this).
func (s *Store) CreateSlot(ctx context.Context, slot server.Slot) (server.Slot, error) {
	doc := newSlotDoc(slot)
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Slot{}, err
	}
	row, err := s.pgSlots.Create(ctx, pgstore.SlotRow{
		Project:   slot.Project,
		SlotIndex: slot.SlotIndex,
		Payload:   payload,
	})
	if errors.Is(err, pgstore.ErrSlotAlreadyExists) {
		return s.GetSlot(ctx, slot.Project, slot.SlotIndex)
	}
	if err != nil {
		return server.Slot{}, err
	}
	return slotFromPGRow(row)
}

// GetSlot returns the slot doc for (project, slot_index). The
// returned Slot carries an ETag string formatted from the pg row's
// updated_at timestamp; pass that to UpdateIfMatch for CAS writes.
func (s *Store) GetSlot(ctx context.Context, project string, slotIndex int) (server.Slot, error) {
	row, err := s.pgSlots.Get(ctx, project, slotIndex)
	if errors.Is(err, pgstore.ErrSlotNotFound) {
		return server.Slot{}, server.ErrNotFound
	}
	if err != nil {
		return server.Slot{}, err
	}
	return slotFromPGRow(row)
}

// ListSlotsByProject returns every slot doc for project, ordered by
// slot_index ascending. Returned slots do not carry etags; callers
// that need to CAS-write must follow up with GetSlot.
func (s *Store) ListSlotsByProject(ctx context.Context, project string) ([]server.Slot, error) {
	rows, err := s.pgSlots.ListByProject(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]server.Slot, 0, len(rows))
	for _, row := range rows {
		slot, err := slotFromPGRow(row)
		if err != nil {
			return nil, err
		}
		// List path intentionally returns slots without an etag —
		// matches the prior cosmos behavior + the SlotStore contract.
		out = append(out, slot.WithETag(""))
	}
	return out, nil
}

// UpdateIfMatch performs a CAS update: read the current slot doc,
// capture its etag (= updated_at), call mutate() with the freshly-
// read slot, and write the result conditional on updated_at being
// unchanged.
//
// Returns server.ErrPreconditionFailed if another writer raced us.
// Returns server.ErrNotFound if the slot doc doesn't exist.
// If mutate returns an error, no write is attempted.
func (s *Store) UpdateIfMatch(ctx context.Context, project string, slotIndex int, mutate func(server.Slot) (server.Slot, error)) (server.Slot, error) {
	current, err := s.GetSlot(ctx, project, slotIndex)
	if err != nil {
		return server.Slot{}, err
	}
	next, err := mutate(current)
	if err != nil {
		return server.Slot{}, err
	}
	if next.Project != current.Project || next.SlotIndex != current.SlotIndex {
		return server.Slot{}, fmt.Errorf("slot mutate must not change identity: (%s:%d) -> (%s:%d)",
			current.Project, current.SlotIndex, next.Project, next.SlotIndex)
	}
	expected, err := parseSlotETag(current.ETag())
	if err != nil {
		return server.Slot{}, err
	}
	doc := newSlotDoc(next)
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Slot{}, err
	}
	row, err := s.pgSlots.UpdateWithCAS(ctx, project, slotIndex, payload, expected)
	if errors.Is(err, pgstore.ErrSlotPreconditionFailed) {
		return server.Slot{}, server.ErrPreconditionFailed
	}
	if errors.Is(err, pgstore.ErrSlotNotFound) {
		return server.Slot{}, server.ErrNotFound
	}
	if err != nil {
		return server.Slot{}, err
	}
	return slotFromPGRow(row)
}

// DeleteSlot removes a slot doc. Idempotent: returns nil if the doc
// is already absent.
func (s *Store) DeleteSlot(ctx context.Context, project string, slotIndex int) error {
	return s.pgSlots.Delete(ctx, project, slotIndex)
}

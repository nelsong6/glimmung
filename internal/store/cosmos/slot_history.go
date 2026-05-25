package cosmos

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	pgstore "github.com/nelsong6/glimmung/internal/store/pg"

	"github.com/nelsong6/glimmung/internal/server"
)

// slotHistoryDoc is the on-the-wire shape for one slot_history entry.
// Retained as the unmarshal target so the prior payload shape (the
// server.SlotHistoryEntry) round-trips through pg's jsonb column.
type slotHistoryDoc struct {
	server.SlotHistoryEntry
}

// AppendSlotHistory writes a new history entry. Assigns a uuid if the
// caller did not provide an ID. Returns the stored entry with its
// assigned ID populated.
func (s *Store) AppendSlotHistory(ctx context.Context, entry server.SlotHistoryEntry) (server.SlotHistoryEntry, error) {
	if strings.TrimSpace(entry.ID) == "" {
		entry.ID = uuid.NewString()
	}
	doc := slotHistoryDoc{SlotHistoryEntry: entry}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.SlotHistoryEntry{}, err
	}
	slotIdx := 0
	if entry.SlotIndex != nil {
		slotIdx = *entry.SlotIndex
	}
	if _, err := s.pgSlots.AppendHistory(ctx, pgstore.SlotHistoryRow{
		ID:        entry.ID,
		Project:   entry.Project,
		SlotIndex: slotIdx,
		Payload:   payload,
	}); err != nil {
		return server.SlotHistoryEntry{}, err
	}
	return entry, nil
}

// ListSlotHistory returns entries for the given project ordered by
// created_at ascending. If slotIndex is non-nil, filters to entries
// for that slot index.
func (s *Store) ListSlotHistory(ctx context.Context, project string, slotIndex *int) ([]server.SlotHistoryEntry, error) {
	rows, err := s.pgSlots.ListHistory(ctx, project, slotIndex)
	if err != nil {
		return nil, err
	}
	out := make([]server.SlotHistoryEntry, 0, len(rows))
	for _, row := range rows {
		var doc slotHistoryDoc
		if err := json.Unmarshal(row.Payload, &doc); err != nil {
			return nil, err
		}
		out = append(out, doc.SlotHistoryEntry)
	}
	return out, nil
}

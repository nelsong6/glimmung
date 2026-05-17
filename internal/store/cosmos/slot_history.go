package cosmos

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
	"github.com/google/uuid"

	"github.com/nelsong6/glimmung/internal/server"
)

// slotHistoryDoc is the on-the-wire shape for one entry in the
// `slot_history` collection. The entry's own ID is the Cosmos document id.
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
	pk := azcosmos.NewPartitionKeyString(entry.Project)
	if _, err := s.slotHistory.CreateItem(ctx, pk, payload, nil); err != nil {
		return server.SlotHistoryEntry{}, err
	}
	return entry, nil
}

// ListSlotHistory returns entries for the given project ordered by
// created_at ascending. If slotIndex is non-nil, filters to entries for
// that slot index. Single-partition query.
func (s *Store) ListSlotHistory(ctx context.Context, project string, slotIndex *int) ([]server.SlotHistoryEntry, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	query := "SELECT * FROM c WHERE c.project = @project"
	params := []azcosmos.QueryParameter{{Name: "@project", Value: project}}
	if slotIndex != nil {
		query += " AND c.slot_index = @slotIndex"
		params = append(params, azcosmos.QueryParameter{Name: "@slotIndex", Value: *slotIndex})
	}
	pager := s.slotHistory.NewQueryItemsPager(query, pk, &azcosmos.QueryOptions{QueryParameters: params})
	var docs []slotHistoryDoc
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			var doc slotHistoryDoc
			if err := json.Unmarshal(item, &doc); err != nil {
				return nil, err
			}
			docs = append(docs, doc)
		}
	}
	sort.SliceStable(docs, func(i, j int) bool {
		return docs[i].SlotHistoryEntry.CreatedAt.Before(docs[j].SlotHistoryEntry.CreatedAt)
	})
	out := make([]server.SlotHistoryEntry, 0, len(docs))
	for _, doc := range docs {
		out = append(out, doc.SlotHistoryEntry)
	}
	return out, nil
}

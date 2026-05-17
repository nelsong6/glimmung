package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/nelsong6/glimmung/internal/server"
)

// slotDoc is the on-the-wire shape for the `slots` Cosmos container. The
// only added field on top of server.Slot is the Cosmos document id, which
// is derived deterministically from project + slot_index so callers can
// point-read by (project, slot_index) without a separate id lookup.
type slotDoc struct {
	ID string `json:"id"`
	server.Slot
}

func newSlotDoc(s server.Slot) slotDoc {
	return slotDoc{ID: server.SlotDocID(s.Project, s.SlotIndex), Slot: s}
}

// CreateSlot writes a new slot doc. If a doc already exists at
// (project, slot_index), returns the existing doc rather than an error
// (idempotent — the migration and the PATCH-count handler both rely on
// this).
func (s *Store) CreateSlot(ctx context.Context, slot server.Slot) (server.Slot, error) {
	doc := newSlotDoc(slot)
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Slot{}, err
	}
	pk := azcosmos.NewPartitionKeyString(slot.Project)
	resp, err := s.slots.CreateItem(ctx, pk, payload, nil)
	if err == nil {
		return slot.WithETag(string(resp.ETag)), nil
	}
	if isCosmosStatus(err, http.StatusConflict) {
		return s.GetSlot(ctx, slot.Project, slot.SlotIndex)
	}
	return server.Slot{}, err
}

// GetSlot returns the slot doc for (project, slot_index). The returned
// Slot carries the Cosmos resource etag; use that with UpdateIfMatch for
// CAS writes.
func (s *Store) GetSlot(ctx context.Context, project string, slotIndex int) (server.Slot, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	id := server.SlotDocID(project, slotIndex)
	read, err := s.slots.ReadItem(ctx, pk, id, nil)
	if err != nil {
		if isCosmosStatus(err, http.StatusNotFound) {
			return server.Slot{}, server.ErrNotFound
		}
		return server.Slot{}, err
	}
	var doc slotDoc
	if err := json.Unmarshal(read.Value, &doc); err != nil {
		return server.Slot{}, err
	}
	return doc.Slot.WithETag(string(read.ETag)), nil
}

// ListSlotsByProject returns every slot doc for the given project, ordered
// by slot_index ascending. Single-partition query — RU cost scales with
// slot count, not project count. The returned slots do not carry etags;
// callers that need to CAS-write must follow up with GetSlot.
func (s *Store) ListSlotsByProject(ctx context.Context, project string) ([]server.Slot, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	pager := s.slots.NewQueryItemsPager(
		"SELECT * FROM c WHERE c.project = @project",
		pk,
		&azcosmos.QueryOptions{
			QueryParameters: []azcosmos.QueryParameter{{Name: "@project", Value: project}},
		},
	)
	var docs []slotDoc
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			var doc slotDoc
			if err := json.Unmarshal(item, &doc); err != nil {
				return nil, err
			}
			docs = append(docs, doc)
		}
	}
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].Slot.SlotIndex < docs[j].Slot.SlotIndex })
	out := make([]server.Slot, 0, len(docs))
	for _, doc := range docs {
		out = append(out, doc.Slot)
	}
	return out, nil
}

// UpdateIfMatch performs a CAS update: read the current slot doc, capture
// its etag, call mutate() with the freshly-read slot, and write the result
// with `IfMatch: <fresh etag>`.
//
// Returns server.ErrPreconditionFailed if another writer raced us between
// our read and our write. The caller can re-call UpdateIfMatch to retry
// against the new state.
//
// Returns server.ErrNotFound if the slot doc doesn't exist.
//
// If mutate returns an error, no write is attempted and the error
// propagates to the caller. Use this for invalid state transitions
// (server.ErrInvalidSlotTransition) and other application-level rejection.
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
	doc := newSlotDoc(next)
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Slot{}, err
	}
	pk := azcosmos.NewPartitionKeyString(project)
	etag := azcore.ETag(current.ETag())
	opts := &azcosmos.ItemOptions{IfMatchEtag: &etag}
	resp, err := s.slots.ReplaceItem(ctx, pk, doc.ID, payload, opts)
	if err != nil {
		if isCosmosStatus(err, http.StatusPreconditionFailed) {
			return server.Slot{}, server.ErrPreconditionFailed
		}
		if isCosmosStatus(err, http.StatusNotFound) {
			return server.Slot{}, server.ErrNotFound
		}
		return server.Slot{}, err
	}
	return next.WithETag(string(resp.ETag)), nil
}

// DeleteSlot removes a slot doc. Idempotent: returns nil if the doc is
// already absent. Used by the count-decrease path after the caller has
// verified no active lease references the slot.
func (s *Store) DeleteSlot(ctx context.Context, project string, slotIndex int) error {
	pk := azcosmos.NewPartitionKeyString(project)
	id := server.SlotDocID(project, slotIndex)
	if _, err := s.slots.DeleteItem(ctx, pk, id, nil); err != nil {
		if isCosmosStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// errSlotDocAlreadyExists is not exported — CreateSlot translates Cosmos
// 409 Conflict into a GetSlot fetch + return (the idempotent path). This
// constant is here so the intent is documented at the implementation
// site and so future refactors (e.g., wanting a distinct error for the
// "already exists" case) have a clear name to start from.
var errSlotDocAlreadyExists = errors.New("slot doc already exists")

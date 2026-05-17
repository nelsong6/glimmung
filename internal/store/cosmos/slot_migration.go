package cosmos

// Migration: strip the retired `project.metadata.native_standby_dns.slots[]`
// embedded array after the boot migrator has copied each entry into the
// dedicated `slots` Cosmos container.
//
// This file is the *single* production code path that is allowed to
// reference the legacy array shape, per
// scripts/check-slot-storage-migration.mjs. It exists to *delete* the
// legacy data, not to read it; the presence check before delete is
// idempotency, not a runtime fallback.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/nelsong6/glimmung/internal/server"
)

// StripProjectTestEnvironmentSlotsArray removes the legacy
// `metadata.native_standby_dns.slots[]` array from a project doc.
// Called by the one-shot slot-storage-rework migration after slot data
// has been copied to the new `slots` collection.
//
// Idempotent: if the array is already absent, the call is a no-op.
// Returns server.ErrNotFound if the project doc itself is missing.
func (s *Store) StripProjectTestEnvironmentSlotsArray(ctx context.Context, project string) error {
	partitionKey := azcosmos.NewPartitionKeyString(project)
	read, err := s.projects.ReadItem(ctx, partitionKey, project, nil)
	if err != nil {
		if isCosmosStatus(err, http.StatusNotFound) {
			return server.ErrNotFound
		}
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(read.Value, &doc); err != nil {
		return err
	}
	metadata, _ := doc["metadata"].(map[string]any)
	if metadata == nil {
		return nil
	}
	standbyDNS, _ := metadata["native_standby_dns"].(map[string]any)
	if standbyDNS == nil {
		return nil
	}
	if _, present := standbyDNS["slots"]; !present {
		return nil
	}
	delete(standbyDNS, "slots")
	metadata["native_standby_dns"] = standbyDNS
	doc["metadata"] = metadata
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if _, err := s.projects.ReplaceItem(ctx, partitionKey, project, payload, nil); err != nil {
		return err
	}
	return nil
}

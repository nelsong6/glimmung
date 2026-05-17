package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MigrateProjectSlotsIntoCollection is the one-shot startup migration that
// copies every project's embedded `metadata.native_standby_dns.slots[]`
// array into the `slots` collection as one document per slot, then
// strips the array from the project doc.
//
// Per `docs/test-slot-storage-rework.md` this runs synchronously at pod
// boot before the readiness probe goes live. It is idempotent:
//
//   - CreateSlot uses `If-None-Match: *` semantics; a doc that already
//     exists is left alone.
//   - Stripping the slots[] array is a no-op when the array is already
//     gone.
//   - The migration can be re-run any number of times without producing
//     a different result.
//
// Returns a summary suitable for logging: number of projects scanned,
// slot docs created vs already-existed, project docs stripped of legacy
// arrays. The first non-nil error short-circuits — subsequent pod starts
// will re-run and pick up where this left off.
func MigrateProjectSlotsIntoCollection(ctx context.Context, store ReadStore) (SlotMigrationSummary, error) {
	slotStore, ok := store.(SlotStore)
	if !ok || slotStore == nil {
		return SlotMigrationSummary{}, errors.New("slot store not configured")
	}
	stripper, _ := store.(legacyProjectSlotsArrayStripper)
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return SlotMigrationSummary{}, fmt.Errorf("list projects: %w", err)
	}
	summary := SlotMigrationSummary{}
	now := time.Now().UTC()
	for _, project := range projects {
		projectKey := firstNonEmpty(project.Name, project.ID)
		if projectKey == "" {
			continue
		}
		summary.ProjectsScanned++
		legacy := readLegacyProjectSlots(project)
		if len(legacy) > 0 {
			summary.ProjectsWithLegacySlots++
		}
		for _, entry := range legacy {
			slot := slotFromLegacyEntry(projectKey, entry, now)
			if _, err := slotStore.GetSlot(ctx, projectKey, slot.SlotIndex); err == nil {
				summary.SlotsAlreadyMigrated++
				continue
			} else if !errors.Is(err, ErrNotFound) {
				return summary, fmt.Errorf("probe slot project=%s slot_index=%d: %w", projectKey, slot.SlotIndex, err)
			}
			if _, err := slotStore.CreateSlot(ctx, slot); err != nil {
				return summary, fmt.Errorf("migrate slot project=%s slot_index=%d: %w", projectKey, slot.SlotIndex, err)
			}
			summary.SlotsCreated++
		}
		if stripper != nil && len(legacy) > 0 {
			if err := stripper.StripProjectTestEnvironmentSlotsArray(ctx, projectKey); err != nil {
				return summary, fmt.Errorf("strip legacy slots array project=%s: %w", projectKey, err)
			}
			summary.ProjectsStripped++
		}
	}
	return summary, nil
}

// SlotMigrationSummary is what the migration logs at boot.
type SlotMigrationSummary struct {
	ProjectsScanned         int
	ProjectsWithLegacySlots int
	ProjectsStripped        int
	SlotsCreated            int
	SlotsAlreadyMigrated    int
}

func (s SlotMigrationSummary) String() string {
	return fmt.Sprintf(
		"projects_scanned=%d projects_with_legacy_slots=%d projects_stripped=%d slots_created=%d slots_already_migrated=%d",
		s.ProjectsScanned, s.ProjectsWithLegacySlots, s.ProjectsStripped, s.SlotsCreated, s.SlotsAlreadyMigrated,
	)
}

// legacyProjectSlotsArrayStripper is implemented by stores that can strip
// the legacy `metadata.native_standby_dns.slots[]` array from a project
// doc after migration. The cosmos store implements this; fakes used in
// unit tests may omit it (the migration tolerates a no-op).
type legacyProjectSlotsArrayStripper interface {
	StripProjectTestEnvironmentSlotsArray(ctx context.Context, project string) error
}

// readLegacyProjectSlots returns the slots[] array entries from a
// project's embedded native_standby_dns metadata, ordered by slot_index
// ascending. Returns an empty slice if the array is absent or malformed.
func readLegacyProjectSlots(project Project) []legacySlotEntry {
	standbyDNS, ok := mapFromMap(project.Metadata, "native_standby_dns")
	if !ok {
		return nil
	}
	rawSlots, ok := standbyDNS["slots"].([]any)
	if !ok {
		return nil
	}
	out := make([]legacySlotEntry, 0, len(rawSlots))
	for _, raw := range rawSlots {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		entry := legacySlotEntry{raw: m}
		if v, ok := positiveIntFromMap(m, "slot_index"); ok {
			entry.slotIndex = v
		} else if v, ok := positiveIntFromMap(m, "slotIndex"); ok {
			entry.slotIndex = v
		} else {
			continue
		}
		if v, ok := stringFromMap(m, "slot_name"); ok && strings.TrimSpace(v) != "" {
			entry.slotName = strings.TrimSpace(v)
		} else if v, ok := stringFromMap(m, "slotName"); ok && strings.TrimSpace(v) != "" {
			entry.slotName = strings.TrimSpace(v)
		}
		if v, ok := stringFromMap(m, "state"); ok {
			entry.state = strings.TrimSpace(v)
		}
		if v, ok := stringFromMap(m, "updated_at"); ok {
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				entry.updatedAt = t
			}
		}
		if v, ok := stringFromMap(m, "ready_at"); ok {
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				entry.readyAt = t
			}
		}
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].slotIndex < out[j].slotIndex })
	return out
}

type legacySlotEntry struct {
	slotIndex int
	slotName  string
	state     string
	updatedAt time.Time
	readyAt   time.Time
	raw       map[string]any
}

// slotFromLegacyEntry maps the legacy embedded shape into a new Slot doc.
// The state field is translated using the documented mapping:
//   - `warming` -> `provisioning`
//   - `ready` (also empty)  -> `provisioned`
//   - `active` -> `running`
//   - `activating`, `cleaning`, `error` unchanged
//
// Any unmapped state defaults to `unseeded` so the migration produces a
// known-good state; the recovery sweep will re-seed if needed.
func slotFromLegacyEntry(project string, entry legacySlotEntry, now time.Time) Slot {
	slotName := entry.slotName
	if slotName == "" {
		slotName = fmt.Sprintf("%s-slot-%d", project, entry.slotIndex)
	}
	state := translateLegacyState(entry.state)
	updatedAt := entry.updatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	slot := Slot{
		Project:   project,
		SlotIndex: entry.slotIndex,
		SlotName:  slotName,
		State:     state,
		UpdatedAt: updatedAt.UTC(),
	}
	if !entry.readyAt.IsZero() {
		t := entry.readyAt.UTC()
		slot.ProvisionedAt = &t
	}
	// Preserve the operator-visible `detail` field across migration —
	// it's the human-readable explanation that pairs with non-terminal
	// states and errors. Dropping it on migration loses diagnostic
	// context that the dashboard surfaces.
	if v, ok := stringFromMap(entry.raw, "detail"); ok && v != "" {
		detail := v
		slot.Detail = &detail
	}
	return slot
}

func translateLegacyState(state string) string {
	switch strings.TrimSpace(state) {
	case "warming":
		return SlotStateProvisioning
	case "ready", "":
		return SlotStateProvisioned
	case "active":
		return SlotStateRunning
	case SlotStateActivating, SlotStateCleaning, SlotStateError, SlotStateProvisioning, SlotStateProvisioned, SlotStateRunning, SlotStateUnseeded:
		return state
	default:
		return SlotStateUnseeded
	}
}

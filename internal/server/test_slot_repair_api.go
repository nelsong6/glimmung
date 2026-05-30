package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type TestEnvironmentRepairResponse struct {
	Project   string  `json:"project"`
	SlotIndex int     `json:"slot_index"`
	SlotName  string  `json:"slot_name"`
	State     string  `json:"state"`
	StatusURL string  `json:"status_url"`
	URL       *string `json:"url,omitempty"`
}

func repairProjectTestEnvironment(store ReadStore, preparer TestSlotPreparer, minter NativeGitHubTokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if preparer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "test-slot preparer not configured")
			return
		}
		slotStore := slotStoreFromReadStore(store)
		if slotStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "slot store not configured")
			return
		}
		stateStore, ok := store.(StateStore)
		if !ok || stateStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "test-slot lease state store not configured")
			return
		}

		projectKey := strings.TrimSpace(r.PathValue("project"))
		if projectKey == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		slotName := strings.TrimSpace(r.PathValue("slot_name"))
		if slotName == "" {
			writeProblem(w, http.StatusBadRequest, "slot_name required")
			return
		}

		project, found, err := findProjectByKey(r.Context(), store, projectKey)
		if err != nil {
			writeInternalError(w, r, err, "list projects failed")
			return
		}
		if !found {
			writeProblem(w, http.StatusNotFound, "project not found")
			return
		}
		projectName := firstNonEmpty(project.Name, project.ID, projectKey)
		slotIndex, err := configuredSlotIndexForName(projectName, project, slotName)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "test environment not found")
				return
			}
			writeInternalError(w, r, err, "resolve test environment failed")
			return
		}

		if lease, ok, err := activeTestSlotLeaseForSlot(r.Context(), stateStore, project, projectName, slotIndex, slotName); err != nil {
			writeInternalError(w, r, err, "list test-slot leases failed")
			return
		} else if ok {
			ref := LeasePublicRefFromLease(lease)
			if ref == "" {
				ref = slotName
			}
			writeProblem(w, http.StatusConflict, fmt.Sprintf("cannot repair active leased slot %s", ref))
			return
		}

		key := projectName + ":warm:" + slotName
		if _, loaded := testSlotWarmups.LoadOrStore(key, struct{}{}); loaded {
			writeProblem(w, http.StatusConflict, "test environment repair already in progress")
			return
		}
		defer testSlotWarmups.Delete(key)

		lease := testEnvironmentWarmupLease(project, slotIndex, slotName)
		now := time.Now().UTC()
		if _, err := claimSlotPreliminaryRepair(r.Context(), store, projectName, slotIndex, slotName, now); err != nil {
			switch {
			case errors.Is(err, ErrConflict):
				writeProblem(w, http.StatusConflict, err.Error())
			case errors.Is(err, ErrNotFound):
				writeProblem(w, http.StatusNotFound, "test environment not found")
			default:
				writeInternalError(w, r, err, "claim test environment repair failed")
			}
			return
		}

		if err := preparer.RepairTestSlotPreliminaries(r.Context(), lease, project, minter); err != nil {
			if _, writeErr := markSlotError(r.Context(), store, projectName, slotIndex, time.Now().UTC(), err); writeErr != nil {
				writeInternalError(w, r, writeErr, "record test environment repair failure failed")
				return
			}
			writeProblem(w, http.StatusBadGateway, "test environment repair failed")
			return
		}
		cleanupTestSlotInstaller(r.Context(), preparer, lease, project, nil)
		repaired, err := markSlotProvisioned(r.Context(), store, projectName, slotIndex, time.Now().UTC())
		if err != nil {
			writeInternalError(w, r, err, "mark test environment repaired failed")
			return
		}
		wakeRunQueue(projectName)

		writeJSON(w, http.StatusOK, TestEnvironmentRepairResponse{
			Project:   projectName,
			SlotIndex: slotIndex,
			SlotName:  slotName,
			State:     repaired.State,
			StatusURL: "/v1/projects/" + projectName + "/test-environments/" + slotName,
			URL:       testSlotURL(project, &slotName),
		})
	}
}

func configuredSlotIndexForName(projectName string, project Project, slotName string) (int, error) {
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return 0, ErrNotFound
	}
	count := projectTestSlotCount(Settings{}, project)
	for slotIndex := 1; slotIndex <= count; slotIndex++ {
		if testEnvironmentName(projectName, slotIndex, project, Lease{}) == slotName {
			return slotIndex, nil
		}
	}
	return 0, ErrNotFound
}

// activeTestSlotLeaseForSlot reports whether a live lease is currently
// holding the slot, so repair can refuse to revalidate capacity that is in
// use. "Live" is any non-terminal lease (claimed or active) referencing the
// slot — both test-slot checkout leases and native run leases (env-prep and
// later phases reserve a slot with a non-checkout native lease). A terminal
// (released/expired) lease never matches: its reservation is an orphan that
// repair is allowed to clear, not a live hold it must protect.
func activeTestSlotLeaseForSlot(ctx context.Context, store StateStore, project Project, projectName string, slotIndex int, slotName string) (Lease, bool, error) {
	leases, err := store.ListLeases(ctx)
	if err != nil {
		return Lease{}, false, err
	}
	projectNames := map[string]bool{}
	for _, name := range []string{projectName, project.Name, project.ID} {
		if strings.TrimSpace(name) != "" {
			projectNames[strings.TrimSpace(name)] = true
		}
	}
	for _, lease := range leases {
		if lease.State != "claimed" && lease.State != "active" {
			continue
		}
		if !projectNames[lease.Project] {
			continue
		}
		matchesIndex := false
		if index := nativeSlotIndexFromMetadata(lease.Metadata); index != nil && *index == slotIndex {
			matchesIndex = true
		}
		if matchesIndex || nativeSlotNameMatches(lease.Metadata, slotName) {
			return lease, true, nil
		}
	}
	return Lease{}, false, nil
}

func claimSlotPreliminaryRepair(ctx context.Context, store ReadStore, project string, slotIndex int, slotName string, now time.Time) (Slot, error) {
	if _, err := ensureSlotExists(ctx, store, project, slotIndex, slotName, now); err != nil {
		return Slot{}, err
	}
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return Slot{}, errSlotStoreNotConfigured
	}
	return slotStore.UpdateIfMatch(ctx, project, slotIndex, func(slot Slot) (Slot, error) {
		switch slot.State {
		case SlotStateUnseeded, SlotStateProvisioning, SlotStateProvisioned:
			return repairProvisioning(slot, now)
		case SlotStateError:
			if slot.CleanupError != nil && strings.TrimSpace(*slot.CleanupError) != "" {
				return slot, fmt.Errorf("%w: slot %s has a cleanup error; retry return or cleanup first", ErrConflict, slotName)
			}
			return repairProvisioning(slot, now)
		case SlotStateActivating, SlotStateRunning, SlotStateCleaning:
			return slot, fmt.Errorf("%w: slot %s is %s", ErrConflict, slotName, slot.State)
		default:
			return slot, fmt.Errorf("%w: slot %s is in unknown state %q", ErrConflict, slotName, slot.State)
		}
	})
}

// repairProvisioning walks a repairable slot back to provisioning and clears
// any active_lease_ref it still carries. The repair handler has already
// verified via activeTestSlotLeaseForSlot that no live lease holds this slot,
// so a non-empty ref here is an orphaned reservation — the canonical case
// being the stale-lease startup sweep terminalizing a lease without releasing
// its slot. The subsequent provisioned transition (MarkProvisioned) also
// clears the ref; doing it here keeps the intermediate provisioning state
// honest and makes the orphan-recovery intent explicit.
func repairProvisioning(slot Slot, now time.Time) (Slot, error) {
	next, err := slot.MarkProvisioning(now)
	if err != nil {
		return slot, err
	}
	next.ActiveLeaseRef = nil
	return next, nil
}

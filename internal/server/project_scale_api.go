package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ProjectTestEnvironmentScaler interface {
	SetProjectTestEnvironmentCount(ctx context.Context, project string, count int) (Project, error)
}

type ProjectTestEnvironmentSlotStatusWriter interface {
	SetProjectTestEnvironmentSlotStatus(ctx context.Context, project string, status TestEnvironmentSlotStatus) (Project, error)
}

type TestEnvironmentScaleRequest struct {
	Count *int `json:"count"`
}

type TestEnvironmentSlotStatus struct {
	SlotIndex             int                          `json:"slot_index"`
	SlotName              string                       `json:"slot_name"`
	State                 string                       `json:"state"`
	UpdatedAt             time.Time                    `json:"updated_at"`
	Detail                *string                      `json:"detail,omitempty"`
	ReadyAt               *time.Time                   `json:"ready_at,omitempty"`
	ActivationAttempt     *int                         `json:"activation_attempt,omitempty"`
	ActivationState       *string                      `json:"activation_state,omitempty"`
	ActivationStartedAt   *time.Time                   `json:"activation_started_at,omitempty"`
	ActivationCompletedAt *time.Time                   `json:"activation_completed_at,omitempty"`
	ActivationJobName     *string                      `json:"activation_job_name,omitempty"`
	ActivationError       *string                      `json:"activation_error,omitempty"`
	CleanupState          *string                      `json:"cleanup_state,omitempty"`
	CleanupStartedAt      *time.Time                   `json:"cleanup_started_at,omitempty"`
	CleanupCompletedAt    *time.Time                   `json:"cleanup_completed_at,omitempty"`
	CleanupError          *string                      `json:"cleanup_error,omitempty"`
	ReturnHistory         []TestSlotReturnHistoryEntry `json:"test_slot_return_history,omitempty"`
}

type TestSlotReturnHistoryEntry struct {
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

func scaleProjectTestEnvironments(store ReadStore, workloadIdentities NativeWorkloadIdentityReconciler, managedOrigins ManagedOriginReconciler, preparer TestSlotPreparer, _ NativeGitHubTokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scaler, ok := store.(ProjectTestEnvironmentScaler)
		if !ok || scaler == nil {
			writeProblem(w, http.StatusServiceUnavailable, "project scaler not configured")
			return
		}
		project := r.PathValue("project")
		if project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}

		var req TestEnvironmentScaleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.Count == nil || *req.Count < 0 || *req.Count > 50 {
			writeProblem(w, http.StatusUnprocessableEntity, "count must be between 0 and 50")
			return
		}

		before, hasBefore, err := findProjectByKey(r.Context(), store, project)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "list projects failed")
			return
		}
		var removedSlots []TestEnvironmentSlotStatus
		if hasBefore {
			removedSlots = testEnvironmentSlotsAboveCount(before, *req.Count)
		}
		if hasBefore && len(removedSlots) > 0 {
			activeRemoved, err := activeTestSlotLeasesAboveCount(r.Context(), store, before, project, *req.Count)
			if err != nil {
				if errors.Is(err, ErrUnsupported) {
					writeProblem(w, http.StatusServiceUnavailable, "test-slot lease state store not configured")
					return
				}
				writeProblem(w, http.StatusInternalServerError, "list test-slot leases failed")
				return
			}
			if len(activeRemoved) > 0 {
				lease := activeRemoved[0]
				slotName := nativeSlotNameFromMetadata(lease.Metadata)
				name := LeasePublicRefFromLease(lease)
				if slotName != nil && strings.TrimSpace(*slotName) != "" {
					name = strings.TrimSpace(*slotName)
				}
				writeProblem(w, http.StatusConflict, fmt.Sprintf("cannot scale test environments below active leased slot %s", name))
				return
			}
		}

		updated, err := scaler.SetProjectTestEnvironmentCount(r.Context(), project, *req.Count)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "project not found")
				return
			}
			writeProblem(w, http.StatusInternalServerError, "scale project test environments failed")
			return
		}
		if workloadIdentities != nil {
			status, err := workloadIdentities.ReconcileNativeWorkloadIdentities(r.Context(), updated)
			if status.State != "" && status.State != NativeWorkloadIdentityStatusSkipped {
				statusWriter, ok := store.(ProjectNativeWorkloadIdentityStatusWriter)
				if !ok || statusWriter == nil {
					writeProblem(w, http.StatusServiceUnavailable, "project workload identity status store not configured")
					return
				}
				persisted, persistErr := statusWriter.SetProjectNativeWorkloadIdentityStatus(r.Context(), project, status)
				if persistErr != nil {
					writeProblem(w, http.StatusInternalServerError, "record workload identity status failed")
					return
				}
				updated = persisted
			}
			if err != nil {
				writeProblem(w, http.StatusBadGateway, "workload identity reconciliation failed")
				return
			}
		}
		// Reconcile glimmung-owned auth.romaine.life slot origins. The
		// wildcard is invariant under scale (it's derived from
		// native_standby_dns.record_base, not from count), but running
		// reconciliation here gives operators an idempotent self-heal:
		// re-issuing the same scale call retries a failed origin upsert.
		// Failure surfaces on the project's managed_auth_origins_status
		// row but does not abort the scale operation — slots are already
		// reconciled at this point; broken sign-in is a softer failure
		// than a half-scaled project.
		// See nelsong6/glimmung#142 stage 2.
		if managedOrigins != nil {
			originStatus, originErr := managedOrigins.ReconcileManagedOrigins(r.Context(), updated)
			if originStatus.State != "" && originStatus.State != ManagedAuthOriginStatusSkipped {
				originWriter, ok := store.(ProjectManagedAuthOriginStatusWriter)
				if !ok || originWriter == nil {
					writeProblem(w, http.StatusServiceUnavailable, "project managed auth origin status store not configured")
					return
				}
				persistedOrigins, persistErr := originWriter.SetProjectManagedAuthOriginStatus(r.Context(), project, originStatus)
				if persistErr != nil {
					writeProblem(w, http.StatusInternalServerError, "record managed auth origin status failed")
					return
				}
				updated = persistedOrigins
			}
			if originErr != nil {
				writeProblem(w, http.StatusBadGateway, "managed auth origin reconciliation failed")
				return
			}
		}
		if preparer != nil && len(removedSlots) > 0 {
			if err := deprovisionProjectTestEnvironments(r.Context(), preparer, before, removedSlots); err != nil {
				writeProblem(w, http.StatusBadGateway, err.Error())
				return
			}
		}
		if preparer != nil && *req.Count > 0 {
			warmed, err := warmProjectTestEnvironments(r.Context(), store, preparer, project, updated, *req.Count)
			if err != nil {
				writeProblem(w, http.StatusBadGateway, err.Error())
				return
			}
			if warmed.ID != "" || warmed.Name != "" {
				updated = warmed
			}
		}
		writeJSON(w, http.StatusOK, updated)
	}
}

func activeTestSlotLeasesAboveCount(ctx context.Context, store ReadStore, project Project, projectKey string, count int) ([]Lease, error) {
	stateStore, ok := store.(StateStore)
	if !ok || stateStore == nil {
		return nil, ErrUnsupported
	}
	leases, err := stateStore.ListLeases(ctx)
	if err != nil {
		return nil, err
	}
	projectNames := map[string]bool{}
	for _, name := range []string{projectKey, project.Name, project.ID} {
		if strings.TrimSpace(name) != "" {
			projectNames[strings.TrimSpace(name)] = true
		}
	}
	active := make([]Lease, 0)
	for _, lease := range leases {
		if lease.State != "claimed" || !boolFromMap(lease.Metadata, "test_slot_checkout") {
			continue
		}
		if !projectNames[lease.Project] {
			continue
		}
		slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
		if slotIndex == nil || *slotIndex <= count {
			continue
		}
		active = append(active, lease)
	}
	sort.SliceStable(active, func(i, j int) bool {
		left := nativeSlotIndexFromMetadata(active[i].Metadata)
		right := nativeSlotIndexFromMetadata(active[j].Metadata)
		if left != nil && right != nil && *left != *right {
			return *left < *right
		}
		return active[i].RequestedAt.Before(active[j].RequestedAt)
	})
	return active, nil
}

func warmProjectTestEnvironments(ctx context.Context, store ReadStore, preparer TestSlotPreparer, projectKey string, project Project, count int) (Project, error) {
	writer, ok := store.(ProjectTestEnvironmentSlotStatusWriter)
	if !ok || writer == nil {
		return Project{}, nil
	}
	current := project
	projectName := firstNonEmpty(project.Name, project.ID, projectKey)
	for slotIndex := 1; slotIndex <= count; slotIndex++ {
		if testEnvironmentSlotState(current, slotIndex) == "ready" {
			continue
		}
		slotName := testEnvironmentName(projectName, slotIndex, current, Lease{})
		warming := TestEnvironmentSlotStatus{
			SlotIndex: slotIndex,
			SlotName:  slotName,
			State:     "warming",
			UpdatedAt: time.Now().UTC(),
		}
		updated, err := writer.SetProjectTestEnvironmentSlotStatus(ctx, projectKey, warming)
		if err != nil {
			return current, err
		}
		current = updated

		lease := testEnvironmentWarmupLease(current, slotIndex, slotName)
		if err := preparer.EnsureTestSlotPreliminaries(ctx, lease, current); err != nil {
			detail := err.Error()
			_, _ = writer.SetProjectTestEnvironmentSlotStatus(ctx, projectKey, TestEnvironmentSlotStatus{
				SlotIndex: slotIndex,
				SlotName:  slotName,
				State:     "error",
				UpdatedAt: time.Now().UTC(),
				Detail:    &detail,
			})
			return current, err
		}
		cleanupTestSlotInstaller(ctx, preparer, lease, current, nil)
		now := time.Now().UTC()
		updated, err = writer.SetProjectTestEnvironmentSlotStatus(ctx, projectKey, TestEnvironmentSlotStatus{
			SlotIndex: slotIndex,
			SlotName:  slotName,
			State:     "ready",
			UpdatedAt: now,
			ReadyAt:   &now,
		})
		if err != nil {
			return current, err
		}
		current = updated
	}
	return current, nil
}

func deprovisionProjectTestEnvironments(ctx context.Context, preparer TestSlotPreparer, project Project, slots []TestEnvironmentSlotStatus) error {
	for _, slot := range slots {
		if strings.TrimSpace(slot.SlotName) == "" {
			continue
		}
		lease := testEnvironmentWarmupLease(project, slot.SlotIndex, slot.SlotName)
		if err := preparer.DeprovisionTestSlot(ctx, lease, project); err != nil {
			return fmt.Errorf("deprovision test slot %s: %w", slot.SlotName, err)
		}
	}
	return nil
}

func findProjectByKey(ctx context.Context, store ReadStore, key string) (Project, bool, error) {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return Project{}, false, err
	}
	for _, project := range projects {
		if project.Name == key || project.ID == key {
			return project, true, nil
		}
	}
	return Project{}, false, nil
}

func testEnvironmentWarmupLease(project Project, slotIndex int, slotName string) Lease {
	host := "native-k8s"
	return Lease{
		Project: firstNonEmpty(project.Name, project.ID),
		Host:    &host,
		State:   "warming",
		Metadata: map[string]any{
			"test_slot_checkout":        true,
			"native_k8s":                true,
			"native_slot_index":         strconv.Itoa(slotIndex),
			"native_slot_name":          slotName,
			"native_sessions_namespace": testSlotSessionsNamespace(slotName, project),
		},
	}
}

func testEnvironmentSlotsAboveCount(project Project, count int) []TestEnvironmentSlotStatus {
	removed := make([]TestEnvironmentSlotStatus, 0)
	projectName := firstNonEmpty(project.Name, project.ID)
	if standbyDNS, ok := mapFromMap(project.Metadata, "native_standby_dns"); ok {
		for _, slot := range mapSliceFromAnySlice(anySlice(standbyDNS["slots"])) {
			index, ok := positiveIntFromMap(slot, "slot_index")
			if !ok {
				index, ok = positiveIntFromMap(slot, "slotIndex")
			}
			if !ok || index <= count {
				continue
			}
			slotName, _ := stringFromMap(slot, "slot_name")
			if strings.TrimSpace(slotName) == "" {
				slotName, _ = stringFromMap(slot, "slotName")
			}
			if strings.TrimSpace(slotName) == "" {
				slotName = testEnvironmentName(projectName, index, project, Lease{})
			}
			removed = append(removed, TestEnvironmentSlotStatus{
				SlotIndex: index,
				SlotName:  strings.TrimSpace(slotName),
			})
		}
	}
	sort.SliceStable(removed, func(i, j int) bool {
		return removed[i].SlotIndex < removed[j].SlotIndex
	})
	return removed
}

func testEnvironmentSlotState(project Project, slotIndex int) string {
	if standbyDNS, ok := mapFromMap(project.Metadata, "native_standby_dns"); ok {
		for _, slot := range mapSliceFromAnySlice(anySlice(standbyDNS["slots"])) {
			n, ok := positiveIntFromMap(slot, "slot_index")
			if !ok {
				n, ok = positiveIntFromMap(slot, "slotIndex")
			}
			if !ok || n != slotIndex {
				continue
			}
			if value, ok := stringFromMap(slot, "state"); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

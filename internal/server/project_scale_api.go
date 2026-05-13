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
	SlotIndex int        `json:"slot_index"`
	SlotName  string     `json:"slot_name"`
	State     string     `json:"state"`
	UpdatedAt time.Time  `json:"updated_at"`
	Detail    *string    `json:"detail,omitempty"`
	ReadyAt   *time.Time `json:"ready_at,omitempty"`
}

func scaleProjectTestEnvironments(store ReadStore, authRedirects NativeAuthRedirectReconciler, preparer TestSlotPreparer, minter NativeGitHubTokenMinter) http.HandlerFunc {
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

		updated, err := scaler.SetProjectTestEnvironmentCount(r.Context(), project, *req.Count)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "project not found")
				return
			}
			writeProblem(w, http.StatusInternalServerError, "scale project test environments failed")
			return
		}
		if authRedirects != nil {
			status, err := authRedirects.ReconcileNativeAuthRedirects(r.Context(), updated)
			if status.State != "" && status.State != NativeAuthRedirectStatusSkipped {
				statusWriter, ok := store.(ProjectNativeAuthRedirectStatusWriter)
				if !ok || statusWriter == nil {
					writeProblem(w, http.StatusServiceUnavailable, "project auth redirect status store not configured")
					return
				}
				persisted, persistErr := statusWriter.SetProjectNativeAuthRedirectStatus(r.Context(), project, status)
				if persistErr != nil {
					writeProblem(w, http.StatusInternalServerError, "record auth redirect status failed")
					return
				}
				updated = persisted
			}
			if err != nil {
				writeProblem(w, http.StatusBadGateway, "auth redirect reconciliation failed")
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
			warmed, err := warmProjectTestEnvironments(r.Context(), store, preparer, minter, project, updated, *req.Count)
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

func warmProjectTestEnvironments(ctx context.Context, store ReadStore, preparer TestSlotPreparer, minter NativeGitHubTokenMinter, projectKey string, project Project, count int) (Project, error) {
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
		if err := preparer.EnsureTestSlot(ctx, lease, current, minter); err != nil {
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

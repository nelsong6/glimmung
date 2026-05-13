package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type TestSlotCheckoutRequest struct {
	Project       string              `json:"project"`
	Workflow      *string             `json:"workflow"`
	Requester     LeaseRequesterInput `json:"requester"`
	TankSessionID *string             `json:"tank_session_id"`
	TTLSeconds    *int                `json:"ttl_seconds"`
}

type TestSlotCheckoutResult struct {
	State     string  `json:"state"`
	Project   string  `json:"project"`
	Workflow  string  `json:"workflow"`
	SlotIndex *int    `json:"slot_index,omitempty"`
	SlotName  *string `json:"slot_name,omitempty"`
	URL       *string `json:"url,omitempty"`
	Lease     string  `json:"lease"`
	Host      *string `json:"host,omitempty"`
	Detail    *string `json:"detail,omitempty"`
}

type TestSlotReturnRequest struct {
	Project   string  `json:"project"`
	SlotIndex *int    `json:"slot_index"`
	SlotName  *string `json:"slot_name"`
}

type TestSlotReturnResult struct {
	State          string  `json:"state"`
	Project        string  `json:"project"`
	Lease          string  `json:"lease"`
	SlotIndex      *int    `json:"slot_index,omitempty"`
	SlotName       *string `json:"slot_name,omitempty"`
	CleanupStarted bool    `json:"cleanup_started"`
}

func checkoutTestSlot(store ReadStore, preparer TestSlotPreparer, minter NativeGitHubTokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		leaseStore, ok := store.(LeaseStore)
		if !ok || leaseStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease store not configured")
			return
		}
		var req TestSlotCheckoutRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		req.Project = strings.TrimSpace(req.Project)
		if req.Project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		project, ok := findProjectForTestSlot(r, w, store, req.Project)
		if !ok {
			return
		}
		workflow := "test-slot-checkout"
		if req.Workflow != nil && strings.TrimSpace(*req.Workflow) != "" {
			workflow = strings.TrimSpace(*req.Workflow)
		}
		metadata := map[string]any{
			"test_slot_checkout": true,
		}
		requester := req.Requester
		if strings.TrimSpace(requester.Consumer) == "" {
			requester.Consumer = "test-slot"
		}
		if strings.TrimSpace(requester.Kind) == "" {
			requester.Kind = "checkout"
		}
		if strings.TrimSpace(requester.Ref) == "" {
			requester.Ref = testSlotRequesterRef(req)
		}
		lease, host, err := leaseStore.AcquireLease(r.Context(), LeaseAcquireRequest{
			Project:    req.Project,
			Workflow:   &workflow,
			Metadata:   metadata,
			Requester:  requester,
			TTLSeconds: req.TTLSeconds,
		})
		if err != nil {
			var validationErr ValidationError
			if errors.As(err, &validationErr) {
				writeProblem(w, http.StatusBadRequest, validationErr.Message)
				return
			}
			if errors.Is(err, ErrUnavailable) {
				writeProblem(w, http.StatusServiceUnavailable, "no ready test environment slots available")
				return
			}
			writeProblem(w, http.StatusInternalServerError, "test-slot checkout failed")
			return
		}
		if preparer != nil && lease.State == "claimed" && boolFromMap(lease.Metadata, "test_slot_checkout") {
			if err := preparer.ActivateTestSlotRuntime(r.Context(), lease, project, minter); err != nil {
				markLeaseSlotStatus(r.Context(), store, project, lease, "error", err)
				_ = preparer.ReturnTestSlotRuntime(r.Context(), lease, project)
				_, _ = leaseStore.CancelLeaseByRef(r.Context(), req.Project, LeasePublicRefFromLease(lease))
				writeProblem(w, http.StatusBadGateway, "test-slot runtime activation failed")
				return
			}
		}
		writeJSON(w, http.StatusOK, testSlotCheckoutResponse(project, workflow, lease, host))
	}
}

func returnTestSlot(store ReadStore, preparer TestSlotPreparer, _ NativeGitHubTokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		leaseStore, ok := store.(LeaseStore)
		stateStore, hasState := store.(StateStore)
		if !ok || leaseStore == nil || !hasState || stateStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "test-slot store not configured")
			return
		}
		var req TestSlotReturnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Project = strings.TrimSpace(req.Project)
		if req.Project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		lease, err := resolveTestSlotLease(r, stateStore, req)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "test slot lease not found")
				return
			}
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		cleanupStarted := lease.State == "claimed" && boolFromMap(lease.Metadata, "test_slot_checkout")
		if preparer != nil && cleanupStarted {
			project, ok := findProjectForTestSlot(r, w, store, req.Project)
			if !ok {
				return
			}
			markLeaseSlotStatus(r.Context(), store, project, lease, "warming", nil)
			if err := preparer.ReturnTestSlotRuntime(r.Context(), lease, project); err != nil {
				markLeaseSlotStatus(r.Context(), store, project, lease, "error", err)
				writeProblem(w, http.StatusBadGateway, "test-slot cleanup failed")
				return
			}
			if err := preparer.EnsureTestSlotPreliminaries(r.Context(), lease, project); err != nil {
				markLeaseSlotStatus(r.Context(), store, project, lease, "error", err)
				writeProblem(w, http.StatusBadGateway, "test-slot rewarm failed")
				return
			}
			markLeaseSlotStatus(r.Context(), store, project, lease, "ready", nil)
		}
		result, err := leaseStore.CancelLeaseByRef(r.Context(), req.Project, LeasePublicRefFromLease(lease))
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "test-slot return failed")
			return
		}
		resp := TestSlotReturnResult{
			State:          result.State,
			Project:        req.Project,
			Lease:          result.LeaseRef,
			SlotIndex:      nativeSlotIndexFromMetadata(lease.Metadata),
			SlotName:       nativeSlotNameFromMetadata(lease.Metadata),
			CleanupStarted: cleanupStarted,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func markLeaseSlotStatus(ctx context.Context, store ReadStore, project Project, lease Lease, state string, cause error) {
	writer, ok := store.(ProjectTestEnvironmentSlotStatusWriter)
	if !ok || writer == nil {
		return
	}
	slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
	slotName := nativeSlotNameFromMetadata(lease.Metadata)
	if slotIndex == nil || slotName == nil {
		return
	}
	now := time.Now().UTC()
	var detail *string
	if cause != nil {
		text := cause.Error()
		detail = &text
	}
	status := TestEnvironmentSlotStatus{
		SlotIndex: *slotIndex,
		SlotName:  *slotName,
		State:     state,
		UpdatedAt: now,
		Detail:    detail,
	}
	if state == "ready" {
		status.ReadyAt = &now
	}
	_, _ = writer.SetProjectTestEnvironmentSlotStatus(ctx, firstNonEmpty(lease.Project, project.ID, project.Name), status)
}

func findProjectForTestSlot(r *http.Request, w http.ResponseWriter, store ReadStore, name string) (Project, bool) {
	projects, err := store.ListProjects(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "list projects failed")
		return Project{}, false
	}
	for _, project := range projects {
		if project.Name == name || project.ID == name {
			return project, true
		}
	}
	writeProblem(w, http.StatusBadRequest, fmt.Sprintf("project %q not registered", name))
	return Project{}, false
}

func testSlotCheckoutResponse(project Project, workflow string, lease Lease, host *Host) TestSlotCheckoutResult {
	slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
	slotName := nativeSlotNameFromMetadata(lease.Metadata)
	url := testSlotURL(project, slotName)
	ref := LeasePublicRefFromLease(lease)
	var hostName *string
	if host != nil {
		hostName = &host.Name
	}
	var detail *string
	if host == nil {
		text := "slot unavailable; checkout request is waiting"
		detail = &text
	}
	return TestSlotCheckoutResult{
		State:     lease.State,
		Project:   project.Name,
		Workflow:  workflow,
		SlotIndex: slotIndex,
		SlotName:  slotName,
		URL:       url,
		Lease:     ref,
		Host:      hostName,
		Detail:    detail,
	}
}

func resolveTestSlotLease(r *http.Request, store StateStore, req TestSlotReturnRequest) (Lease, error) {
	if req.SlotIndex == nil && (req.SlotName == nil || strings.TrimSpace(*req.SlotName) == "") {
		return Lease{}, errors.New("slot_index or slot_name required")
	}
	leases, err := store.ListLeases(r.Context())
	if err != nil {
		return Lease{}, err
	}
	targetName := ""
	if req.SlotName != nil {
		targetName = strings.TrimSpace(*req.SlotName)
	}
	var candidates []Lease
	for _, lease := range leases {
		if lease.Project != req.Project || !boolFromMap(lease.Metadata, "test_slot_checkout") {
			continue
		}
		if lease.State != "claimed" && lease.State != "pending" {
			continue
		}
		if targetName != "" && nativeSlotNameMatches(lease.Metadata, targetName) {
			candidates = append(candidates, lease)
			continue
		}
		if req.SlotIndex != nil {
			if slot := nativeSlotIndexFromMetadata(lease.Metadata); slot != nil && *slot == *req.SlotIndex {
				candidates = append(candidates, lease)
			}
		}
	}
	if len(candidates) == 0 {
		return Lease{}, ErrNotFound
	}
	sortLeasesForReturn(candidates)
	return candidates[0], nil
}

func testSlotRequesterRef(req TestSlotCheckoutRequest) string {
	if req.TankSessionID != nil && strings.TrimSpace(*req.TankSessionID) != "" {
		return "tank-session-" + strings.TrimSpace(*req.TankSessionID)
	}
	return req.Project
}

func testSlotPrefix(project Project) string {
	if standby, ok := mapFromMap(project.Metadata, "native_standby_dns"); ok {
		if value, ok := stringFromMap(standby, "slot_prefix"); ok && strings.TrimSpace(value) != "" {
			return strings.Trim(strings.TrimSpace(value), ".")
		}
		if value, ok := stringFromMap(standby, "slotPrefix"); ok && strings.TrimSpace(value) != "" {
			return strings.Trim(strings.TrimSpace(value), ".")
		}
	}
	return firstNonEmpty(project.Name, project.ID)
}

func testSlotURL(project Project, slotName *string) *string {
	if slotName == nil || strings.TrimSpace(*slotName) == "" {
		return nil
	}
	if standby, ok := mapFromMap(project.Metadata, "native_standby_dns"); ok {
		if base, ok := stringFromMap(standby, "record_base"); ok && strings.TrimSpace(base) != "" {
			value := "https://" + strings.TrimSpace(*slotName) + "." + strings.Trim(strings.TrimSpace(base), ".") + "/"
			return &value
		}
		if base, ok := stringFromMap(standby, "recordBase"); ok && strings.TrimSpace(base) != "" {
			value := "https://" + strings.TrimSpace(*slotName) + "." + strings.Trim(strings.TrimSpace(base), ".") + "/"
			return &value
		}
	}
	return nil
}

func nativeSlotIndexFromMetadata(metadata map[string]any) *int {
	if n, ok := positiveIntFromMap(metadata, "native_slot_index"); ok {
		return &n
	}
	return nil
}

func nativeSlotNameFromMetadata(metadata map[string]any) *string {
	if value, ok := stringFromMap(metadata, "native_slot_name"); ok && strings.TrimSpace(value) != "" {
		clean := strings.TrimSpace(value)
		return &clean
	}
	return nil
}

func nativeSlotNameMatches(metadata map[string]any, target string) bool {
	slotName := nativeSlotNameFromMetadata(metadata)
	return slotName != nil && *slotName == target
}

func sortLeasesForReturn(leases []Lease) {
	sort.SliceStable(leases, func(i, j int) bool {
		if leases[i].State != leases[j].State {
			return leases[i].State == "claimed"
		}
		return leases[i].RequestedAt.Before(leases[j].RequestedAt)
	})
}

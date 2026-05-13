package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type TestSlotCheckoutRequest struct {
	Project       string              `json:"project"`
	Workflow      *string             `json:"workflow"`
	SlotIndex     *int                `json:"slot_index"`
	Mode          string              `json:"mode"`
	Requester     LeaseRequesterInput `json:"requester"`
	TankSessionID *string             `json:"tank_session_id"`
	PhaseInputs   map[string]string   `json:"phase_inputs"`
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

func checkoutTestSlot(store ReadStore, preparer TestSlotPreparer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		leaseStore, ok := store.(LeaseStore)
		if !ok || leaseStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "lease store not configured")
			return
		}
		var req TestSlotCheckoutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Project = strings.TrimSpace(req.Project)
		if req.Project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		mode := strings.TrimSpace(strings.ToLower(req.Mode))
		if mode == "" {
			mode = "provision"
		}
		if mode != "provision" && mode != "clean_slate" {
			writeProblem(w, http.StatusBadRequest, "mode must be 'provision' or 'clean_slate'")
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
		slotName := ""
		if req.SlotIndex != nil {
			slotName = testSlotName(project, *req.SlotIndex)
		}
		phaseInputs := map[string]any{}
		for k, v := range req.PhaseInputs {
			phaseInputs[k] = v
		}
		if req.SlotIndex != nil {
			phaseInputs["validation_slot_index"] = strconv.Itoa(*req.SlotIndex)
			phaseInputs["slot_name"] = slotName
			phaseInputs["namespace"] = slotName
		}
		phaseInputs["test_slot_mode"] = mode
		phaseInputs["clean_slate"] = strconv.FormatBool(mode == "clean_slate")

		metadata := map[string]any{
			"test_slot_checkout": true,
			"test_slot_mode":     mode,
			"phase_inputs":       phaseInputs,
			"native_slot_prefix": testSlotPrefix(project),
		}
		if slotName != "" {
			metadata["native_slot_name"] = slotName
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
			writeProblem(w, http.StatusInternalServerError, "test-slot checkout failed")
			return
		}
		if host != nil && preparer != nil {
			if err := preparer.EnsureTestSlot(r.Context(), lease); err != nil {
				_, _ = leaseStore.CancelLeaseByRef(r.Context(), req.Project, LeasePublicRefFromLease(lease))
				writeProblem(w, http.StatusInternalServerError, "failed to prepare test environment for slot")
				return
			}
		}
		writeJSON(w, http.StatusOK, testSlotCheckoutResponse(project, workflow, lease, host, req.SlotIndex))
	}
}

func returnTestSlot(store ReadStore, preparer TestSlotPreparer) http.HandlerFunc {
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
			_ = preparer.ReturnTestSlot(r.Context(), lease)
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

func testSlotCheckoutResponse(project Project, workflow string, lease Lease, host *Host, requestedSlot *int) TestSlotCheckoutResult {
	slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
	if slotIndex == nil {
		slotIndex = requestedSlot
	}
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
	if req.SlotIndex != nil {
		return fmt.Sprintf("%s-slot-%d", req.Project, *req.SlotIndex)
	}
	return req.Project
}

func testSlotName(project Project, slotIndex int) string {
	return fmt.Sprintf("%s-%d", testSlotPrefix(project), slotIndex)
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
	if phaseInputs, ok := mapFromMap(metadata, "phase_inputs"); ok {
		if n, ok := positiveIntFromMap(phaseInputs, "validation_slot_index"); ok {
			return &n
		}
	}
	return nil
}

func nativeSlotNameFromMetadata(metadata map[string]any) *string {
	for _, key := range []string{"native_slot_name", "slot_name", "namespace"} {
		if value, ok := stringFromMap(metadata, key); ok && strings.TrimSpace(value) != "" {
			clean := strings.TrimSpace(value)
			return &clean
		}
	}
	if phaseInputs, ok := mapFromMap(metadata, "phase_inputs"); ok {
		for _, key := range []string{"slot_name", "namespace"} {
			if value, ok := stringFromMap(phaseInputs, key); ok && strings.TrimSpace(value) != "" {
				clean := strings.TrimSpace(value)
				return &clean
			}
		}
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

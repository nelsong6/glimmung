package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	testSlotStateActivating = "activating"
	testSlotStateActive     = "active"
	testSlotStateReady      = "ready"
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
	Usable    bool    `json:"usable"`
	StatusURL *string `json:"status_url,omitempty"`
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

var testSlotActivations sync.Map

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
			if _, err := setLeaseSlotStatus(r.Context(), store, project, lease, testSlotStateActivating, nil); err != nil {
				_, _ = leaseStore.CancelLeaseByRef(r.Context(), req.Project, LeasePublicRefFromLease(lease))
				writeProblem(w, http.StatusInternalServerError, "record test-slot activation state failed")
				return
			}
			beginTestSlotActivation(store, preparer, minter, project, lease, nil)
			writeJSON(w, http.StatusAccepted, testSlotCheckoutResponse(project, workflow, lease, host, testSlotStateActivating))
			return
		}
		writeJSON(w, http.StatusOK, testSlotCheckoutResponse(project, workflow, lease, host, lease.State))
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
	_, _ = setLeaseSlotStatus(ctx, store, project, lease, state, cause)
}

func setLeaseSlotStatus(ctx context.Context, store ReadStore, project Project, lease Lease, state string, cause error) (Project, error) {
	writer, ok := store.(ProjectTestEnvironmentSlotStatusWriter)
	if !ok || writer == nil {
		return Project{}, errors.New("test-slot status store not configured")
	}
	slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
	slotName := nativeSlotNameFromMetadata(lease.Metadata)
	if slotIndex == nil || slotName == nil {
		return Project{}, errors.New("test-slot lease is missing slot metadata")
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
	if state == testSlotStateReady {
		status.ReadyAt = &now
	}
	return writer.SetProjectTestEnvironmentSlotStatus(ctx, firstNonEmpty(lease.Project, project.ID, project.Name), status)
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

func testSlotCheckoutResponse(project Project, workflow string, lease Lease, host *Host, state string) TestSlotCheckoutResult {
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
	if state == testSlotStateActivating {
		text := "test-slot runtime activation is in progress"
		detail = &text
	}
	statusURL := testSlotStatusURL(project, slotName)
	return TestSlotCheckoutResult{
		State:     state,
		Project:   project.Name,
		Workflow:  workflow,
		SlotIndex: slotIndex,
		SlotName:  slotName,
		URL:       url,
		Lease:     ref,
		Host:      hostName,
		Usable:    state == testSlotStateActive,
		StatusURL: statusURL,
		Detail:    detail,
	}
}

func testSlotStatusURL(project Project, slotName *string) *string {
	if slotName == nil || strings.TrimSpace(*slotName) == "" {
		return nil
	}
	projectName := firstNonEmpty(project.Name, project.ID)
	if strings.TrimSpace(projectName) == "" {
		return nil
	}
	value := "/v1/projects/" + projectName + "/test-environments/" + strings.TrimSpace(*slotName)
	return &value
}

func beginTestSlotActivation(store ReadStore, preparer TestSlotPreparer, minter NativeGitHubTokenMinter, project Project, lease Lease, logf func(string, ...any)) bool {
	if preparer == nil {
		return false
	}
	key := testSlotActivationKey(lease)
	if key == "" {
		return false
	}
	if _, loaded := testSlotActivations.LoadOrStore(key, struct{}{}); loaded {
		return false
	}
	go func() {
		defer testSlotActivations.Delete(key)
		activateTestSlotRuntime(context.Background(), store, preparer, minter, project, lease, logf)
	}()
	return true
}

func activateTestSlotRuntime(parent context.Context, store ReadStore, preparer TestSlotPreparer, minter NativeGitHubTokenMinter, project Project, lease Lease, logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	err := preparer.ActivateTestSlotRuntime(ctx, lease, project, minter)
	cancel()
	if !testSlotLeaseStillClaimed(context.Background(), store, lease) {
		return
	}
	if !testSlotLeaseStillActivating(context.Background(), store, project, lease) {
		return
	}
	if err == nil {
		if _, statusErr := setLeaseSlotStatus(context.Background(), store, project, lease, testSlotStateActive, nil); statusErr != nil && logf != nil {
			logf("record test-slot activation success failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), statusErr)
		}
		return
	}

	if logf != nil {
		logf("test-slot activation failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), err)
	}
	markLeaseSlotStatus(context.Background(), store, project, lease, "error", err)

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	if cleanupErr := preparer.ReturnTestSlotRuntime(cleanupCtx, lease, project); cleanupErr != nil && logf != nil {
		logf("test-slot activation cleanup failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), cleanupErr)
	}
	cleanupCancel()

	if leaseStore, ok := store.(LeaseStore); ok && leaseStore != nil {
		cancelCtx, cancelRelease := context.WithTimeout(context.Background(), 30*time.Second)
		if _, cancelErr := leaseStore.CancelLeaseByRef(cancelCtx, lease.Project, LeasePublicRefFromLease(lease)); cancelErr != nil && logf != nil {
			logf("test-slot activation lease release failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), cancelErr)
		}
		cancelRelease()
	}
}

func testSlotActivationKey(lease Lease) string {
	if strings.TrimSpace(lease.ID) != "" {
		return lease.Project + ":id:" + strings.TrimSpace(lease.ID)
	}
	if lease.LeaseNumber != nil && *lease.LeaseNumber > 0 {
		return fmt.Sprintf("%s:number:%d", lease.Project, *lease.LeaseNumber)
	}
	ref := LeasePublicRefFromLease(lease)
	if strings.TrimSpace(ref) == "" {
		return ""
	}
	return lease.Project + ":" + ref
}

func testSlotLeaseStillClaimed(ctx context.Context, store ReadStore, lease Lease) bool {
	stateStore, ok := store.(StateStore)
	if !ok || stateStore == nil {
		return true
	}
	leases, err := stateStore.ListLeases(ctx)
	if err != nil {
		return true
	}
	for _, current := range leases {
		if sameTestSlotLease(current, lease) {
			return current.State == "claimed"
		}
	}
	return false
}

func sameTestSlotLease(left Lease, right Lease) bool {
	if left.Project != right.Project {
		return false
	}
	if strings.TrimSpace(left.ID) != "" && strings.TrimSpace(right.ID) != "" {
		return left.ID == right.ID
	}
	if left.LeaseNumber != nil && right.LeaseNumber != nil {
		return *left.LeaseNumber == *right.LeaseNumber
	}
	return LeasePublicRefFromLease(left) == LeasePublicRefFromLease(right)
}

func testSlotLeaseStillActivating(ctx context.Context, store ReadStore, project Project, lease Lease) bool {
	slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
	if slotIndex == nil {
		return false
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return true
	}
	for _, current := range projects {
		if current.Name != lease.Project && current.ID != lease.Project && current.Name != project.Name && current.ID != project.ID {
			continue
		}
		status, ok := testEnvironmentSlotStatus(current, *slotIndex)
		return ok && status.State == testSlotStateActivating
	}
	return false
}

func StartTestSlotActivationRecoveryLoop(ctx context.Context, store ReadStore, preparer TestSlotPreparer, minter NativeGitHubTokenMinter, interval time.Duration, logf func(string, ...any)) {
	if store == nil || preparer == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	recoverActivatingTestSlots(ctx, store, preparer, minter, 30*time.Second, logf)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recoverActivatingTestSlots(ctx, store, preparer, minter, 30*time.Second, logf)
		}
	}
}

func recoverActivatingTestSlots(ctx context.Context, store ReadStore, preparer TestSlotPreparer, minter NativeGitHubTokenMinter, minAge time.Duration, logf func(string, ...any)) int {
	if store == nil || preparer == nil {
		return 0
	}
	stateStore, ok := store.(StateStore)
	if !ok || stateStore == nil {
		return 0
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		if logf != nil {
			logf("test-slot activation recovery list projects failed: %v", err)
		}
		return 0
	}
	leases, err := stateStore.ListLeases(ctx)
	if err != nil {
		if logf != nil {
			logf("test-slot activation recovery list leases failed: %v", err)
		}
		return 0
	}
	projectsByKey := map[string]Project{}
	for _, project := range projects {
		if project.Name != "" {
			projectsByKey[project.Name] = project
		}
		if project.ID != "" {
			projectsByKey[project.ID] = project
		}
	}
	started := 0
	now := time.Now()
	for _, lease := range leases {
		if lease.State != "claimed" || !boolFromMap(lease.Metadata, "test_slot_checkout") {
			continue
		}
		slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
		if slotIndex == nil {
			continue
		}
		project, ok := projectsByKey[lease.Project]
		if !ok {
			continue
		}
		status, ok := testEnvironmentSlotStatus(project, *slotIndex)
		if !ok || status.State != testSlotStateActivating {
			continue
		}
		if !status.UpdatedAt.IsZero() && now.Sub(status.UpdatedAt) < minAge {
			continue
		}
		if beginTestSlotActivation(store, preparer, minter, project, lease, logf) {
			started++
		}
	}
	return started
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

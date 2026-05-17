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
	testSlotStateCleaning   = "cleaning"
	testSlotStateReady      = "ready"
	// testSlotStateWarming is recorded while the reconciler runs preliminary
	// reconciliation for a slot. It is the only state through which a slot
	// reaches `ready` from "no record at all". Nothing else may default a slot
	// to this string; in particular the state API must not synthesize it as a
	// UI placeholder for missing records.
	testSlotStateWarming = "warming"

	// testSlotDefaultTTLSeconds is the TTL applied to a test-slot lease when
	// the caller does not pass `ttl_seconds`. Interactive review of a test
	// slot routinely takes longer than the generic 15-minute lease default —
	// signing in, hot-swapping, working through a session — so the default
	// here is one hour. Callers (smoke workflows, automation) that want a
	// tighter bound still pass `ttl_seconds` explicitly and override this.
	testSlotDefaultTTLSeconds = 3600
)

type TestSlotCheckoutRequest struct {
	Project       string              `json:"project"`
	Workflow      *string             `json:"workflow"`
	Requester     LeaseRequesterInput `json:"requester"`
	TankSessionID *string             `json:"tank_session_id"`
	TTLSeconds    *int                `json:"ttl_seconds"`
}

type TestSlotCheckoutResult struct {
	State                string  `json:"state"`
	Project              string  `json:"project"`
	Workflow             string  `json:"workflow"`
	SlotIndex            *int    `json:"slot_index,omitempty"`
	SlotName             *string `json:"slot_name,omitempty"`
	URL                  *string `json:"url,omitempty"`
	PlaywrightWSEndpoint *string `json:"playwright_ws_endpoint,omitempty"`
	Lease                string  `json:"lease"`
	Host                 *string `json:"host,omitempty"`
	Usable               bool    `json:"usable"`
	StatusURL            *string `json:"status_url,omitempty"`
	Detail               *string `json:"detail,omitempty"`
}

type TestSlotReturnRequest struct {
	Project         string  `json:"project"`
	SlotIndex       *int    `json:"slot_index"`
	SlotName        *string `json:"slot_name"`
	CallerPodIP     *string `json:"caller_pod_ip"`
	CallerSessionID *string `json:"caller_session_id"`
	Source          *string `json:"source"`
	Reason          *string `json:"reason"`
}

type TestSlotReturnResult struct {
	State          string  `json:"state"`
	Project        string  `json:"project"`
	Lease          string  `json:"lease"`
	SlotIndex      *int    `json:"slot_index,omitempty"`
	SlotName       *string `json:"slot_name,omitempty"`
	CleanupStarted bool    `json:"cleanup_started"`
	Usable         bool    `json:"usable"`
	StatusURL      *string `json:"status_url,omitempty"`
	Detail         *string `json:"detail,omitempty"`
}

var testSlotActivations sync.Map
var testSlotCleanups sync.Map

func checkoutTestSlot(settings Settings, store ReadStore, preparer TestSlotPreparer, minter NativeGitHubTokenMinter) http.HandlerFunc {
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
		ttlSeconds := req.TTLSeconds
		if ttlSeconds == nil {
			defaultTTL := testSlotDefaultTTLSeconds
			ttlSeconds = &defaultTTL
		}
		lease, err := leaseStore.AcquireLease(r.Context(), LeaseAcquireRequest{
			Project:    req.Project,
			Workflow:   &workflow,
			Metadata:   metadata,
			Requester:  requester,
			TTLSeconds: ttlSeconds,
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
			if _, err := setLeaseSlotActivationStarting(r.Context(), store, project, lease); err != nil {
				_, _ = leaseStore.CancelLeaseByRef(r.Context(), req.Project, LeasePublicRefFromLease(lease))
				writeProblem(w, http.StatusInternalServerError, "record test-slot activation state failed")
				return
			}
			// Arm TTL expiry as soon as the lease is claimed. Cleanup
			// pathways (return / callback release / admin cancel) call
			// cancelLeaseExpiryTimer on the way out, so the timer only
			// fires if nobody returned the lease in time.
			armLeaseExpiryTimer(store, preparer, project, lease, nil)
			beginTestSlotActivation(store, preparer, minter, project, lease, nil)
			writeJSON(w, http.StatusAccepted, testSlotCheckoutResponse(settings, project, workflow, lease, testSlotStateActivating))
			return
		}
		writeJSON(w, http.StatusOK, testSlotCheckoutResponse(settings, project, workflow, lease, lease.State))
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
		historyEntry := testSlotReturnHistoryEntry(lease, testSlotReturnAudit{
			Source:          stringPointerOrDefault(req.Source, "api.test_slots.return"),
			Reason:          trimmedOptionalString(req.Reason),
			CallerPodIP:     trimmedOptionalString(req.CallerPodIP),
			CallerSessionID: trimmedOptionalString(req.CallerSessionID),
			CleanupStarted:  cleanupStarted,
		})
		if preparer != nil && cleanupStarted {
			project, ok := findProjectForTestSlot(r, w, store, req.Project)
			if !ok {
				return
			}
			if _, err := setLeaseSlotCleanupStarting(r.Context(), store, project, lease, historyEntry); err != nil {
				writeProblem(w, http.StatusInternalServerError, "record test-slot cleanup state failed")
				return
			}
			beginTestSlotCleanup(store, preparer, project, lease, true, nil)
			writeJSON(w, http.StatusAccepted, testSlotReturnResponse(project, req.Project, lease, testSlotStateCleaning, true))
			return
		}
		result, err := leaseStore.CancelLeaseByRef(r.Context(), req.Project, LeasePublicRefFromLease(lease))
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "test-slot return failed")
			return
		}
		if project, ok, err := findProjectByKey(r.Context(), store, req.Project); err == nil && ok {
			_, _ = appendLeaseSlotReturnHistory(r.Context(), store, project, lease, historyEntry)
		}
		resp := TestSlotReturnResult{
			State:          result.State,
			Project:        req.Project,
			Lease:          result.LeaseRef,
			SlotIndex:      nativeSlotIndexFromMetadata(lease.Metadata),
			SlotName:       nativeSlotNameFromMetadata(lease.Metadata),
			CleanupStarted: cleanupStarted,
			Usable:         false,
			StatusURL:      testSlotStatusURL(Project{Name: req.Project}, nativeSlotNameFromMetadata(lease.Metadata)),
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func markLeaseSlotStatus(ctx context.Context, store ReadStore, project Project, lease Lease, state string, cause error) {
	_, _ = setLeaseSlotStatus(ctx, store, project, lease, state, cause)
}

type testSlotStatusMutation func(*TestEnvironmentSlotStatus, time.Time)

type testSlotReturnAudit struct {
	Source          string
	Reason          *string
	CallerPodIP     *string
	CallerSessionID *string
	CleanupStarted  bool
}

func setLeaseSlotActivationStarting(ctx context.Context, store ReadStore, project Project, lease Lease) (Project, error) {
	return setLeaseSlotStatus(ctx, store, project, lease, testSlotStateActivating, nil, func(status *TestEnvironmentSlotStatus, now time.Time) {
		if attempt := testSlotActivationAttempt(lease); attempt != nil {
			status.ActivationAttempt = attempt
		}
		state := testSlotStateActivating
		jobName := testSlotInstallerJobName(lease)
		status.ActivationState = &state
		status.ActivationStartedAt = &now
		status.ActivationCompletedAt = nil
		status.ActivationJobName = &jobName
		status.ActivationError = nil
	})
}

func setLeaseSlotActivationFinished(ctx context.Context, store ReadStore, project Project, lease Lease, state string, cause error) (Project, error) {
	return setLeaseSlotStatus(ctx, store, project, lease, state, cause, func(status *TestEnvironmentSlotStatus, now time.Time) {
		if attempt := testSlotActivationAttempt(lease); attempt != nil {
			status.ActivationAttempt = attempt
		}
		if status.ActivationStartedAt == nil {
			status.ActivationStartedAt = &now
		}
		jobName := testSlotInstallerJobName(lease)
		status.ActivationState = &state
		status.ActivationCompletedAt = &now
		status.ActivationJobName = &jobName
		if cause != nil {
			text := cause.Error()
			status.ActivationError = &text
		} else {
			status.ActivationError = nil
		}
	})
}

func testSlotReturnHistoryEntry(lease Lease, audit testSlotReturnAudit) TestSlotReturnHistoryEntry {
	source := strings.TrimSpace(audit.Source)
	if source == "" {
		source = "api.test_slots.return"
	}
	var requester *string
	if value, ok := stringFromMap(lease.Metadata, "requester_ref"); ok && strings.TrimSpace(value) != "" {
		clean := strings.TrimSpace(value)
		requester = &clean
	}
	return TestSlotReturnHistoryEntry{
		Event:           "return_requested",
		CreatedAt:       time.Now().UTC(),
		Project:         lease.Project,
		SlotIndex:       nativeSlotIndexFromMetadata(lease.Metadata),
		SlotName:        nativeSlotNameFromMetadata(lease.Metadata),
		LeaseRef:        LeasePublicRefFromLease(lease),
		LeaseNumber:     lease.LeaseNumber,
		LeaseRequester:  requester,
		CallerPodIP:     audit.CallerPodIP,
		CallerSessionID: audit.CallerSessionID,
		Source:          source,
		Reason:          audit.Reason,
		CleanupStarted:  audit.CleanupStarted,
	}
}

func appendBoundedTestSlotReturnHistory(current []TestSlotReturnHistoryEntry, entries ...TestSlotReturnHistoryEntry) []TestSlotReturnHistoryEntry {
	history := append([]TestSlotReturnHistoryEntry{}, current...)
	for _, entry := range entries {
		if entry.Event == "" {
			continue
		}
		history = append(history, entry)
	}
	if len(history) > 20 {
		history = history[len(history)-20:]
	}
	return history
}

func trimmedOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	clean := strings.TrimSpace(*value)
	if clean == "" {
		return nil
	}
	return &clean
}

func stringPointerOrDefault(value *string, fallback string) string {
	if cleaned := trimmedOptionalString(value); cleaned != nil {
		return *cleaned
	}
	return fallback
}

func setLeaseSlotCleanupStarting(ctx context.Context, store ReadStore, project Project, lease Lease, historyEntries ...TestSlotReturnHistoryEntry) (Project, error) {
	return setLeaseSlotStatus(ctx, store, project, lease, testSlotStateCleaning, nil, func(status *TestEnvironmentSlotStatus, now time.Time) {
		state := testSlotStateCleaning
		status.CleanupState = &state
		status.CleanupStartedAt = &now
		status.CleanupCompletedAt = nil
		status.CleanupError = nil
		status.ReturnHistory = appendBoundedTestSlotReturnHistory(status.ReturnHistory, historyEntries...)
	})
}

func appendLeaseSlotReturnHistory(ctx context.Context, store ReadStore, project Project, lease Lease, historyEntry TestSlotReturnHistoryEntry) (Project, error) {
	if historyEntry.Event == "" {
		return Project{}, nil
	}
	writer, ok := store.(ProjectTestEnvironmentSlotStatusWriter)
	if !ok || writer == nil {
		return Project{}, errors.New("test-slot status store not configured")
	}
	slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
	slotName := nativeSlotNameFromMetadata(lease.Metadata)
	if slotIndex == nil || slotName == nil {
		return Project{}, errors.New("test-slot lease is missing slot metadata")
	}
	status := TestEnvironmentSlotStatus{
		SlotIndex: *slotIndex,
		SlotName:  *slotName,
		State:     testSlotStateReady,
		UpdatedAt: time.Now().UTC(),
	}
	if current, ok := currentLeaseSlotStatus(ctx, store, project, lease, *slotIndex); ok {
		status = current
		if status.SlotName == "" {
			status.SlotName = *slotName
		}
		if status.State == "" {
			status.State = testSlotStateReady
		}
		status.UpdatedAt = time.Now().UTC()
	}
	status.ReturnHistory = appendBoundedTestSlotReturnHistory(status.ReturnHistory, historyEntry)
	return writer.SetProjectTestEnvironmentSlotStatus(ctx, firstNonEmpty(lease.Project, project.ID, project.Name), status)
}

func setLeaseSlotCleanupFinished(ctx context.Context, store ReadStore, project Project, lease Lease, state string, cause error) (Project, error) {
	return setLeaseSlotStatus(ctx, store, project, lease, state, cause, func(status *TestEnvironmentSlotStatus, now time.Time) {
		status.CleanupState = &state
		if status.CleanupStartedAt == nil {
			status.CleanupStartedAt = &now
		}
		status.CleanupCompletedAt = &now
		if cause != nil {
			text := cause.Error()
			status.CleanupError = &text
		} else {
			status.CleanupError = nil
		}
	})
}

func setLeaseSlotStatus(ctx context.Context, store ReadStore, project Project, lease Lease, state string, cause error, mutations ...testSlotStatusMutation) (Project, error) {
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
	}
	if current, ok := currentLeaseSlotStatus(ctx, store, project, lease, *slotIndex); ok {
		status = current
		if status.SlotName == "" {
			status.SlotName = *slotName
		}
	}
	status.State = state
	status.UpdatedAt = now
	status.Detail = detail
	if state == testSlotStateReady {
		status.ReadyAt = &now
	}
	for _, mutate := range mutations {
		if mutate != nil {
			mutate(&status, now)
		}
	}
	return writer.SetProjectTestEnvironmentSlotStatus(ctx, firstNonEmpty(lease.Project, project.ID, project.Name), status)
}

func currentLeaseSlotStatus(ctx context.Context, store ReadStore, project Project, lease Lease, slotIndex int) (TestEnvironmentSlotStatus, bool) {
	projects, err := store.ListProjects(ctx)
	if err == nil {
		for _, current := range projects {
			if current.Name != lease.Project && current.ID != lease.Project && current.Name != project.Name && current.ID != project.ID {
				continue
			}
			if status, ok := testEnvironmentSlotStatus(current, slotIndex); ok {
				return status, true
			}
		}
	}
	return testEnvironmentSlotStatus(project, slotIndex)
}

func testSlotActivationAttempt(lease Lease) *int {
	if lease.LeaseNumber == nil || *lease.LeaseNumber <= 0 {
		return nil
	}
	value := *lease.LeaseNumber
	return &value
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

func testSlotCheckoutResponse(settings Settings, project Project, workflow string, lease Lease, state string) TestSlotCheckoutResult {
	slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
	slotName := nativeSlotNameFromMetadata(lease.Metadata)
	url := testSlotURL(project, slotName)
	ref := LeasePublicRefFromLease(lease)
	hostName := lease.Host
	var detail *string
	if hostName == nil {
		text := "slot unavailable; checkout request is waiting"
		detail = &text
	}
	if state == testSlotStateActivating {
		text := "test-slot runtime activation is in progress"
		detail = &text
	}
	statusURL := testSlotStatusURL(project, slotName)
	var playwrightEndpoint *string
	if slotName != nil {
		playwrightEndpoint = PlaywrightWSEndpointFor(settings, *slotName)
	}
	return TestSlotCheckoutResult{
		State:                state,
		Project:              project.Name,
		Workflow:             workflow,
		SlotIndex:            slotIndex,
		SlotName:             slotName,
		URL:                  url,
		PlaywrightWSEndpoint: playwrightEndpoint,
		Lease:                ref,
		Host:                 hostName,
		Usable:               state == testSlotStateActive,
		StatusURL:            statusURL,
		Detail:               detail,
	}
}

func testSlotReturnResponse(project Project, projectName string, lease Lease, state string, cleanupStarted bool) TestSlotReturnResult {
	slotName := nativeSlotNameFromMetadata(lease.Metadata)
	var detail *string
	if state == testSlotStateCleaning {
		text := "test-slot runtime cleanup is in progress"
		detail = &text
	}
	return TestSlotReturnResult{
		State:          state,
		Project:        firstNonEmpty(project.Name, project.ID, projectName),
		Lease:          LeasePublicRefFromLease(lease),
		SlotIndex:      nativeSlotIndexFromMetadata(lease.Metadata),
		SlotName:       slotName,
		CleanupStarted: cleanupStarted,
		Usable:         false,
		StatusURL:      testSlotStatusURL(project, slotName),
		Detail:         detail,
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
		if _, statusErr := setLeaseSlotActivationFinished(context.Background(), store, project, lease, testSlotStateActive, nil); statusErr != nil && logf != nil {
			logf("record test-slot activation success failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), statusErr)
		}
		cleanupTestSlotInstaller(context.Background(), preparer, lease, project, logf)
		return
	}

	if logf != nil {
		logf("test-slot activation failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), err)
	}
	_, _ = setLeaseSlotActivationFinished(context.Background(), store, project, lease, "error", err)

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	if cleanupErr := preparer.ReturnTestSlotRuntime(cleanupCtx, lease, project); cleanupErr != nil && logf != nil {
		logf("test-slot activation cleanup failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), cleanupErr)
	}
	cleanupCancel()

	if leaseStore, ok := store.(LeaseCanceller); ok && leaseStore != nil {
		// Activation failed; the timer we armed at checkout is no longer
		// meaningful (the lease is going away). Stop it.
		cancelLeaseExpiryTimer(LeasePublicRefFromLease(lease))
		cancelCtx, cancelRelease := context.WithTimeout(context.Background(), 30*time.Second)
		if _, cancelErr := leaseStore.CancelLeaseByRef(cancelCtx, lease.Project, LeasePublicRefFromLease(lease)); cancelErr != nil && logf != nil {
			logf("test-slot activation lease release failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), cancelErr)
		}
		cancelRelease()
	}
}

func cleanupTestSlotInstaller(parent context.Context, preparer TestSlotPreparer, lease Lease, project Project, logf func(string, ...any)) {
	cleaner, ok := preparer.(TestSlotInstallerCleaner)
	if !ok || cleaner == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if err := cleaner.CleanupTestSlotInstaller(ctx, lease, project); err != nil && logf != nil {
		logf("test-slot installer cleanup failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), err)
	}
}

func beginTestSlotCleanup(store ReadStore, preparer TestSlotPreparer, project Project, lease Lease, releaseLease bool, logf func(string, ...any)) bool {
	if preparer == nil {
		return false
	}
	key := testSlotActivationKey(lease)
	if key == "" {
		return false
	}
	if _, loaded := testSlotCleanups.LoadOrStore(key, struct{}{}); loaded {
		return false
	}
	// Whatever triggered cleanup (explicit return, callback release, admin
	// cancel, or TTL expiry firing this very path), we don't want the TTL
	// timer to fire a second time. Stop is idempotent.
	cancelLeaseExpiryTimer(LeasePublicRefFromLease(lease))
	go func() {
		defer testSlotCleanups.Delete(key)
		cleanupTestSlotRuntime(context.Background(), store, preparer, project, lease, releaseLease, logf)
	}()
	return true
}

func cleanupTestSlotRuntime(parent context.Context, store ReadStore, preparer TestSlotPreparer, project Project, lease Lease, releaseLease bool, logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	err := preparer.ReturnTestSlotRuntime(ctx, lease, project)
	if err == nil {
		err = preparer.EnsureTestSlotPreliminaries(ctx, lease, project)
	}
	cancel()
	if err != nil {
		if logf != nil {
			logf("test-slot cleanup failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), err)
		}
		_, _ = setLeaseSlotCleanupFinished(context.Background(), store, project, lease, "error", err)
		return
	}

	if releaseLease && testSlotLeaseStillClaimed(context.Background(), store, lease) {
		leaseStore, ok := store.(LeaseCanceller)
		if !ok || leaseStore == nil {
			err := errors.New("lease store not configured")
			_, _ = setLeaseSlotCleanupFinished(context.Background(), store, project, lease, "error", err)
			return
		}
		cancelCtx, cancelRelease := context.WithTimeout(context.Background(), 30*time.Second)
		_, cancelErr := leaseStore.CancelLeaseByRef(cancelCtx, lease.Project, LeasePublicRefFromLease(lease))
		cancelRelease()
		if cancelErr != nil {
			if logf != nil {
				logf("test-slot cleanup lease release failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), cancelErr)
			}
			_, _ = setLeaseSlotCleanupFinished(context.Background(), store, project, lease, "error", cancelErr)
			return
		}
	}
	if _, statusErr := setLeaseSlotCleanupFinished(context.Background(), store, project, lease, testSlotStateReady, nil); statusErr != nil && logf != nil {
		logf("record test-slot cleanup success failed project=%s lease=%s: %v", lease.Project, LeasePublicRefFromLease(lease), statusErr)
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
	return testSlotLeaseStillState(ctx, store, project, lease, testSlotStateActivating)
}

func testSlotLeaseStillState(ctx context.Context, store ReadStore, project Project, lease Lease, state string) bool {
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
		return ok && status.State == state
	}
	return false
}

// RecoverInFlightTestSlots is the one-shot startup pass that replaces the old
// polling reconciler. It walks Cosmos once after the process boots and:
//
//   - Re-arms TTL expiry timers for every still-claimed test-slot lease,
//     computing remaining duration from `assigned_at + ttl_seconds`. A lease
//     whose deadline has already passed fires cleanup immediately.
//   - Resumes any in-flight `activating` / `cleaning` / `warming` work whose
//     driving goroutine died with the previous process. The goroutines are
//     fresh; the durable Cosmos state is what makes the resume meaningful.
//   - Fires warmup for slots within `count` that have no `slots[*]` entry
//     (this is also covered by the PATCH-count handler, but startup is the
//     other valid moment for the same trigger).
//
// After this returns, lifecycle changes are driven exclusively by HTTP
// handlers (PATCH count, checkout, return, callback release) and per-lease
// AfterFunc timers. No periodic polling.
func RecoverInFlightTestSlots(ctx context.Context, store ReadStore, preparer TestSlotPreparer, minter NativeGitHubTokenMinter, logf func(string, ...any)) {
	if store == nil || preparer == nil {
		return
	}
	stateStore, ok := store.(StateStore)
	if !ok || stateStore == nil {
		return
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		if logf != nil {
			logf("test-slot recovery list projects failed: %v", err)
		}
		return
	}
	leases, err := stateStore.ListLeases(ctx)
	if err != nil {
		if logf != nil {
			logf("test-slot recovery list leases failed: %v", err)
		}
		return
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
	claimedSlots := map[string]map[int]bool{}
	claimedLeases := map[string]map[int]Lease{}
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
		projectKey := firstNonEmpty(project.Name, project.ID, lease.Project)
		if claimedSlots[projectKey] == nil {
			claimedSlots[projectKey] = map[int]bool{}
			claimedLeases[projectKey] = map[int]Lease{}
		}
		claimedSlots[projectKey][*slotIndex] = true
		claimedLeases[projectKey][*slotIndex] = lease

		// Re-arm the TTL expiry timer. The lease was claimed by a previous
		// glimmung process whose in-memory timer died with it; the durable
		// `assigned_at + ttl_seconds` lets us reconstruct the deadline.
		armLeaseExpiryTimer(store, preparer, project, lease, logf)

		// Resume in-flight per-slot work that the previous process started
		// but did not finish. testSlotActivations / testSlotCleanups dedup
		// per-lease so a fresh handler request that races with this is safe.
		status, hasStatus := testEnvironmentSlotStatus(project, *slotIndex)
		if !hasStatus {
			continue
		}
		switch status.State {
		case testSlotStateActivating:
			beginTestSlotActivation(store, preparer, minter, project, lease, logf)
		case testSlotStateCleaning:
			beginTestSlotCleanup(store, preparer, project, lease, true, logf)
		case testSlotStateActive:
			// Installer cleanup is a one-shot at end of activation; on
			// startup, drive it once defensively for slots that reached
			// active before the previous process exited.
			cleanupTestSlotInstaller(ctx, preparer, lease, project, logf)
		}
	}

	for _, project := range projects {
		projectName := firstNonEmpty(project.Name, project.ID)
		if projectName == "" {
			continue
		}
		statuses := testEnvironmentSlotStatuses(project)

		// Resume `cleaning` for slots whose lease is already gone — that's
		// the "cleanup started, lease released, but cleanup goroutine died"
		// recovery case.
		for slotIndex, status := range statuses {
			if status.State != testSlotStateCleaning {
				continue
			}
			if claimedSlots[projectName][slotIndex] {
				continue
			}
			slotName := status.SlotName
			if strings.TrimSpace(slotName) == "" {
				slotName = testEnvironmentName(projectName, slotIndex, project, Lease{})
			}
			lease := testEnvironmentWarmupLease(project, slotIndex, slotName)
			beginTestSlotCleanup(store, preparer, project, lease, false, logf)
		}

		// Warm missing or stale-`warming` slots. Same per-slot dedup as the
		// PATCH-count handler uses, so racing startup-vs-PATCH is safe.
		EnsureProjectTestSlotsWarmed(ctx, store, preparer, project, claimedSlots[projectName], logf)
	}
}

// EnsureProjectTestSlotsWarmed walks `1..count` for the project and fires a
// warm goroutine for every slot index whose `slots[*]` entry is missing or
// stale `warming`. Called from two triggers: PATCH /test-environments/count
// (immediately after the count is written) and the startup recovery sweep.
// Both paths dedup via `testSlotWarmups`, so the same trigger firing twice or
// the two triggers racing each other are both safe.
func EnsureProjectTestSlotsWarmed(_ context.Context, store ReadStore, preparer TestSlotPreparer, project Project, claimed map[int]bool, logf func(string, ...any)) {
	if preparer == nil {
		return
	}
	slotCount := projectTestSlotCount(Settings{}, project)
	if slotCount <= 0 {
		return
	}
	statuses := testEnvironmentSlotStatuses(project)
	for slotIndex := 1; slotIndex <= slotCount; slotIndex++ {
		if claimed[slotIndex] {
			// A claimed lease drives its own lifecycle (activation /
			// cleaning / active). Don't re-warm under it.
			continue
		}
		status, hasStatus := statuses[slotIndex]
		switch {
		case !hasStatus:
			// missing entirely → seed and warm
		case status.State == testSlotStateWarming:
			// in-flight warming whose goroutine may have died; the
			// per-slot dedup map decides whether to actually re-fire
		default:
			continue
		}
		beginTestSlotWarmup(store, preparer, project, slotIndex, logf)
	}
}

// testSlotWarmups serializes warmup work per slot so concurrent reconciler
// ticks don't double-fire EnsureTestSlotPreliminaries against the same slot.
var testSlotWarmups sync.Map

func beginTestSlotWarmup(store ReadStore, preparer TestSlotPreparer, project Project, slotIndex int, logf func(string, ...any)) bool {
	if preparer == nil {
		return false
	}
	projectKey := firstNonEmpty(project.Name, project.ID)
	if projectKey == "" {
		return false
	}
	slotName := testEnvironmentName(projectKey, slotIndex, project, Lease{})
	if strings.TrimSpace(slotName) == "" {
		return false
	}
	key := projectKey + ":warm:" + slotName
	if _, loaded := testSlotWarmups.LoadOrStore(key, struct{}{}); loaded {
		return false
	}
	go func() {
		defer testSlotWarmups.Delete(key)
		warmTestSlot(context.Background(), store, preparer, project, slotIndex, slotName, logf)
	}()
	return true
}

func warmTestSlot(ctx context.Context, store ReadStore, preparer TestSlotPreparer, project Project, slotIndex int, slotName string, logf func(string, ...any)) {
	writer, ok := store.(ProjectTestEnvironmentSlotStatusWriter)
	if !ok || writer == nil {
		if logf != nil {
			logf("test-slot warmup skipped: status writer not configured project=%s slot=%s", firstNonEmpty(project.Name, project.ID), slotName)
		}
		return
	}
	projectKey := firstNonEmpty(project.Name, project.ID)
	now := time.Now().UTC()
	updated, err := writer.SetProjectTestEnvironmentSlotStatus(ctx, projectKey, TestEnvironmentSlotStatus{
		SlotIndex: slotIndex,
		SlotName:  slotName,
		State:     testSlotStateWarming,
		UpdatedAt: now,
	})
	if err != nil {
		if logf != nil {
			logf("test-slot warmup record start failed project=%s slot=%s: %v", projectKey, slotName, err)
		}
		return
	}
	current := updated
	lease := testEnvironmentWarmupLease(current, slotIndex, slotName)
	if err := preparer.EnsureTestSlotPreliminaries(ctx, lease, current); err != nil {
		detail := err.Error()
		if _, writeErr := writer.SetProjectTestEnvironmentSlotStatus(ctx, projectKey, TestEnvironmentSlotStatus{
			SlotIndex: slotIndex,
			SlotName:  slotName,
			State:     "error",
			UpdatedAt: time.Now().UTC(),
			Detail:    &detail,
		}); writeErr != nil && logf != nil {
			logf("test-slot warmup record error failed project=%s slot=%s: %v", projectKey, slotName, writeErr)
		}
		if logf != nil {
			logf("test-slot warmup failed project=%s slot=%s: %v", projectKey, slotName, err)
		}
		return
	}
	cleanupTestSlotInstaller(ctx, preparer, lease, current, logf)
	readyAt := time.Now().UTC()
	if _, err := writer.SetProjectTestEnvironmentSlotStatus(ctx, projectKey, TestEnvironmentSlotStatus{
		SlotIndex: slotIndex,
		SlotName:  slotName,
		State:     testSlotStateReady,
		UpdatedAt: readyAt,
		ReadyAt:   &readyAt,
	}); err != nil && logf != nil {
		logf("test-slot warmup record ready failed project=%s slot=%s: %v", projectKey, slotName, err)
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

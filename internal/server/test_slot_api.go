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

	"github.com/nelsong6/glimmung/internal/metrics"
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

// testSlotActivation tracks one in-flight activation goroutine. Stored in
// testSlotActivations keyed by testSlotActivationKey(lease). Cleanup paths
// (return, callback release, TTL expiry) cancel the activation and wait on
// done before issuing K8s deletes, so the activation goroutine has fully
// unwound by the time cleanup starts deleting resources it might recreate.
//
// The activation goroutine closes done from a defer; cancel is idempotent
// and is also called from the same defer chain. Callers that miss the
// goroutine (token already gone from the map) treat that as "no activation
// to cancel" — the goroutine already exited.
type testSlotActivation struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// testSlotActivations maps testSlotActivationKey(lease) → *testSlotActivation
// for every activation goroutine currently in flight on this pod. Cleanup
// paths look up the token, call cancel(), and wait on done before issuing
// K8s deletes — see cancelInflightActivation.
var testSlotActivations sync.Map
var testSlotCleanups sync.Map

// testSlotInflight tracks every long-running goroutine spawned by the
// begin* functions (warmup, activation, cleanup). Graceful shutdown in
// cmd/glimmung-go/main.go waits on this group after the HTTP server has
// drained, so in-flight Helm operations get a chance to finish on
// SIGTERM before the pod exits. Without this, a pod evicted mid-Helm-
// install would leave a partial release that the next pod's recovery
// sweep has to clean up.
var testSlotInflight sync.WaitGroup

// WaitForInflightTestSlots blocks until every background test-slot
// goroutine (warmup, activation, cleanup) has finished, or until ctx is
// done. Called from main.go's shutdown sequence with a context whose
// deadline matches the Pod's terminationGracePeriodSeconds.
//
// Returns ctx.Err() if the deadline lands before goroutines drain (some
// Helm operation took longer than the budget). The caller's only
// reasonable response is to log it and exit — the next pod's recovery
// sweep will pick up the orphan state.
func WaitForInflightTestSlots(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		testSlotInflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

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
		lease, err := acquireLeaseInstrumented(r.Context(), LeasePurposeTestSlotCheckout, LeaseAcquireRequest{
			Project:    req.Project,
			Workflow:   &workflow,
			Metadata:   metadata,
			Requester:  requester,
			TTLSeconds: ttlSeconds,
		}, leaseStore.AcquireLease)
		if err != nil {
			var validationErr ValidationError
			if errors.As(err, &validationErr) {
				writeProblem(w, http.StatusBadRequest, validationErr.Message)
				return
			}
			if errors.Is(err, ErrUnavailable) {
				// Operational saturation, not a bug: every native slot
				// for this project is already claimed. writeUnavailable
				// emits slog.Warn + glimmung_unavailable_total{reason=
				// test_slot_saturation} so the event lands on a
				// dashboard rather than being a silent 503.
				writeUnavailable(w, r, "no ready test environment slots available", "test_slot_saturation")
				return
			}
			writeInternalError(w, r, err, "test-slot checkout failed")
			return
		}
		if preparer != nil && lease.State == "claimed" && boolFromMap(lease.Metadata, "test_slot_checkout") {
			if _, err := setLeaseSlotActivationStarting(r.Context(), store, project, lease); err != nil {
				_, _ = leaseStore.CancelLeaseByRef(r.Context(), req.Project, LeasePublicRefFromLease(lease))
				writeInternalError(w, r, err, "record test-slot activation state failed")
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
		audit := testSlotReturnAudit{
			Source:          stringPointerOrDefault(req.Source, "api.test_slots.return"),
			Reason:          trimmedOptionalString(req.Reason),
			CallerPodIP:     trimmedOptionalString(req.CallerPodIP),
			CallerSessionID: trimmedOptionalString(req.CallerSessionID),
			CleanupStarted:  cleanupStarted,
		}
		historyEntry := testSlotReturnHistoryEntry(lease, audit)
		if preparer != nil && cleanupStarted {
			project, ok := findProjectForTestSlot(r, w, store, req.Project)
			if !ok {
				return
			}
			// Route through claimTestSlotCleanup so every cleanup-entry
			// path uses the same etag-CAS as the TTL-timer path. Multi-
			// replica safety relies on this: simultaneous return + timer
			// + callback-release calls all race the same CAS and exactly
			// one wins; the rest get ErrPreconditionFailed (the slot is
			// already in `cleaning`) and respond with 202 too — same
			// observable outcome from the caller's perspective.
			//
			// error→cleaning is a valid prior transition (recovery
			// retry); see slot.go validSlotTransitions[SlotStateError].
			if _, err := claimTestSlotCleanup(r.Context(), store, project, lease, audit); err != nil {
				if errors.Is(err, ErrPreconditionFailed) {
					metrics.RecordTestSlotCleanupClaim(activationCancelReturn, metrics.CleanupClaimOutcomeLostRace)
					// Another replica/timer already started cleanup. The
					// slot is in `cleaning` durably, so respond with the
					// same 202 the granted-claim path returns. The caller
					// polls /v1/state for completion either way.
					writeJSON(w, http.StatusAccepted, testSlotReturnResponse(project, req.Project, lease, testSlotStateCleaning, true))
					return
				}
				metrics.RecordTestSlotCleanupClaim(activationCancelReturn, metrics.CleanupClaimOutcomeError)
				writeInternalError(w, r, err, "claim test-slot cleanup failed")
				return
			}
			metrics.RecordTestSlotCleanupClaim(activationCancelReturn, metrics.CleanupClaimOutcomeGranted)
			beginTestSlotCleanup(store, preparer, project, lease, true, activationCancelReturn, nil)
			writeJSON(w, http.StatusAccepted, testSlotReturnResponse(project, req.Project, lease, testSlotStateCleaning, true))
			return
		}
		result, err := leaseStore.CancelLeaseByRef(r.Context(), req.Project, LeasePublicRefFromLease(lease))
		if err != nil {
			writeInternalError(w, r, err, "test-slot return failed")
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

// markLeaseSlotStatus is a fire-and-forget wrapper for the small number
// of call sites that just want to flip the slot's state without caring
// about errors (e.g., recording an `error` state when a goroutine bails
// mid-flight and the caller is already returning the user-facing error).
// Routes to the appropriate Mark* helper based on the requested state.
func markLeaseSlotStatus(ctx context.Context, store ReadStore, _ Project, lease Lease, state string, cause error) {
	now := time.Now().UTC()
	switch state {
	case testSlotStateActivating:
		_, _ = markLeaseSlotActivating(ctx, store, lease, testSlotInstallerJobName(lease), now)
	case testSlotStateActive:
		_, _ = markLeaseSlotRunning(ctx, store, lease, now)
	case testSlotStateCleaning:
		_, _ = markLeaseSlotCleaning(ctx, store, lease, now)
	case testSlotStateReady:
		_, _ = markLeaseSlotCleaned(ctx, store, lease, now)
	case "error":
		c := cause
		if c == nil {
			c = errors.New("test-slot transition to error without cause")
		}
		_, _ = markLeaseSlotError(ctx, store, lease, now, c)
	}
}

type testSlotStatusMutation func(*TestEnvironmentSlotStatus, time.Time)

type testSlotReturnAudit struct {
	Source          string
	Reason          *string
	CallerPodIP     *string
	CallerSessionID *string
	CleanupStarted  bool
}

func setLeaseSlotActivationStarting(ctx context.Context, store ReadStore, _ Project, lease Lease) (Project, error) {
	jobName := testSlotInstallerJobName(lease)
	if _, err := markLeaseSlotActivating(ctx, store, lease, jobName, time.Now().UTC()); err != nil {
		return Project{}, err
	}
	return Project{}, nil
}

func setLeaseSlotActivationFinished(ctx context.Context, store ReadStore, _ Project, lease Lease, state string, cause error) (Project, error) {
	now := time.Now().UTC()
	switch state {
	case testSlotStateActive:
		if _, err := markLeaseSlotRunning(ctx, store, lease, now); err != nil {
			return Project{}, err
		}
	case "error":
		errMsg := cause
		if errMsg == nil {
			errMsg = errors.New("activation failed without cause")
		}
		if _, err := markLeaseSlotError(ctx, store, lease, now, errMsg); err != nil {
			return Project{}, err
		}
	default:
		return Project{}, fmt.Errorf("setLeaseSlotActivationFinished: unsupported state %q", state)
	}
	return Project{}, nil
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

func setLeaseSlotCleanupStarting(ctx context.Context, store ReadStore, _ Project, lease Lease, historyEntries ...TestSlotReturnHistoryEntry) (Project, error) {
	now := time.Now().UTC()
	if _, err := markLeaseSlotCleaning(ctx, store, lease, now); err != nil {
		return Project{}, err
	}
	for _, entry := range historyEntries {
		if entry.Event == "" {
			continue
		}
		if err := appendLeaseSlotHistory(ctx, store, slotHistoryEntryFromLegacy(entry)); err != nil {
			return Project{}, err
		}
	}
	return Project{}, nil
}

// slotHistoryEntryFromLegacy translates a legacy TestSlotReturnHistoryEntry
// into the new SlotHistoryEntry shape. The two structs are field-equivalent
// modulo the ID column; the ID is left blank so AppendSlotHistory assigns
// a uuid.
func slotHistoryEntryFromLegacy(entry TestSlotReturnHistoryEntry) SlotHistoryEntry {
	return SlotHistoryEntry{
		Event:           entry.Event,
		CreatedAt:       entry.CreatedAt,
		Project:         entry.Project,
		SlotIndex:       entry.SlotIndex,
		SlotName:        entry.SlotName,
		LeaseRef:        entry.LeaseRef,
		LeaseNumber:     entry.LeaseNumber,
		LeaseRequester:  entry.LeaseRequester,
		CallerPodIP:     entry.CallerPodIP,
		CallerSessionID: entry.CallerSessionID,
		Source:          entry.Source,
		Reason:          entry.Reason,
		CleanupStarted:  entry.CleanupStarted,
	}
}

func appendLeaseSlotReturnHistory(ctx context.Context, store ReadStore, _ Project, lease Lease, historyEntry TestSlotReturnHistoryEntry) (Project, error) {
	if historyEntry.Event == "" {
		return Project{}, nil
	}
	if err := appendLeaseSlotHistory(ctx, store, slotHistoryEntryFromLegacy(historyEntry)); err != nil {
		return Project{}, err
	}
	return Project{}, nil
}

func setLeaseSlotCleanupFinished(ctx context.Context, store ReadStore, _ Project, lease Lease, state string, cause error) (Project, error) {
	now := time.Now().UTC()
	switch state {
	case testSlotStateReady:
		if _, err := markLeaseSlotCleaned(ctx, store, lease, now); err != nil {
			return Project{}, err
		}
	case "error":
		errMsg := cause
		if errMsg == nil {
			errMsg = errors.New("cleanup failed without cause")
		}
		// Ensure the slot is in `cleaning` before transitioning to
		// error. The cleanup-pathway contract is that cleanup-finished
		// reports the outcome of a cleanup; the error therefore
		// belongs to the cleanup phase (CleanupError), not the
		// activation phase. Walking through cleaning makes MarkError
		// populate the correct phase-specific error field.
		if _, err := markLeaseSlotCleaning(ctx, store, lease, now); err != nil && !errors.Is(err, ErrInvalidSlotTransition) {
			return Project{}, err
		}
		if _, err := markLeaseSlotError(ctx, store, lease, now, errMsg); err != nil {
			return Project{}, err
		}
	default:
		return Project{}, fmt.Errorf("setLeaseSlotCleanupFinished: unsupported state %q", state)
	}
	return Project{}, nil
}

// setLeaseSlotStatus and currentLeaseSlotStatus were the central writer/
// reader against the legacy `project.metadata.native_standby_dns.slots[]`
// array. Stage 2 of the slot-storage rework replaced them with the
// markLeaseSlot*/SlotStore.UpdateIfMatch path. The functions stay deleted;
// any new code path that thinks it needs them should use the Mark* helpers
// directly so the slot row stays the single source of truth.

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
		writeInternalError(w, r, err, "list projects failed")
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
	ctx, cancel := context.WithCancel(context.Background())
	token := &testSlotActivation{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	if _, loaded := testSlotActivations.LoadOrStore(key, token); loaded {
		// Another goroutine already owns this key. Release the unused
		// cancel func so it isn't leaked, then bail.
		cancel()
		return false
	}
	testSlotInflight.Add(1)
	go func() {
		defer testSlotInflight.Done()
		defer testSlotActivations.Delete(key)
		defer cancel()
		defer close(token.done)
		activateTestSlotRuntime(ctx, store, preparer, minter, project, lease, logf)
	}()
	return true
}

// cancelInflightActivation cancels an in-flight activation goroutine for
// the given lease key and waits for it to unwind (or for waitCtx to expire).
// Returns true if there was an activation to cancel.
//
// Cleanup paths call this before issuing K8s deletes so the activation
// goroutine — which directly creates the slot-playwright Deployment and
// other lease-scoped runtime resources — is fully out of the way before
// cleanup starts removing those same resources. Without the await, the
// activation goroutine would race cleanup and recreate resources cleanup
// just deleted, causing waitForNoPodsInNamespaces to spin until its
// 5-minute timeout fires and the slot lands in `error`.
//
// `cause` is recorded on glimmung_test_slot_activation_cancelled_total so
// the cancel-from-cleanup path is observable on a dashboard. cause must be
// one of the activationCancel* constants below.
func cancelInflightActivation(waitCtx context.Context, key, cause string) bool {
	if key == "" {
		return false
	}
	raw, ok := testSlotActivations.Load(key)
	if !ok {
		return false
	}
	token, ok := raw.(*testSlotActivation)
	if !ok || token == nil {
		return false
	}
	token.cancel()
	metrics.RecordTestSlotActivationCancelled(cause)
	select {
	case <-token.done:
	case <-waitCtx.Done():
	}
	return true
}

// Activation-cancel cause labels. Closed enum so the
// glimmung_test_slot_activation_cancelled_total{cause} cardinality is bounded.
const (
	activationCancelReturn          = "return"
	activationCancelCallbackRelease = "callback_release"
	activationCancelTTLExpiry       = "ttl_expiry"
	activationCancelRecovery        = "recovery"
)

func activateTestSlotRuntime(parent context.Context, store ReadStore, preparer TestSlotPreparer, minter NativeGitHubTokenMinter, project Project, lease Lease, logf func(string, ...any)) {
	// Cross-replica dedup story for activation: there is no meaningful state
	// transition we could CAS on (the slot stays in `activating` throughout
	// the Helm install). We rely on three layers instead:
	//   1. Per-process dedup via testSlotActivations sync.Map — same pod
	//      never spawns two activation goroutines for the same lease.
	//   2. The recoveryMinAge gate in RecoverInFlightTestSlots — a fresh
	//      pod's startup sweep skips activating states whose updated_at is
	//      recent, assuming the previous pod is still alive and working.
	//   3. Helm itself returns a retryable "another operation in progress"
	//      error if two pods do happen to install the same release at the
	//      same instant — noisy but non-fatal; the winner's status update
	//      makes the slot converge.
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

func beginTestSlotCleanup(store ReadStore, preparer TestSlotPreparer, project Project, lease Lease, releaseLease bool, cause string, logf func(string, ...any)) bool {
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
	testSlotInflight.Add(1)
	go func() {
		defer testSlotInflight.Done()
		defer testSlotCleanups.Delete(key)
		// Cancel any in-flight activation goroutine for this lease and
		// wait for it to unwind BEFORE we issue any K8s deletes. The
		// activation goroutine directly creates the lease-scoped
		// Playwright Deployment (and waits on the installer Job that
		// helm-installs the project's runtime workloads); without this
		// await, cleanup races those creates and waitForNoPodsInNamespaces
		// times out against a slot whose live activation keeps recreating
		// the pods being deleted. See docs/test-slot-lifecycle.md.
		//
		// Bounded by activationCancelWait so a misbehaving activation
		// can't strand cleanup indefinitely; expiry is rare and the K8s
		// deletes below are idempotent against any tail-end resources
		// activation may still create after the wait expires.
		waitCtx, cancelWait := context.WithTimeout(context.Background(), activationCancelWait)
		cancelInflightActivation(waitCtx, key, cause)
		cancelWait()
		cleanupTestSlotRuntime(context.Background(), store, preparer, project, lease, releaseLease, logf)
	}()
	return true
}

// activationCancelWait bounds how long beginTestSlotCleanup waits for an
// in-flight activation goroutine to honor cancellation. activation unwind
// is dominated by the time it takes the in-flight K8s API call to return
// ctx.Err() (sub-second) plus a Cosmos read in the post-cancel state-checks
// (~100ms). 30s is generous; expiry is observable via
// glimmung_test_slot_activation_cancelled_total minus the cleanup-completed
// counter (a delta means an activation didn't unwind cleanly).
const activationCancelWait = 30 * time.Second

func cleanupTestSlotRuntime(parent context.Context, store ReadStore, preparer TestSlotPreparer, project Project, lease Lease, releaseLease bool, logf func(string, ...any)) {
	// Cross-replica dedup for cleanup: initiation paths (return, callback
	// release, TTL timer) already go through the etag-conditional
	// claimTestSlotCleanup, which does the meaningful `active → cleaning`
	// CAS. The resume path (RecoverInFlightTestSlots seeing `cleaning`)
	// uses per-process dedup + recoveryMinAge — the same three-layer story
	// as activation (see activateTestSlotRuntime). Helm uninstall is
	// idempotent in the "release not found" sense if two replicas do hit
	// it concurrently.
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
	return testSlotLeaseStillState(ctx, store, project, lease, SlotStateActivating)
}

func testSlotLeaseStillState(ctx context.Context, store ReadStore, _ Project, lease Lease, state string) bool {
	slotIndex := nativeSlotIndexFromMetadata(lease.Metadata)
	if slotIndex == nil {
		return false
	}
	slotStore := slotStoreFromReadStore(store)
	if slotStore == nil {
		return true
	}
	slot, err := slotStore.GetSlot(ctx, lease.Project, *slotIndex)
	if err != nil {
		// Slot doc missing or other store error: be defensive and let the
		// caller proceed (the only consequence is a redundant transition
		// attempt that the state machine will catch).
		return true
	}
	return slot.State == state
}

// recoveredSlotStatus reads the slot's current state for the recovery
// sweep. Prefers the SlotStore collection (the durable source post-#518);
// falls back to the legacy project-metadata reader so in-memory tests
// with fakes that only populate project metadata keep working.
//
// Returns (state, updated_at, cleanup_error, found). cleanup_error is
// surfaced separately because the error→cleaning recovery branch needs
// to distinguish "error from a failed cleanup" (retry-eligible) from
// "error from a failed activation whose inline cleanup did not record
// a separate cleanup_error" (skip; lease is typically gone by then).
func recoveredSlotStatus(ctx context.Context, store ReadStore, project Project, slotIndex int) (string, time.Time, *string, bool) {
	if slotStore := slotStoreFromReadStore(store); slotStore != nil {
		projectKey := firstNonEmpty(project.Name, project.ID)
		if projectKey != "" {
			if slot, err := slotStore.GetSlot(ctx, projectKey, slotIndex); err == nil {
				return slot.State, slot.UpdatedAt, slot.CleanupError, true
			}
		}
	}
	status, ok := testEnvironmentSlotStatus(project, slotIndex)
	if !ok {
		return "", time.Time{}, nil, false
	}
	// The legacy embedded array predates the cleanup_error field; tests
	// that drive recovery via legacy fakes won't exercise the
	// error→cleaning retry path, which is correct (error recovery is a
	// new behavior tied to the new shape).
	return status.State, status.UpdatedAt, nil, true
}

// recoveryMinAge is the lower bound on `updated_at` for an in-flight status
// (warming/activating/cleaning) before the recovery sweep considers it
// "the previous pod is dead, I should resume." Recent in-flight states are
// almost certainly still being worked by another live pod (the rolling-
// update overlap case), so re-firing the goroutine would race the live
// pod's Helm operation. Five minutes covers typical Helm install latency
// with headroom; graceful shutdown's in-flight wait keeps the orphan-
// state rate low so the 5-minute recovery delay is a rare path.
const recoveryMinAge = 5 * time.Minute

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
	// Idempotent slot-storage migration: copies any project's legacy
	// `metadata.native_standby_dns.slots[]` array into the `slots`
	// collection. Production startup runs this before recovery via
	// cmd/glimmung-go/main.go; calling it again here is a no-op when the
	// legacy arrays are already stripped, but it keeps tests and any
	// uncommon boot order working without extra setup.
	if _, err := MigrateProjectSlotsIntoCollection(ctx, store); err != nil {
		if logf != nil {
			logf("test-slot recovery slot-storage migration failed: %v", err)
		}
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
		//
		// Multi-replica safety: skip resume if the in-flight state was
		// updated recently. During a rolling-update overlap the other pod
		// is almost certainly still doing the work — re-firing would race
		// the live Helm operation. The etag CAS inside each operation's
		// goroutine is the second line of defense for the simultaneous-
		// start case.
		//
		// Slot status is read from the new SlotStore collection where the
		// store implements it; the legacy testEnvironmentSlotStatus reader
		// only sees post-#518-migration project metadata (which is empty)
		// so it would silently skip every slot otherwise. The fallback
		// keeps in-memory single-replica tests with legacy-only fakes
		// rendering until they migrate.
		slotState, updatedAt, slotCleanupError, hasStatus := recoveredSlotStatus(ctx, store, project, *slotIndex)
		if !hasStatus {
			continue
		}
		recent := !updatedAt.IsZero() && time.Since(updatedAt) < recoveryMinAge
		switch slotState {
		case SlotStateActivating:
			if recent {
				continue
			}
			beginTestSlotActivation(store, preparer, minter, project, lease, logf)
		case SlotStateCleaning:
			if recent {
				continue
			}
			beginTestSlotCleanup(store, preparer, project, lease, true, activationCancelRecovery, logf)
		case SlotStateError:
			// error→cleaning retry: a prior cleanup attempt left the slot
			// in error with cleanup_error set, and the lease is still
			// claimed (no one released it). Re-fire cleanup. K8s ops
			// underneath are idempotent so the retry either converges on
			// success or re-errors with new diagnostic context appended
			// to slot_history. activation_error-only slots (cleanup not
			// yet attempted) are skipped — the activation-error path
			// already runs cleanup inline before recording error, and
			// re-running activation against a slot whose lease may have
			// been canceled is not a safe automatic recovery.
			if slotCleanupError == nil {
				continue
			}
			if recent {
				continue
			}
			beginTestSlotCleanup(store, preparer, project, lease, true, activationCancelRecovery, logf)
		case SlotStateRunning, testSlotStateActive:
			// Installer cleanup is a one-shot at end of activation; on
			// startup, drive it once defensively for slots that reached
			// running/active before the previous process exited.
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
		// recovery case. Skip if recent (assume another live pod is
		// finishing it); CAS inside cleanupTestSlotRuntime is the defense
		// against the simultaneous-start case.
		for slotIndex, status := range statuses {
			if status.State != testSlotStateCleaning {
				continue
			}
			if claimedSlots[projectName][slotIndex] {
				continue
			}
			if !status.UpdatedAt.IsZero() && time.Since(status.UpdatedAt) < recoveryMinAge {
				continue
			}
			slotName := status.SlotName
			if strings.TrimSpace(slotName) == "" {
				slotName = testEnvironmentName(projectName, slotIndex, project, Lease{})
			}
			lease := testEnvironmentWarmupLease(project, slotIndex, slotName)
			beginTestSlotCleanup(store, preparer, project, lease, false, activationCancelRecovery, logf)
		}

		// Slot-collection sweep for `error` slots without a claimed lease.
		// These are slots whose prior cleanup failed AND whose lease release
		// then succeeded or was lost — there's no one driving recovery via
		// the public API. Re-fire cleanup with releaseLease=false; the
		// cleanup pathway will converge on `provisioned` if K8s state is
		// reachable, or re-error with new diagnostic context appended to
		// slot_history. The legacy testEnvironmentSlotStatuses loop above
		// only sees pre-#518-migration project metadata, so this is the
		// only path that recovers orphan-error slots after migration.
		if slotStore := slotStoreFromReadStore(store); slotStore != nil {
			if rows, err := slotStore.ListSlotsByProject(ctx, projectName); err == nil {
				for _, slot := range rows {
					if slot.State != SlotStateError {
						continue
					}
					if claimedSlots[projectName][slot.SlotIndex] {
						// A claimed lease drives recovery through the
						// claimed-leases loop above.
						continue
					}
					if slot.CleanupError == nil {
						// activation_error only; cleanup wasn't attempted
						// (or its outcome wasn't recorded). Re-running
						// cleanup against a slot that may never have had
						// a partial helm install is a no-op; skip to
						// avoid the spurious K8s 404 storm.
						continue
					}
					if !slot.UpdatedAt.IsZero() && time.Since(slot.UpdatedAt) < recoveryMinAge {
						continue
					}
					slotName := slot.SlotName
					if strings.TrimSpace(slotName) == "" {
						slotName = testEnvironmentName(projectName, slot.SlotIndex, project, Lease{})
					}
					lease := testEnvironmentWarmupLease(project, slot.SlotIndex, slotName)
					beginTestSlotCleanup(store, preparer, project, lease, false, activationCancelRecovery, logf)
				}
			} else if logf != nil {
				logf("test-slot recovery slot collection list failed project=%s: %v", projectName, err)
			}
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
			// in-flight warming. Skip if recent — another live pod is
			// probably mid-warmup (rolling-update overlap case). The
			// etag CAS in warmTestSlot handles the simultaneous-start
			// case.
			if !status.UpdatedAt.IsZero() && time.Since(status.UpdatedAt) < recoveryMinAge {
				continue
			}
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
	testSlotInflight.Add(1)
	go func() {
		defer testSlotInflight.Done()
		defer testSlotWarmups.Delete(key)
		warmTestSlot(context.Background(), store, preparer, project, slotIndex, slotName, logf)
	}()
	return true
}

func warmTestSlot(ctx context.Context, store ReadStore, preparer TestSlotPreparer, project Project, slotIndex int, slotName string, logf func(string, ...any)) {
	projectKey := firstNonEmpty(project.Name, project.ID)
	if projectKey == "" {
		return
	}
	now := time.Now().UTC()

	// Ensure the slot doc exists in the `slots` collection. PATCH-count
	// and boot recovery normally seed it; this call is a defensive
	// idempotent guarantee for any path that fires warmup without going
	// through those seeders (in particular: tests).
	if _, err := ensureSlotExists(ctx, store, projectKey, slotIndex, slotName, now); err != nil {
		if logf != nil {
			logf("test-slot warmup ensure-slot-exists failed project=%s slot=%s: %v", projectKey, slotName, err)
		}
		return
	}

	// Transition unseeded -> provisioning via per-slot CAS. Two racing
	// goroutines (recovery sweep + PATCH-count handler, two replicas
	// during a rollout) take this transition together; exactly one's
	// write commits, the other gets ErrPreconditionFailed and exits.
	// No cross-slot contention exists — each slot is its own doc.
	if _, err := markSlotProvisioning(ctx, store, projectKey, slotIndex, now); err != nil {
		switch {
		case errors.Is(err, ErrPreconditionFailed):
			// Another goroutine won the claim; ours is a no-op.
			return
		case errors.Is(err, ErrInvalidSlotTransition):
			// Slot is already past the unseeded->provisioning gate
			// (provisioned, activating, running, cleaning, or error).
			// The recovery sweep should already be skipping these,
			// but tolerate the race silently.
			return
		default:
			if logf != nil {
				logf("test-slot warmup mark-provisioning failed project=%s slot=%s: %v", projectKey, slotName, err)
			}
			return
		}
	}

	lease := testEnvironmentWarmupLease(project, slotIndex, slotName)
	if err := preparer.EnsureTestSlotPreliminaries(ctx, lease, project); err != nil {
		if _, writeErr := markSlotError(ctx, store, projectKey, slotIndex, time.Now().UTC(), err); writeErr != nil && logf != nil {
			logf("test-slot warmup record error failed project=%s slot=%s: %v", projectKey, slotName, writeErr)
		}
		if logf != nil {
			logf("test-slot warmup failed project=%s slot=%s: %v", projectKey, slotName, err)
		}
		return
	}
	cleanupTestSlotInstaller(ctx, preparer, lease, project, logf)
	if _, err := markSlotProvisioned(ctx, store, projectKey, slotIndex, time.Now().UTC()); err != nil && logf != nil {
		logf("test-slot warmup mark-provisioned failed project=%s slot=%s: %v", projectKey, slotName, err)
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

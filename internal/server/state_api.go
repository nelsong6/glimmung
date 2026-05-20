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

	"github.com/nelsong6/glimmung/internal/domain/publicids"
)

type StateStore interface {
	ReadStore
	ListLeases(ctx context.Context) ([]Lease, error)
	// AnyLockHeld reports whether any lock with the given scope is
	// currently held. Feeds StateSnapshot.InflightLocks so the SPA can
	// derive its in-flight pulse from the SSE stream instead of
	// polling /v1/issues + /v1/touchpoints. scope is the lock kind
	// ("issue" or "pr") and matches the partition key of the locks
	// container.
	AnyLockHeld(ctx context.Context, scope string) (bool, error)
}

type StateSnapshot struct {
	ActiveLeases            []LeasePublic           `json:"active_leases"`
	TestEnvironments        []TestEnvironmentPublic `json:"test_environments"`
	WaitingTestSlotRequests []TestSlotRequestPublic `json:"waiting_test_slot_requests"`
	TestLeaseDefaults       TestLeaseDefaults       `json:"test_lease_defaults"`
	Projects                []Project               `json:"projects"`
	Workflows               []Workflow              `json:"workflows"`
	// InflightLocks summarizes whether any issue-scoped or pr-scoped
	// lock is currently held. The SPA's "needs attention" nav uses
	// this as a derived state on top of the SSE snapshot; before this
	// field existed it polled /v1/issues + /v1/touchpoints every 20s
	// only to compute the same boolean.
	InflightLocks InflightLocksSummary `json:"inflight_locks"`
}

// InflightLocksSummary carries the cheapest summary the SPA needs to
// drive its in-flight indicator. Two single-partition Cosmos queries
// populate it on every snapshot tick (one per scope), so the cost is
// bounded.
type InflightLocksSummary struct {
	Issues bool `json:"issues"`
	PRs    bool `json:"prs"`
}

type Lease struct {
	ID                 string         `json:"-"`
	Kind               string         `json:"-"`
	LeaseNumber        *int           `json:"lease_number"`
	Project            string         `json:"project"`
	Workflow           *string        `json:"workflow"`
	Host               *string        `json:"host"`
	State              string         `json:"state"`
	Requirements       map[string]any `json:"requirements"`
	Metadata           map[string]any `json:"metadata"`
	RequestedAt        time.Time      `json:"requested_at"`
	AssignedAt         *time.Time     `json:"assigned_at"`
	ReleasedAt         *time.Time     `json:"released_at"`
	TTLSeconds         int            `json:"ttl_seconds"`
	RequestedSlotIndex *int           `json:"-"`
	FulfilledAt        *time.Time     `json:"-"`
	FulfilledLeaseRef  *string        `json:"-"`
}

type LeasePublic struct {
	Ref                  string          `json:"ref"`
	LeaseNumber          *int            `json:"lease_number"`
	Project              string          `json:"project"`
	Workflow             *string         `json:"workflow"`
	Host                 *string         `json:"host"`
	State                string          `json:"state"`
	Requirements         map[string]any  `json:"requirements"`
	Metadata             map[string]any  `json:"metadata"`
	Requester            *LeaseRequester `json:"requester"`
	RequestedAt          time.Time       `json:"requested_at"`
	AssignedAt           *time.Time      `json:"assigned_at"`
	ReleasedAt           *time.Time      `json:"released_at"`
	TTLSeconds           int             `json:"ttl_seconds"`
	PlaywrightWSEndpoint *string         `json:"playwright_ws_endpoint,omitempty"`
}

type LeaseRequester struct {
	Consumer string            `json:"consumer"`
	Kind     string            `json:"kind"`
	Ref      string            `json:"ref"`
	Label    *string           `json:"label"`
	URL      *string           `json:"url"`
	Metadata map[string]string `json:"metadata"`
}

type TestSlotRequestPublic struct {
	Ref                string          `json:"ref"`
	Project            string          `json:"project"`
	Workflow           string          `json:"workflow"`
	State              string          `json:"state"`
	RequestedSlotIndex *int            `json:"requested_slot_index"`
	Requester          *LeaseRequester `json:"requester"`
	Metadata           map[string]any  `json:"metadata"`
	RequestedAt        time.Time       `json:"requested_at"`
	FulfilledAt        *time.Time      `json:"fulfilled_at"`
	FulfilledLeaseRef  *string         `json:"fulfilled_lease_ref"`
	TTLSeconds         int             `json:"ttl_seconds"`
}

type TestEnvironmentPublic struct {
	Project               string                       `json:"project"`
	SlotIndex             int                          `json:"slot_index"`
	SlotName              string                       `json:"slot_name"`
	State                 string                       `json:"state"`
	Usable                bool                         `json:"usable"`
	Detail                *string                      `json:"detail,omitempty"`
	UpdatedAt             *time.Time                   `json:"updated_at,omitempty"`
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
	Lease                 *LeasePublic                 `json:"lease"`
	WaitingRequests       []TestSlotRequestPublic      `json:"waiting_requests"`
	PlaywrightWSEndpoint  *string                      `json:"playwright_ws_endpoint,omitempty"`
}

func stateSnapshot(settings Settings, store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := loadStateSnapshot(r.Context(), settings, store)
		if err != nil {
			writeStateSnapshotError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	}
}

func testEnvironmentStatus(settings Settings, store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := strings.TrimSpace(r.PathValue("project"))
		slotName := strings.TrimSpace(r.PathValue("slot_name"))
		if project == "" || slotName == "" {
			writeProblem(w, http.StatusBadRequest, "project and slot_name required")
			return
		}
		snapshot, err := loadStateSnapshot(r.Context(), settings, store)
		if err != nil {
			writeStateSnapshotError(w, r, err)
			return
		}
		projectName := project
		for _, candidate := range snapshot.Projects {
			if candidate.Name == project || candidate.ID == project {
				projectName = firstNonEmpty(candidate.Name, candidate.ID, project)
				break
			}
		}
		for _, env := range snapshot.TestEnvironments {
			if env.Project == projectName && env.SlotName == slotName {
				writeJSON(w, http.StatusOK, env)
				return
			}
		}
		writeProblem(w, http.StatusNotFound, "test environment slot not found")
	}
}

func stateEvents(settings Settings, store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeInternalError(w, r, errors.New("http.ResponseWriter does not implement http.Flusher"), "streaming unsupported")
			return
		}
		snapshot, err := loadStateSnapshot(r.Context(), settings, store)
		if err != nil {
			writeStateSnapshotError(w, r, err)
			return
		}

		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		w.Header().Set("connection", "keep-alive")

		for {
			payload, err := json.Marshal(snapshot)
			if err != nil {
				writeInternalError(w, r, err, "encode state snapshot failed")
				return
			}
			_, _ = fmt.Fprintf(w, "event: state\ndata: %s\n\n", payload)
			flusher.Flush()

			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Second):
			}
			snapshot, err = loadStateSnapshot(r.Context(), settings, store)
			if err != nil {
				return
			}
		}
	}
}

type stateSnapshotError struct {
	status  int
	message string
}

func (e stateSnapshotError) Error() string {
	return e.message
}

func loadStateSnapshot(ctx context.Context, settings Settings, store ReadStore) (StateSnapshot, error) {
	stateStore, ok := store.(StateStore)
	if !ok || stateStore == nil {
		return StateSnapshot{}, stateSnapshotError{status: http.StatusServiceUnavailable, message: "state store not configured"}
	}

	projects, err := stateStore.ListProjects(ctx)
	if err != nil {
		return StateSnapshot{}, stateSnapshotError{status: http.StatusInternalServerError, message: "list projects failed"}
	}
	workflows, err := stateStore.ListWorkflows(ctx)
	if err != nil {
		return StateSnapshot{}, stateSnapshotError{status: http.StatusInternalServerError, message: "list workflows failed"}
	}
	leases, err := stateStore.ListLeases(ctx)
	if err != nil {
		return StateSnapshot{}, stateSnapshotError{status: http.StatusInternalServerError, message: "list leases failed"}
	}
	// Inflight-lock summary feeds the SPA's nav pulse. A lookup error
	// here is non-fatal — the SSE stream still delivers the rest of the
	// snapshot — but it is observable via the per-query Cosmos metrics
	// shipped in the observability stage. Treat an error as "no locks
	// held" rather than failing the whole snapshot so a transient
	// Cosmos hiccup does not blank the dashboard.
	inflight := InflightLocksSummary{}
	if held, lerr := stateStore.AnyLockHeld(ctx, "issue"); lerr == nil {
		inflight.Issues = held
	}
	if held, lerr := stateStore.AnyLockHeld(ctx, "pr"); lerr == nil {
		inflight.PRs = held
	}

	snapshot := computeStateSnapshot(ctx, settings, store, projects, workflows, leases)
	snapshot.InflightLocks = inflight
	return snapshot, nil
}

func writeStateSnapshotError(w http.ResponseWriter, r *http.Request, err error) {
	if snapshotErr, ok := err.(stateSnapshotError); ok {
		writeProblem(w, snapshotErr.status, snapshotErr.message)
		return
	}
	writeInternalError(w, r, err, "load state snapshot failed")
}

func computeStateSnapshot(
	ctx context.Context,
	settings Settings,
	store ReadStore,
	projects []Project,
	workflows []Workflow,
	leases []Lease,
) StateSnapshot {
	active := make([]Lease, 0)
	waiting := make([]TestSlotRequestPublic, 0)
	for _, lease := range leases {
		switch {
		case lease.Kind == "test_slot_request" && lease.State == "waiting":
			waiting = append(waiting, testRequestToPublic(lease))
		case lease.State == "claimed":
			active = append(active, lease)
		}
	}

	activePublic := make([]LeasePublic, 0, len(active))
	for _, lease := range active {
		activePublic = append(activePublic, leaseToPublicForState(settings, lease))
	}

	return StateSnapshot{
		ActiveLeases:            activePublic,
		TestEnvironments:        testEnvironmentsFromSnapshot(ctx, settings, store, projects, active, waiting),
		WaitingTestSlotRequests: waiting,
		TestLeaseDefaults:       readTestLeaseDefaultsOrFallback(ctx, store),
		Projects:                sliceOrEmpty(projects),
		Workflows:               sliceOrEmpty(workflows),
	}
}

func leaseToPublic(lease Lease) LeasePublic {
	return LeasePublic{
		Ref:          leasePublicRef(lease),
		LeaseNumber:  lease.LeaseNumber,
		Project:      lease.Project,
		Workflow:     lease.Workflow,
		Host:         lease.Host,
		State:        lease.State,
		Requirements: mapOrEmpty(lease.Requirements),
		Metadata:     mapOrEmpty(lease.Metadata),
		Requester:    requesterFromMetadata(lease.Metadata),
		RequestedAt:  lease.RequestedAt,
		AssignedAt:   lease.AssignedAt,
		ReleasedAt:   lease.ReleasedAt,
		TTLSeconds:   defaultTTLSeconds(lease.TTLSeconds),
	}
}

// leaseToPublicForState wraps leaseToPublic for the state snapshot, attaching
// the slot-playwright WebSocket endpoint when the lease holds a slot and the
// cluster is running playwright-enabled slots. mcp-glimmung's
// `inspect_browser_url` reads this field to route browser inspection through
// the leased slot's Playwright pod.
func leaseToPublicForState(settings Settings, lease Lease) LeasePublic {
	public := leaseToPublic(lease)
	if slotName, ok := stringFromMap(lease.Metadata, "native_slot_name"); ok {
		public.PlaywrightWSEndpoint = PlaywrightWSEndpointFor(settings, slotName)
	}
	return public
}

// LeasePublicRefFromLease is the exported wrapper used by the cosmos store.
func LeasePublicRefFromLease(lease Lease) string {
	return leasePublicRef(lease)
}

func leasePublicRef(lease Lease) string {
	slotName := ""
	if value, ok := stringFromMap(lease.Metadata, "native_slot_name"); ok {
		slotName = strings.TrimSpace(value)
	}
	return publicids.LeaseRef(lease.Project, slotName, lease.LeaseNumber)
}

func testRequestToPublic(lease Lease) TestSlotRequestPublic {
	workflow := "test-slot-checkout"
	if lease.Workflow != nil && *lease.Workflow != "" {
		workflow = *lease.Workflow
	}
	return TestSlotRequestPublic{
		Ref:                lease.Project + "/test-requests/" + lease.ID,
		Project:            lease.Project,
		Workflow:           workflow,
		State:              lease.State,
		RequestedSlotIndex: lease.RequestedSlotIndex,
		Requester:          requesterFromMetadata(lease.Metadata),
		Metadata:           mapOrEmpty(lease.Metadata),
		RequestedAt:        lease.RequestedAt,
		FulfilledAt:        lease.FulfilledAt,
		FulfilledLeaseRef:  lease.FulfilledLeaseRef,
		TTLSeconds:         defaultTTLSeconds(lease.TTLSeconds),
	}
}

func testEnvironmentsFromSnapshot(
	ctx context.Context,
	settings Settings,
	store ReadStore,
	projects []Project,
	active []Lease,
	waiting []TestSlotRequestPublic,
) []TestEnvironmentPublic {
	projectsByName := map[string]Project{}
	projectNames := map[string]bool{}
	for _, project := range projects {
		name := firstNonEmpty(project.Name, project.ID)
		if name == "" {
			continue
		}
		projectsByName[name] = project
		projectNames[name] = true
	}

	claimedByProject := map[string]map[int]Lease{}
	for _, lease := range active {
		if !boolFromMap(lease.Metadata, "test_slot_checkout") && !boolFromMap(lease.Metadata, "native_k8s") {
			continue
		}
		slotIndex, ok := positiveIntFromMap(lease.Metadata, "native_slot_index")
		if !ok {
			continue
		}
		projectNames[lease.Project] = true
		if claimedByProject[lease.Project] == nil {
			claimedByProject[lease.Project] = map[int]Lease{}
		}
		claimedByProject[lease.Project][slotIndex] = lease
	}

	waitingByProject := map[string]map[int][]TestSlotRequestPublic{}
	for _, req := range waiting {
		projectNames[req.Project] = true
		if req.RequestedSlotIndex == nil {
			continue
		}
		if waitingByProject[req.Project] == nil {
			waitingByProject[req.Project] = map[int][]TestSlotRequestPublic{}
		}
		waitingByProject[req.Project][*req.RequestedSlotIndex] = append(
			waitingByProject[req.Project][*req.RequestedSlotIndex],
			req,
		)
	}

	names := make([]string, 0, len(projectNames))
	for name := range projectNames {
		names = append(names, name)
	}
	sort.Strings(names)

	slotStore := slotStoreFromReadStore(store)
	historyStore := slotHistoryStoreFromReadStore(store)

	envs := make([]TestEnvironmentPublic, 0)
	for _, projectName := range names {
		project := projectsByName[projectName]
		slotCount := projectTestSlotCount(settings, project)

		// Per-slot view of the durable slots collection. Production code
		// reads slot state exclusively from here; legacy fixtures in
		// tests are bridged into the collection by the fake's
		// ListSlotsByProject, so the rendering path doesn't need a
		// fallback to the embedded array.
		slotsByIndex := map[int]Slot{}
		historiesByIndex := map[int][]SlotHistoryEntry{}
		if slotStore != nil {
			if rows, err := slotStore.ListSlotsByProject(ctx, projectName); err == nil {
				for _, row := range rows {
					slotsByIndex[row.SlotIndex] = row
				}
			}
		}
		if historyStore != nil {
			if entries, err := historyStore.ListSlotHistory(ctx, projectName, nil); err == nil {
				for _, entry := range entries {
					if entry.SlotIndex == nil {
						continue
					}
					historiesByIndex[*entry.SlotIndex] = append(historiesByIndex[*entry.SlotIndex], entry)
				}
			}
		}

		for slotIndex := 1; slotIndex <= slotCount; slotIndex++ {
			lease, claimed := claimedByProject[projectName][slotIndex]
			var publicLease *LeasePublic
			slot, hasSlot := slotsByIndex[slotIndex]

			var (
				state                 string
				detail                *string
				updatedAt             *time.Time
				provisionedAt         *time.Time
				activationAttempt     *int
				activationStartedAt   *time.Time
				activationCompletedAt *time.Time
				activationJobName     *string
				activationError       *string
				activationState       *string
				cleanupStartedAt      *time.Time
				cleanupCompletedAt    *time.Time
				cleanupError          *string
				cleanupState          *string
			)
			if hasSlot {
				state = publicSlotState(slot)
				detail = slot.Detail
				if !slot.UpdatedAt.IsZero() {
					value := slot.UpdatedAt
					updatedAt = &value
				}
				provisionedAt = slot.ProvisionedAt
				activationAttempt = slot.ActivationAttempt
				activationStartedAt = slot.ActivationStartedAt
				activationCompletedAt = slot.ActivationCompletedAt
				activationJobName = slot.ActivationJobName
				activationError = slot.ActivationError
				activationState = derivedActivationState(slot)
				cleanupStartedAt = slot.CleanupStartedAt
				cleanupCompletedAt = slot.CleanupCompletedAt
				cleanupError = slot.CleanupError
				cleanupState = derivedCleanupState(slot)
			}

			usable := false
			if claimed {
				value := leaseToPublicForState(settings, lease)
				publicLease = &value
				if state == "available" || state == "" || state == SlotStateProvisioned {
					if boolFromMap(lease.Metadata, "test_slot_checkout") {
						state = "claimed"
					} else {
						state = "reserved"
					}
				}
				usable = state == SlotStateRunning || state == testSlotStateActive
			}

			slotName := testEnvironmentName(projectName, slotIndex, project, lease)
			env := TestEnvironmentPublic{
				Project:               projectName,
				SlotIndex:             slotIndex,
				SlotName:              slotName,
				State:                 state,
				Usable:                usable,
				Detail:                detail,
				UpdatedAt:             updatedAt,
				ReadyAt:               provisionedAt,
				ActivationAttempt:     activationAttempt,
				ActivationState:       activationState,
				ActivationStartedAt:   activationStartedAt,
				ActivationCompletedAt: activationCompletedAt,
				ActivationJobName:     activationJobName,
				ActivationError:       activationError,
				CleanupState:          cleanupState,
				CleanupStartedAt:      cleanupStartedAt,
				CleanupCompletedAt:    cleanupCompletedAt,
				CleanupError:          cleanupError,
				Lease:                 publicLease,
				WaitingRequests:       sliceOrEmpty(waitingByProject[projectName][slotIndex]),
				PlaywrightWSEndpoint:  PlaywrightWSEndpointFor(settings, slotName),
			}
			if entries := historiesByIndex[slotIndex]; len(entries) > 0 {
				env.ReturnHistory = legacyHistoryFromEntries(entries)
			}
			envs = append(envs, env)
		}
	}
	return envs
}

// publicSlotState maps the new internal Slot.State to the value emitted
// on /v1/state. The user-facing snapshot uses `available` as the derived
// term for "provisioned + no active_lease_ref", per the lifecycle
// contract.
func publicSlotState(slot Slot) string {
	if slot.State == SlotStateProvisioned && slot.ActiveLeaseRef == nil {
		return "available"
	}
	return slot.State
}

// derivedActivationState is the legacy `activation_state` field derived
// from the new Slot's activation-* fields. Preserved on the wire so
// existing consumers (dashboard, mcp tools) keep rendering.
func derivedActivationState(slot Slot) *string {
	switch {
	case slot.ActivationError != nil:
		state := "error"
		return &state
	case slot.ActivationCompletedAt != nil:
		state := "active"
		return &state
	case slot.ActivationStartedAt != nil:
		state := "activating"
		return &state
	}
	return nil
}

// derivedCleanupState mirrors derivedActivationState for the cleanup phase.
func derivedCleanupState(slot Slot) *string {
	if slot.CleanupCompletedAt != nil {
		switch slot.State {
		case SlotStateProvisioned:
			state := "ready"
			return &state
		case SlotStateError:
			state := "error"
			return &state
		}
	}
	if slot.CleanupStartedAt != nil {
		state := "cleaning"
		return &state
	}
	return nil
}

// legacyHistoryFromEntries converts new SlotHistoryEntry rows to the
// legacy TestSlotReturnHistoryEntry shape for wire compatibility.
func legacyHistoryFromEntries(entries []SlotHistoryEntry) []TestSlotReturnHistoryEntry {
	out := make([]TestSlotReturnHistoryEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, TestSlotReturnHistoryEntry{
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
		})
	}
	return out
}

func projectTestSlotCount(_ Settings, project Project) int {
	metadata := project.Metadata
	if standbyDNS, ok := mapFromMap(metadata, "native_standby_dns"); ok {
		if count, ok := positiveIntFromMap(standbyDNS, "count"); ok {
			return count
		}
	}
	return 0
}

// The legacy slot-status readers (testEnvironmentSlot* and the supporting
// pointer/history helpers) that used to walk
// project.metadata.native_standby_dns.slots have been removed. The
// slot-storage rework (PR #518) split slot state into its own Cosmos
// collection and the boot migration strips the embedded array.
// Production reads go through SlotStore.GetSlot / ListSlotsByProject;
// legacy test fixtures are bridged into the new collection by the
// fake's ListSlotsByProject.

func testEnvironmentName(project string, slotIndex int, projectDoc Project, _ Lease) string {
	prefix := firstNonEmpty(projectDoc.Name, projectDoc.ID, project)
	if standbyDNS, ok := mapFromMap(projectDoc.Metadata, "native_standby_dns"); ok {
		if value, ok := stringFromMap(standbyDNS, "slot_prefix"); ok && strings.TrimSpace(value) != "" {
			prefix = strings.Trim(strings.TrimSpace(value), ".")
		}
		if value, ok := stringFromMap(standbyDNS, "slotPrefix"); ok && strings.TrimSpace(value) != "" {
			prefix = strings.Trim(strings.TrimSpace(value), ".")
		}
	}
	return prefix + "-" + strconv.Itoa(slotIndex)
}

func requesterFromMetadata(metadata map[string]any) *LeaseRequester {
	raw, ok := mapFromMap(metadata, "requester")
	if !ok {
		return nil
	}
	consumer, _ := stringFromMap(raw, "consumer")
	kind, _ := stringFromMap(raw, "kind")
	ref, _ := stringFromMap(raw, "ref")
	if consumer == "" || kind == "" || ref == "" {
		return nil
	}
	var label *string
	if value, ok := stringFromMap(raw, "label"); ok {
		label = &value
	}
	var url *string
	if value, ok := stringFromMap(raw, "url"); ok {
		url = &value
	}
	metadataValue := map[string]string{}
	if rawMetadata, ok := mapFromMap(raw, "metadata"); ok {
		for key, value := range rawMetadata {
			if str, ok := value.(string); ok {
				metadataValue[key] = str
			}
		}
	}
	return &LeaseRequester{
		Consumer: consumer,
		Kind:     kind,
		Ref:      ref,
		Label:    label,
		URL:      url,
		Metadata: metadataValue,
	}
}

func defaultTTLSeconds(value int) int {
	if value == 0 {
		return 900
	}
	return value
}

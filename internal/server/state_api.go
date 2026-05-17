package server

import (
	"context"
	"encoding/json"
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
}

type StateSnapshot struct {
	ActiveLeases            []LeasePublic           `json:"active_leases"`
	TestEnvironments        []TestEnvironmentPublic `json:"test_environments"`
	WaitingTestSlotRequests []TestSlotRequestPublic `json:"waiting_test_slot_requests"`
	Projects                []Project               `json:"projects"`
	Workflows               []Workflow              `json:"workflows"`
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
			writeStateSnapshotError(w, err)
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
			writeStateSnapshotError(w, err)
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
			writeProblem(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		snapshot, err := loadStateSnapshot(r.Context(), settings, store)
		if err != nil {
			writeStateSnapshotError(w, err)
			return
		}

		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		w.Header().Set("connection", "keep-alive")

		for {
			payload, err := json.Marshal(snapshot)
			if err != nil {
				writeProblem(w, http.StatusInternalServerError, "encode state snapshot failed")
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

	return computeStateSnapshot(settings, projects, workflows, leases), nil
}

func writeStateSnapshotError(w http.ResponseWriter, err error) {
	if snapshotErr, ok := err.(stateSnapshotError); ok {
		writeProblem(w, snapshotErr.status, snapshotErr.message)
		return
	}
	writeProblem(w, http.StatusInternalServerError, "load state snapshot failed")
}

func computeStateSnapshot(
	settings Settings,
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
		TestEnvironments:        testEnvironmentsFromSnapshot(settings, projects, active, waiting),
		WaitingTestSlotRequests: waiting,
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
	settings Settings,
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
		if !boolFromMap(lease.Metadata, "test_slot_checkout") {
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

	envs := make([]TestEnvironmentPublic, 0)
	for _, projectName := range names {
		project := projectsByName[projectName]
		slotCount := projectTestSlotCount(settings, project)
		slotStatuses := testEnvironmentSlotStatuses(project)
		for slotIndex := 1; slotIndex <= slotCount; slotIndex++ {
			lease, claimed := claimedByProject[projectName][slotIndex]
			var publicLease *LeasePublic
			slotStatus := slotStatuses[slotIndex]
			state := firstNonEmpty(slotStatus.State, "warming")
			usable := false
			if state == testSlotStateReady {
				state = "available"
			}
			if claimed {
				if slotStatus.State == testSlotStateActivating || slotStatus.State == testSlotStateActive || slotStatus.State == testSlotStateCleaning || slotStatus.State == "error" {
					state = slotStatus.State
				} else {
					state = "claimed"
				}
				usable = state == testSlotStateActive
				value := leaseToPublicForState(settings, lease)
				publicLease = &value
			}
			var updatedAt *time.Time
			if !slotStatus.UpdatedAt.IsZero() {
				value := slotStatus.UpdatedAt
				updatedAt = &value
			}
			slotName := testEnvironmentName(projectName, slotIndex, project, lease)
			envs = append(envs, TestEnvironmentPublic{
				Project:               projectName,
				SlotIndex:             slotIndex,
				SlotName:              slotName,
				State:                 state,
				Usable:                usable,
				Detail:                slotStatus.Detail,
				UpdatedAt:             updatedAt,
				ReadyAt:               slotStatus.ReadyAt,
				ActivationAttempt:     slotStatus.ActivationAttempt,
				ActivationState:       slotStatus.ActivationState,
				ActivationStartedAt:   slotStatus.ActivationStartedAt,
				ActivationCompletedAt: slotStatus.ActivationCompletedAt,
				ActivationJobName:     slotStatus.ActivationJobName,
				ActivationError:       slotStatus.ActivationError,
				CleanupState:          slotStatus.CleanupState,
				CleanupStartedAt:      slotStatus.CleanupStartedAt,
				CleanupCompletedAt:    slotStatus.CleanupCompletedAt,
				CleanupError:          slotStatus.CleanupError,
				ReturnHistory:         slotStatus.ReturnHistory,
				Lease:                 publicLease,
				WaitingRequests:       sliceOrEmpty(waitingByProject[projectName][slotIndex]),
				PlaywrightWSEndpoint:  PlaywrightWSEndpointFor(settings, slotName),
			})
		}
	}
	return envs
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

func testEnvironmentSlotStates(project Project) map[int]string {
	statuses := testEnvironmentSlotStatuses(project)
	states := map[int]string{}
	for index, status := range statuses {
		states[index] = status.State
	}
	return states
}

func testEnvironmentSlotStatus(project Project, slotIndex int) (TestEnvironmentSlotStatus, bool) {
	statuses := testEnvironmentSlotStatuses(project)
	status, ok := statuses[slotIndex]
	return status, ok
}

func testEnvironmentSlotStatuses(project Project) map[int]TestEnvironmentSlotStatus {
	statuses := map[int]TestEnvironmentSlotStatus{}
	if standbyDNS, ok := mapFromMap(project.Metadata, "native_standby_dns"); ok {
		for _, slot := range mapSliceFromAnySlice(anySlice(standbyDNS["slots"])) {
			index, ok := positiveIntFromMap(slot, "slot_index")
			if !ok {
				index, ok = positiveIntFromMap(slot, "slotIndex")
			}
			if !ok {
				continue
			}
			status := TestEnvironmentSlotStatus{SlotIndex: index}
			if value, ok := stringFromMap(slot, "slot_name"); ok && strings.TrimSpace(value) != "" {
				status.SlotName = strings.TrimSpace(value)
			}
			if status.SlotName == "" {
				if value, ok := stringFromMap(slot, "slotName"); ok && strings.TrimSpace(value) != "" {
					status.SlotName = strings.TrimSpace(value)
				}
			}
			if value, ok := stringFromMap(slot, "state"); ok && strings.TrimSpace(value) != "" {
				status.State = strings.TrimSpace(value)
			}
			if value, ok := stringFromMap(slot, "detail"); ok && strings.TrimSpace(value) != "" {
				detail := strings.TrimSpace(value)
				status.Detail = &detail
			}
			if value, ok := stringFromMap(slot, "updated_at"); ok && strings.TrimSpace(value) != "" {
				if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
					status.UpdatedAt = parsed
				}
			}
			if value, ok := stringFromMap(slot, "updatedAt"); ok && status.UpdatedAt.IsZero() && strings.TrimSpace(value) != "" {
				if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
					status.UpdatedAt = parsed
				}
			}
			if value, ok := stringFromMap(slot, "ready_at"); ok && strings.TrimSpace(value) != "" {
				if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
					status.ReadyAt = &parsed
				}
			}
			if value, ok := stringFromMap(slot, "readyAt"); ok && status.ReadyAt == nil && strings.TrimSpace(value) != "" {
				if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
					status.ReadyAt = &parsed
				}
			}
			status.ActivationAttempt = slotPositiveIntPointer(slot, "activation_attempt", "activationAttempt")
			status.ActivationState = slotStringPointer(slot, "activation_state", "activationState")
			status.ActivationStartedAt = slotTimePointer(slot, "activation_started_at", "activationStartedAt")
			status.ActivationCompletedAt = slotTimePointer(slot, "activation_completed_at", "activationCompletedAt")
			status.ActivationJobName = slotStringPointer(slot, "activation_job_name", "activationJobName")
			status.ActivationError = slotStringPointer(slot, "activation_error", "activationError")
			status.CleanupState = slotStringPointer(slot, "cleanup_state", "cleanupState")
			status.CleanupStartedAt = slotTimePointer(slot, "cleanup_started_at", "cleanupStartedAt")
			status.CleanupCompletedAt = slotTimePointer(slot, "cleanup_completed_at", "cleanupCompletedAt")
			status.CleanupError = slotStringPointer(slot, "cleanup_error", "cleanupError")
			status.ReturnHistory = testSlotReturnHistoryFromAny(slot["test_slot_return_history"])
			statuses[index] = status
		}
	}
	return statuses
}

func testSlotReturnHistoryFromAny(raw any) []TestSlotReturnHistoryEntry {
	var history []TestSlotReturnHistoryEntry
	for _, item := range anySlice(raw) {
		entryMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		event, _ := stringFromMap(entryMap, "event")
		source, _ := stringFromMap(entryMap, "source")
		leaseRef, _ := stringFromMap(entryMap, "lease_ref")
		createdAt := slotTimePointer(entryMap, "created_at", "createdAt")
		if event == "" || source == "" || leaseRef == "" || createdAt == nil {
			continue
		}
		entry := TestSlotReturnHistoryEntry{
			Event:           event,
			CreatedAt:       *createdAt,
			Project:         stringValue(entryMap["project"]),
			SlotIndex:       slotPositiveIntPointer(entryMap, "slot_index", "slotIndex"),
			SlotName:        slotStringPointer(entryMap, "slot_name", "slotName"),
			LeaseRef:        leaseRef,
			LeaseNumber:     slotPositiveIntPointer(entryMap, "lease_number", "leaseNumber"),
			LeaseRequester:  slotStringPointer(entryMap, "lease_requester", "leaseRequester"),
			CallerPodIP:     slotStringPointer(entryMap, "caller_pod_ip", "callerPodIP"),
			CallerSessionID: slotStringPointer(entryMap, "caller_session_id", "callerSessionID"),
			Source:          source,
			Reason:          slotStringPointer(entryMap, "reason"),
			CleanupStarted:  boolFromMap(entryMap, "cleanup_started"),
		}
		history = append(history, entry)
	}
	return history
}

func slotStringPointer(slot map[string]any, keys ...string) *string {
	for _, key := range keys {
		if value, ok := stringFromMap(slot, key); ok && strings.TrimSpace(value) != "" {
			text := strings.TrimSpace(value)
			return &text
		}
	}
	return nil
}

func slotTimePointer(slot map[string]any, keys ...string) *time.Time {
	for _, key := range keys {
		if value, ok := stringFromMap(slot, key); ok && strings.TrimSpace(value) != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
				return &parsed
			}
		}
	}
	return nil
}

func slotPositiveIntPointer(slot map[string]any, keys ...string) *int {
	for _, key := range keys {
		if value, ok := positiveIntFromMap(slot, key); ok {
			return &value
		}
	}
	return nil
}

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

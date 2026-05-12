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
	ListHosts(ctx context.Context) ([]Host, error)
	ListLeases(ctx context.Context) ([]Lease, error)
}

type StateSnapshot struct {
	Hosts                   []HostPublic            `json:"hosts"`
	PendingLeases           []LeasePublic           `json:"pending_leases"`
	ActiveLeases            []LeasePublic           `json:"active_leases"`
	TestEnvironments        []TestEnvironmentPublic `json:"test_environments"`
	WaitingTestSlotRequests []TestSlotRequestPublic `json:"waiting_test_slot_requests"`
	Projects                []Project               `json:"projects"`
	Workflows               []Workflow              `json:"workflows"`
}

type Host struct {
	ID             string         `json:"-"`
	Name           string         `json:"name"`
	Capabilities   map[string]any `json:"capabilities"`
	CurrentLeaseID *string        `json:"-"`
	LastHeartbeat  *time.Time     `json:"last_heartbeat"`
	LastUsedAt     *time.Time     `json:"last_used_at"`
	Drained        bool           `json:"drained"`
	CreatedAt      time.Time      `json:"created_at"`
}

type HostPublic struct {
	Name            string         `json:"name"`
	Capabilities    map[string]any `json:"capabilities"`
	CurrentLeaseRef *string        `json:"current_lease_ref"`
	LastHeartbeat   *time.Time     `json:"last_heartbeat"`
	LastUsedAt      *time.Time     `json:"last_used_at"`
	Drained         bool           `json:"drained"`
	CreatedAt       time.Time      `json:"created_at"`
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
	Ref          string          `json:"ref"`
	LeaseNumber  *int            `json:"lease_number"`
	Project      string          `json:"project"`
	Workflow     *string         `json:"workflow"`
	Host         *string         `json:"host"`
	State        string          `json:"state"`
	Requirements map[string]any  `json:"requirements"`
	Metadata     map[string]any  `json:"metadata"`
	Requester    *LeaseRequester `json:"requester"`
	RequestedAt  time.Time       `json:"requested_at"`
	AssignedAt   *time.Time      `json:"assigned_at"`
	ReleasedAt   *time.Time      `json:"released_at"`
	TTLSeconds   int             `json:"ttl_seconds"`
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
	Project         string                  `json:"project"`
	SlotIndex       int                     `json:"slot_index"`
	SlotName        string                  `json:"slot_name"`
	State           string                  `json:"state"`
	Lease           *LeasePublic            `json:"lease"`
	WaitingRequests []TestSlotRequestPublic `json:"waiting_requests"`
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
	hosts, err := stateStore.ListHosts(ctx)
	if err != nil {
		return StateSnapshot{}, stateSnapshotError{status: http.StatusInternalServerError, message: "list hosts failed"}
	}
	leases, err := stateStore.ListLeases(ctx)
	if err != nil {
		return StateSnapshot{}, stateSnapshotError{status: http.StatusInternalServerError, message: "list leases failed"}
	}

	return computeStateSnapshot(settings, projects, workflows, hosts, leases), nil
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
	hosts []Host,
	leases []Lease,
) StateSnapshot {
	pending := make([]Lease, 0)
	active := make([]Lease, 0)
	waiting := make([]TestSlotRequestPublic, 0)
	leaseRefsByID := map[string]string{}
	for _, lease := range leases {
		switch {
		case lease.Kind == "test_slot_request" && lease.State == "waiting":
			waiting = append(waiting, testRequestToPublic(lease))
		case lease.State == "pending":
			pending = append(pending, lease)
			leaseRefsByID[lease.ID] = leasePublicRef(lease)
		case lease.State == "claimed":
			active = append(active, lease)
			leaseRefsByID[lease.ID] = leasePublicRef(lease)
		}
	}

	publicHosts := make([]HostPublic, 0, len(hosts))
	for _, host := range hosts {
		publicHosts = append(publicHosts, hostToPublic(host, leaseRefsByID))
	}
	pendingPublic := make([]LeasePublic, 0, len(pending))
	for _, lease := range pending {
		pendingPublic = append(pendingPublic, leaseToPublic(lease))
	}
	activePublic := make([]LeasePublic, 0, len(active))
	for _, lease := range active {
		activePublic = append(activePublic, leaseToPublic(lease))
	}

	return StateSnapshot{
		Hosts:                   publicHosts,
		PendingLeases:           pendingPublic,
		ActiveLeases:            activePublic,
		TestEnvironments:        testEnvironmentsFromSnapshot(settings, projects, active, waiting),
		WaitingTestSlotRequests: waiting,
		Projects:                sliceOrEmpty(projects),
		Workflows:               sliceOrEmpty(workflows),
	}
}

func hostToPublic(host Host, leaseRefsByID map[string]string) HostPublic {
	var leaseRef *string
	if host.CurrentLeaseID != nil {
		if value, ok := leaseRefsByID[*host.CurrentLeaseID]; ok {
			leaseRef = &value
		}
	}
	return HostPublic{
		Name:            host.Name,
		Capabilities:    mapOrEmpty(host.Capabilities),
		CurrentLeaseRef: leaseRef,
		LastHeartbeat:   host.LastHeartbeat,
		LastUsedAt:      host.LastUsedAt,
		Drained:         host.Drained,
		CreatedAt:       host.CreatedAt,
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

// LeasePublicRefFromLease is the exported wrapper used by the cosmos store.
func LeasePublicRefFromLease(lease Lease) string {
	return leasePublicRef(lease)
}

func leasePublicRef(lease Lease) string {
	slotName := ""
	if value, ok := stringFromMap(lease.Metadata, "native_slot_name"); ok {
		slotName = strings.TrimSpace(value)
	}
	if slotName == "" {
		if phaseInputs, ok := mapFromMap(lease.Metadata, "phase_inputs"); ok {
			if value, ok := stringFromMap(phaseInputs, "slot_name"); ok {
				slotName = strings.TrimSpace(value)
			}
		}
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
		for slotIndex := range claimedByProject[projectName] {
			if slotIndex > slotCount {
				slotCount = slotIndex
			}
		}
		for slotIndex := range waitingByProject[projectName] {
			if slotIndex > slotCount {
				slotCount = slotIndex
			}
		}
		for slotIndex := 1; slotIndex <= slotCount; slotIndex++ {
			lease, claimed := claimedByProject[projectName][slotIndex]
			var publicLease *LeasePublic
			state := "available"
			if claimed {
				state = "claimed"
				value := leaseToPublic(lease)
				publicLease = &value
			}
			envs = append(envs, TestEnvironmentPublic{
				Project:         projectName,
				SlotIndex:       slotIndex,
				SlotName:        testEnvironmentName(projectName, slotIndex, project, lease),
				State:           state,
				Lease:           publicLease,
				WaitingRequests: sliceOrEmpty(waitingByProject[projectName][slotIndex]),
			})
		}
	}
	return envs
}

func projectTestSlotCount(settings Settings, project Project) int {
	metadata := project.Metadata
	if standbyDNS, ok := mapFromMap(metadata, "native_standby_dns"); ok {
		if count, ok := positiveIntFromMap(standbyDNS, "count"); ok {
			return count
		}
	}
	if projectRequiresNativeWorkflows(project) {
		if settings.NativeRunnerProjectConcurrency > 0 {
			return settings.NativeRunnerProjectConcurrency
		}
		return 1
	}
	return 0
}

func testEnvironmentName(project string, slotIndex int, projectDoc Project, lease Lease) string {
	if value, ok := stringFromMap(lease.Metadata, "native_slot_name"); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
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

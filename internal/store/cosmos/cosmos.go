package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/server"
)

type Store struct {
	projects  *azcosmos.ContainerClient
	workflows *azcosmos.ContainerClient
	hosts     *azcosmos.ContainerClient
	leases    *azcosmos.ContainerClient
}

func NewFromSettings(settings server.Settings) (*Store, error) {
	if settings.CosmosEndpoint == "" || settings.CosmosDatabase == "" {
		return nil, errors.New("COSMOS_ENDPOINT and COSMOS_DATABASE are required")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create default Azure credential: %w", err)
	}
	client, err := azcosmos.NewClient(settings.CosmosEndpoint, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("create cosmos client: %w", err)
	}
	projects, err := client.NewContainer(settings.CosmosDatabase, "projects")
	if err != nil {
		return nil, fmt.Errorf("create projects container client: %w", err)
	}
	workflows, err := client.NewContainer(settings.CosmosDatabase, "workflows")
	if err != nil {
		return nil, fmt.Errorf("create workflows container client: %w", err)
	}
	hosts, err := client.NewContainer(settings.CosmosDatabase, "hosts")
	if err != nil {
		return nil, fmt.Errorf("create hosts container client: %w", err)
	}
	leases, err := client.NewContainer(settings.CosmosDatabase, "leases")
	if err != nil {
		return nil, fmt.Errorf("create leases container client: %w", err)
	}
	return &Store{projects: projects, workflows: workflows, hosts: hosts, leases: leases}, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]server.Project, error) {
	var docs []projectDoc
	if err := queryAll(ctx, s.projects, &docs); err != nil {
		return nil, err
	}
	rows := make([]server.Project, 0, len(docs))
	for _, doc := range docs {
		rows = append(rows, projectFromDoc(doc))
	}
	return rows, nil
}

func (s *Store) UpsertProject(ctx context.Context, req server.ProjectRegister) (server.Project, error) {
	doc := projectWriteDoc{
		ID:         req.Name,
		Name:       req.Name,
		GitHubRepo: req.GitHubRepo,
		Metadata:   mapOrEmpty(req.Metadata),
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	partitionKey := azcosmos.NewPartitionKeyString(req.Name)
	existing, err := s.projects.ReadItem(ctx, partitionKey, req.Name, nil)
	if err == nil {
		var existingDoc projectDoc
		if err := json.Unmarshal(existing.Value, &existingDoc); err == nil && existingDoc.CreatedAt != "" {
			doc.CreatedAt = existingDoc.CreatedAt
		}
		payload, err := json.Marshal(doc)
		if err != nil {
			return server.Project{}, err
		}
		if _, err := s.projects.ReplaceItem(ctx, partitionKey, req.Name, payload, nil); err != nil {
			return server.Project{}, err
		}
		return projectFromDoc(projectDoc{
			ID:         doc.ID,
			Name:       doc.Name,
			GitHubRepo: doc.GitHubRepo,
			Metadata:   doc.Metadata,
			CreatedAt:  doc.CreatedAt,
		}), nil
	}
	if !isCosmosStatus(err, http.StatusNotFound) {
		return server.Project{}, err
	}

	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Project{}, err
	}
	if _, err := s.projects.CreateItem(ctx, partitionKey, payload, nil); err != nil {
		return server.Project{}, err
	}
	return projectFromDoc(projectDoc{
		ID:         doc.ID,
		Name:       doc.Name,
		GitHubRepo: doc.GitHubRepo,
		Metadata:   doc.Metadata,
		CreatedAt:  doc.CreatedAt,
	}), nil
}

func (s *Store) SetProjectTestEnvironmentCount(ctx context.Context, project string, count int) (server.Project, error) {
	partitionKey := azcosmos.NewPartitionKeyString(project)
	read, err := s.projects.ReadItem(ctx, partitionKey, project, nil)
	if err != nil {
		if isCosmosStatus(err, http.StatusNotFound) {
			return server.Project{}, server.ErrNotFound
		}
		return server.Project{}, err
	}

	var doc map[string]any
	if err := json.Unmarshal(read.Value, &doc); err != nil {
		return server.Project{}, err
	}
	metadata, _ := doc["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	standbyDNS, _ := metadata["native_standby_dns"].(map[string]any)
	if standbyDNS == nil {
		standbyDNS = map[string]any{}
	}
	standbyDNS["count"] = count
	metadata["native_standby_dns"] = standbyDNS
	doc["metadata"] = metadata

	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Project{}, err
	}
	if _, err := s.projects.ReplaceItem(ctx, partitionKey, project, payload, nil); err != nil {
		return server.Project{}, err
	}
	return projectFromMap(doc)
}

func (s *Store) ListWorkflows(ctx context.Context) ([]server.Workflow, error) {
	var docs []workflowDoc
	if err := queryAll(ctx, s.workflows, &docs); err != nil {
		return nil, err
	}
	rows := make([]server.Workflow, 0, len(docs))
	for _, doc := range docs {
		rows = append(rows, workflowFromDoc(doc))
	}
	return rows, nil
}

func (s *Store) DeleteWorkflow(ctx context.Context, project string, name string) (server.Workflow, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	read, err := s.workflows.ReadItem(ctx, pk, name, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return server.Workflow{}, server.ErrNotFound
	}
	if err != nil {
		return server.Workflow{}, err
	}
	var doc workflowDoc
	if err := json.Unmarshal(read.Value, &doc); err != nil {
		return server.Workflow{}, err
	}
	if _, err := s.workflows.DeleteItem(ctx, pk, name, nil); err != nil {
		return server.Workflow{}, err
	}
	return workflowFromDoc(doc), nil
}

func (s *Store) ListHosts(ctx context.Context) ([]server.Host, error) {
	var docs []hostDoc
	if err := queryAll(ctx, s.hosts, &docs); err != nil {
		return nil, err
	}
	rows := make([]server.Host, 0, len(docs))
	for _, doc := range docs {
		rows = append(rows, hostFromDoc(doc))
	}
	return rows, nil
}

func (s *Store) UpsertHost(ctx context.Context, input server.HostRegistration) (server.Host, error) {
	pk := azcosmos.NewPartitionKeyString(input.Name)
	read, err := s.hosts.ReadItem(ctx, pk, input.Name, nil)
	if err == nil {
		var doc map[string]any
		if err := json.Unmarshal(read.Value, &doc); err != nil {
			return server.Host{}, err
		}
		if input.Capabilities != nil {
			doc["capabilities"] = input.Capabilities
		} else if _, ok := doc["capabilities"]; !ok {
			doc["capabilities"] = map[string]any{}
		}
		if input.Drained != nil {
			doc["drained"] = *input.Drained
		}
		body, err := json.Marshal(doc)
		if err != nil {
			return server.Host{}, err
		}
		if _, err := s.hosts.ReplaceItem(ctx, pk, input.Name, body, nil); err != nil {
			return server.Host{}, err
		}
		return hostFromMap(doc)
	}
	if !isCosmosStatus(err, http.StatusNotFound) {
		return server.Host{}, err
	}

	doc := hostWriteDoc{
		ID:             input.Name,
		Name:           input.Name,
		Capabilities:   mapOrEmpty(input.Capabilities),
		CurrentLeaseID: nil,
		LastHeartbeat:  nil,
		LastUsedAt:     nil,
		Drained:        boolPtrValue(input.Drained),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return server.Host{}, err
	}
	if _, err := s.hosts.CreateItem(ctx, pk, body, nil); err != nil {
		return server.Host{}, err
	}
	return hostFromMap(map[string]any{
		"id":             doc.ID,
		"name":           doc.Name,
		"capabilities":   doc.Capabilities,
		"currentLeaseId": doc.CurrentLeaseID,
		"lastHeartbeat":  doc.LastHeartbeat,
		"lastUsedAt":     doc.LastUsedAt,
		"drained":        doc.Drained,
		"createdAt":      doc.CreatedAt,
	})
}

func (s *Store) ListLeases(ctx context.Context) ([]server.Lease, error) {
	var docs []leaseDoc
	if err := queryAll(ctx, s.leases, &docs); err != nil {
		return nil, err
	}
	rows := make([]server.Lease, 0, len(docs))
	for _, doc := range docs {
		rows = append(rows, leaseFromDoc(doc))
	}
	return rows, nil
}

func queryAll[T any](ctx context.Context, container *azcosmos.ContainerClient, target *[]T) error {
	return queryAllWhere(ctx, container, "SELECT * FROM c", nil, target)
}

func queryAllWhere[T any](ctx context.Context, container *azcosmos.ContainerClient, query string, parameters []azcosmos.QueryParameter, target *[]T) error {
	pager := container.NewQueryItemsPager(query, azcosmos.NewPartitionKey(), &azcosmos.QueryOptions{
		QueryParameters: parameters,
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			var row T
			if err := json.Unmarshal(item, &row); err != nil {
				return err
			}
			*target = append(*target, row)
		}
	}
	return nil
}

type projectDoc struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	GitHubRepo string         `json:"githubRepo"`
	ArgoCDApp  string         `json:"argocdApp"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  string         `json:"createdAt"`
}

type projectWriteDoc struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	GitHubRepo string         `json:"githubRepo"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  string         `json:"createdAt"`
}

type workflowDoc struct {
	ID                  string         `json:"id"`
	Project             string         `json:"project"`
	Name                string         `json:"name"`
	Phases              []phaseDoc     `json:"phases"`
	PR                  prDoc          `json:"pr"`
	Budget              budgetDoc      `json:"budget"`
	TriggerLabel        *string        `json:"triggerLabel"`
	DefaultRequirements map[string]any `json:"defaultRequirements"`
	Metadata            map[string]any `json:"metadata"`
	CreatedAt           string         `json:"createdAt"`
}

type hostDoc struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Capabilities   map[string]any `json:"capabilities"`
	CurrentLeaseID *string        `json:"currentLeaseId"`
	LastHeartbeat  string         `json:"lastHeartbeat"`
	LastUsedAt     string         `json:"lastUsedAt"`
	Drained        bool           `json:"drained"`
	CreatedAt      string         `json:"createdAt"`
}

type hostWriteDoc struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Capabilities   map[string]any `json:"capabilities"`
	CurrentLeaseID *string        `json:"currentLeaseId"`
	LastHeartbeat  *string        `json:"lastHeartbeat"`
	LastUsedAt     *string        `json:"lastUsedAt"`
	Drained        bool           `json:"drained"`
	CreatedAt      string         `json:"createdAt"`
}

type leaseDoc struct {
	ID                 string         `json:"id"`
	Kind               string         `json:"kind"`
	LeaseNumber        *int           `json:"leaseNumber"`
	Project            string         `json:"project"`
	Workflow           *string        `json:"workflow"`
	Host               *string        `json:"host"`
	State              string         `json:"state"`
	Requirements       map[string]any `json:"requirements"`
	Metadata           map[string]any `json:"metadata"`
	RequestedAt        string         `json:"requestedAt"`
	AssignedAt         string         `json:"assignedAt"`
	ReleasedAt         string         `json:"releasedAt"`
	TTLSeconds         int            `json:"ttlSeconds"`
	RequestedSlotIndex *int           `json:"requestedSlotIndex"`
	FulfilledAt        string         `json:"fulfilledAt"`
	FulfilledLeaseRef  *string        `json:"fulfilledLeaseRef"`
}

type phaseDoc struct {
	Name                     string            `json:"name"`
	Kind                     string            `json:"kind"`
	WorkflowFilename         string            `json:"workflowFilename"`
	WorkflowRef              string            `json:"workflowRef"`
	Inputs                   map[string]string `json:"inputs"`
	Outputs                  []string          `json:"outputs"`
	Requirements             map[string]any    `json:"requirements"`
	Verify                   bool              `json:"verify"`
	RecyclePolicy            *recyclePolicyDoc `json:"recyclePolicy"`
	Always                   bool              `json:"always"`
	EvidenceVerificationGate bool              `json:"evidenceVerificationGate"`
	DependsOn                []string          `json:"dependsOn"`
	Jobs                     []nativeJobDoc    `json:"jobs"`
}

type recyclePolicyDoc struct {
	MaxAttempts int      `json:"maxAttempts"`
	On          []string `json:"on"`
	LandsAt     string   `json:"landsAt"`
}

type nativeJobDoc struct {
	ID             string            `json:"id"`
	Name           *string           `json:"name"`
	Image          string            `json:"image"`
	Command        []string          `json:"command"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	Steps          []nativeStepDoc   `json:"steps"`
	TimeoutSeconds *int              `json:"timeoutSeconds"`
}

type nativeStepDoc struct {
	Slug  string  `json:"slug"`
	Title *string `json:"title"`
}

type prDoc struct {
	Enabled       bool              `json:"enabled"`
	RecyclePolicy *recyclePolicyDoc `json:"recyclePolicy"`
}

type budgetDoc struct {
	Total float64 `json:"total"`
}

func projectFromDoc(doc projectDoc) server.Project {
	return server.Project{
		ID:         firstNonEmpty(doc.ID, doc.Name),
		Name:       doc.Name,
		GitHubRepo: doc.GitHubRepo,
		ArgoCDApp:  doc.ArgoCDApp,
		Metadata:   mapOrEmpty(doc.Metadata),
		CreatedAt:  parseTimeOrNow(doc.CreatedAt),
	}
}

func projectFromMap(doc map[string]any) (server.Project, error) {
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Project{}, err
	}
	var typed projectDoc
	if err := json.Unmarshal(payload, &typed); err != nil {
		return server.Project{}, err
	}
	return projectFromDoc(typed), nil
}

func workflowFromDoc(doc workflowDoc) server.Workflow {
	phases := make([]server.PhaseSpec, 0, len(doc.Phases))
	for _, phase := range doc.Phases {
		phases = append(phases, phaseFromDoc(phase))
	}
	inferSequentialDependsOn(phases)

	return server.Workflow{
		ID:                  firstNonEmpty(doc.ID, doc.Name),
		Project:             doc.Project,
		Name:                doc.Name,
		Phases:              phases,
		PR:                  prFromDoc(doc.PR),
		Budget:              budget.Config{Total: defaultBudgetTotal(doc.Budget.Total)},
		TriggerLabel:        doc.TriggerLabel,
		DefaultRequirements: mapOrEmpty(doc.DefaultRequirements),
		Metadata:            mapOrEmpty(doc.Metadata),
		CreatedAt:           parseTimeOrNow(doc.CreatedAt),
	}
}

func hostFromDoc(doc hostDoc) server.Host {
	return server.Host{
		ID:             firstNonEmpty(doc.ID, doc.Name),
		Name:           doc.Name,
		Capabilities:   mapOrEmpty(doc.Capabilities),
		CurrentLeaseID: doc.CurrentLeaseID,
		LastHeartbeat:  parseOptionalTime(doc.LastHeartbeat),
		LastUsedAt:     parseOptionalTime(doc.LastUsedAt),
		Drained:        doc.Drained,
		CreatedAt:      parseTimeOrNow(doc.CreatedAt),
	}
}

func hostFromMap(doc map[string]any) (server.Host, error) {
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Host{}, err
	}
	var typed hostDoc
	if err := json.Unmarshal(payload, &typed); err != nil {
		return server.Host{}, err
	}
	return hostFromDoc(typed), nil
}

func leaseFromDoc(doc leaseDoc) server.Lease {
	return server.Lease{
		ID:                 doc.ID,
		Kind:               doc.Kind,
		LeaseNumber:        doc.LeaseNumber,
		Project:            doc.Project,
		Workflow:           doc.Workflow,
		Host:               doc.Host,
		State:              firstNonEmpty(doc.State, "claimed"),
		Requirements:       mapOrEmpty(doc.Requirements),
		Metadata:           mapOrEmpty(doc.Metadata),
		RequestedAt:        parseTimeOrNow(doc.RequestedAt),
		AssignedAt:         parseOptionalTime(doc.AssignedAt),
		ReleasedAt:         parseOptionalTime(doc.ReleasedAt),
		TTLSeconds:         defaultTTLSeconds(doc.TTLSeconds),
		RequestedSlotIndex: doc.RequestedSlotIndex,
		FulfilledAt:        parseOptionalTime(doc.FulfilledAt),
		FulfilledLeaseRef:  doc.FulfilledLeaseRef,
	}
}

func phaseFromDoc(doc phaseDoc) server.PhaseSpec {
	jobs := make([]server.NativeJobSpec, 0, len(doc.Jobs))
	for _, job := range doc.Jobs {
		jobs = append(jobs, jobFromDoc(job))
	}
	return server.PhaseSpec{
		Name:                     doc.Name,
		Kind:                     firstNonEmpty(doc.Kind, "gha_dispatch"),
		WorkflowFilename:         doc.WorkflowFilename,
		WorkflowRef:              firstNonEmpty(doc.WorkflowRef, "main"),
		Inputs:                   stringMapOrEmpty(doc.Inputs),
		Outputs:                  sliceOrEmpty(doc.Outputs),
		Requirements:             doc.Requirements,
		Verify:                   doc.Verify,
		RecyclePolicy:            recyclePolicyFromDoc(doc.RecyclePolicy),
		Always:                   doc.Always,
		EvidenceVerificationGate: doc.EvidenceVerificationGate,
		DependsOn:                sliceOrEmpty(doc.DependsOn),
		Jobs:                     jobs,
	}
}

func jobFromDoc(doc nativeJobDoc) server.NativeJobSpec {
	steps := make([]server.NativeStepSpec, 0, len(doc.Steps))
	for _, step := range doc.Steps {
		steps = append(steps, server.NativeStepSpec{Slug: step.Slug, Title: step.Title})
	}
	return server.NativeJobSpec{
		ID:             doc.ID,
		Name:           doc.Name,
		Image:          doc.Image,
		Command:        sliceOrEmpty(doc.Command),
		Args:           sliceOrEmpty(doc.Args),
		Env:            stringMapOrEmpty(doc.Env),
		Steps:          steps,
		TimeoutSeconds: doc.TimeoutSeconds,
	}
}

func prFromDoc(doc prDoc) server.PrPrimitive {
	return server.PrPrimitive{Enabled: doc.Enabled, RecyclePolicy: recyclePolicyFromDoc(doc.RecyclePolicy)}
}

func recyclePolicyFromDoc(doc *recyclePolicyDoc) *server.RecyclePolicy {
	if doc == nil {
		return nil
	}
	maxAttempts := doc.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 3
	}
	return &server.RecyclePolicy{
		MaxAttempts: maxAttempts,
		On:          sliceOrEmpty(doc.On),
		LandsAt:     firstNonEmpty(doc.LandsAt, "self"),
	}
}

func inferSequentialDependsOn(phases []server.PhaseSpec) {
	hasExplicitDepends := false
	for _, phase := range phases {
		if len(phase.DependsOn) > 0 {
			hasExplicitDepends = true
			break
		}
	}
	if hasExplicitDepends {
		return
	}
	for i := range phases {
		if i == 0 {
			continue
		}
		if phases[i].Always {
			deps := make([]string, 0, i)
			for j := 0; j < i; j++ {
				if !phases[j].Always {
					deps = append(deps, phases[j].Name)
				}
			}
			phases[i].DependsOn = deps
			continue
		}
		phases[i].DependsOn = []string{phases[i-1].Name}
	}
}

func parseTimeOrNow(value string) time.Time {
	if value == "" {
		return time.Now().UTC()
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Now().UTC()
	}
	return parsed
}

func parseOptionalTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return &parsed
}

func defaultBudgetTotal(total float64) float64 {
	if total == 0 {
		return 25
	}
	return total
}

func defaultTTLSeconds(ttl int) int {
	if ttl == 0 {
		return 900
	}
	return ttl
}

func boolPtrValue(value *bool) bool {
	return value != nil && *value
}

func isCosmosStatus(err error, status int) bool {
	var responseErr *azcore.ResponseError
	return errors.As(err, &responseErr) && responseErr.StatusCode == status
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func mapOrEmpty(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	return values
}

func stringMapOrEmpty(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return values
}

func sliceOrEmpty[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

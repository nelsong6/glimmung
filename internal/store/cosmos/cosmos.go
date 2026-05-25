package cosmos

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
	"github.com/google/uuid"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/publicids"
	"github.com/nelsong6/glimmung/internal/server"
	pgstore "github.com/nelsong6/glimmung/internal/store/pg"
)

type Store struct {
	projects                 *azcosmos.ContainerClient
	workflows                *azcosmos.ContainerClient
	leases                   *azcosmos.ContainerClient
	runs                     *azcosmos.ContainerClient
	runEvents                *azcosmos.ContainerClient
	issues                   *azcosmos.ContainerClient
	locks                    *azcosmos.ContainerClient
	reports                  *azcosmos.ContainerClient
	playbooks                *azcosmos.ContainerClient
	signals                  *azcosmos.ContainerClient
	slots                    *azcosmos.ContainerClient
	slotHistory              *azcosmos.ContainerClient
	nativeProjectConcurrency int

	// pgLocks is the Postgres-backed lock store. Injected by main.go
	// via SetPGLocks after both cosmos.Store and pg.LocksStore are
	// constructed. All Claim/Release/IsHeld/ListHeld lock operations
	// flow through this field; cosmos.Store no longer talks to its
	// `locks` container client for those operations (Stage 2b cutover
	// per docs/postgres-migration.md). The `locks` field above is
	// retained only so ListAllLockDocsForMigration can read the
	// pre-migration snapshot for the one-shot Migrate copy — Stage 2i
	// deletes both this field and the cosmos lock container client
	// entirely.
	pgLocks *pgstore.LocksStore

	// pgRunEvents is the Postgres-backed run-events store. Injected
	// via SetPGRunEvents (Stage 2c). RecordNativeEventByID and
	// ListNativeEventsByID delegate event storage to this field;
	// cosmos.Store no longer writes to or reads from its `runEvents`
	// container for those operations. The `runEvents` container client
	// is retained only so ListAllRunEventDocsForMigration can feed the
	// one-shot Migrate copy. Stage 2i deletes both.
	pgRunEvents *pgstore.RunEventsStore
}

// SetPGLocks injects the Postgres-backed lock store. Called once at
// startup by cmd/glimmung-go/main.go after pg.LocksStore is constructed.
// Methods that read lock state (ListIssues, ListTouchpoints,
// GetIssueDetailByNumber, the touchpoint detail builder) all route
// through s.pgLocks once this is set.
func (s *Store) SetPGLocks(locks *pgstore.LocksStore) {
	s.pgLocks = locks
}

// SetPGRunEvents injects the Postgres-backed run-events store. Called
// once at startup by cmd/glimmung-go/main.go after pg.RunEventsStore is
// constructed. RecordNativeEventByID writes go to pg via this field;
// ListNativeEventsByID reads through it.
func (s *Store) SetPGRunEvents(runEvents *pgstore.RunEventsStore) {
	s.pgRunEvents = runEvents
}

const workflowSchemaKind = "workflow_schema"

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
	leases, err := client.NewContainer(settings.CosmosDatabase, "leases")
	if err != nil {
		return nil, fmt.Errorf("create leases container client: %w", err)
	}
	runs, err := client.NewContainer(settings.CosmosDatabase, "runs")
	if err != nil {
		return nil, fmt.Errorf("create runs container client: %w", err)
	}
	runEvents, err := client.NewContainer(settings.CosmosDatabase, "run_events")
	if err != nil {
		return nil, fmt.Errorf("create run_events container client: %w", err)
	}
	issues, err := client.NewContainer(settings.CosmosDatabase, "issues")
	if err != nil {
		return nil, fmt.Errorf("create issues container client: %w", err)
	}
	locks, err := client.NewContainer(settings.CosmosDatabase, "locks")
	if err != nil {
		return nil, fmt.Errorf("create locks container client: %w", err)
	}
	reports, err := client.NewContainer(settings.CosmosDatabase, "reports")
	if err != nil {
		return nil, fmt.Errorf("create reports container client: %w", err)
	}
	playbooks, err := client.NewContainer(settings.CosmosDatabase, "playbooks")
	if err != nil {
		return nil, fmt.Errorf("create playbooks container client: %w", err)
	}
	signals, err := client.NewContainer(settings.CosmosDatabase, "signals")
	if err != nil {
		return nil, fmt.Errorf("create signals container client: %w", err)
	}
	slots, err := client.NewContainer(settings.CosmosDatabase, "slots")
	if err != nil {
		return nil, fmt.Errorf("create slots container client: %w", err)
	}
	slotHistory, err := client.NewContainer(settings.CosmosDatabase, "slot_history")
	if err != nil {
		return nil, fmt.Errorf("create slot_history container client: %w", err)
	}
	return &Store{
		projects:                 projects,
		workflows:                workflows,
		leases:                   leases,
		runs:                     runs,
		runEvents:                runEvents,
		issues:                   issues,
		locks:                    locks,
		reports:                  reports,
		playbooks:                playbooks,
		signals:                  signals,
		slots:                    slots,
		slotHistory:              slotHistory,
		nativeProjectConcurrency: settings.NativeRunnerProjectConcurrency,
	}, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]server.Project, error) {
	var docs []projectDoc
	if err := crossPartitionQuery(ctx, s.projects, "SELECT * FROM c WHERE NOT IS_DEFINED(c.kind) OR c.kind = 'project'", nil, &docs); err != nil {
		return nil, err
	}
	rows := make([]server.Project, 0, len(docs))
	for _, doc := range docs {
		rows = append(rows, projectFromDoc(doc))
	}
	return rows, nil
}

// listProjectNames returns the partition-key values for every registered
// project, suitable for fanOutByProject. Cosmos lists projects in any
// order; the slice ordering is not contractual.
func (s *Store) listProjectNames(ctx context.Context) ([]string, error) {
	var docs []projectDoc
	if err := crossPartitionQuery(ctx, s.projects, "SELECT c.id FROM c WHERE NOT IS_DEFINED(c.kind) OR c.kind = 'project'", nil, &docs); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(docs))
	for _, doc := range docs {
		if doc.ID != "" {
			names = append(names, doc.ID)
		}
	}
	return names, nil
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
	// The legacy `slots` embedded array is the durable source of slot
	// state on pre-#518 projects; the boot migration strips it once and
	// PATCH-count must not write it back. Slot state lives in the
	// `slots` Cosmos collection, owned by the SlotStore. Decreasing the
	// count is handled by the orchestrator's PATCH handler deleting the
	// affected slot rows via SlotStore.DeleteSlot.
	delete(standbyDNS, "slots")
	metadata["native_standby_dns"] = standbyDNS
	if workloadIdentity, ok := metadata["native_standby_workload_identity"].(map[string]any); ok {
		workloadIdentity["count"] = count
		metadata["native_standby_workload_identity"] = workloadIdentity
	}
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

func (s *Store) SetProjectNativeWorkloadIdentityStatus(ctx context.Context, project string, status server.NativeWorkloadIdentityStatus) (server.Project, error) {
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
	metadata["native_standby_workload_identity_status"] = status
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

// SetProjectManagedAuthOriginStatus persists the result of the
// glimmung-owned auth.romaine.life origin reconciler on the project's
// metadata under `managed_auth_origins_status`. Mirrors
// SetProjectNativeWorkloadIdentityStatus exactly. See
// nelsong6/glimmung#142 stage 2.
func (s *Store) SetProjectManagedAuthOriginStatus(ctx context.Context, project string, status server.ManagedAuthOriginStatus) (server.Project, error) {
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
	metadata["managed_auth_origins_status"] = status
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

const (
	testLeaseDefaultsDocID   = "__glimmung_test_lease_defaults"
	testLeaseDefaultsDocKind = "test_lease_defaults"
)

func (s *Store) ReadTestLeaseDefaults(ctx context.Context) (server.TestLeaseDefaults, error) {
	pk := azcosmos.NewPartitionKeyString(testLeaseDefaultsDocID)
	read, err := s.projects.ReadItem(ctx, pk, testLeaseDefaultsDocID, nil)
	if err != nil {
		if isCosmosStatus(err, http.StatusNotFound) {
			return server.TestLeaseDefaults{}, server.ErrNotFound
		}
		return server.TestLeaseDefaults{}, err
	}
	var doc testLeaseDefaultsDoc
	if err := json.Unmarshal(read.Value, &doc); err != nil {
		return server.TestLeaseDefaults{}, err
	}
	return server.TestLeaseDefaults{
		GlobalTTLSeconds:     doc.GlobalTTLSeconds,
		HotSwapMinTTLSeconds: doc.HotSwapMinTTLSeconds,
	}, nil
}

func (s *Store) SetGlobalTestLeaseDefaultTTL(ctx context.Context, ttlSeconds *int) (server.TestLeaseDefaults, error) {
	if ttlSeconds != nil && *ttlSeconds <= 0 {
		return server.TestLeaseDefaults{}, server.ValidationError{Message: "ttl_seconds must be positive"}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc := testLeaseDefaultsDoc{
		ID:        testLeaseDefaultsDocID,
		Kind:      testLeaseDefaultsDocKind,
		CreatedAt: now,
		UpdatedAt: now,
	}
	pk := azcosmos.NewPartitionKeyString(testLeaseDefaultsDocID)
	read, err := s.projects.ReadItem(ctx, pk, testLeaseDefaultsDocID, nil)
	if err == nil {
		if err := json.Unmarshal(read.Value, &doc); err != nil {
			return server.TestLeaseDefaults{}, err
		}
		doc.ID = testLeaseDefaultsDocID
		doc.Kind = testLeaseDefaultsDocKind
		if doc.CreatedAt == "" {
			doc.CreatedAt = now
		}
		doc.UpdatedAt = now
	} else if !isCosmosStatus(err, http.StatusNotFound) {
		return server.TestLeaseDefaults{}, err
	}
	if ttlSeconds == nil {
		doc.GlobalTTLSeconds = 0
	} else {
		doc.GlobalTTLSeconds = *ttlSeconds
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.TestLeaseDefaults{}, err
	}
	if _, err := s.projects.UpsertItem(ctx, pk, payload, nil); err != nil {
		return server.TestLeaseDefaults{}, err
	}
	return server.TestLeaseDefaults{
		GlobalTTLSeconds:     doc.GlobalTTLSeconds,
		HotSwapMinTTLSeconds: doc.HotSwapMinTTLSeconds,
	}, nil
}

func (s *Store) SetGlobalTestLeaseHotSwapMinTTL(ctx context.Context, ttlSeconds *int) (server.TestLeaseDefaults, error) {
	if ttlSeconds != nil && *ttlSeconds <= 0 {
		return server.TestLeaseDefaults{}, server.ValidationError{Message: "ttl_seconds must be positive"}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc := testLeaseDefaultsDoc{
		ID:        testLeaseDefaultsDocID,
		Kind:      testLeaseDefaultsDocKind,
		CreatedAt: now,
		UpdatedAt: now,
	}
	pk := azcosmos.NewPartitionKeyString(testLeaseDefaultsDocID)
	read, err := s.projects.ReadItem(ctx, pk, testLeaseDefaultsDocID, nil)
	if err == nil {
		if err := json.Unmarshal(read.Value, &doc); err != nil {
			return server.TestLeaseDefaults{}, err
		}
		doc.ID = testLeaseDefaultsDocID
		doc.Kind = testLeaseDefaultsDocKind
		if doc.CreatedAt == "" {
			doc.CreatedAt = now
		}
		doc.UpdatedAt = now
	} else if !isCosmosStatus(err, http.StatusNotFound) {
		return server.TestLeaseDefaults{}, err
	}
	if ttlSeconds == nil {
		doc.HotSwapMinTTLSeconds = 0
	} else {
		doc.HotSwapMinTTLSeconds = *ttlSeconds
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.TestLeaseDefaults{}, err
	}
	if _, err := s.projects.UpsertItem(ctx, pk, payload, nil); err != nil {
		return server.TestLeaseDefaults{}, err
	}
	return server.TestLeaseDefaults{
		GlobalTTLSeconds:     doc.GlobalTTLSeconds,
		HotSwapMinTTLSeconds: doc.HotSwapMinTTLSeconds,
	}, nil
}

func (s *Store) SetProjectTestLeaseDefaultTTL(ctx context.Context, project string, ttlSeconds *int) (server.Project, error) {
	if ttlSeconds != nil && *ttlSeconds <= 0 {
		return server.Project{}, server.ValidationError{Message: "ttl_seconds must be positive"}
	}
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
	delete(metadata, "testLeaseDefaultTTLSeconds")
	if ttlSeconds == nil {
		delete(metadata, "test_lease_default_ttl_seconds")
	} else {
		metadata["test_lease_default_ttl_seconds"] = *ttlSeconds
	}
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

func (s *Store) SetProjectTestLeaseHotSwapMinTTL(ctx context.Context, project string, ttlSeconds *int) (server.Project, error) {
	if ttlSeconds != nil && *ttlSeconds <= 0 {
		return server.Project{}, server.ValidationError{Message: "ttl_seconds must be positive"}
	}
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
	delete(metadata, "testLeaseHotSwapMinTTLSeconds")
	if ttlSeconds == nil {
		delete(metadata, "test_lease_hot_swap_min_ttl_seconds")
	} else {
		metadata["test_lease_hot_swap_min_ttl_seconds"] = *ttlSeconds
	}
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

// StripProjectTestEnvironmentSlotsArray removes the legacy
// `metadata.native_standby_dns.slots[]` array from a project doc.
// Called by the one-shot slot-storage-rework migration after slot data
// has been copied to the new `slots` collection.
//
// Idempotent: if the array is already absent, the call is a no-op
// (still does the read-modify-write so the project doc's updated_at
// advances, which is harmless).
func (s *Store) StripProjectTestEnvironmentSlotsArray(ctx context.Context, project string) error {
	partitionKey := azcosmos.NewPartitionKeyString(project)
	read, err := s.projects.ReadItem(ctx, partitionKey, project, nil)
	if err != nil {
		if isCosmosStatus(err, http.StatusNotFound) {
			return server.ErrNotFound
		}
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(read.Value, &doc); err != nil {
		return err
	}
	metadata, _ := doc["metadata"].(map[string]any)
	if metadata == nil {
		return nil
	}
	standbyDNS, _ := metadata["native_standby_dns"].(map[string]any)
	if standbyDNS == nil {
		return nil
	}
	if _, present := standbyDNS["slots"]; !present {
		return nil
	}
	delete(standbyDNS, "slots")
	metadata["native_standby_dns"] = standbyDNS
	doc["metadata"] = metadata
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if _, err := s.projects.ReplaceItem(ctx, partitionKey, project, payload, nil); err != nil {
		return err
	}
	return nil
}

// SetProjectTestEnvironmentSlotStatus and its IfMatch sibling were
// retired with the slot-storage rework. Slot status now lives in the
// `slots` collection; writes go through Store.UpdateIfMatch.

// ReadProject performs a Cosmos point read for one project doc, returning
// the captured resource etag on the Project so callers can do etag-conditional
// writes. Use this (rather than ListProjects) when you intend to race for an
// optimistic-concurrency claim — list queries don't expose per-row etags.
func (s *Store) ReadProject(ctx context.Context, project string) (server.Project, error) {
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
	p, err := projectFromMap(doc)
	if err != nil {
		return server.Project{}, err
	}
	return p.WithETag(string(read.ETag)), nil
}

// The legacy slot-status writers that produced
// project.metadata.native_standby_dns.slots entries have been removed.
// Slot state lives in the `slots` Cosmos collection, written
// exclusively through SlotStore.UpdateIfMatch. The boot migration in
// internal/server/slot_migration.go strips the embedded array; this
// PATCH-count handler no longer rewrites it.

func (s *Store) readProjectDoc(ctx context.Context, project string) (projectDoc, error) {
	read, err := s.projects.ReadItem(ctx, azcosmos.NewPartitionKeyString(project), project, nil)
	if err != nil {
		if isCosmosStatus(err, http.StatusNotFound) {
			return projectDoc{}, server.ErrNotFound
		}
		return projectDoc{}, err
	}
	var doc projectDoc
	if err := json.Unmarshal(read.Value, &doc); err != nil {
		return projectDoc{}, err
	}
	return doc, nil
}

func (s *Store) ListWorkflows(ctx context.Context) ([]server.Workflow, error) {
	var docs []workflowDoc
	if err := crossPartitionQuery(ctx, s.workflows, "SELECT * FROM c", nil, &docs); err != nil {
		return nil, err
	}
	rows := make([]server.Workflow, 0, len(docs))
	for _, doc := range docs {
		if isWorkflowSchemaDoc(doc) {
			continue
		}
		rows = append(rows, workflowFromDoc(doc))
	}
	return rows, nil
}

func (s *Store) UpsertWorkflow(ctx context.Context, req server.WorkflowRegister) (server.Workflow, error) {
	projectDoc, err := s.readProjectDoc(ctx, req.Project)
	if errors.Is(err, server.ErrNotFound) {
		return server.Workflow{}, server.ValidationError{Message: "project " + req.Project + " does not exist; register it first"}
	}
	if err != nil {
		return server.Workflow{}, err
	}
	normalizeWorkflowRegisterForProjectDoc(&req, projectDoc)
	if err := validateWorkflowForProject(projectDoc, req); err != nil {
		return server.Workflow{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc := workflowDocFromRegister(req, now)
	doc.Kind = "workflow"
	doc.SchemaRef = workflowSchemaRef(doc)
	schemaDoc := workflowSchemaDocFromWorkflow(doc)
	pk := azcosmos.NewPartitionKeyString(req.Project)
	if err := s.createWorkflowSchemaIfMissing(ctx, pk, schemaDoc); err != nil {
		return server.Workflow{}, err
	}
	existing, err := s.workflows.ReadItem(ctx, pk, req.Name, nil)
	if err == nil {
		var existingDoc workflowDoc
		if err := json.Unmarshal(existing.Value, &existingDoc); err == nil && existingDoc.CreatedAt != "" {
			doc.CreatedAt = existingDoc.CreatedAt
		}
		payload, err := json.Marshal(doc)
		if err != nil {
			return server.Workflow{}, err
		}
		if _, err := s.workflows.ReplaceItem(ctx, pk, req.Name, payload, nil); err != nil {
			return server.Workflow{}, err
		}
		return workflowFromDoc(doc), nil
	}
	if !isCosmosStatus(err, http.StatusNotFound) {
		return server.Workflow{}, err
	}

	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Workflow{}, err
	}
	if _, err := s.workflows.CreateItem(ctx, pk, payload, nil); err != nil {
		return server.Workflow{}, err
	}
	return workflowFromDoc(doc), nil
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

func (s *Store) PatchWorkflow(ctx context.Context, project string, name string, req server.WorkflowPatchRequest) (server.Workflow, error) {
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
	reg := workflowRegisterFromDoc(doc)
	if req.PREnabled != nil {
		reg.PR.Enabled = *req.PREnabled
	}
	if req.BudgetTotal != nil {
		reg.Budget.Total = *req.BudgetTotal
	}
	return s.UpsertWorkflow(ctx, reg)
}

func (s *Store) ListLeases(ctx context.Context) ([]server.Lease, error) {
	var docs []leaseDoc
	if err := crossPartitionQuery(ctx, s.leases, "SELECT * FROM c", nil, &docs); err != nil {
		return nil, err
	}
	rows := make([]server.Lease, 0, len(docs))
	for _, doc := range docs {
		lease, ok := listedLeaseFromDoc(doc)
		if !ok {
			continue
		}
		rows = append(rows, lease)
	}
	return rows, nil
}

func (s *Store) ReadLeaseByCallbackToken(ctx context.Context, token string) (server.Lease, error) {
	doc, err := s.readLeaseDocByCallbackToken(ctx, token)
	if err != nil {
		return server.Lease{}, err
	}
	return leaseFromDoc(doc), nil
}

func (s *Store) HeartbeatLeaseByCallbackToken(ctx context.Context, token string) (server.Lease, error) {
	doc, err := s.readLeaseDocByCallbackToken(ctx, token)
	if err != nil {
		return server.Lease{}, err
	}
	if doc.State != "claimed" {
		return server.Lease{}, server.ErrInactive
	}
	return leaseFromDoc(doc), nil
}

func (s *Store) ReleaseLeaseByCallbackToken(ctx context.Context, token string) (server.Lease, error) {
	doc, err := s.readLeaseDocByCallbackToken(ctx, token)
	if err != nil {
		return server.Lease{}, err
	}
	if doc.State == "released" || doc.State == "expired" {
		lease := leaseFromDoc(doc)
		if boolValue(lease.Metadata["native_k8s"]) && !boolValue(lease.Metadata["test_slot_checkout"]) {
			_ = s.releaseNativeSlotReservation(ctx, lease, time.Now().UTC())
		}
		return lease, nil
	}
	if boolValue(doc.Metadata["test_slot_checkout"]) {
		return server.Lease{}, server.ErrUnsupported
	}

	now := time.Now().UTC()
	doc.State = "released"
	doc.ReleasedAt = now.Format(time.RFC3339Nano)
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Lease{}, err
	}
	partitionKey := azcosmos.NewPartitionKeyString(doc.Project)
	if _, err := s.leases.ReplaceItem(ctx, partitionKey, doc.ID, payload, nil); err != nil {
		return server.Lease{}, err
	}
	lease := leaseFromDoc(doc)
	if boolValue(lease.Metadata["native_k8s"]) && !boolValue(lease.Metadata["test_slot_checkout"]) {
		_ = s.releaseNativeSlotReservation(ctx, lease, now)
	}
	return lease, nil
}

func (s *Store) ListProjectRuns(ctx context.Context, project string, limit int) ([]server.RunReport, error) {
	var docs []runDoc
	if err := singlePartitionQuery(
		ctx,
		s.runs,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @project ORDER BY c.updated_at DESC",
		[]azcosmos.QueryParameter{{Name: "@project", Value: project}},
		&docs,
	); err != nil {
		return nil, err
	}
	if limit < len(docs) {
		docs = docs[:limit]
	}
	return runReportsFromDocs(docs), nil
}

func (s *Store) GetRunReportByNumber(ctx context.Context, project string, issueNumber int, runNumber string) (server.RunReport, error) {
	docs, err := s.issueRunDocs(ctx, project, issueNumber)
	if err != nil {
		return server.RunReport{}, err
	}
	numbers := runNumberMap(docs)
	for _, doc := range docs {
		display := ""
		if doc.RunDisplayNumber != nil {
			display = strings.TrimSpace(*doc.RunDisplayNumber)
		}
		if display != "" && display == strings.TrimSpace(runNumber) {
			return runReportFromDoc(doc, runRefMapFromDocs(docs)), nil
		}
		if fmt.Sprintf("%d", numbers[doc.ID]) == strings.TrimSpace(runNumber) {
			return runReportFromDoc(doc, runRefMapFromDocs(docs)), nil
		}
	}
	return server.RunReport{}, server.ErrNotFound
}

func (s *Store) ListIssues(ctx context.Context, filter server.IssueListFilter) ([]server.IssueRow, error) {
	state := strings.ToLower(strings.TrimSpace(firstNonEmpty(filter.State, "open")))
	if state != "open" && state != "closed" && state != "all" {
		return nil, server.ValidationError{Message: fmt.Sprintf("state must be 'open', 'closed', or 'all', not %q", filter.State)}
	}
	issues, err := s.listIssueDocs(ctx)
	if err != nil {
		return nil, err
	}
	runDocs, err := s.listRunDocs(ctx)
	if err != nil {
		return nil, err
	}
	// Lock state read moved from cosmos.locks container to pg.LocksStore
	// in Stage 2b. ListHeldByScope returns only currently-held + unexpired
	// locks, so the per-row check below collapses to a simple map lookup.
	heldIssueLocks, err := s.pgLocks.ListHeldByScope(ctx, "issue")
	if err != nil {
		return nil, err
	}
	runContext := issueRunContext(runDocs)

	now := time.Now().UTC()
	_ = now // retained for any future per-row time math; locks no longer need it.
	rows := make([]server.IssueRow, 0, len(issues))
	for _, issue := range issues {
		if filter.Project != "" && issue.Project != filter.Project {
			continue
		}
		if state != "all" && firstNonEmpty(issue.State, "open") != state {
			continue
		}
		row := issueRowFromDoc(issue)
		run := runContext.latestByIssueID[issue.ID]
		if run == nil {
			run = runContext.latestByProjectNumber[fmt.Sprintf("%s#%d", issue.Project, issue.Number)]
		}
		if run != nil {
			numbers := runContext.numbersByIssue[fmt.Sprintf("%s#%d", run.Project, run.IssueNumber)]
			runNumber := run.RunNumber
			if runNumber == nil {
				if value := numbers[run.ID]; value > 0 {
					runNumber = &value
				}
			}
			display := runDisplayNumber(*run, numbers[run.ID])
			row.LastRunNumber = runNumber
			row.LastRunRef = optionalNonEmptyStringPtr(publicids.RunRef(issue.Project, &issue.Number, display))
			row.LastRunState = optionalNonEmptyStringPtr(run.State)
			row.LastRunAbortReason = emptyStringNil(run.AbortReason)
			if row.Workflow == nil {
				row.Workflow = optionalNonEmptyStringPtr(run.Workflow)
			}
		}
		if filter.Workflow != "" && (row.Workflow == nil || *row.Workflow != filter.Workflow) {
			continue
		}
		if _, held := heldIssueLocks[fmt.Sprintf("%s#%d", issue.Project, issue.Number)]; held {
			row.IssueLockHeld = true
		}
		if filter.NeedsAttention && !issueRowNeedsAttention(row) {
			continue
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Project != rows[j].Project {
			return rows[i].Project < rows[j].Project
		}
		left, right := 0, 0
		if rows[i].Number != nil {
			left = *rows[i].Number
		}
		if rows[j].Number != nil {
			right = *rows[j].Number
		}
		if left != right {
			return left > right
		}
		return rows[i].Ref < rows[j].Ref
	})
	if filter.Limit != nil && *filter.Limit < len(rows) {
		rows = rows[:*filter.Limit]
	}
	return rows, nil
}

func (s *Store) GetIssueDetailByNumber(ctx context.Context, project string, number int) (server.IssueDetail, error) {
	issue, err := s.readIssueByNumber(ctx, project, number)
	if err != nil {
		return server.IssueDetail{}, err
	}
	detail := issueDetailFromDoc(issue)
	latestRun, runDocs, err := s.latestRunForIssue(ctx, issue)
	if err != nil {
		return server.IssueDetail{}, err
	}
	if latestRun != nil {
		numbers := runNumberMap(runDocs)
		runNumber := latestRun.RunNumber
		if runNumber == nil {
			if value := numbers[latestRun.ID]; value > 0 {
				runNumber = &value
			}
		}
		display := runDisplayNumber(*latestRun, numbers[latestRun.ID])
		detail.LastRunNumber = runNumber
		detail.LastRunRef = optionalNonEmptyStringPtr(publicids.RunRef(issue.Project, &issue.Number, display))
		detail.LastRunState = optionalNonEmptyStringPtr(latestRun.State)
	}
	held, err := s.pgLocks.IssueLockHeld(ctx, issue.Project, issue.Number)
	if err != nil {
		return server.IssueDetail{}, err
	}
	detail.IssueLockHeld = held
	return detail, nil
}

func (s *Store) ArchiveIssueByNumber(ctx context.Context, req server.IssueArchive) (server.IssueDetail, error) {
	doc, err := s.readIssueByNumber(ctx, req.Project, req.Number)
	if err != nil {
		return server.IssueDetail{}, err
	}
	note := capitalize(req.Action)
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		note = note + ": " + reason
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc.Comments = append(doc.Comments, issueCommentDoc{
		ID:        uuid.NewString(),
		Author:    req.Author,
		Body:      note,
		CreatedAt: now,
		UpdatedAt: now,
	})
	doc.UpdatedAt = now
	if doc.State == "open" || doc.State == "" {
		doc.State = "closed"
		doc.ClosedAt = &now
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.IssueDetail{}, err
	}
	if _, err := s.issues.ReplaceItem(ctx, azcosmos.NewPartitionKeyString(doc.Project), doc.ID, payload, nil); err != nil {
		return server.IssueDetail{}, err
	}
	return s.GetIssueDetailByNumber(ctx, doc.Project, doc.Number)
}

const issueCounterPrefix = "__counter:issue-number:"
const canonicalIssuePredicate = "IS_DEFINED(c.number) AND c.number > 0 AND (c.state = 'open' OR c.state = 'closed')"

type issueNumberCounterDoc struct {
	ID              string `json:"id"`
	Project         string `json:"project"`
	Kind            string `json:"kind"`
	NextIssueNumber int    `json:"next_issue_number"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func (s *Store) nextIssueNumber(ctx context.Context, project string) (int, error) {
	counterID := issueCounterPrefix + project
	pk := azcosmos.NewPartitionKeyString(project)
	for range 3 {
		resp, err := s.issues.ReadItem(ctx, pk, counterID, nil)
		if isCosmosStatus(err, http.StatusNotFound) {
			// seed from highest existing number
			highest, seedErr := s.highestIssueNumber(ctx, project)
			if seedErr != nil {
				return 0, seedErr
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			seed := issueNumberCounterDoc{
				ID:              counterID,
				Project:         project,
				Kind:            "issue_number_counter",
				NextIssueNumber: highest + 2,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			payload, marshalErr := json.Marshal(seed)
			if marshalErr != nil {
				return 0, marshalErr
			}
			_, createErr := s.issues.CreateItem(ctx, pk, payload, nil)
			if isCosmosStatus(createErr, http.StatusConflict) {
				continue
			}
			if createErr != nil {
				return 0, createErr
			}
			return highest + 1, nil
		}
		if err != nil {
			return 0, err
		}
		var counter issueNumberCounterDoc
		if unmarshalErr := json.Unmarshal(resp.Value, &counter); unmarshalErr != nil {
			return 0, unmarshalErr
		}
		allocated := counter.NextIssueNumber - 1
		if allocated < 1 {
			allocated = 1
		}
		counter.NextIssueNumber = allocated + 2
		counter.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		payload, marshalErr := json.Marshal(counter)
		if marshalErr != nil {
			return 0, marshalErr
		}
		etag := resp.ETag
		_, replErr := s.issues.ReplaceItem(ctx, pk, counterID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag})
		if isCosmosStatus(replErr, http.StatusPreconditionFailed) {
			continue
		}
		if replErr != nil {
			return 0, replErr
		}
		return allocated + 1, nil
	}
	return 0, errors.New("issue number counter conflict after retries")
}

func (s *Store) highestIssueNumber(ctx context.Context, project string) (int, error) {
	var docs []issueDoc
	if err := singlePartitionQuery(
		ctx,
		s.issues,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @project AND "+canonicalIssuePredicate,
		[]azcosmos.QueryParameter{{Name: "@project", Value: project}},
		&docs,
	); err != nil {
		return 0, err
	}
	highest := 0
	for _, d := range docs {
		if d.Number > highest {
			highest = d.Number
		}
	}
	return highest, nil
}

func (s *Store) CreateIssue(ctx context.Context, req server.IssueCreate) (server.IssueDetail, error) {
	number, err := s.nextIssueNumber(ctx, req.Project)
	if err != nil {
		return server.IssueDetail{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc := issueDoc{
		ID:      uuid.NewString(),
		Number:  number,
		Project: req.Project,
		Title:   req.Title,
		Body:    req.Body,
		Labels:  sliceOrEmpty(req.Labels),
		State:   "open",
		Metadata: issueMetadataDoc{
			Workflow: req.Workflow,
		},
		Comments:  []issueCommentDoc{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.IssueDetail{}, err
	}
	if _, err := s.issues.CreateItem(ctx, azcosmos.NewPartitionKeyString(req.Project), payload, nil); err != nil {
		return server.IssueDetail{}, err
	}
	return s.GetIssueDetailByNumber(ctx, req.Project, number)
}

func (s *Store) PatchIssueByNumber(ctx context.Context, req server.IssuePatch) (server.IssueDetail, error) {
	doc, err := s.readIssueByNumber(ctx, req.Project, req.Number)
	if err != nil {
		return server.IssueDetail{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if req.Title != nil {
		doc.Title = *req.Title
	}
	if req.Body != nil {
		doc.Body = *req.Body
	}
	if req.Labels != nil {
		doc.Labels = *req.Labels
	}
	if req.State != nil {
		target := strings.ToLower(*req.State)
		switch target {
		case "closed":
			if doc.State != "closed" {
				doc.State = "closed"
				doc.ClosedAt = &now
			}
		case "open":
			if doc.State == "closed" {
				doc.State = "open"
				doc.ClosedAt = nil
			}
		default:
			return server.IssueDetail{}, server.ValidationError{Message: "state must be 'open' or 'closed'"}
		}
	}
	doc.UpdatedAt = now
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.IssueDetail{}, err
	}
	if _, err := s.issues.ReplaceItem(ctx, azcosmos.NewPartitionKeyString(doc.Project), doc.ID, payload, nil); err != nil {
		return server.IssueDetail{}, err
	}
	return s.GetIssueDetailByNumber(ctx, doc.Project, doc.Number)
}

func (s *Store) AddIssueComment(ctx context.Context, req server.IssueCommentAdd) (server.IssueComment, error) {
	doc, err := s.readIssueByNumber(ctx, req.Project, req.Number)
	if err != nil {
		return server.IssueComment{}, err
	}
	if strings.TrimSpace(req.Body) == "" {
		return server.IssueComment{}, server.ValidationError{Message: "body required"}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	comment := issueCommentDoc{
		ID:        uuid.NewString(),
		Author:    req.Author,
		Body:      req.Body,
		CreatedAt: now,
		UpdatedAt: now,
	}
	doc.Comments = append(doc.Comments, comment)
	doc.UpdatedAt = now
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.IssueComment{}, err
	}
	if _, err := s.issues.ReplaceItem(ctx, azcosmos.NewPartitionKeyString(doc.Project), doc.ID, payload, nil); err != nil {
		return server.IssueComment{}, err
	}
	t, _ := time.Parse(time.RFC3339Nano, comment.CreatedAt)
	return server.IssueComment{
		ID:        comment.ID,
		Author:    comment.Author,
		Body:      comment.Body,
		CreatedAt: t,
		UpdatedAt: t,
	}, nil
}

func (s *Store) UpdateIssueComment(ctx context.Context, req server.IssueCommentUpdate) (server.IssueComment, error) {
	doc, err := s.readIssueByNumber(ctx, req.Project, req.Number)
	if err != nil {
		return server.IssueComment{}, err
	}
	if strings.TrimSpace(req.Body) == "" {
		return server.IssueComment{}, server.ValidationError{Message: "body required"}
	}
	idx := -1
	for i, c := range doc.Comments {
		if c.ID == req.CommentID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return server.IssueComment{}, server.ErrNotFound
	}
	if doc.Comments[idx].Author != req.Author {
		return server.IssueComment{}, server.ErrForbidden
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc.Comments[idx].Body = req.Body
	doc.Comments[idx].UpdatedAt = now
	doc.UpdatedAt = now
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.IssueComment{}, err
	}
	if _, err := s.issues.ReplaceItem(ctx, azcosmos.NewPartitionKeyString(doc.Project), doc.ID, payload, nil); err != nil {
		return server.IssueComment{}, err
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, doc.Comments[idx].CreatedAt)
	updatedAt, _ := time.Parse(time.RFC3339Nano, now)
	return server.IssueComment{
		ID:        doc.Comments[idx].ID,
		Author:    doc.Comments[idx].Author,
		Body:      doc.Comments[idx].Body,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

func (s *Store) DeleteIssueComment(ctx context.Context, req server.IssueCommentDelete) (server.IssueDetail, error) {
	doc, err := s.readIssueByNumber(ctx, req.Project, req.Number)
	if err != nil {
		return server.IssueDetail{}, err
	}
	found := false
	filtered := doc.Comments[:0]
	for _, c := range doc.Comments {
		if c.ID == req.CommentID {
			found = true
			continue
		}
		filtered = append(filtered, c)
	}
	if !found {
		return server.IssueDetail{}, server.ErrNotFound
	}
	doc.Comments = filtered
	doc.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.IssueDetail{}, err
	}
	if _, err := s.issues.ReplaceItem(ctx, azcosmos.NewPartitionKeyString(doc.Project), doc.ID, payload, nil); err != nil {
		return server.IssueDetail{}, err
	}
	return s.GetIssueDetailByNumber(ctx, doc.Project, doc.Number)
}

func (s *Store) readIssueByNumber(ctx context.Context, project string, number int) (issueDoc, error) {
	var docs []issueDoc
	if err := singlePartitionQuery(
		ctx,
		s.issues,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @project AND c.number = @number AND "+canonicalIssuePredicate,
		[]azcosmos.QueryParameter{
			{Name: "@project", Value: project},
			{Name: "@number", Value: number},
		},
		&docs,
	); err != nil {
		return issueDoc{}, err
	}
	if len(docs) == 0 {
		return issueDoc{}, server.ErrNotFound
	}
	return docs[0], nil
}

func (s *Store) listIssueDocs(ctx context.Context) ([]issueDoc, error) {
	var docs []issueDoc
	if err := crossPartitionQuery(ctx, s.issues, "SELECT * FROM c WHERE "+canonicalIssuePredicate, nil, &docs); err != nil {
		return nil, err
	}
	return canonicalIssueDocs(docs), nil
}

func canonicalIssueDocs(docs []issueDoc) []issueDoc {
	filtered := docs[:0]
	for _, doc := range docs {
		if !isCanonicalIssueDoc(doc) {
			continue
		}
		filtered = append(filtered, doc)
	}
	return filtered
}

func isCanonicalIssueDoc(doc issueDoc) bool {
	if doc.ID == "" || doc.Project == "" || doc.Number <= 0 {
		return false
	}
	return doc.State == "open" || doc.State == "closed"
}

func (s *Store) listRunDocs(ctx context.Context) ([]runDoc, error) {
	var docs []runDoc
	if err := crossPartitionQuery(ctx, s.runs, "SELECT * FROM c", nil, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}

// ListAllLockDocsForMigration reads every lock document in the cosmos
// `locks` container and returns it in the narrow shape pg.LocksStore's
// Migrate path expects. Runs once per pod start (idempotent via ON
// CONFLICT DO NOTHING on the receiving side). Stage 2i removes this
// method along with the cosmos lock container client entirely.
//
// "All scopes" here means every doc in the container regardless of
// scope. The user opted for "full copy of everything" in the migration
// plan, so released and expired rows are preserved too.
func (s *Store) ListAllLockDocsForMigration(ctx context.Context) ([]pgstore.LockMigrationRow, error) {
	if s == nil || s.locks == nil {
		return nil, nil
	}
	var docs []lockDoc
	if err := crossPartitionQuery(ctx, s.locks, "SELECT * FROM c", nil, &docs); err != nil {
		return nil, err
	}
	out := make([]pgstore.LockMigrationRow, 0, len(docs))
	for _, doc := range docs {
		row := pgstore.LockMigrationRow{
			Scope: doc.Scope,
			Key:   doc.Key,
			State: doc.State,
		}
		if doc.HeldBy != nil {
			row.HolderID = *doc.HeldBy
		}
		if expires := parseOptionalTime(doc.ExpiresAt); expires != nil {
			row.ExpiresAt = expires
		}
		if claimed := parseOptionalTime(doc.ClaimedAt); claimed != nil {
			row.ClaimedAt = claimed
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *Store) latestRunForIssue(ctx context.Context, issue issueDoc) (*runDoc, []runDoc, error) {
	var docs []runDoc
	if issue.ID != "" {
		if err := singlePartitionQuery(
			ctx,
			s.runs,
			azcosmos.NewPartitionKeyString(issue.Project),
			"SELECT * FROM c WHERE c.project = @project AND c.issue_id = @issue_id",
			[]azcosmos.QueryParameter{
				{Name: "@project", Value: issue.Project},
				{Name: "@issue_id", Value: issue.ID},
			},
			&docs,
		); err != nil {
			return nil, nil, err
		}
	}
	if len(docs) == 0 {
		var err error
		docs, err = s.issueRunDocs(ctx, issue.Project, issue.Number)
		if err != nil {
			return nil, nil, err
		}
	}
	if len(docs) == 0 {
		return nil, docs, nil
	}
	sort.SliceStable(docs, func(i, j int) bool {
		return docs[i].CreatedAt < docs[j].CreatedAt
	})
	latest := docs[len(docs)-1]
	return &latest, docs, nil
}

func (s *Store) issueRunDocs(ctx context.Context, project string, issueNumber int) ([]runDoc, error) {
	var docs []runDoc
	if err := singlePartitionQuery(
		ctx,
		s.runs,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @project AND c.issue_number = @issue_number ORDER BY c.created_at ASC",
		[]azcosmos.QueryParameter{
			{Name: "@project", Value: project},
			{Name: "@issue_number", Value: issueNumber},
		},
		&docs,
	); err != nil {
		return nil, err
	}
	sort.SliceStable(docs, func(i, j int) bool {
		return docs[i].CreatedAt < docs[j].CreatedAt
	})
	return docs, nil
}

func (s *Store) readLeaseDocByCallbackToken(ctx context.Context, token string) (leaseDoc, error) {
	var docs []leaseDoc
	if err := crossPartitionQuery(
		ctx,
		s.leases,
		"SELECT * FROM c WHERE c.metadata.lease_callback_token = @token",
		[]azcosmos.QueryParameter{{Name: "@token", Value: token}},
		&docs,
	); err != nil {
		return leaseDoc{}, err
	}
	if len(docs) == 0 {
		return leaseDoc{}, server.ErrNotFound
	}
	if len(docs) > 1 {
		return leaseDoc{}, server.ErrConflict
	}
	return docs[0], nil
}

// Cosmos query primitives live in query.go. The legacy queryAll /
// queryAllWhere helpers that defaulted to an empty partition key were
// deleted as part of the queryAllWhere → singlePartitionQuery migration.
// See docs/cosmos-partition-strategy.md and the migration guard at
// scripts/check-cosmos-queries.sh.

type projectDoc struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind,omitempty"`
	Name       string         `json:"name"`
	GitHubRepo string         `json:"githubRepo"`
	ArgoCDApp  string         `json:"argocdApp"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  string         `json:"createdAt"`
}

type projectWriteDoc struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind,omitempty"`
	Name       string         `json:"name"`
	GitHubRepo string         `json:"githubRepo"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  string         `json:"createdAt"`
}

type testLeaseDefaultsDoc struct {
	ID                   string `json:"id"`
	Kind                 string `json:"kind"`
	GlobalTTLSeconds     int    `json:"globalTTLSeconds,omitempty"`
	HotSwapMinTTLSeconds int    `json:"hotSwapMinTTLSeconds,omitempty"`
	CreatedAt            string `json:"createdAt"`
	UpdatedAt            string `json:"updatedAt"`
}

type workflowDoc struct {
	ID                  string         `json:"id"`
	Kind                string         `json:"kind,omitempty"`
	Project             string         `json:"project"`
	Name                string         `json:"name"`
	SchemaRef           string         `json:"schema_ref,omitempty"`
	Phases              []phaseDoc     `json:"phases"`
	PR                  prDoc          `json:"pr"`
	Budget              budgetDoc      `json:"budget"`
	DefaultRequirements map[string]any `json:"defaultRequirements"`
	Metadata            map[string]any `json:"metadata"`
	CreatedAt           string         `json:"createdAt"`
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

type runDoc struct {
	ID                  string              `json:"id"`
	Project             string              `json:"project"`
	Workflow            string              `json:"workflow"`
	WorkflowSchemaRef   string              `json:"workflow_schema_ref,omitempty"`
	RunNumber           *int                `json:"run_number"`
	RunCycleNumber      *int                `json:"run_cycle_number,omitempty"`
	RunDisplayNumber    *string             `json:"run_display_number"`
	ParentRunID         *string             `json:"parent_run_id"`
	RootRunID           *string             `json:"root_run_id"`
	OriginKind          *string             `json:"origin_kind"`
	IsCycle             bool                `json:"is_cycle"`
	CycleNumber         *int                `json:"cycle_number"`
	IssueID             string              `json:"issue_id"`
	IssueRepo           string              `json:"issue_repo"`
	IssueNumber         int                 `json:"issue_number"`
	PRNumber            *int                `json:"pr_number"`
	State               string              `json:"state"`
	QueueState          *string             `json:"queue_state,omitempty"`
	AdmissionError      *string             `json:"admission_error,omitempty"`
	SlotLeaseRef        *string             `json:"slot_lease_ref,omitempty"`
	Attempts            []attemptDoc        `json:"attempts"`
	PhaseExecutions     []phaseExecutionDoc `json:"phase_executions,omitempty"`
	CumulativeCostUSD   float64             `json:"cumulative_cost_usd"`
	Budget              *budgetDoc          `json:"budget,omitempty"`
	ValidationURL       *string             `json:"validation_url"`
	ScreenshotsMarkdown *string             `json:"screenshots_markdown"`
	AbortReason         *string             `json:"abort_reason"`
	EntrypointPhase     *string             `json:"entrypoint_phase,omitempty"`
	TriggerSource       map[string]any      `json:"trigger_source"`
	CreatedAt           string              `json:"created_at"`
	UpdatedAt           string              `json:"updated_at"`
	// Fields for mutation operations (populated from Cosmos documents as needed).
	CallbackToken     *string `json:"callback_token,omitempty"`
	IssueLockHolderID *string `json:"issue_lock_holder_id,omitempty"`
	PRLockHolderID    *string `json:"pr_lock_holder_id,omitempty"`
}

type phaseExecutionDoc struct {
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	State        string            `json:"state"`
	Reason       *string           `json:"reason,omitempty"`
	CreatedAt    string            `json:"created_at"`
	DispatchedAt *string           `json:"dispatched_at,omitempty"`
	StartedAt    *string           `json:"started_at,omitempty"`
	CompletedAt  *string           `json:"completed_at,omitempty"`
	Jobs         []jobExecutionDoc `json:"jobs"`
}

type jobExecutionDoc struct {
	ID           string             `json:"id"`
	Name         *string            `json:"name,omitempty"`
	State        string             `json:"state"`
	Reason       *string            `json:"reason,omitempty"`
	K8sJobName   *string            `json:"k8s_job_name,omitempty"`
	CreatedAt    string             `json:"created_at"`
	DispatchedAt *string            `json:"dispatched_at,omitempty"`
	StartedAt    *string            `json:"started_at,omitempty"`
	CompletedAt  *string            `json:"completed_at,omitempty"`
	Steps        []stepExecutionDoc `json:"steps"`
}

type stepExecutionDoc struct {
	Slug        string  `json:"slug"`
	Title       *string `json:"title,omitempty"`
	State       string  `json:"state"`
	Reason      *string `json:"reason,omitempty"`
	ExitCode    *int    `json:"exit_code,omitempty"`
	CreatedAt   string  `json:"created_at"`
	StartedAt   *string `json:"started_at,omitempty"`
	CompletedAt *string `json:"completed_at,omitempty"`
}

type issueDoc struct {
	ID        string            `json:"id"`
	Number    int               `json:"number"`
	Project   string            `json:"project"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Labels    []string          `json:"labels"`
	State     string            `json:"state"`
	Metadata  issueMetadataDoc  `json:"metadata"`
	Comments  []issueCommentDoc `json:"comments"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
	ClosedAt  *string           `json:"closed_at,omitempty"`
}

type issueMetadataDoc struct {
	Workflow *string `json:"workflow"`
}

type issueCommentDoc struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type lockDoc struct {
	ID              string         `json:"id"`
	Scope           string         `json:"scope"`
	Key             string         `json:"key"`
	State           string         `json:"state"`
	HeldBy          *string        `json:"held_by,omitempty"`
	ClaimedAt       string         `json:"claimed_at,omitempty"`
	ExpiresAt       string         `json:"expires_at"`
	LastHeartbeatAt string         `json:"last_heartbeat_at,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

type attemptDoc struct {
	AttemptIndex          int                               `json:"attempt_index"`
	Phase                 string                            `json:"phase"`
	PhaseKind             string                            `json:"phase_kind"`
	WorkflowFilename      string                            `json:"workflow_filename"`
	DispatchedAt          string                            `json:"dispatched_at"`
	CompletedAt           string                            `json:"completed_at"`
	Conclusion            *string                           `json:"conclusion"`
	Verification          *verificationDoc                  `json:"verification"`
	SummaryMarkdown       *string                           `json:"summary_markdown"`
	CostUSD               *float64                          `json:"cost_usd"`
	Decision              *string                           `json:"decision"`
	LogArchiveURL         *string                           `json:"log_archive_url"`
	PhaseOutputs          map[string]string                 `json:"phase_outputs,omitempty"`
	JobCompletions        map[string]nativeJobCompletionDoc `json:"job_completions,omitempty"`
	CarryForward          bool                              `json:"carry_forward,omitempty"`
	CancelRequestedAt     *string                           `json:"cancel_requested_at,omitempty"`
	CancelReason          *string                           `json:"cancel_reason,omitempty"`
	CapabilityTokenSHA256 *string                           `json:"capability_token_sha256,omitempty"`
}

type nativeJobCompletionDoc struct {
	JobID               string            `json:"job_id"`
	CompletedAt         string            `json:"completed_at"`
	Conclusion          string            `json:"conclusion"`
	Verification        *verificationDoc  `json:"verification,omitempty"`
	SummaryMarkdown     *string           `json:"summary_markdown,omitempty"`
	ScreenshotsMarkdown *string           `json:"screenshots_markdown,omitempty"`
	CostUSD             float64           `json:"cost_usd,omitempty"`
	PhaseOutputs        map[string]string `json:"phase_outputs,omitempty"`
}

type nativeEventDoc struct {
	ID           string         `json:"id"`
	Project      string         `json:"project"`
	RunID        string         `json:"run_id"`
	AttemptIndex int            `json:"attempt_index"`
	Phase        string         `json:"phase"`
	JobID        string         `json:"job_id"`
	Seq          int            `json:"seq"`
	Event        string         `json:"event"`
	StepSlug     string         `json:"step_slug"`
	Message      string         `json:"message"`
	ExitCode     *int           `json:"exit_code"`
	Metadata     map[string]any `json:"metadata"`
	CreatedAt    string         `json:"created_at"`
}

type verificationDoc struct {
	Status       string   `json:"status"`
	Reasons      []string `json:"reasons"`
	EvidenceRefs []string `json:"evidence_refs"`
	CostUSD      float64  `json:"cost_usd"`
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
	ID               string              `json:"id"`
	Name             *string             `json:"name"`
	Image            string              `json:"image"`
	Command          []string            `json:"command"`
	Args             []string            `json:"args"`
	Env              map[string]string   `json:"env"`
	Steps            []nativeStepDoc     `json:"steps"`
	TimeoutSeconds   *int                `json:"timeoutSeconds"`
	Managed          bool                `json:"managed,omitempty"`
	Checkout         *nativeCheckoutDoc  `json:"checkout,omitempty"`
	ExtraCheckouts   []nativeCheckoutDoc `json:"extraCheckouts,omitempty"`
	WorkingDirectory string              `json:"workingDirectory,omitempty"`
	Shell            string              `json:"shell,omitempty"`
}

type nativeStepDoc struct {
	Slug             string            `json:"slug"`
	Title            *string           `json:"title"`
	Type             string            `json:"type,omitempty"`
	Run              string            `json:"run,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	WorkingDirectory string            `json:"workingDirectory,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
}

type nativeCheckoutDoc struct {
	Repo string `json:"repo,omitempty"`
	Ref  string `json:"ref,omitempty"`
	Path string `json:"path,omitempty"`
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

	return server.Workflow{
		ID:                  firstNonEmpty(doc.ID, doc.Name),
		Project:             doc.Project,
		Name:                doc.Name,
		SchemaRef:           firstNonEmpty(doc.SchemaRef, workflowSchemaRef(doc)),
		Phases:              phases,
		PR:                  prFromDoc(doc.PR),
		Budget:              budget.Config{Total: defaultBudgetTotal(doc.Budget.Total)},
		DefaultRequirements: mapOrEmpty(doc.DefaultRequirements),
		Metadata:            mapOrEmpty(doc.Metadata),
		CreatedAt:           parseTimeOrNow(doc.CreatedAt),
	}
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

func listedLeaseFromDoc(doc leaseDoc) (server.Lease, bool) {
	if isLeaseBookkeepingDoc(doc) {
		return server.Lease{}, false
	}
	return leaseFromDoc(doc), true
}

func isLeaseBookkeepingDoc(doc leaseDoc) bool {
	return doc.Kind == "lease_number_counter" || strings.HasPrefix(doc.ID, leaseCounterPrefix)
}

func runReportsFromDocs(docs []runDoc) []server.RunReport {
	docsByIssue := map[string][]runDoc{}
	for _, doc := range docs {
		if doc.Project == "" || doc.IssueNumber == 0 {
			continue
		}
		key := fmt.Sprintf("%s#%d", doc.Project, doc.IssueNumber)
		docsByIssue[key] = append(docsByIssue[key], doc)
	}
	lineageByID := map[string]string{}
	for _, group := range docsByIssue {
		sort.SliceStable(group, func(i, j int) bool {
			return group[i].CreatedAt < group[j].CreatedAt
		})
		for id, ref := range runRefMapFromDocs(group) {
			lineageByID[id] = ref
		}
	}
	reports := make([]server.RunReport, 0, len(docs))
	for _, doc := range docs {
		reports = append(reports, runReportFromDoc(doc, lineageByID))
	}
	return reports
}

func issueDetailFromDoc(doc issueDoc) server.IssueDetail {
	comments := make([]server.IssueComment, 0, len(doc.Comments))
	for _, comment := range doc.Comments {
		comments = append(comments, server.IssueComment{
			ID:        comment.ID,
			Author:    comment.Author,
			Body:      comment.Body,
			CreatedAt: parseTimeOrNow(comment.CreatedAt),
			UpdatedAt: parseTimeOrNow(comment.UpdatedAt),
		})
	}
	number := doc.Number
	return server.IssueDetail{
		Ref:      publicids.IssueRef(doc.Project, &number),
		Project:  doc.Project,
		Repo:     nil,
		Number:   &number,
		Title:    doc.Title,
		Body:     doc.Body,
		State:    firstNonEmpty(doc.State, "open"),
		Labels:   sliceOrEmpty(doc.Labels),
		HTMLURL:  nil,
		Comments: comments,
	}
}

func issueRowFromDoc(doc issueDoc) server.IssueRow {
	number := doc.Number
	return server.IssueRow{
		Ref:      publicids.IssueRef(doc.Project, &number),
		Project:  doc.Project,
		Workflow: emptyStringNil(doc.Metadata.Workflow),
		Repo:     nil,
		Number:   &number,
		Title:    doc.Title,
		State:    firstNonEmpty(doc.State, "open"),
		Labels:   sliceOrEmpty(doc.Labels),
		HTMLURL:  nil,
	}
}

type issueRuns struct {
	latestByIssueID       map[string]*runDoc
	latestByProjectNumber map[string]*runDoc
	numbersByIssue        map[string]map[string]int
}

func issueRunContext(docs []runDoc) issueRuns {
	ctx := issueRuns{
		latestByIssueID:       map[string]*runDoc{},
		latestByProjectNumber: map[string]*runDoc{},
		numbersByIssue:        map[string]map[string]int{},
	}
	groups := map[string][]runDoc{}
	for _, doc := range docs {
		if doc.IssueID != "" {
			current := ctx.latestByIssueID[doc.IssueID]
			if current == nil || doc.CreatedAt > current.CreatedAt {
				value := doc
				ctx.latestByIssueID[doc.IssueID] = &value
			}
		}
		if doc.Project != "" && doc.IssueNumber > 0 {
			key := fmt.Sprintf("%s#%d", doc.Project, doc.IssueNumber)
			groups[key] = append(groups[key], doc)
			current := ctx.latestByProjectNumber[key]
			if current == nil || doc.CreatedAt > current.CreatedAt {
				value := doc
				ctx.latestByProjectNumber[key] = &value
			}
		}
	}
	for key, group := range groups {
		sort.SliceStable(group, func(i, j int) bool {
			return group[i].CreatedAt < group[j].CreatedAt
		})
		ctx.numbersByIssue[key] = runNumberMap(group)
	}
	return ctx
}

func issueRowNeedsAttention(row server.IssueRow) bool {
	if row.IssueLockHeld || row.LastRunRef == nil || row.LastRunState == nil {
		return false
	}
	switch *row.LastRunState {
	case "aborted", "review_required", "passed", "failed", "needs_review":
		return true
	default:
		return false
	}
}

func capitalize(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func runRefMapFromDocs(docs []runDoc) map[string]string {
	numbers := runNumberMap(docs)
	refs := map[string]string{}
	for _, doc := range docs {
		if doc.ID == "" {
			continue
		}
		display := runDisplayNumber(doc, numbers[doc.ID])
		refs[doc.ID] = publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), display)
	}
	return refs
}

func runNumberMap(docs []runDoc) map[string]int {
	assigned := map[string]int{}
	used := map[int]bool{}
	for _, doc := range docs {
		n := runLedgerNumber(doc)
		if n <= 0 || used[n] {
			continue
		}
		assigned[doc.ID] = n
		used[n] = true
	}
	next := 1
	for _, doc := range docs {
		if _, ok := assigned[doc.ID]; ok {
			continue
		}
		for used[next] {
			next++
		}
		assigned[doc.ID] = next
		used[next] = true
	}
	return assigned
}

func runLedgerNumber(doc runDoc) int {
	if doc.CycleNumber != nil && *doc.CycleNumber > 0 {
		return *doc.CycleNumber
	}
	if doc.RunNumber != nil && *doc.RunNumber > 0 {
		return *doc.RunNumber
	}
	return 0
}

func runReportFromDoc(doc runDoc, lineageByID map[string]string) server.RunReport {
	numbers := runNumberMap([]runDoc{doc})
	display := runDisplayNumber(doc, numbers[doc.ID])
	runRef := lineageByID[doc.ID]
	if runRef == "" {
		runRef = publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), display)
	}
	attempts := make([]server.RunReportAttempt, 0, len(doc.Attempts))
	var completed *time.Time
	for _, attempt := range doc.Attempts {
		reportAttempt := runReportAttemptFromDoc(attempt, lineageByID)
		if reportAttempt.CompletedAt != nil && (completed == nil || reportAttempt.CompletedAt.After(*completed)) {
			value := *reportAttempt.CompletedAt
			completed = &value
		}
		attempts = append(attempts, reportAttempt)
	}
	if doc.State == "in_progress" {
		completed = nil
	}
	var currentPhase *string
	if len(doc.Attempts) > 0 {
		currentPhase = optionalNonEmptyStringPtr(doc.Attempts[len(doc.Attempts)-1].Phase)
	}
	parentID := doc.ParentRunID
	rootID := doc.RootRunID
	if (rootID == nil || *rootID == "") && parentID != nil && *parentID != "" {
		rootID = parentID
	}
	originKind := doc.OriginKind
	if (originKind == nil || *originKind == "") && parentID != nil && *parentID != "" {
		if value := stringValue(doc.TriggerSource["kind"]); value != "" {
			originKind = optionalNonEmptyStringPtr(value)
		} else {
			originKind = optionalNonEmptyStringPtr("cycle")
		}
	}
	return server.RunReport{
		ID:                  doc.ID,
		Ref:                 runRef + "/report",
		Project:             doc.Project,
		RunRef:              runRef,
		RunNumber:           doc.RunNumber,
		RunDisplayNumber:    optionalNonEmptyStringPtr(display),
		ParentRunRef:        refPtr(lineageByID, parentID),
		RootRunRef:          refPtr(lineageByID, rootID),
		OriginKind:          emptyStringNil(originKind),
		EntrypointPhase:     emptyStringNil(doc.EntrypointPhase),
		IsCycle:             doc.IsCycle,
		CycleNumber:         doc.CycleNumber,
		RunCycleNumber:      doc.RunCycleNumber,
		WorkflowSchemaRef:   doc.WorkflowSchemaRef,
		QueueState:          emptyStringNil(doc.QueueState),
		AdmissionError:      emptyStringNil(doc.AdmissionError),
		SlotLeaseRef:        emptyStringNil(doc.SlotLeaseRef),
		Workflow:            doc.Workflow,
		IssueRef:            optionalNonEmptyStringPtr(publicids.IssueRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber))),
		IssueRepo:           optionalNonEmptyStringPtr(doc.IssueRepo),
		IssueNumber:         positiveIssueNumberPtr(doc.IssueNumber),
		State:               firstNonEmpty(doc.State, "in_progress"),
		CurrentPhase:        currentPhase,
		AttemptsCount:       len(doc.Attempts),
		PhaseExecutions:     runPhaseExecutionsFromDocs(doc.PhaseExecutions),
		CumulativeCostUSD:   doc.CumulativeCostUSD,
		ValidationURL:       emptyStringNil(doc.ValidationURL),
		ScreenshotsMarkdown: emptyStringNil(doc.ScreenshotsMarkdown),
		AbortReason:         emptyStringNil(doc.AbortReason),
		StartedAt:           parseTimeOrNow(doc.CreatedAt),
		CompletedAt:         completed,
		UpdatedAt:           parseTimeOrNow(doc.UpdatedAt),
		Attempts:            attempts,
	}
}

func runPhaseExecutionsFromDocs(docs []phaseExecutionDoc) []server.RunPhaseExecution {
	out := make([]server.RunPhaseExecution, 0, len(docs))
	for _, doc := range docs {
		jobs := make([]server.RunJobExecution, 0, len(doc.Jobs))
		for _, job := range doc.Jobs {
			steps := make([]server.RunStepExecution, 0, len(job.Steps))
			for _, step := range job.Steps {
				steps = append(steps, server.RunStepExecution{
					Slug:        step.Slug,
					Title:       emptyStringNil(step.Title),
					State:       firstNonEmpty(step.State, "not_started"),
					Reason:      emptyStringNil(step.Reason),
					ExitCode:    step.ExitCode,
					CreatedAt:   step.CreatedAt,
					StartedAt:   emptyStringNil(step.StartedAt),
					CompletedAt: emptyStringNil(step.CompletedAt),
				})
			}
			jobs = append(jobs, server.RunJobExecution{
				ID:           job.ID,
				Name:         emptyStringNil(job.Name),
				State:        firstNonEmpty(job.State, "not_started"),
				Reason:       emptyStringNil(job.Reason),
				K8sJobName:   emptyStringNil(job.K8sJobName),
				CreatedAt:    job.CreatedAt,
				DispatchedAt: emptyStringNil(job.DispatchedAt),
				StartedAt:    emptyStringNil(job.StartedAt),
				CompletedAt:  emptyStringNil(job.CompletedAt),
				Steps:        steps,
			})
		}
		out = append(out, server.RunPhaseExecution{
			Name:         doc.Name,
			Kind:         doc.Kind,
			State:        firstNonEmpty(doc.State, "not_started"),
			Reason:       emptyStringNil(doc.Reason),
			CreatedAt:    doc.CreatedAt,
			DispatchedAt: emptyStringNil(doc.DispatchedAt),
			StartedAt:    emptyStringNil(doc.StartedAt),
			CompletedAt:  emptyStringNil(doc.CompletedAt),
			Jobs:         jobs,
		})
	}
	return out
}

func runReportAttemptFromDoc(doc attemptDoc, lineageByID map[string]string) server.RunReportAttempt {
	var verificationStatus *string
	evidenceRefs := []string{}
	var cost *float64
	if doc.Verification != nil {
		verificationStatus = optionalNonEmptyStringPtr(doc.Verification.Status)
		evidenceRefs = sliceOrEmpty(doc.Verification.EvidenceRefs)
		if doc.CostUSD == nil {
			cost = &doc.Verification.CostUSD
		}
	}
	if doc.CostUSD != nil {
		cost = doc.CostUSD
	}
	jobCompletions := make([]server.RunAttemptJobCompletion, 0, len(doc.JobCompletions))
	for _, completion := range doc.JobCompletions {
		jobCompletions = append(jobCompletions, runAttemptJobCompletionFromDoc(completion))
	}
	sort.SliceStable(jobCompletions, func(i, j int) bool {
		return jobCompletions[i].JobID < jobCompletions[j].JobID
	})
	return server.RunReportAttempt{
		AttemptIndex:       doc.AttemptIndex,
		Phase:              doc.Phase,
		PhaseKind:          firstNonEmpty(doc.PhaseKind, "k8s_job"),
		WorkflowFilename:   doc.WorkflowFilename,
		DispatchedAt:       parseTimeOrNow(doc.DispatchedAt),
		CompletedAt:        parseOptionalTime(doc.CompletedAt),
		Conclusion:         emptyStringNil(doc.Conclusion),
		VerificationStatus: verificationStatus,
		EvidenceRefs:       evidenceRefs,
		SummaryMarkdown:    emptyStringNil(doc.SummaryMarkdown),
		Decision:           emptyStringNil(doc.Decision),
		CostUSD:            cost,
		LogArchiveURL:      emptyStringNil(doc.LogArchiveURL),
		PhaseOutputs:       mapStringOrEmpty(doc.PhaseOutputs),
		JobCompletions:     jobCompletions,
	}
}

func runAttemptJobCompletionFromDoc(doc nativeJobCompletionDoc) server.RunAttemptJobCompletion {
	var verificationStatus *string
	verificationReasons := []string{}
	if doc.Verification != nil {
		verificationStatus = optionalNonEmptyStringPtr(doc.Verification.Status)
		verificationReasons = sliceOrEmpty(doc.Verification.Reasons)
	}
	return server.RunAttemptJobCompletion{
		JobID:               doc.JobID,
		CompletedAt:         parseOptionalTime(doc.CompletedAt),
		Conclusion:          doc.Conclusion,
		VerificationStatus:  verificationStatus,
		VerificationReasons: verificationReasons,
		CostUSD:             doc.CostUSD,
		PhaseOutputs:        mapStringOrEmpty(doc.PhaseOutputs),
	}
}

func phaseExecutionDocsFromWorkflow(wf server.Workflow, createdAt string, entrypointPhase *string) []phaseExecutionDoc {
	out := make([]phaseExecutionDoc, 0, len(wf.Phases))
	beforeEntrypoint := strings.TrimSpace(stringOrEmpty(entrypointPhase)) != ""
	for _, phase := range wf.Phases {
		phase = server.CanonicalNativePhase(phase)
		state := "not_started"
		if beforeEntrypoint {
			if phase.Name == strings.TrimSpace(stringOrEmpty(entrypointPhase)) {
				beforeEntrypoint = false
			} else {
				state = "skipped"
			}
		}
		jobs := make([]jobExecutionDoc, 0, len(phase.Jobs))
		for _, job := range phase.Jobs {
			jobID := strings.TrimSpace(job.ID)
			if jobID == "" {
				continue
			}
			steps := make([]stepExecutionDoc, 0, len(job.Steps))
			for _, step := range job.Steps {
				slug := strings.TrimSpace(step.Slug)
				if slug == "" {
					continue
				}
				steps = append(steps, stepExecutionDoc{
					Slug:      slug,
					Title:     emptyStringNil(step.Title),
					State:     state,
					CreatedAt: createdAt,
				})
			}
			if len(steps) == 0 {
				steps = append(steps, stepExecutionDoc{
					Slug:      "job",
					Title:     emptyStringNil(job.Name),
					State:     state,
					CreatedAt: createdAt,
				})
			}
			jobs = append(jobs, jobExecutionDoc{
				ID:        jobID,
				Name:      emptyStringNil(job.Name),
				State:     state,
				CreatedAt: createdAt,
				Steps:     steps,
			})
		}
		if len(jobs) == 0 {
			jobID := firstNonEmpty(phase.WorkflowFilename, phase.Name, "phase")
			jobs = append(jobs, jobExecutionDoc{
				ID:        jobID,
				Name:      optionalNonEmptyStringPtr(jobID),
				State:     state,
				CreatedAt: createdAt,
				Steps: []stepExecutionDoc{{
					Slug:      "workflow-run",
					Title:     optionalNonEmptyStringPtr("Workflow run"),
					State:     state,
					CreatedAt: createdAt,
				}},
			})
		}
		out = append(out, phaseExecutionDoc{
			Name:      phase.Name,
			Kind:      firstNonEmpty(phase.Kind, "k8s_job"),
			State:     state,
			CreatedAt: createdAt,
			Jobs:      jobs,
		})
	}
	return out
}

func (s *Store) workflowForRunExecution(ctx context.Context, project, workflowName, schemaRef string) (*server.Workflow, error) {
	if strings.TrimSpace(schemaRef) != "" {
		wf, err := s.GetWorkflowBySchemaRef(ctx, project, schemaRef)
		if err != nil {
			return nil, err
		}
		if wf != nil {
			return wf, nil
		}
	}
	wf, err := s.GetWorkflowByName(ctx, project, workflowName)
	if err != nil {
		return nil, err
	}
	if wf == nil {
		return nil, server.ValidationError{Message: fmt.Sprintf("workflow %q is not registered", workflowName)}
	}
	return wf, nil
}

func runDisplayNumber(doc runDoc, fallback int) string {
	if doc.RunDisplayNumber != nil && strings.TrimSpace(*doc.RunDisplayNumber) != "" {
		return strings.TrimSpace(*doc.RunDisplayNumber)
	}
	if doc.RunNumber != nil {
		return fmt.Sprintf("%d", *doc.RunNumber)
	}
	if fallback > 0 {
		return fmt.Sprintf("%d", fallback)
	}
	return ""
}

func positiveIssueNumberPtr(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}

func optionalNonEmptyStringPtr(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func emptyStringNil(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return value
}

func refPtr(refs map[string]string, id *string) *string {
	if id == nil || *id == "" {
		return nil
	}
	return optionalNonEmptyStringPtr(refs[*id])
}

func workflowDocFromRegister(req server.WorkflowRegister, createdAt string) workflowDoc {
	phases := make([]phaseDoc, 0, len(req.Phases))
	for _, phase := range req.Phases {
		phases = append(phases, phaseDocFromSpec(phase))
	}
	return workflowDoc{
		ID:                  req.Name,
		Project:             req.Project,
		Name:                req.Name,
		Phases:              phases,
		PR:                  prDocFromSpec(req.PR),
		Budget:              budgetDoc{Total: defaultBudgetTotal(req.Budget.Total)},
		DefaultRequirements: mapOrEmpty(req.DefaultRequirements),
		Metadata:            mapOrEmpty(req.Metadata),
		CreatedAt:           createdAt,
	}
}

func workflowRegisterFromDoc(doc workflowDoc) server.WorkflowRegister {
	phases := make([]server.PhaseSpec, 0, len(doc.Phases))
	for _, phase := range doc.Phases {
		phases = append(phases, phaseFromDoc(phase))
	}
	return server.WorkflowRegister{
		Project:             doc.Project,
		Name:                doc.Name,
		Phases:              phases,
		PR:                  prFromDoc(doc.PR),
		Budget:              budget.Config{Total: defaultBudgetTotal(doc.Budget.Total)},
		DefaultRequirements: mapOrEmpty(doc.DefaultRequirements),
		Metadata:            mapOrEmpty(doc.Metadata),
	}
}

func workflowSchemaRef(doc workflowDoc) string {
	canonical := struct {
		Project             string         `json:"project"`
		Name                string         `json:"name"`
		Phases              []phaseDoc     `json:"phases"`
		PR                  prDoc          `json:"pr"`
		Budget              budgetDoc      `json:"budget"`
		DefaultRequirements map[string]any `json:"defaultRequirements"`
		Metadata            map[string]any `json:"metadata"`
	}{
		Project:             doc.Project,
		Name:                doc.Name,
		Phases:              doc.Phases,
		PR:                  doc.PR,
		Budget:              doc.Budget,
		DefaultRequirements: mapOrEmpty(doc.DefaultRequirements),
		Metadata:            mapOrEmpty(doc.Metadata),
	}
	payload, _ := json.Marshal(canonical)
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("wfs_%x", sum[:8])
}

func workflowSchemaDocID(name, schemaRef string) string {
	return "schema:" + name + ":" + schemaRef
}

func workflowSchemaDocFromWorkflow(doc workflowDoc) workflowDoc {
	doc.ID = workflowSchemaDocID(doc.Name, doc.SchemaRef)
	doc.Kind = workflowSchemaKind
	return doc
}

func isWorkflowSchemaDoc(doc workflowDoc) bool {
	return doc.Kind == workflowSchemaKind || strings.HasPrefix(doc.ID, "schema:")
}

func (s *Store) createWorkflowSchemaIfMissing(ctx context.Context, pk azcosmos.PartitionKey, doc workflowDoc) error {
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if _, err := s.workflows.CreateItem(ctx, pk, payload, nil); err != nil && !isCosmosStatus(err, http.StatusConflict) {
		return err
	}
	return nil
}

func phaseDocFromSpec(phase server.PhaseSpec) phaseDoc {
	phase = server.CanonicalNativePhase(phase)
	jobs := make([]nativeJobDoc, 0, len(phase.Jobs))
	for _, job := range phase.Jobs {
		jobs = append(jobs, nativeJobDocFromSpec(job))
	}
	return phaseDoc{
		Name:                     phase.Name,
		Kind:                     firstNonEmpty(phase.Kind, "k8s_job"),
		WorkflowFilename:         phase.WorkflowFilename,
		WorkflowRef:              firstNonEmpty(phase.WorkflowRef, "main"),
		Inputs:                   stringMapOrEmpty(phase.Inputs),
		Outputs:                  sliceOrEmpty(phase.Outputs),
		Requirements:             mapOrEmpty(phase.Requirements),
		Verify:                   phase.Verify,
		RecyclePolicy:            recyclePolicyDocFromSpec(phase.RecyclePolicy),
		Always:                   phase.Always,
		EvidenceVerificationGate: phase.EvidenceVerificationGate,
		DependsOn:                sliceOrEmpty(phase.DependsOn),
		Jobs:                     jobs,
	}
}

func nativeJobDocFromSpec(job server.NativeJobSpec) nativeJobDoc {
	steps := make([]nativeStepDoc, 0, len(job.Steps))
	for _, step := range job.Steps {
		steps = append(steps, nativeStepDoc{
			Slug:             step.Slug,
			Title:            step.Title,
			Type:             step.Type,
			Run:              step.Run,
			Shell:            step.Shell,
			WorkingDirectory: step.WorkingDirectory,
			Env:              stringMapOrEmpty(step.Env),
		})
	}
	extraCheckouts := make([]nativeCheckoutDoc, 0, len(job.ExtraCheckouts))
	for _, checkout := range job.ExtraCheckouts {
		extraCheckouts = append(extraCheckouts, nativeCheckoutDocFromSpec(checkout))
	}
	return nativeJobDoc{
		ID:               job.ID,
		Name:             job.Name,
		Image:            job.Image,
		Command:          sliceOrEmpty(job.Command),
		Args:             sliceOrEmpty(job.Args),
		Env:              stringMapOrEmpty(job.Env),
		Steps:            steps,
		TimeoutSeconds:   job.TimeoutSeconds,
		Managed:          job.Managed,
		Checkout:         nativeCheckoutDocPtrFromSpec(job.Checkout),
		ExtraCheckouts:   extraCheckouts,
		WorkingDirectory: job.WorkingDirectory,
		Shell:            job.Shell,
	}
}

func nativeCheckoutDocPtrFromSpec(checkout *server.NativeCheckoutSpec) *nativeCheckoutDoc {
	if checkout == nil {
		return nil
	}
	doc := nativeCheckoutDocFromSpec(*checkout)
	return &doc
}

func nativeCheckoutDocFromSpec(checkout server.NativeCheckoutSpec) nativeCheckoutDoc {
	return nativeCheckoutDoc{
		Repo: checkout.Repo,
		Ref:  checkout.Ref,
		Path: checkout.Path,
	}
}

func prDocFromSpec(pr server.PrPrimitive) prDoc {
	return prDoc{Enabled: pr.Enabled, RecyclePolicy: recyclePolicyDocFromSpec(pr.RecyclePolicy)}
}

func recyclePolicyDocFromSpec(policy *server.RecyclePolicy) *recyclePolicyDoc {
	if policy == nil {
		return nil
	}
	return &recyclePolicyDoc{
		MaxAttempts: policy.MaxAttempts,
		On:          sliceOrEmpty(policy.On),
		LandsAt:     policy.LandsAt,
	}
}

func workflowFromMap(doc map[string]any) (server.Workflow, error) {
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Workflow{}, err
	}
	var typed workflowDoc
	if err := json.Unmarshal(payload, &typed); err != nil {
		return server.Workflow{}, err
	}
	return workflowFromDoc(typed), nil
}

func validateWorkflowForProject(project projectDoc, req server.WorkflowRegister) error {
	return server.ValidateWorkflowRegister(req)
}

func normalizeWorkflowRegisterForProjectDoc(req *server.WorkflowRegister, project projectDoc) {
	for i := range req.Phases {
		req.Phases[i].Kind = strings.TrimSpace(req.Phases[i].Kind)
		if req.Phases[i].Kind == "" {
			req.Phases[i].Kind = "k8s_job"
		}
		if req.Phases[i].WorkflowRef == "" {
			req.Phases[i].WorkflowRef = "main"
		}
		if req.Phases[i].Inputs == nil {
			req.Phases[i].Inputs = map[string]string{}
		}
		req.Phases[i].Outputs = sliceOrEmpty(req.Phases[i].Outputs)
		req.Phases[i].DependsOn = sliceOrEmpty(req.Phases[i].DependsOn)
		req.Phases[i].Jobs = sliceOrEmpty(req.Phases[i].Jobs)
		req.Phases[i] = server.CanonicalNativePhase(req.Phases[i])
	}
}

func projectRequiresNativeWorkflows(project projectDoc) bool {
	metadata := project.Metadata
	if boolValue(metadata["native_webapp"]) || boolValue(metadata["nativeWebapp"]) {
		return true
	}
	appKind := firstNonEmpty(
		stringValue(metadata["app_kind"]),
		stringValue(metadata["appKind"]),
		stringValue(metadata["app_type"]),
		stringValue(metadata["appType"]),
		stringValue(metadata["kind"]),
	)
	return isNativeWebappKind(appKind)
}

func isNativeWebappKind(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "native_webapp", "native-webapp", "native webapp",
		"native_web_app", "native-web-app", "native web app":
		return true
	default:
		return false
	}
}

func boolValue(value any) bool {
	typed, ok := value.(bool)
	return ok && typed
}

func positiveIntValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, typed > 0
	case int64:
		return int(typed), typed > 0
	case float64:
		n := int(typed)
		return n, typed == float64(n) && n > 0
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(typed))
		return n, err == nil && n > 0
	default:
		return 0, false
	}
}

func anyMapValue(raw any) map[string]any {
	if value, ok := raw.(map[string]any); ok {
		return value
	}
	return map[string]any{}
}

func mapSliceValue(raw any) []map[string]any {
	switch typed := raw.(type) {
	case []map[string]any:
		return typed
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, value := range typed {
			if item, ok := value.(map[string]any); ok {
				out = append(out, item)
			}
		}
		return out
	default:
		return nil
	}
}

func firstAny(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func stringValue(value any) string {
	typed, ok := value.(string)
	if !ok {
		return ""
	}
	return typed
}

func phaseFromDoc(doc phaseDoc) server.PhaseSpec {
	jobs := make([]server.NativeJobSpec, 0, len(doc.Jobs))
	for _, job := range doc.Jobs {
		jobs = append(jobs, jobFromDoc(job))
	}
	return server.PhaseSpec{
		Name:                     doc.Name,
		Kind:                     firstNonEmpty(doc.Kind, "k8s_job"),
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
		steps = append(steps, server.NativeStepSpec{
			Slug:             step.Slug,
			Title:            step.Title,
			Type:             step.Type,
			Run:              step.Run,
			Shell:            step.Shell,
			WorkingDirectory: step.WorkingDirectory,
			Env:              stringMapOrEmpty(step.Env),
		})
	}
	extraCheckouts := make([]server.NativeCheckoutSpec, 0, len(doc.ExtraCheckouts))
	for _, checkout := range doc.ExtraCheckouts {
		extraCheckouts = append(extraCheckouts, nativeCheckoutFromDoc(checkout))
	}
	return server.NativeJobSpec{
		ID:               doc.ID,
		Name:             doc.Name,
		Image:            doc.Image,
		Command:          sliceOrEmpty(doc.Command),
		Args:             sliceOrEmpty(doc.Args),
		Env:              stringMapOrEmpty(doc.Env),
		Steps:            steps,
		TimeoutSeconds:   doc.TimeoutSeconds,
		Managed:          doc.Managed,
		Checkout:         nativeCheckoutPtrFromDoc(doc.Checkout),
		ExtraCheckouts:   extraCheckouts,
		WorkingDirectory: doc.WorkingDirectory,
		Shell:            doc.Shell,
	}
}

func nativeCheckoutPtrFromDoc(doc *nativeCheckoutDoc) *server.NativeCheckoutSpec {
	if doc == nil {
		return nil
	}
	checkout := nativeCheckoutFromDoc(*doc)
	return &checkout
}

func nativeCheckoutFromDoc(doc nativeCheckoutDoc) server.NativeCheckoutSpec {
	return server.NativeCheckoutSpec{
		Repo: doc.Repo,
		Ref:  doc.Ref,
		Path: doc.Path,
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

// Touchpoint store.

type touchpointDoc struct {
	ID            string           `json:"id"`
	Project       string           `json:"project"`
	Repo          string           `json:"repo"`
	Number        int              `json:"number"`
	Title         string           `json:"title"`
	Body          string           `json:"body"`
	State         string           `json:"state"`
	Branch        string           `json:"branch"`
	BaseRef       string           `json:"base_ref"`
	HeadSHA       string           `json:"head_sha"`
	HTMLURL       string           `json:"html_url"`
	LinkedIssueID *string          `json:"linked_issue_id"`
	LinkedRunID   *string          `json:"linked_run_id"`
	MergedAt      *string          `json:"merged_at"`
	MergedBy      *string          `json:"merged_by"`
	Comments      []map[string]any `json:"comments"`
	Reviews       []map[string]any `json:"reviews"`
	CreatedAt     string           `json:"created_at"`
	UpdatedAt     string           `json:"updated_at"`
}

func (s *Store) ListTouchpoints(ctx context.Context, filter server.TouchpointListFilter) ([]server.TouchpointRow, error) {
	// The reports container is partitioned by /project. When the caller
	// scopes to one project, this is a single-partition query and ORDER
	// BY works locally. When the caller asks for the cross-project index
	// (the touchpoints landing view), the Go SDK cannot fan out an
	// ORDER BY query for us — we fan out per project here and merge in
	// Go. See docs/cosmos-partition-strategy.md.
	var touchpointDocs []touchpointDoc

	if filter.Project != "" {
		// Single-partition path: include ORDER BY so Cosmos sorts within
		// the partition; merge is unnecessary.
		predicates := []string{"c.project = @project"}
		params := []azcosmos.QueryParameter{{Name: "@project", Value: filter.Project}}
		if filter.Repo != "" {
			predicates = append(predicates, "c.repo = @repo")
			params = append(params, azcosmos.QueryParameter{Name: "@repo", Value: filter.Repo})
		}
		if filter.State != "" {
			predicates = append(predicates, "c.state = @state")
			params = append(params, azcosmos.QueryParameter{Name: "@state", Value: filter.State})
		}
		query := "SELECT * FROM c WHERE " + strings.Join(predicates, " AND ") + " ORDER BY c.updated_at DESC"
		if err := singlePartitionQuery(
			ctx,
			s.reports,
			azcosmos.NewPartitionKeyString(filter.Project),
			query,
			params,
			&touchpointDocs,
		); err != nil {
			return nil, err
		}
	} else {
		// Cross-project fan-out: query each project partition in turn,
		// then sort the merged result in Go.
		projects, err := s.listProjectNames(ctx)
		if err != nil {
			return nil, err
		}
		predicates := []string{"c.project = @project"}
		var params []azcosmos.QueryParameter
		if filter.Repo != "" {
			predicates = append(predicates, "c.repo = @repo")
			params = append(params, azcosmos.QueryParameter{Name: "@repo", Value: filter.Repo})
		}
		if filter.State != "" {
			predicates = append(predicates, "c.state = @state")
			params = append(params, azcosmos.QueryParameter{Name: "@state", Value: filter.State})
		}
		query := "SELECT * FROM c WHERE " + strings.Join(predicates, " AND ")
		if err := fanOutByProject(ctx, s.reports, projects, query, params, &touchpointDocs); err != nil {
			return nil, err
		}
		sort.SliceStable(touchpointDocs, func(i, j int) bool {
			return touchpointDocs[i].UpdatedAt > touchpointDocs[j].UpdatedAt
		})
	}

	// Enrich with issue and run data.
	issueDocs, _ := s.listIssueDocs(ctx)
	runDocs, _ := s.listRunDocs(ctx)
	// PR lock read moved from cosmos.locks container to pg.LocksStore in
	// Stage 2b. ListHeldByScope("pr") returns only currently-held +
	// unexpired locks; convert to the map[key]bool shape the existing
	// touchpoint row builder expects.
	heldPRLocks, _ := s.pgLocks.ListHeldByScope(ctx, "pr")
	prLockByKey := make(map[string]bool, len(heldPRLocks))
	for key := range heldPRLocks {
		prLockByKey[key] = true
	}

	issueRefByID, issueNumberByID := buildIssueIndexes(issueDocs)
	runRefByID, runByLinkedIssueID, runByRepoPR := buildRunIndexes(runDocs)

	now := time.Now().UTC()
	rows := make([]server.TouchpointRow, 0, len(touchpointDocs))
	for _, doc := range touchpointDocs {
		row := touchpointRowFromDoc(doc, issueRefByID, issueNumberByID, runRefByID, runByLinkedIssueID, runByRepoPR, prLockByKey, now)
		rows = append(rows, row)
	}
	if filter.Limit != nil && *filter.Limit < len(rows) {
		rows = rows[:*filter.Limit]
	}
	return rows, nil
}

func (s *Store) GetTouchpointForIssue(ctx context.Context, project string, issueNumber int) (server.TouchpointDetail, error) {
	issueDoc, err := s.readIssueByNumber(ctx, project, issueNumber)
	if err != nil {
		return server.TouchpointDetail{}, server.ErrNotFound
	}
	// Find touchpoint by linked_issue_id.
	var docs []touchpointDoc
	if err := singlePartitionQuery(ctx, s.reports,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @project AND c.linked_issue_id = @iid ORDER BY c.updated_at DESC",
		[]azcosmos.QueryParameter{
			{Name: "@project", Value: project},
			{Name: "@iid", Value: issueDoc.ID},
		},
		&docs,
	); err != nil {
		return server.TouchpointDetail{}, err
	}
	if len(docs) == 0 {
		return server.TouchpointDetail{}, server.ErrNotFound
	}
	return s.buildTouchpointDetail(ctx, docs[0])
}

func (s *Store) EnsureTouchpoint(ctx context.Context, req server.TouchpointCreate) (server.TouchpointDetail, error) {
	// Resolve linked_issue_id by ref if provided.
	var linkedIssueID *string
	if req.LinkedIssueRef != "" {
		linkedIssueID = s.resolveIssueIDByRef(ctx, req.Project, req.LinkedIssueRef)
	}
	var linkedRunID *string
	if req.LinkedRunRef != "" {
		linkedRunID = s.resolveRunIDByRef(ctx, req.Project, req.LinkedRunRef)
	}

	// If we have a linked issue, check for an existing touchpoint for that issue.
	if linkedIssueID != nil {
		var docs []touchpointDoc
		_ = singlePartitionQuery(ctx, s.reports,
			azcosmos.NewPartitionKeyString(req.Project),
			"SELECT * FROM c WHERE c.project = @project AND c.linked_issue_id = @iid ORDER BY c.updated_at DESC",
			[]azcosmos.QueryParameter{
				{Name: "@project", Value: req.Project},
				{Name: "@iid", Value: *linkedIssueID},
			},
			&docs,
		)
		if len(docs) > 0 {
			doc := docs[0]
			// Patch linkages if caller is providing them.
			if linkedRunID != nil && (doc.LinkedRunID == nil || *doc.LinkedRunID != *linkedRunID) {
				doc.LinkedRunID = linkedRunID
				doc.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				_ = s.replaceTouchpointDoc(ctx, doc)
			}
			return s.buildTouchpointDetail(ctx, doc)
		}
	}

	// Fall back to (repo, number) idempotency key. PRs are unique per
	// repo so this lookup is by design cross-project; touchpoint detail
	// uniqueness is enforced at write time by the (repo, number) key.
	var existingDocs []touchpointDoc
	_ = crossPartitionQuery(ctx, s.reports,
		"SELECT * FROM c WHERE c.repo = @repo AND c.number = @num",
		[]azcosmos.QueryParameter{
			{Name: "@repo", Value: req.Repo},
			{Name: "@num", Value: req.Number},
		},
		&existingDocs,
	)
	if len(existingDocs) > 0 {
		doc := existingDocs[0]
		// Attach linkages if not already set.
		updated := false
		if linkedIssueID != nil && doc.LinkedIssueID == nil {
			doc.LinkedIssueID = linkedIssueID
			updated = true
		}
		if linkedRunID != nil && (doc.LinkedRunID == nil || *doc.LinkedRunID != *linkedRunID) {
			doc.LinkedRunID = linkedRunID
			updated = true
		}
		if updated {
			doc.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = s.replaceTouchpointDoc(ctx, doc)
		}
		return s.buildTouchpointDetail(ctx, doc)
	}

	// Create a new touchpoint.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc := touchpointDoc{
		ID:            uuid.New().String(),
		Project:       req.Project,
		Repo:          req.Repo,
		Number:        req.Number,
		Title:         req.Title,
		Body:          req.Body,
		State:         "ready",
		Branch:        req.Branch,
		BaseRef:       firstNonEmpty(req.BaseRef, "main"),
		HeadSHA:       req.HeadSHA,
		HTMLURL:       req.HTMLURL,
		LinkedIssueID: linkedIssueID,
		LinkedRunID:   linkedRunID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.TouchpointDetail{}, err
	}
	partitionKey := azcosmos.NewPartitionKeyString(req.Project)
	if _, err := s.reports.CreateItem(ctx, partitionKey, payload, nil); err != nil {
		return server.TouchpointDetail{}, err
	}
	return s.buildTouchpointDetail(ctx, doc)
}

func (s *Store) buildTouchpointDetail(ctx context.Context, doc touchpointDoc) (server.TouchpointDetail, error) {
	// Look up linked run. Runs are partitioned by /project; touchpoints
	// and their linked runs share a project, so scope the lookups
	// accordingly. doc.Project is the partition key.
	var run *runDoc
	if doc.LinkedRunID != nil && *doc.LinkedRunID != "" {
		var runDocs []runDoc
		if err := singlePartitionQuery(ctx, s.runs,
			azcosmos.NewPartitionKeyString(doc.Project),
			"SELECT * FROM c WHERE c.project = @project AND c.id = @id",
			[]azcosmos.QueryParameter{
				{Name: "@project", Value: doc.Project},
				{Name: "@id", Value: *doc.LinkedRunID},
			},
			&runDocs,
		); err == nil && len(runDocs) > 0 {
			run = &runDocs[0]
		}
	}
	if run == nil {
		// Fall back to latest run by (repo, pr_number) scoped to this
		// touchpoint's project — runs for the touchpoint's PR live in
		// the same project as the touchpoint itself.
		var runDocs []runDoc
		if err := singlePartitionQuery(ctx, s.runs,
			azcosmos.NewPartitionKeyString(doc.Project),
			"SELECT * FROM c WHERE c.project = @project AND c.issue_repo = @repo AND c.pr_number = @num ORDER BY c.created_at DESC",
			[]azcosmos.QueryParameter{
				{Name: "@project", Value: doc.Project},
				{Name: "@repo", Value: doc.Repo},
				{Name: "@num", Value: doc.Number},
			},
			&runDocs,
		); err == nil && len(runDocs) > 0 {
			run = &runDocs[0]
		}
	}

	// Look up linked issue. Issues are partitioned by /project; the
	// linked issue lives in the touchpoint's project.
	var linkedIssueRef *string
	var linkedIssueNumber *int
	var linkedIssueTitle *string
	if doc.LinkedIssueID != nil && *doc.LinkedIssueID != "" {
		var issueDocs []issueDoc
		_ = singlePartitionQuery(ctx, s.issues,
			azcosmos.NewPartitionKeyString(doc.Project),
			"SELECT * FROM c WHERE c.project = @project AND c.id = @id AND "+canonicalIssuePredicate,
			[]azcosmos.QueryParameter{
				{Name: "@project", Value: doc.Project},
				{Name: "@id", Value: *doc.LinkedIssueID},
			},
			&issueDocs,
		)
		if len(issueDocs) > 0 {
			issue := issueDocs[0]
			ref := publicids.IssueRef(issue.Project, &issue.Number)
			linkedIssueRef = &ref
			linkedIssueNumber = &issue.Number
			linkedIssueTitle = &issue.Title
		}
	}

	// PR lock check moved from cosmos.locks container to pg.LocksStore
	// in Stage 2b.
	prLockHeld, _ := s.pgLocks.PRLockHeld(ctx, doc.Repo, doc.Number)

	detail := server.TouchpointDetail{
		Ref:            publicids.TouchpointRef(doc.Repo, &doc.Number),
		Project:        doc.Project,
		Repo:           doc.Repo,
		PRNumber:       doc.Number,
		Title:          doc.Title,
		Body:           doc.Body,
		State:          firstNonEmpty(doc.State, "ready"),
		Merged:         doc.MergedAt != nil,
		BaseRef:        firstNonEmpty(doc.BaseRef, "main"),
		HeadSHA:        doc.HeadSHA,
		LinkedIssueRef: linkedIssueRef,
		IssueNumber:    linkedIssueNumber,
		IssueTitle:     linkedIssueTitle,
		Comments:       sliceOrEmpty(doc.Comments),
		Reviews:        sliceOrEmpty(doc.Reviews),
		PRLockHeld:     prLockHeld,
	}
	if doc.PRBranchStr() != "" {
		b := doc.PRBranchStr()
		detail.PRBranch = &b
	}
	if doc.HTMLURL != "" {
		detail.HTMLURL = &doc.HTMLURL
	}
	if run != nil {
		allRunDocs, _ := s.issueRunDocs(ctx, doc.Project, 0)
		_ = allRunDocs
		runRefMap := runRefMapFromDocs([]runDoc{*run})
		ref := runRefMap[run.ID]
		if ref != "" {
			detail.RunRef = &ref
			detail.LinkedRunRef = &ref
		}
		detail.RunState = ptrString(firstNonEmpty(run.State, ""))
		detail.ValidationURL = run.ValidationURL
		detail.ScreenshotsMarkdown = run.ScreenshotsMarkdown
		detail.RunAttempts = len(run.Attempts)
		detail.RunCumulativeCostUSD = run.CumulativeCostUSD
		if run.IssueNumber > 0 {
			detail.IssueNumber = &run.IssueNumber
		}
		detail.RunAttemptHistory = buildAttemptHistory(run.Attempts)
	}
	return detail, nil
}

func (s *Store) replaceTouchpointDoc(ctx context.Context, doc touchpointDoc) error {
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	partitionKey := azcosmos.NewPartitionKeyString(doc.Project)
	_, err = s.reports.ReplaceItem(ctx, partitionKey, doc.ID, payload, nil)
	return err
}

func (s *Store) resolveIssueIDByRef(ctx context.Context, project, ref string) *string {
	// ref format: "{project}#{number}" â€” parse the number part.
	parts := strings.SplitN(ref, "#", 2)
	if len(parts) != 2 {
		return nil
	}
	num, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil
	}
	doc, err := s.readIssueByNumber(ctx, project, num)
	if err != nil {
		return nil
	}
	return &doc.ID
}

func (s *Store) resolveRunIDByRef(ctx context.Context, project, ref string) *string {
	// ref format: "{project}#{issue_number}/runs/{run_part}"
	hashIdx := strings.Index(ref, "#")
	slashRuns := strings.Index(ref, "/runs/")
	if hashIdx < 0 || slashRuns < 0 || slashRuns <= hashIdx {
		return nil
	}
	issueNum, err := strconv.Atoi(ref[hashIdx+1 : slashRuns])
	if err != nil {
		return nil
	}
	runPart := ref[slashRuns+len("/runs/"):]
	if runPart == "" {
		return nil
	}
	docs, err := s.issueRunDocs(ctx, project, issueNum)
	if err != nil {
		return nil
	}
	numbers := runNumberMap(docs)
	for _, doc := range docs {
		display := ""
		if doc.RunDisplayNumber != nil {
			display = strings.TrimSpace(*doc.RunDisplayNumber)
		}
		if (display != "" && display == strings.TrimSpace(runPart)) ||
			fmt.Sprintf("%d", numbers[doc.ID]) == strings.TrimSpace(runPart) {
			id := doc.ID
			return &id
		}
	}
	return nil
}

func (d touchpointDoc) PRBranchStr() string {
	return d.Branch
}

// buildIssueIndexes builds maps from issue ID â†’ ref and issue ID â†’ number.
func buildIssueIndexes(docs []issueDoc) (map[string]string, map[string]int) {
	refByID := make(map[string]string, len(docs))
	numByID := make(map[string]int, len(docs))
	for _, d := range docs {
		ref := publicids.IssueRef(d.Project, &d.Number)
		refByID[d.ID] = ref
		numByID[d.ID] = d.Number
	}
	return refByID, numByID
}

// buildRunIndexes builds maps: run ID â†’ ref, linked_issue_id â†’ run, (repo,pr) â†’ run.
func buildRunIndexes(docs []runDoc) (map[string]string, map[string]*runDoc, map[string]*runDoc) {
	refByID := runRefMapFromDocs(docs)
	byLinkedIssue := make(map[string]*runDoc)
	byRepoPR := make(map[string]*runDoc)
	for i := range docs {
		d := &docs[i]
		if d.IssueID != "" {
			cur := byLinkedIssue[d.IssueID]
			if cur == nil || d.CreatedAt > cur.CreatedAt {
				byLinkedIssue[d.IssueID] = d
			}
		}
		if d.IssueRepo != "" && d.PRNumber != nil {
			key := fmt.Sprintf("%s#%d", d.IssueRepo, *d.PRNumber)
			cur := byRepoPR[key]
			if cur == nil || d.CreatedAt > cur.CreatedAt {
				byRepoPR[key] = d
			}
		}
	}
	return refByID, byLinkedIssue, byRepoPR
}

func touchpointRowFromDoc(
	doc touchpointDoc,
	issueRefByID map[string]string,
	issueNumByID map[string]int,
	runRefByID map[string]string,
	runByLinkedIssue map[string]*runDoc,
	runByRepoPR map[string]*runDoc,
	prLockByKey map[string]bool,
	now time.Time,
) server.TouchpointRow {
	row := server.TouchpointRow{
		Ref:      publicids.TouchpointRef(doc.Repo, &doc.Number),
		Project:  doc.Project,
		Repo:     doc.Repo,
		PRNumber: doc.Number,
		Title:    doc.Title,
		State:    firstNonEmpty(doc.State, "ready"),
		Merged:   doc.MergedAt != nil,
	}
	if doc.Branch != "" {
		row.PRBranch = &doc.Branch
	}
	if doc.HTMLURL != "" {
		row.HTMLURL = &doc.HTMLURL
	}

	if doc.LinkedIssueID != nil && *doc.LinkedIssueID != "" {
		if ref, ok := issueRefByID[*doc.LinkedIssueID]; ok {
			row.LinkedIssueRef = &ref
		}
		if num, ok := issueNumByID[*doc.LinkedIssueID]; ok {
			row.IssueNumber = &num
		}
	}

	// Find associated run.
	var run *runDoc
	if doc.LinkedIssueID != nil && *doc.LinkedIssueID != "" {
		run = runByLinkedIssue[*doc.LinkedIssueID]
	}
	if run == nil {
		key := fmt.Sprintf("%s#%d", doc.Repo, doc.Number)
		run = runByRepoPR[key]
	}
	if run != nil {
		if ref := runRefByID[run.ID]; ref != "" {
			row.RunRef = &ref
			row.LinkedRunRef = &ref
		}
		row.RunState = ptrString(run.State)
		row.ValidationURL = run.ValidationURL
		row.RunAttempts = len(run.Attempts)
		row.RunCumulativeCostUSD = run.CumulativeCostUSD
		if run.IssueNumber > 0 && row.IssueNumber == nil {
			row.IssueNumber = &run.IssueNumber
		}
	}

	lockKey := fmt.Sprintf("%s#%d", doc.Repo, doc.Number)
	row.PRLockHeld = prLockByKey[lockKey]
	_ = now
	return row
}

func buildAttemptHistory(attempts []attemptDoc) []map[string]any {
	out := make([]map[string]any, 0, len(attempts))
	for _, a := range attempts {
		entry := map[string]any{
			"attempt_index":     a.AttemptIndex,
			"phase":             a.Phase,
			"workflow_filename": a.WorkflowFilename,
			"dispatched_at":     a.DispatchedAt,
			"completed_at":      a.CompletedAt,
			"conclusion":        a.Conclusion,
			"cost_usd":          a.CostUSD,
			"decision":          a.Decision,
			"log_archive_url":   a.LogArchiveURL,
		}
		if a.Verification != nil {
			entry["verification_status"] = a.Verification.Status
			entry["evidence_refs"] = a.Verification.EvidenceRefs
		}
		out = append(out, entry)
	}
	return out
}

func ptrString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mapStringOrEmpty(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return values
}

func parseTimeOrZero(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// â”€â”€ Playbook store â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type playbookEntryDoc struct {
	ID              string         `json:"id"`
	Title           *string        `json:"title"`
	Issue           map[string]any `json:"issue"`
	DependsOn       []string       `json:"depends_on"`
	ManualGate      bool           `json:"manual_gate"`
	State           string         `json:"state"`
	CreatedIssueID  *string        `json:"created_issue_id"`
	CreatedIssueRef *string        `json:"created_issue_ref,omitempty"`
	RunID           *string        `json:"run_id"`
	RunRef          *string        `json:"run_ref,omitempty"`
	CompletedAt     *string        `json:"completed_at"`
	Metadata        map[string]any `json:"metadata"`
}

type playbookDoc struct {
	ID                  string             `json:"id"`
	SchemaVersion       int                `json:"schema_version"`
	Project             string             `json:"project"`
	Title               string             `json:"title"`
	Description         string             `json:"description"`
	Entries             []playbookEntryDoc `json:"entries"`
	ConcurrencyLimit    *int               `json:"concurrency_limit"`
	IntegrationStrategy string             `json:"integration_strategy"`
	State               string             `json:"state"`
	Metadata            map[string]any     `json:"metadata"`
	CreatedAt           string             `json:"created_at"`
	UpdatedAt           string             `json:"updated_at"`
}

func (s *Store) ListPlaybooks(ctx context.Context, filter server.PlaybookListFilter) ([]server.PlaybookPublic, error) {
	// playbooks container is partitioned by /project. Single-partition
	// when filter.Project is set, fan out otherwise. See
	// docs/cosmos-partition-strategy.md.
	var docs []playbookDoc
	if filter.Project != "" {
		predicates := []string{"c.project = @project"}
		params := []azcosmos.QueryParameter{{Name: "@project", Value: filter.Project}}
		if filter.State != "" {
			predicates = append(predicates, "c.state = @state")
			params = append(params, azcosmos.QueryParameter{Name: "@state", Value: filter.State})
		}
		query := "SELECT * FROM c WHERE " + strings.Join(predicates, " AND ") + " ORDER BY c.created_at DESC"
		if err := singlePartitionQuery(ctx, s.playbooks,
			azcosmos.NewPartitionKeyString(filter.Project),
			query, params, &docs); err != nil {
			return nil, err
		}
	} else {
		projects, err := s.listProjectNames(ctx)
		if err != nil {
			return nil, err
		}
		predicates := []string{"c.project = @project"}
		var params []azcosmos.QueryParameter
		if filter.State != "" {
			predicates = append(predicates, "c.state = @state")
			params = append(params, azcosmos.QueryParameter{Name: "@state", Value: filter.State})
		}
		query := "SELECT * FROM c WHERE " + strings.Join(predicates, " AND ")
		if err := fanOutByProject(ctx, s.playbooks, projects, query, params, &docs); err != nil {
			return nil, err
		}
		sort.SliceStable(docs, func(i, j int) bool {
			return docs[i].CreatedAt > docs[j].CreatedAt
		})
	}
	if filter.Limit != nil && *filter.Limit < len(docs) {
		docs = docs[:*filter.Limit]
	}
	out := make([]server.PlaybookPublic, 0, len(docs))
	for _, doc := range docs {
		out = append(out, s.playbookToPublic(ctx, doc))
	}
	return out, nil
}

func (s *Store) GetPlaybook(ctx context.Context, project, ref string) (server.PlaybookPublic, error) {
	var docs []playbookDoc
	if err := singlePartitionQuery(ctx, s.playbooks,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @project ORDER BY c.created_at DESC",
		[]azcosmos.QueryParameter{{Name: "@project", Value: project}},
		&docs,
	); err != nil {
		return server.PlaybookPublic{}, err
	}
	for _, doc := range docs {
		if playbookPublicRef(doc) == ref {
			return s.playbookToPublic(ctx, doc), nil
		}
	}
	return server.PlaybookPublic{}, server.ErrNotFound
}

func (s *Store) CreatePlaybook(ctx context.Context, req server.PlaybookCreate) (server.PlaybookPublic, error) {
	// Verify project exists.
	if _, err := s.readProjectDoc(ctx, req.Project); err != nil {
		return server.PlaybookPublic{}, server.ErrNotFound
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	metadata := mapOrEmpty(req.Metadata)
	if _, hasRef := metadata["public_ref"]; !hasRef {
		t, _ := time.Parse(time.RFC3339Nano, now)
		metadata["public_ref"] = playbookSlug(req.Title) + "-" + t.UTC().Format("20060102150405")
	}
	entries := make([]playbookEntryDoc, 0, len(req.Entries))
	for _, e := range req.Entries {
		entries = append(entries, playbookEntryDoc{
			ID:         e.ID,
			Title:      e.Title,
			Issue:      playbookIssueSpecToMap(e.Issue),
			DependsOn:  sliceOrEmpty(e.DependsOn),
			ManualGate: e.ManualGate,
			State:      "pending",
			Metadata:   mapOrEmpty(e.Metadata),
		})
	}
	doc := playbookDoc{
		ID:                  uuid.New().String(),
		SchemaVersion:       1,
		Project:             req.Project,
		Title:               req.Title,
		Description:         req.Description,
		Entries:             entries,
		ConcurrencyLimit:    req.ConcurrencyLimit,
		IntegrationStrategy: firstNonEmpty(req.IntegrationStrategy, "isolated_prs"),
		State:               "draft",
		Metadata:            metadata,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.PlaybookPublic{}, err
	}
	if _, err := s.playbooks.CreateItem(ctx, azcosmos.NewPartitionKeyString(req.Project), payload, nil); err != nil {
		return server.PlaybookPublic{}, err
	}
	return s.playbookToPublic(ctx, doc), nil
}

func (s *Store) playbookToPublic(ctx context.Context, doc playbookDoc) server.PlaybookPublic {
	entries := make([]server.PlaybookEntryPublic, 0, len(doc.Entries))
	for _, e := range doc.Entries {
		pub := server.PlaybookEntryPublic{
			ID:          e.ID,
			Title:       e.Title,
			Issue:       playbookIssueSpecFromMap(e.Issue),
			DependsOn:   sliceOrEmpty(e.DependsOn),
			ManualGate:  e.ManualGate,
			State:       firstNonEmpty(e.State, "pending"),
			CompletedAt: e.CompletedAt,
			Metadata:    mapOrEmpty(e.Metadata),
		}
		if e.CreatedIssueRef != nil && *e.CreatedIssueRef != "" {
			pub.CreatedIssueRef = e.CreatedIssueRef
		}
		if e.RunRef != nil && *e.RunRef != "" {
			pub.RunRef = e.RunRef
		}
		// Resolve created_issue_ref from created_issue_id. The created
		// issue lives in the playbook's project (issues partition key
		// is /project).
		if e.CreatedIssueID != nil && *e.CreatedIssueID != "" {
			var issueDocs []issueDoc
			if err := singlePartitionQuery(ctx, s.issues,
				azcosmos.NewPartitionKeyString(doc.Project),
				"SELECT * FROM c WHERE c.project = @project AND c.id = @id AND "+canonicalIssuePredicate,
				[]azcosmos.QueryParameter{
					{Name: "@project", Value: doc.Project},
					{Name: "@id", Value: *e.CreatedIssueID},
				},
				&issueDocs,
			); err == nil && len(issueDocs) > 0 {
				ref := publicids.IssueRef(issueDocs[0].Project, &issueDocs[0].Number)
				pub.CreatedIssueRef = &ref
			}
		}
		if e.RunID != nil && *e.RunID != "" {
			if ref := s.resolveLastTouchedRunRef(ctx, doc.Project, e.RunID); ref != nil {
				pub.RunRef = ref
			}
		}
		entries = append(entries, pub)
	}
	metadata := mapOrEmpty(doc.Metadata)
	delete(metadata, "public_ref")
	return server.PlaybookPublic{
		SchemaVersion:       max(doc.SchemaVersion, 1),
		Ref:                 playbookPublicRef(doc),
		Project:             doc.Project,
		Title:               doc.Title,
		Description:         doc.Description,
		Entries:             entries,
		ConcurrencyLimit:    doc.ConcurrencyLimit,
		IntegrationStrategy: firstNonEmpty(doc.IntegrationStrategy, "isolated_prs"),
		State:               firstNonEmpty(doc.State, "draft"),
		Metadata:            metadata,
		CreatedAt:           doc.CreatedAt,
		UpdatedAt:           doc.UpdatedAt,
	}
}

func playbookPublicRef(doc playbookDoc) string {
	if ref, ok := doc.Metadata["public_ref"].(string); ok && strings.TrimSpace(ref) != "" {
		return strings.TrimSpace(ref)
	}
	t, _ := time.Parse(time.RFC3339Nano, doc.CreatedAt)
	return playbookSlug(doc.Title) + "-" + t.UTC().Format("20060102150405")
}

func playbookSlug(title string) string {
	slug := strings.ToLower(title)
	var b strings.Builder
	prev := false
	for _, ch := range slug {
		alnum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
		if alnum {
			b.WriteRune(ch)
			prev = false
		} else if !prev {
			b.WriteByte('-')
			prev = true
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		return "playbook"
	}
	return result
}

func playbookIssueSpecToMap(spec server.PlaybookIssueSpec) map[string]any {
	m := map[string]any{
		"title":  spec.Title,
		"body":   spec.Body,
		"labels": sliceOrEmpty(spec.Labels),
	}
	if spec.Workflow != nil {
		m["workflow"] = *spec.Workflow
	}
	if spec.Metadata != nil {
		m["metadata"] = spec.Metadata
	}
	return m
}

func playbookIssueSpecFromMap(m map[string]any) server.PlaybookIssueSpec {
	spec := server.PlaybookIssueSpec{
		Title:    stringValue(m["title"]),
		Body:     stringValue(m["body"]),
		Metadata: mapOrEmpty(nil),
	}
	if labels, ok := m["labels"].([]any); ok {
		for _, l := range labels {
			if s, ok := l.(string); ok {
				spec.Labels = append(spec.Labels, s)
			}
		}
	}
	if wf, ok := m["workflow"].(string); ok && wf != "" {
		spec.Workflow = &wf
	}
	if meta, ok := m["metadata"].(map[string]any); ok {
		spec.Metadata = meta
	}
	return spec
}

// â”€â”€ Portfolio store â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type portfolioElementDoc struct {
	ID               string         `json:"id"`
	Kind             string         `json:"kind"`
	Project          string         `json:"project"`
	Route            string         `json:"route"`
	ElementID        string         `json:"element_id"`
	Title            string         `json:"title"`
	ScreenshotURL    *string        `json:"screenshot_url"`
	PreviewURL       *string        `json:"preview_url"`
	Status           string         `json:"status"`
	Notes            *string        `json:"notes"`
	LastTouchedRunID *string        `json:"last_touched_run_id"`
	Metadata         map[string]any `json:"metadata"`
	CreatedAt        string         `json:"created_at"`
	UpdatedAt        string         `json:"updated_at"`
}

func portfolioElementDocID(project, route, elementID string) string {
	raw := project + "\n" + route + "\n" + elementID
	sum := sha256.Sum256([]byte(raw))
	h := fmt.Sprintf("%x", sum[:])
	if len(h) > 32 {
		return h[:32]
	}
	return h
}

func portfolioElementDocToPublic(doc portfolioElementDoc, runRef *string) server.PortfolioElementPublic {
	return server.PortfolioElementPublic{
		Ref:               server.PortfolioElementRef(doc.Route, doc.ElementID),
		Project:           doc.Project,
		Route:             doc.Route,
		ElementID:         doc.ElementID,
		Title:             doc.Title,
		ScreenshotURL:     doc.ScreenshotURL,
		PreviewURL:        doc.PreviewURL,
		Status:            firstNonEmpty(doc.Status, "needs_review"),
		Notes:             doc.Notes,
		LastTouchedRunRef: runRef,
		Metadata:          mapOrEmpty(doc.Metadata),
		CreatedAt:         doc.CreatedAt,
		UpdatedAt:         doc.UpdatedAt,
	}
}

func (s *Store) resolveLastTouchedRunRef(ctx context.Context, project string, runID *string) *string {
	if runID == nil || *runID == "" {
		return nil
	}
	pk := azcosmos.NewPartitionKeyString(project)
	resp, err := s.runs.ReadItem(ctx, pk, *runID, nil)
	if err != nil {
		return nil
	}
	var doc runDoc
	if err := json.Unmarshal(resp.Value, &doc); err != nil {
		return nil
	}
	docs, _ := s.issueRunDocs(ctx, project, doc.IssueNumber)
	numbers := runNumberMap(docs)
	display := runDisplayNumber(doc, numbers[doc.ID])
	ref := publicids.RunRef(doc.Project, &doc.IssueNumber, display)
	return &ref
}

func (s *Store) resolveRunRefToID(ctx context.Context, project string, ref *string) (*string, error) {
	if ref == nil || *ref == "" {
		return nil, nil
	}
	id := s.resolveRunIDByRef(ctx, project, *ref)
	if id == nil {
		return nil, server.ErrNotFound
	}
	return id, nil
}

func (s *Store) ListPortfolioElements(ctx context.Context, filter server.PortfolioListFilter) ([]server.PortfolioElementPublic, error) {
	// issues container is partitioned by /project. Single-partition
	// when filter.Project is set, fan-out otherwise.
	var docs []portfolioElementDoc
	if filter.Project != "" {
		predicates := []string{"c.kind = @kind", "c.project = @project"}
		params := []azcosmos.QueryParameter{
			{Name: "@kind", Value: "portfolio_element"},
			{Name: "@project", Value: filter.Project},
		}
		if filter.Status != "" {
			predicates = append(predicates, "c.status = @status")
			params = append(params, azcosmos.QueryParameter{Name: "@status", Value: filter.Status})
		}
		query := "SELECT * FROM c WHERE " + strings.Join(predicates, " AND ") + " ORDER BY c.updated_at DESC"
		if err := singlePartitionQuery(ctx, s.issues,
			azcosmos.NewPartitionKeyString(filter.Project),
			query, params, &docs); err != nil {
			return nil, err
		}
	} else {
		projects, err := s.listProjectNames(ctx)
		if err != nil {
			return nil, err
		}
		predicates := []string{"c.kind = @kind", "c.project = @project"}
		params := []azcosmos.QueryParameter{{Name: "@kind", Value: "portfolio_element"}}
		if filter.Status != "" {
			predicates = append(predicates, "c.status = @status")
			params = append(params, azcosmos.QueryParameter{Name: "@status", Value: filter.Status})
		}
		query := "SELECT * FROM c WHERE " + strings.Join(predicates, " AND ")
		if err := fanOutByProject(ctx, s.issues, projects, query, params, &docs); err != nil {
			return nil, err
		}
		sort.SliceStable(docs, func(i, j int) bool {
			return docs[i].UpdatedAt > docs[j].UpdatedAt
		})
	}
	if filter.Limit != nil && *filter.Limit < len(docs) {
		docs = docs[:*filter.Limit]
	}
	out := make([]server.PortfolioElementPublic, 0, len(docs))
	for _, doc := range docs {
		runRef := s.resolveLastTouchedRunRef(ctx, doc.Project, doc.LastTouchedRunID)
		out = append(out, portfolioElementDocToPublic(doc, runRef))
	}
	return out, nil
}

func (s *Store) UpsertPortfolioElement(ctx context.Context, req server.PortfolioElementUpsert) (server.PortfolioElementPublic, error) {
	runID, err := s.resolveRunRefToID(ctx, req.Project, req.LastTouchedRunRef)
	if err != nil {
		return server.PortfolioElementPublic{}, err
	}
	docID := portfolioElementDocID(req.Project, req.Route, req.ElementID)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	createdAt := now
	pk := azcosmos.NewPartitionKeyString(req.Project)
	existing, readErr := s.issues.ReadItem(ctx, pk, docID, nil)
	if readErr == nil {
		var existDoc map[string]any
		if json.Unmarshal(existing.Value, &existDoc) == nil {
			if ca, ok := existDoc["created_at"].(string); ok && ca != "" {
				createdAt = ca
			}
		}
	}
	doc := portfolioElementDoc{
		ID:               docID,
		Kind:             "portfolio_element",
		Project:          req.Project,
		Route:            req.Route,
		ElementID:        req.ElementID,
		Title:            req.Title,
		ScreenshotURL:    req.ScreenshotURL,
		PreviewURL:       req.PreviewURL,
		Status:           firstNonEmpty(req.Status, "needs_review"),
		Notes:            req.Notes,
		LastTouchedRunID: runID,
		Metadata:         mapOrEmpty(req.Metadata),
		CreatedAt:        createdAt,
		UpdatedAt:        now,
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return server.PortfolioElementPublic{}, err
	}
	if _, err := s.issues.UpsertItem(ctx, pk, data, nil); err != nil {
		return server.PortfolioElementPublic{}, err
	}
	runRef := s.resolveLastTouchedRunRef(ctx, doc.Project, doc.LastTouchedRunID)
	return portfolioElementDocToPublic(doc, runRef), nil
}

func (s *Store) PatchPortfolioElement(ctx context.Context, project, ref string, req server.PortfolioElementPatch) (server.PortfolioElementPublic, error) {
	// Resolve ref -> doc ID by scanning project's portfolio elements.
	var docs []portfolioElementDoc
	if err := singlePartitionQuery(ctx, s.issues,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.kind = @kind AND c.project = @project",
		[]azcosmos.QueryParameter{
			{Name: "@kind", Value: "portfolio_element"},
			{Name: "@project", Value: project},
		},
		&docs,
	); err != nil {
		return server.PortfolioElementPublic{}, err
	}
	var target *portfolioElementDoc
	for i := range docs {
		if server.PortfolioElementRef(docs[i].Route, docs[i].ElementID) == ref {
			target = &docs[i]
			break
		}
	}
	if target == nil {
		return server.PortfolioElementPublic{}, server.ErrNotFound
	}

	if req.Title != nil {
		target.Title = *req.Title
	}
	if req.ScreenshotURL != nil {
		target.ScreenshotURL = req.ScreenshotURL
	}
	if req.PreviewURL != nil {
		target.PreviewURL = req.PreviewURL
	}
	if req.Status != nil {
		target.Status = *req.Status
	}
	if req.Notes != nil {
		target.Notes = req.Notes
	}
	if req.Metadata != nil {
		target.Metadata = *req.Metadata
	}
	if req.LastTouchedRunRef != nil {
		runID, err := s.resolveRunRefToID(ctx, project, req.LastTouchedRunRef)
		if err != nil {
			return server.PortfolioElementPublic{}, err
		}
		target.LastTouchedRunID = runID
	}
	target.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(*target)
	if err != nil {
		return server.PortfolioElementPublic{}, err
	}
	pk := azcosmos.NewPartitionKeyString(project)
	if _, err := s.issues.ReplaceItem(ctx, pk, target.ID, data, nil); err != nil {
		return server.PortfolioElementPublic{}, server.ErrNotFound
	}
	runRef := s.resolveLastTouchedRunRef(ctx, target.Project, target.LastTouchedRunID)
	return portfolioElementDocToPublic(*target, runRef), nil
}

// â”€â”€ Playbook gate â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func (s *Store) PatchPlaybookEntryGate(ctx context.Context, project, ref, entryID string, manualGate bool) (server.PlaybookPublic, error) {
	var docs []playbookDoc
	if err := singlePartitionQuery(ctx, s.playbooks,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @project ORDER BY c.created_at DESC",
		[]azcosmos.QueryParameter{{Name: "@project", Value: project}},
		&docs,
	); err != nil {
		return server.PlaybookPublic{}, err
	}
	var target *playbookDoc
	for i := range docs {
		if playbookPublicRef(docs[i]) == ref {
			target = &docs[i]
			break
		}
	}
	if target == nil {
		return server.PlaybookPublic{}, server.ErrNotFound
	}
	found := false
	for i := range target.Entries {
		if target.Entries[i].ID == entryID {
			target.Entries[i].ManualGate = manualGate
			if target.Entries[i].Metadata == nil {
				target.Entries[i].Metadata = map[string]any{}
			}
			target.Entries[i].Metadata["manual_gate_updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			found = true
			break
		}
	}
	if !found {
		return server.PlaybookPublic{}, server.ErrNotFound
	}
	target.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(*target)
	if err != nil {
		return server.PlaybookPublic{}, err
	}
	pk := azcosmos.NewPartitionKeyString(project)
	if _, err := s.playbooks.ReplaceItem(ctx, pk, target.ID, data, nil); err != nil {
		return server.PlaybookPublic{}, fmt.Errorf("replace playbook: %w", err)
	}
	return s.playbookToPublic(ctx, *target), nil
}

func (s *Store) AdvancePlaybook(ctx context.Context, project, ref string, dispatch server.PlaybookEntryDispatcher) (server.PlaybookPublic, error) {
	target, err := s.readPlaybookDocByRef(ctx, project, ref)
	if err != nil {
		return server.PlaybookPublic{}, err
	}
	if err := s.advancePlaybookDoc(ctx, target, dispatch); err != nil {
		return server.PlaybookPublic{}, err
	}
	if err := s.replacePlaybookDoc(ctx, target); err != nil {
		return server.PlaybookPublic{}, err
	}
	return s.playbookToPublic(ctx, *target), nil
}

func (s *Store) AdvancePlaybooksForRun(ctx context.Context, project, runID string, dispatch server.PlaybookEntryDispatcher) error {
	runRef, err := s.runRefByID(ctx, project, runID)
	if err != nil {
		return err
	}
	var docs []playbookDoc
	if err := singlePartitionQuery(ctx, s.playbooks,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @project ORDER BY c.created_at DESC",
		[]azcosmos.QueryParameter{{Name: "@project", Value: project}},
		&docs,
	); err != nil {
		return err
	}
	for i := range docs {
		if !playbookReferencesRun(docs[i], runID, runRef) {
			continue
		}
		if err := s.advancePlaybookDoc(ctx, &docs[i], dispatch); err != nil {
			return err
		}
		if err := s.replacePlaybookDoc(ctx, &docs[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readPlaybookDocByRef(ctx context.Context, project, ref string) (*playbookDoc, error) {
	var docs []playbookDoc
	if err := singlePartitionQuery(ctx, s.playbooks,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @project ORDER BY c.created_at DESC",
		[]azcosmos.QueryParameter{{Name: "@project", Value: project}},
		&docs,
	); err != nil {
		return nil, err
	}
	for i := range docs {
		if playbookPublicRef(docs[i]) == ref {
			return &docs[i], nil
		}
	}
	return nil, server.ErrNotFound
}

func (s *Store) replacePlaybookDoc(ctx context.Context, doc *playbookDoc) error {
	doc.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(*doc)
	if err != nil {
		return err
	}
	pk := azcosmos.NewPartitionKeyString(doc.Project)
	if _, err := s.playbooks.ReplaceItem(ctx, pk, doc.ID, data, nil); err != nil {
		return fmt.Errorf("replace playbook: %w", err)
	}
	return nil
}

func (s *Store) advancePlaybookDoc(ctx context.Context, doc *playbookDoc, dispatch server.PlaybookEntryDispatcher) error {
	s.refreshPlaybookEntries(ctx, doc)
	if doc.State == "cancelled" {
		return nil
	}
	if playbookAllSucceeded(*doc) {
		doc.State = "succeeded"
		return nil
	}
	if playbookHasBlockingFailure(*doc) {
		doc.State = "failed"
		return nil
	}

	limit := 1
	if doc.ConcurrencyLimit != nil && *doc.ConcurrencyLimit > 0 {
		limit = *doc.ConcurrencyLimit
	}
	active := 0
	for _, entry := range doc.Entries {
		if entry.State == "running" {
			active++
		}
	}

	started := 0
	for i := range doc.Entries {
		if active >= limit {
			break
		}
		entry := &doc.Entries[i]
		if !playbookEntryReady(*doc, *entry) {
			continue
		}
		if dispatch == nil {
			entry.State = "failed"
			entry.CompletedAt = stringPtrValue(time.Now().UTC().Format(time.RFC3339Nano))
			entry.Metadata = mergeMetadata(entry.Metadata, map[string]any{
				"dispatch_state":  "failed",
				"dispatch_detail": "playbook dispatcher not configured",
			})
			continue
		}
		workContext := playbookWorkContextForEntry(doc, entry)
		result, err := dispatch(ctx, server.PlaybookEntryDispatch{
			Project:             doc.Project,
			PlaybookID:          doc.ID,
			PlaybookRef:         playbookPublicRef(*doc),
			EntryID:             entry.ID,
			Issue:               playbookIssueSpecFromMap(entry.Issue),
			CreatedIssueRef:     entry.CreatedIssueRef,
			IntegrationStrategy: firstNonEmpty(doc.IntegrationStrategy, "isolated_prs"),
			WorkContext:         workContext,
		})
		entry.Metadata = mergeMetadata(entry.Metadata, map[string]any{
			"work_context":   workContext,
			"dispatch_state": result.State,
		})
		if result.Detail != nil {
			entry.Metadata["dispatch_detail"] = *result.Detail
		}
		if result.CreatedIssueRef != nil && *result.CreatedIssueRef != "" {
			entry.CreatedIssueRef = result.CreatedIssueRef
			entry.State = "created"
		}
		if result.RunID != nil && *result.RunID != "" {
			entry.RunID = result.RunID
		}
		if result.RunRef != nil && *result.RunRef != "" {
			entry.RunRef = result.RunRef
		}
		if err != nil {
			entry.State = "failed"
			entry.CompletedAt = stringPtrValue(time.Now().UTC().Format(time.RFC3339Nano))
			entry.Metadata["dispatch_error"] = err.Error()
			continue
		}
		switch result.State {
		case "dispatched", "pending", "already_running":
			entry.State = "running"
			active++
			started++
		default:
			entry.State = "failed"
			entry.CompletedAt = stringPtrValue(time.Now().UTC().Format(time.RFC3339Nano))
		}
	}

	if playbookAllSucceeded(*doc) {
		doc.State = "succeeded"
	} else if playbookHasBlockingFailure(*doc) {
		doc.State = "failed"
	} else if playbookHasOpenManualGate(*doc) {
		doc.State = "paused"
	} else if active > 0 || started > 0 {
		doc.State = "running"
	} else {
		doc.State = "ready"
	}
	return nil
}

func (s *Store) refreshPlaybookEntries(ctx context.Context, doc *playbookDoc) {
	for i := range doc.Entries {
		entry := &doc.Entries[i]
		run, ok := s.playbookEntryRun(ctx, doc.Project, *entry)
		if !ok {
			continue
		}
		switch run.State {
		case "passed":
			entry.State = "succeeded"
			entry.CompletedAt = stringPtrValue(firstNonEmpty(run.UpdatedAt, time.Now().UTC().Format(time.RFC3339Nano)))
		case "review_required", "aborted":
			entry.State = "failed"
			entry.CompletedAt = stringPtrValue(firstNonEmpty(run.UpdatedAt, time.Now().UTC().Format(time.RFC3339Nano)))
			entry.Metadata = mergeMetadata(entry.Metadata, map[string]any{
				"run_state":    run.State,
				"abort_reason": stringOrEmpty(run.AbortReason),
			})
		case "in_progress":
			if entry.State == "" || entry.State == "pending" || entry.State == "created" {
				entry.State = "running"
			}
		}
	}
}

func (s *Store) playbookEntryRun(ctx context.Context, project string, entry playbookEntryDoc) (runDoc, bool) {
	var runID string
	if entry.RunID != nil && *entry.RunID != "" {
		runID = *entry.RunID
	} else if entry.RunRef != nil && *entry.RunRef != "" {
		resolved := s.resolveRunIDByRef(ctx, project, *entry.RunRef)
		if resolved != nil {
			runID = *resolved
		}
	}
	if runID == "" {
		return runDoc{}, false
	}
	pk := azcosmos.NewPartitionKeyString(project)
	resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
	if err != nil {
		return runDoc{}, false
	}
	var doc runDoc
	if err := json.Unmarshal(resp.Value, &doc); err != nil {
		return runDoc{}, false
	}
	return doc, true
}

func (s *Store) runRefByID(ctx context.Context, project, runID string) (string, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return "", server.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var doc runDoc
	if err := json.Unmarshal(resp.Value, &doc); err != nil {
		return "", err
	}
	siblings, _ := s.issueRunDocs(ctx, project, doc.IssueNumber)
	numbers := runNumberMap(siblings)
	return publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), runDisplayNumber(doc, numbers[doc.ID])), nil
}

func playbookReferencesRun(doc playbookDoc, runID, runRef string) bool {
	for _, entry := range doc.Entries {
		if entry.RunID != nil && *entry.RunID == runID {
			return true
		}
		if runRef != "" && entry.RunRef != nil && *entry.RunRef == runRef {
			return true
		}
	}
	return false
}

func playbookEntryReady(doc playbookDoc, entry playbookEntryDoc) bool {
	state := firstNonEmpty(entry.State, "pending")
	return (state == "pending" || state == "created") && !entry.ManualGate && playbookEntryDependenciesMet(doc, entry)
}

func playbookEntryDependenciesMet(doc playbookDoc, entry playbookEntryDoc) bool {
	byID := map[string]playbookEntryDoc{}
	for _, candidate := range doc.Entries {
		byID[candidate.ID] = candidate
	}
	for _, dep := range entry.DependsOn {
		candidate, ok := byID[dep]
		if !ok || candidate.State != "succeeded" {
			return false
		}
	}
	return true
}

func playbookAllSucceeded(doc playbookDoc) bool {
	if len(doc.Entries) == 0 {
		return false
	}
	for _, entry := range doc.Entries {
		if entry.State != "succeeded" && entry.State != "skipped" {
			return false
		}
	}
	return true
}

func playbookHasBlockingFailure(doc playbookDoc) bool {
	for _, entry := range doc.Entries {
		if entry.State == "failed" {
			return true
		}
	}
	return false
}

func playbookHasOpenManualGate(doc playbookDoc) bool {
	for _, entry := range doc.Entries {
		if entry.ManualGate && firstNonEmpty(entry.State, "pending") == "pending" && playbookEntryDependenciesMet(doc, entry) {
			return true
		}
	}
	return false
}

func playbookWorkContextForEntry(doc *playbookDoc, entry *playbookEntryDoc) map[string]string {
	strategy := firstNonEmpty(doc.IntegrationStrategy, "isolated_prs")
	baseRef := "main"
	if value, ok := doc.Metadata["base_ref"].(string); ok && strings.TrimSpace(value) != "" {
		baseRef = strings.TrimSpace(value)
	}
	switch strategy {
	case "rolling_main":
		context := map[string]string{
			"id":                "project:" + doc.Project + ":main:" + baseRef,
			"strategy":          strategy,
			"branch":            baseRef,
			"base_ref":          baseRef,
			"owner_playbook_id": doc.ID,
			"current_entry_id":  entry.ID,
			"state":             "in_use",
		}
		doc.Metadata = mergeMetadata(doc.Metadata, map[string]any{"work_context": context})
		return context
	case "shared_feature_branch":
		if existing, ok := doc.Metadata["work_context"].(map[string]any); ok {
			if branch, ok := existing["branch"].(string); ok && branch != "" {
				context := map[string]string{
					"id":                stringValue(existing["id"]),
					"strategy":          strategy,
					"branch":            branch,
					"base_ref":          firstNonEmpty(stringValue(existing["base_ref"]), baseRef),
					"owner_playbook_id": doc.ID,
					"current_entry_id":  entry.ID,
					"state":             "in_use",
				}
				if context["id"] == "" {
					context["id"] = "playbook:" + doc.ID + ":shared"
				}
				doc.Metadata = mergeMetadata(doc.Metadata, map[string]any{"work_context": context})
				return context
			}
		}
		context := map[string]string{
			"id":                "playbook:" + doc.ID + ":shared",
			"strategy":          strategy,
			"branch":            "glimmung/playbooks/" + shortID(doc.ID),
			"base_ref":          baseRef,
			"owner_playbook_id": doc.ID,
			"current_entry_id":  entry.ID,
			"state":             "in_use",
		}
		doc.Metadata = mergeMetadata(doc.Metadata, map[string]any{"work_context": context})
		return context
	default:
		return map[string]string{
			"id":                "playbook:" + doc.ID + ":" + entry.ID,
			"strategy":          strategy,
			"branch":            "glimmung/playbooks/" + shortID(doc.ID) + "/" + playbookSlug(entry.ID),
			"base_ref":          baseRef,
			"owner_playbook_id": doc.ID,
			"current_entry_id":  entry.ID,
			"state":             "in_use",
		}
	}
}

func mergeMetadata(existing map[string]any, values map[string]any) map[string]any {
	out := mapOrEmpty(existing)
	for k, v := range values {
		out[k] = v
	}
	return out
}

func stringPtrValue(value string) *string {
	return &value
}

func shortID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type signalDoc struct {
	ID                string         `json:"id"`
	SchemaVersion     int            `json:"schema_version"`
	TargetType        string         `json:"target_type"`
	TargetRepo        string         `json:"target_repo"`
	TargetID          string         `json:"target_id"`
	Source            string         `json:"source"`
	Payload           map[string]any `json:"payload"`
	State             string         `json:"state"`
	EnqueuedAt        string         `json:"enqueued_at"`
	ProcessedAt       *string        `json:"processed_at,omitempty"`
	ProcessedDecision *string        `json:"processed_decision,omitempty"`
	FailureReason     *string        `json:"failure_reason,omitempty"`
}

// ---------------------------------------------------------------------------
// Lease acquire and cancel
// ---------------------------------------------------------------------------

const leaseCounterPrefix = "__counter:lease-number:"
const maxLeaseConflictRetries = 3

func (s *Store) AcquireLease(ctx context.Context, req server.LeaseAcquireRequest) (server.Lease, error) {
	if !isNativeLeaseRequest(req) {
		return server.Lease{}, server.ValidationError{Message: "native_k8s lease required"}
	}
	return s.acquireNativeLease(ctx, req)
}

func (s *Store) acquireNativeLease(ctx context.Context, req server.LeaseAcquireRequest) (server.Lease, error) {
	if err := validateNativeLeaseSlotIdentity(req.Metadata); err != nil {
		return server.Lease{}, err
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	leaseID := uuid.New().String()
	leaseNumber, err := s.nextLeaseNumber(ctx, req.Project)
	if err != nil {
		return server.Lease{}, fmt.Errorf("next lease number: %w", err)
	}
	ttl := 900
	if req.TTLSeconds != nil && *req.TTLSeconds > 0 {
		ttl = *req.TTLSeconds
	}
	metadata := buildLeaseMetadata(req)
	metadata["native_k8s"] = true
	callbackToken := uuid.New().String()[:32]
	metadata["lease_callback_token"] = callbackToken

	slotIndex, err := s.availableNativeSlot(ctx, req.Project)
	if err != nil {
		return server.Lease{}, err
	}
	if slotIndex == nil {
		return server.Lease{}, server.ErrUnavailable
	}
	hostName := "native-k8s"
	setNativeSlotMetadata(metadata, req.Project, *slotIndex, s.nativeSlotPrefix(ctx, req.Project))
	doc := leaseDoc{
		ID:           leaseID,
		LeaseNumber:  &leaseNumber,
		Project:      req.Project,
		Workflow:     req.Workflow,
		Host:         &hostName,
		State:        "claimed",
		Requirements: req.Requirements,
		Metadata:     metadata,
		RequestedAt:  nowStr,
		AssignedAt:   nowStr,
		TTLSeconds:   ttl,
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Lease{}, err
	}
	if _, err := s.leases.CreateItem(ctx, azcosmos.NewPartitionKeyString(req.Project), payload, nil); err != nil {
		return server.Lease{}, fmt.Errorf("create native lease doc: %w", err)
	}
	lease := leaseFromDoc(doc)
	if err := s.reserveNativeSlotForLease(ctx, lease, now); err != nil {
		_, _ = s.CancelLeaseByRef(ctx, req.Project, server.LeasePublicRefFromLease(lease))
		if errors.Is(err, server.ErrInvalidSlotTransition) || errors.Is(err, server.ErrPreconditionFailed) {
			return server.Lease{}, server.ErrUnavailable
		}
		return server.Lease{}, err
	}
	return lease, nil
}

func (s *Store) reserveNativeSlotForLease(ctx context.Context, lease server.Lease, now time.Time) error {
	slot := nativeSlotIndex(lease.Metadata)
	if slot == nil {
		return fmt.Errorf("native lease missing native_slot_index")
	}
	ref := server.LeasePublicRefFromLease(lease)
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("native lease missing public ref")
	}
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		_, err := s.UpdateIfMatch(ctx, lease.Project, *slot, func(s server.Slot) (server.Slot, error) {
			return s.MarkReserved(now, ref)
		})
		if errors.Is(err, server.ErrPreconditionFailed) {
			continue
		}
		return err
	}
	return server.ErrPreconditionFailed
}

func (s *Store) availableNativeSlot(ctx context.Context, project string) (*int, error) {
	// Native slot checkout is project-local: a project's configured slot
	// count and per-project concurrency cap decide whether that project can
	// lease another slot. Claimed slots in other projects must not block this
	// project, so keep the query on the requested project's partition.
	var docs []leaseDoc
	if err := singlePartitionQuery(
		ctx,
		s.leases,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @project AND c.state = @state AND c.metadata.native_k8s = true",
		[]azcosmos.QueryParameter{
			{Name: "@project", Value: project},
			{Name: "@state", Value: "claimed"},
		},
		&docs,
	); err != nil {
		return nil, err
	}
	readySlots := s.nativeReadySlots(ctx, project)
	return selectAvailableNativeSlot(project, readySlots, docs, s.nativeProjectCap()), nil
}

// nativeReadySlots returns the slot indices that are currently in the
// `provisioned` state and therefore eligible to receive a new lease.
//
// Slot status moved into the dedicated `slots` Cosmos collection in
// #518 ("test-slot: split slot status into its own Cosmos collection")
// so that per-slot warmup writes stopped contending on the project doc's
// etag. The dispatcher path was not updated at the same time: until this
// fix it kept reading `project.metadata.native_standby_dns.slots[]`,
// which #518 stopped populating, so every project's checkout returned
// ErrUnavailable regardless of actual capacity. The Stage 4 deliberate-
// 503 observability shipped in the contemporaneous Cosmos query
// contract rollout surfaced this directly:
// glimmung_unavailable_total{reason="test_slot_saturation"} ticked on
// every checkout attempt.
//
// The query is single-partition (slots PK is /project). count from
// project metadata still bounds the result so a stale slot doc cannot
// extend capacity past the declared scale.
func (s *Store) nativeReadySlots(ctx context.Context, project string) []int {
	doc, err := s.readProjectDoc(ctx, project)
	if err != nil {
		return nil
	}
	standby, ok := doc.Metadata["native_standby_dns"].(map[string]any)
	if !ok {
		return nil
	}
	count, ok := positiveIntValue(standby["count"])
	if !ok {
		return nil
	}
	slotDocs, err := s.ListSlotsByProject(ctx, project)
	if err != nil {
		return nil
	}
	return selectReadySlotIndices(slotDocs, count)
}

// selectReadySlotIndices returns the sorted slot indices that are in
// state provisioned and within the declared 1..count bound. Extracted
// from nativeReadySlots so the selection contract is testable without a
// live Cosmos round trip.
func selectReadySlotIndices(slots []server.Slot, count int) []int {
	out := make([]int, 0, len(slots))
	for _, slot := range slots {
		if slot.SlotIndex < 1 || slot.SlotIndex > count {
			continue
		}
		if slot.State == server.SlotStateProvisioned && slot.ActiveLeaseRef == nil {
			out = append(out, slot.SlotIndex)
		}
	}
	sort.Ints(out)
	return out
}

func selectAvailableNativeSlot(project string, readySlots []int, claimed []leaseDoc, projectCap int) *int {
	used := map[int]bool{}
	projectActive := 0
	for _, doc := range claimed {
		if doc.Project != project {
			continue
		}
		projectActive++
		if slot := nativeSlotIndex(doc.Metadata); slot != nil {
			used[*slot] = true
		}
	}
	if projectActive >= projectCap || projectActive >= len(readySlots) {
		return nil
	}
	for _, slot := range readySlots {
		if !used[slot] {
			selected := slot
			return &selected
		}
	}
	return nil
}

func (s *Store) nativeProjectCap() int {
	if s.nativeProjectConcurrency > 0 {
		return s.nativeProjectConcurrency
	}
	return 5
}

func (s *Store) nativeSlotPrefix(ctx context.Context, project string) string {
	doc, err := s.readProjectDoc(ctx, project)
	if err == nil {
		if standby, ok := doc.Metadata["native_standby_dns"].(map[string]any); ok {
			if prefix, ok := standby["slot_prefix"].(string); ok && strings.TrimSpace(prefix) != "" {
				return strings.Trim(strings.TrimSpace(prefix), ".")
			}
			if prefix, ok := standby["slotPrefix"].(string); ok && strings.TrimSpace(prefix) != "" {
				return strings.Trim(strings.TrimSpace(prefix), ".")
			}
		}
	}
	return project
}

func (s *Store) CancelLeaseByRef(ctx context.Context, project, ref string) (server.CancelLeaseResult, error) {
	// Find the lease doc by iterating all leases for the project.
	var docs []leaseDoc
	if err := singlePartitionQuery(
		ctx, s.leases,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @p",
		[]azcosmos.QueryParameter{{Name: "@p", Value: project}},
		&docs,
	); err != nil {
		return server.CancelLeaseResult{}, fmt.Errorf("query leases: %w", err)
	}
	found := selectLeaseDocByPublicRef(docs, ref)
	if found == nil {
		return server.CancelLeaseResult{}, server.ErrNotFound
	}

	publicRef := server.LeasePublicRefFromLease(leaseFromDoc(*found))

	if found.State == "released" || found.State == "expired" {
		return server.CancelLeaseResult{
			State:    "already_terminal",
			LeaseRef: publicRef,
		}, nil
	}

	found.State = "released"
	found.ReleasedAt = time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(found)
	if err != nil {
		return server.CancelLeaseResult{}, err
	}
	leasePK := azcosmos.NewPartitionKeyString(project)
	if _, err := s.leases.ReplaceItem(ctx, leasePK, found.ID, payload, nil); err != nil {
		return server.CancelLeaseResult{}, fmt.Errorf("release lease: %w", err)
	}
	lease := leaseFromDoc(*found)
	if boolValue(lease.Metadata["native_k8s"]) && !boolValue(lease.Metadata["test_slot_checkout"]) {
		_ = s.releaseNativeSlotReservation(ctx, lease, time.Now().UTC())
	}
	return server.CancelLeaseResult{
		State:    "no_active_run",
		LeaseRef: publicRef,
	}, nil
}

func (s *Store) releaseNativeSlotReservation(ctx context.Context, lease server.Lease, now time.Time) error {
	slot := nativeSlotIndex(lease.Metadata)
	if slot == nil {
		return nil
	}
	ref := server.LeasePublicRefFromLease(lease)
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		_, err := s.UpdateIfMatch(ctx, lease.Project, *slot, func(s server.Slot) (server.Slot, error) {
			return s.MarkReservationReleased(now, ref)
		})
		if errors.Is(err, server.ErrPreconditionFailed) {
			continue
		}
		return err
	}
	return server.ErrPreconditionFailed
}

func (s *Store) ReadLeaseByRef(ctx context.Context, project, ref string) (server.Lease, error) {
	var docs []leaseDoc
	if err := singlePartitionQuery(
		ctx, s.leases,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @p",
		[]azcosmos.QueryParameter{{Name: "@p", Value: project}},
		&docs,
	); err != nil {
		return server.Lease{}, fmt.Errorf("query leases: %w", err)
	}
	found := selectLeaseDocByPublicRef(docs, ref)
	if found == nil {
		return server.Lease{}, server.ErrNotFound
	}
	return leaseFromDoc(*found), nil
}

func (s *Store) UpdateLeaseTTLByRef(ctx context.Context, project, ref string, ttlSeconds int) (server.Lease, error) {
	if ttlSeconds <= 0 {
		return server.Lease{}, server.ValidationError{Message: "ttl_seconds must be positive"}
	}
	docID, err := s.leaseDocIDByPublicRef(ctx, project, ref)
	if err != nil {
		return server.Lease{}, err
	}
	pk := azcosmos.NewPartitionKeyString(project)
	for attempt := 0; attempt < maxLeaseConflictRetries; attempt++ {
		read, err := s.leases.ReadItem(ctx, pk, docID, nil)
		if err != nil {
			if isCosmosStatus(err, http.StatusNotFound) {
				return server.Lease{}, server.ErrNotFound
			}
			return server.Lease{}, fmt.Errorf("read lease: %w", err)
		}
		var doc leaseDoc
		if err := json.Unmarshal(read.Value, &doc); err != nil {
			return server.Lease{}, err
		}
		if doc.Project != project {
			return server.Lease{}, server.ErrNotFound
		}
		if doc.State != "claimed" {
			return server.Lease{}, server.ErrConflict
		}
		doc.TTLSeconds = ttlSeconds
		payload, err := json.Marshal(doc)
		if err != nil {
			return server.Lease{}, err
		}
		etag := azcore.ETag(read.ETag)
		if _, err := s.leases.ReplaceItem(ctx, pk, doc.ID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag}); err != nil {
			if isCosmosStatus(err, http.StatusPreconditionFailed) {
				continue
			}
			return server.Lease{}, fmt.Errorf("update lease TTL: %w", err)
		}
		return leaseFromDoc(doc), nil
	}
	return server.Lease{}, fmt.Errorf("update lease TTL: too many etag conflicts")
}

func (s *Store) leaseDocIDByPublicRef(ctx context.Context, project, ref string) (string, error) {
	var docs []leaseDoc
	if err := singlePartitionQuery(
		ctx, s.leases,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @p",
		[]azcosmos.QueryParameter{{Name: "@p", Value: project}},
		&docs,
	); err != nil {
		return "", fmt.Errorf("query leases: %w", err)
	}
	found := selectLeaseDocByPublicRef(docs, ref)
	if found == nil {
		return "", server.ErrNotFound
	}
	return found.ID, nil
}

func (s *Store) AppendTestSlotHotSwapHistory(ctx context.Context, project, ref string, entry server.TestSlotHotSwapHistoryEntry) (server.Lease, error) {
	var docs []leaseDoc
	if err := singlePartitionQuery(
		ctx, s.leases,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @p",
		[]azcosmos.QueryParameter{{Name: "@p", Value: project}},
		&docs,
	); err != nil {
		return server.Lease{}, fmt.Errorf("query leases: %w", err)
	}
	found := selectLeaseDocByPublicRef(docs, ref)
	if found == nil {
		return server.Lease{}, server.ErrNotFound
	}
	if found.Metadata == nil {
		found.Metadata = map[string]any{}
	}
	history := anySliceValue(found.Metadata["test_slot_hot_swap_history"])
	payload, err := json.Marshal(entry)
	if err != nil {
		return server.Lease{}, err
	}
	var entryMap map[string]any
	if err := json.Unmarshal(payload, &entryMap); err != nil {
		return server.Lease{}, err
	}
	history = append(history, entryMap)
	if len(history) > 20 {
		history = history[len(history)-20:]
	}
	found.Metadata["test_slot_hot_swap_history"] = history
	payload, err = json.Marshal(found)
	if err != nil {
		return server.Lease{}, err
	}
	if _, err := s.leases.ReplaceItem(ctx, azcosmos.NewPartitionKeyString(project), found.ID, payload, nil); err != nil {
		return server.Lease{}, fmt.Errorf("append hot-swap history: %w", err)
	}
	return leaseFromDoc(*found), nil
}

func anySliceValue(raw any) []any {
	if value, ok := raw.([]any); ok {
		return value
	}
	return nil
}

func selectLeaseDocByPublicRef(docs []leaseDoc, ref string) *leaseDoc {
	var found *leaseDoc
	for i := range docs {
		doc := &docs[i]
		lease, ok := listedLeaseFromDoc(*doc)
		if !ok {
			continue
		}
		if server.LeasePublicRefFromLease(lease) != ref {
			continue
		}
		if found == nil || cancelLeaseCandidateRank(*doc) < cancelLeaseCandidateRank(*found) {
			found = doc
		}
	}
	return found
}

func cancelLeaseCandidateRank(doc leaseDoc) int {
	switch doc.State {
	case "claimed":
		return 0
	case "waiting":
		return 1
	default:
		return 2
	}
}

func (s *Store) nextLeaseNumber(ctx context.Context, project string) (int, error) {
	counterID := leaseCounterPrefix + project
	pk := azcosmos.NewPartitionKeyString(project)
	for attempt := 0; attempt < maxLeaseConflictRetries; attempt++ {
		read, err := s.leases.ReadItem(ctx, pk, counterID, nil)
		if isCosmosStatus(err, http.StatusNotFound) {
			// Seed the counter from the highest existing lease number.
			highest, err := s.highestLeaseNumber(ctx, project)
			if err != nil {
				return 0, err
			}
			first := highest + 1
			now := time.Now().UTC().Format(time.RFC3339Nano)
			doc := map[string]any{
				"id":              counterID,
				"project":         project,
				"kind":            "lease_number_counter",
				"lastAllocated":   first,
				"nextLeaseNumber": first + 1,
				"created_at":      now,
				"updated_at":      now,
			}
			payload, _ := json.Marshal(doc)
			if _, err := s.leases.CreateItem(ctx, pk, payload, nil); err != nil {
				if isCosmosStatus(err, http.StatusConflict) {
					continue // lost the seed race, retry
				}
				return 0, fmt.Errorf("seed lease counter: %w", err)
			}
			return first, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read lease counter: %w", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(read.Value, &doc); err != nil {
			return 0, err
		}
		currentNext := int(floatVal(doc["nextLeaseNumber"]))
		if currentNext < 1 {
			currentNext = 1
		}
		updated := cloneMap(doc)
		delete(updated, "_etag")
		delete(updated, "_rid")
		delete(updated, "_self")
		delete(updated, "_attachments")
		delete(updated, "_ts")
		updated["nextLeaseNumber"] = currentNext + 1
		updated["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		payload, _ := json.Marshal(updated)
		etagStr, _ := doc["_etag"].(string)
		etagVal := azcore.ETag(etagStr)
		opts := &azcosmos.ItemOptions{IfMatchEtag: &etagVal}
		if _, err := s.leases.ReplaceItem(ctx, pk, counterID, payload, opts); err != nil {
			if isCosmosStatus(err, http.StatusPreconditionFailed) {
				continue
			}
			return 0, fmt.Errorf("increment lease counter: %w", err)
		}
		return currentNext, nil
	}
	return 0, fmt.Errorf("lease counter conflict after %d retries", maxLeaseConflictRetries)
}

func (s *Store) highestLeaseNumber(ctx context.Context, project string) (int, error) {
	var docs []struct {
		LeaseNumber *float64 `json:"leaseNumber"`
	}
	if err := singlePartitionQuery(
		ctx, s.leases,
		azcosmos.NewPartitionKeyString(project),
		"SELECT c.leaseNumber FROM c WHERE c.project = @p",
		[]azcosmos.QueryParameter{{Name: "@p", Value: project}},
		&docs,
	); err != nil {
		return 0, err
	}
	highest := 0
	for _, doc := range docs {
		if doc.LeaseNumber == nil {
			continue
		}
		n := int(*doc.LeaseNumber)
		if n > highest {
			highest = n
		}
	}
	return highest, nil
}

func buildLeaseMetadata(req server.LeaseAcquireRequest) map[string]any {
	m := map[string]any{}
	for k, v := range req.Metadata {
		m[k] = v
	}
	requesterDoc := map[string]any{
		"consumer": req.Requester.Consumer,
		"kind":     req.Requester.Kind,
		"ref":      req.Requester.Ref,
	}
	if req.Requester.Label != nil {
		requesterDoc["label"] = *req.Requester.Label
	}
	if req.Requester.URL != nil {
		requesterDoc["url"] = *req.Requester.URL
	}
	if len(req.Requester.Metadata) > 0 {
		requesterDoc["metadata"] = req.Requester.Metadata
	}
	m["requester"] = requesterDoc
	m["requester_consumer"] = req.Requester.Consumer
	m["requester_kind"] = req.Requester.Kind
	m["requester_ref"] = req.Requester.Ref
	return m
}

func isNativeLeaseRequest(req server.LeaseAcquireRequest) bool {
	return boolValue(req.Metadata["native_k8s"]) ||
		boolValue(req.Metadata["test_slot_checkout"]) ||
		boolValue(req.Requirements["native_k8s"])
}

func validateNativeLeaseSlotIdentity(metadata map[string]any) error {
	for _, key := range []string{
		"native_slot_index",
		"native_slot_name",
		"native_slot_prefix",
		"native_sessions_namespace",
	} {
		if valueHasMeaning(metadata[key]) {
			return server.ValidationError{Message: "native lease requests may not include caller-supplied slot identity field " + key}
		}
	}
	phaseInputs := anyMapValue(metadata["phase_inputs"])
	for _, key := range []string{"validation_slot_index", "slot_index", "native_slot_index", "native_slot_name"} {
		if valueHasMeaning(phaseInputs[key]) {
			return server.ValidationError{Message: "native lease requests may not include caller-supplied slot identity field phase_inputs." + key}
		}
	}
	if boolValue(metadata["test_slot_checkout"]) && len(phaseInputs) > 0 {
		return server.ValidationError{Message: "test-slot checkout lease requests may not include phase_inputs"}
	}
	return nil
}

func valueHasMeaning(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return true
	}
}

func nativeSlotIndex(metadata map[string]any) *int {
	if n, ok := positiveIntValue(metadata["native_slot_index"]); ok {
		return &n
	}
	return nil
}

func setNativeSlotMetadata(metadata map[string]any, project string, slotIndex int, slotPrefix string) {
	metadata["native_slot_index"] = strconv.Itoa(slotIndex)
	prefix := strings.Trim(strings.TrimSpace(slotPrefix), ".")
	if prefix == "" {
		prefix = project
	}
	metadata["native_slot_name"] = fmt.Sprintf("%s-%d", prefix, slotIndex)
}

func matchesRequirements(caps map[string]any, required map[string]any) bool {
	for key, want := range required {
		have := caps[key]
		switch w := want.(type) {
		case []any:
			h, ok := have.([]any)
			if !ok {
				return false
			}
			haveSet := make(map[any]bool, len(h))
			for _, v := range h {
				haveSet[v] = true
			}
			for _, v := range w {
				if !haveSet[v] {
					return false
				}
			}
		default:
			if have != want {
				return false
			}
		}
	}
	return true
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func floatVal(v any) float64 {
	if v == nil {
		return 0
	}
	switch f := v.(type) {
	case float64:
		return f
	case float32:
		return float64(f)
	case int:
		return float64(f)
	case int64:
		return float64(f)
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// WorkflowSyncStore
// ---------------------------------------------------------------------------

func (s *Store) GetWorkflowByName(ctx context.Context, project, name string) (*server.Workflow, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	read, err := s.workflows.ReadItem(ctx, pk, name, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc workflowDoc
	if err := json.Unmarshal(read.Value, &doc); err != nil {
		return nil, err
	}
	w := workflowFromDoc(doc)
	return &w, nil
}

func (s *Store) GetWorkflowBySchemaRef(ctx context.Context, project, schemaRef string) (*server.Workflow, error) {
	if strings.TrimSpace(schemaRef) == "" {
		return nil, nil
	}
	var docs []workflowDoc
	if err := singlePartitionQuery(
		ctx,
		s.workflows,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @p AND c.schema_ref = @schema_ref",
		[]azcosmos.QueryParameter{
			{Name: "@p", Value: project},
			{Name: "@schema_ref", Value: schemaRef},
		},
		&docs,
	); err != nil {
		return nil, err
	}
	for _, doc := range docs {
		if isWorkflowSchemaDoc(doc) {
			w := workflowFromDoc(doc)
			return &w, nil
		}
	}
	if len(docs) > 0 {
		w := workflowFromDoc(docs[0])
		return &w, nil
	}
	return nil, nil
}

// ReadRunForReplay reads a run document and returns the minimal fields
// needed by the replay decision engine.
func (s *Store) ReadRunForReplay(ctx context.Context, project, runID string) (server.RunReplayData, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return server.RunReplayData{}, server.ErrNotFound
	}
	if err != nil {
		return server.RunReplayData{}, err
	}
	var doc runDoc
	if err := json.Unmarshal(resp.Value, &doc); err != nil {
		return server.RunReplayData{}, err
	}
	return runReplayDataFromDoc(doc), nil
}

func (s *Store) ListQueuedRunCycles(ctx context.Context, project string, limit int) ([]server.RunReplayData, error) {
	var docs []runDoc
	if err := singlePartitionQuery(
		ctx,
		s.runs,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @p AND c.state = @state AND c.queue_state = @queue_state ORDER BY c.created_at ASC",
		[]azcosmos.QueryParameter{
			{Name: "@p", Value: project},
			{Name: "@state", Value: "queued"},
			{Name: "@queue_state", Value: "queued"},
		},
		&docs,
	); err != nil {
		return nil, err
	}
	if limit > 0 && len(docs) > limit {
		docs = docs[:limit]
	}
	out := make([]server.RunReplayData, 0, len(docs))
	for _, doc := range docs {
		out = append(out, runReplayDataFromDoc(doc))
	}
	return out, nil
}

func runReplayDataFromDoc(doc runDoc) server.RunReplayData {
	attempts := make([]server.RunAttemptData, 0, len(doc.Attempts))
	for _, a := range doc.Attempts {
		conclusion := ""
		if a.Conclusion != nil {
			conclusion = *a.Conclusion
		}
		var verif *server.RunVerificationData
		if a.Verification != nil {
			verif = &server.RunVerificationData{
				Status:  a.Verification.Status,
				Reasons: a.Verification.Reasons,
			}
		}
		attempts = append(attempts, server.RunAttemptData{
			AttemptIndex: a.AttemptIndex,
			Phase:        a.Phase,
			Conclusion:   conclusion,
			Verification: verif,
			Decision:     stringOrEmpty(a.Decision),
			Completed:    a.CompletedAt != "",
			CarryForward: a.CarryForward,
			PhaseOutputs: stringMapOrEmpty(a.PhaseOutputs),
		})
	}
	var bdg budget.Config
	if doc.Budget != nil {
		bdg.Total = doc.Budget.Total
	}
	return server.RunReplayData{
		ID:                doc.ID,
		Project:           doc.Project,
		WorkflowName:      doc.Workflow,
		WorkflowSchemaRef: doc.WorkflowSchemaRef,
		Attempts:          attempts,
		CumulativeCostUSD: doc.CumulativeCostUSD,
		Budget:            bdg,
		IssueNumber:       doc.IssueNumber,
		RunNumber:         doc.RunNumber,
		CycleNumber:       doc.CycleNumber,
		RunCycleNumber:    doc.RunCycleNumber,
		RunDisplayNumber:  doc.RunDisplayNumber,
		IssueRepo:         doc.IssueRepo,
		CallbackToken:     doc.CallbackToken,
		IssueLockHolderID: doc.IssueLockHolderID,
		PRNumber:          doc.PRNumber,
		PRLockHolderID:    doc.PRLockHolderID,
		SlotLeaseRef:      doc.SlotLeaseRef,
		EntrypointPhase:   doc.EntrypointPhase,
		TriggerSource:     mapOrEmpty(doc.TriggerSource),
	}
}

func (s *Store) UpsertWorkflowFromRegister(ctx context.Context, reg server.WorkflowRegister) (server.Workflow, error) {
	return s.UpsertWorkflow(ctx, reg)
}

func (s *Store) EnqueueSignal(ctx context.Context, req server.SignalEnqueue) (server.PublicSignal, error) {
	var targetID string
	switch req.TargetType {
	case "pr":
		targetID = req.TargetRef
	case "issue":
		id := s.resolveIssueIDByRef(ctx, req.TargetRepo, req.TargetRef)
		if id == nil {
			return server.PublicSignal{}, server.ErrNotFound
		}
		targetID = *id
	case "run":
		id := s.resolveRunIDByRef(ctx, req.TargetRepo, req.TargetRef)
		if id == nil {
			return server.PublicSignal{}, server.ErrNotFound
		}
		targetID = *id
	default:
		return server.PublicSignal{}, fmt.Errorf("unknown target_type: %s", req.TargetType)
	}

	now := time.Now().UTC()
	payload := req.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	doc := signalDoc{
		ID:            uuid.NewString(),
		SchemaVersion: 1,
		TargetType:    req.TargetType,
		TargetRepo:    req.TargetRepo,
		TargetID:      targetID,
		Source:        req.Source,
		Payload:       payload,
		State:         "pending",
		EnqueuedAt:    now.Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return server.PublicSignal{}, fmt.Errorf("marshal signal: %w", err)
	}
	pk := azcosmos.NewPartitionKeyString(doc.TargetRepo)
	if _, err := s.signals.CreateItem(ctx, pk, data, nil); err != nil {
		return server.PublicSignal{}, fmt.Errorf("create signal: %w", err)
	}
	ref := fmt.Sprintf("signal:%s:%s:%s:%s", doc.TargetType, doc.TargetRepo, req.TargetRef, now.Format(time.RFC3339Nano))
	return server.PublicSignal{
		Ref:        ref,
		TargetType: doc.TargetType,
		TargetRepo: doc.TargetRepo,
		TargetRef:  req.TargetRef,
		Source:     doc.Source,
		State:      doc.State,
		EnqueuedAt: now,
	}, nil
}

func (s *Store) ListGraphSignals(ctx context.Context, filter server.GraphSignalFilter) ([]server.GraphSignal, error) {
	// signals container is partitioned by /target_repo and the dashboard
	// asks for a global view across every repo. Cosmos cannot serve an
	// ORDER BY across partitions from the Go SDK, so we drop ORDER BY
	// here and sort in Go after — the docs are small (one per pending
	// signal) and ListPendingSignals already follows the same pattern.
	query := "SELECT * FROM c"
	var params []azcosmos.QueryParameter
	if strings.TrimSpace(filter.State) != "" {
		query += " WHERE c.state = @state"
		params = append(params, azcosmos.QueryParameter{Name: "@state", Value: filter.State})
	}
	var docs []signalDoc
	if err := crossPartitionQuery(ctx, s.signals, query, params, &docs); err != nil {
		return nil, err
	}
	sort.Slice(docs, func(i, j int) bool {
		return docs[i].EnqueuedAt < docs[j].EnqueuedAt
	})
	signals := make([]server.GraphSignal, 0, len(docs))
	for _, doc := range docs {
		signals = append(signals, server.GraphSignal{
			ID:                doc.ID,
			TargetType:        doc.TargetType,
			TargetRepo:        doc.TargetRepo,
			TargetID:          doc.TargetID,
			Source:            doc.Source,
			Payload:           mapOrEmpty(doc.Payload),
			State:             firstNonEmpty(doc.State, "pending"),
			EnqueuedAt:        parseTimeOrNow(doc.EnqueuedAt),
			ProcessedDecision: doc.ProcessedDecision,
			FailureReason:     doc.FailureReason,
		})
	}
	return signals, nil
}

func (s *Store) ListPendingSignals(ctx context.Context, limit int) ([]server.QueuedSignal, error) {
	// Global dispatch queue across every repo; signals container is
	// partitioned by /target_repo. Cross-partition scan with no ORDER
	// BY — the gateway handles this directly.
	query := "SELECT * FROM c WHERE c.state = @state"
	var docs []signalDoc
	if err := crossPartitionQuery(ctx, s.signals, query,
		[]azcosmos.QueryParameter{{Name: "@state", Value: "pending"}},
		&docs,
	); err != nil {
		return nil, err
	}
	sort.Slice(docs, func(i, j int) bool {
		return docs[i].EnqueuedAt < docs[j].EnqueuedAt
	})
	if limit > 0 && len(docs) > limit {
		docs = docs[:limit]
	}
	out := make([]server.QueuedSignal, 0, len(docs))
	for _, doc := range docs {
		out = append(out, queuedSignalFromDoc(doc))
	}
	return out, nil
}

func (s *Store) MarkSignalProcessing(ctx context.Context, signal server.QueuedSignal) (server.QueuedSignal, bool, error) {
	doc, etag, err := s.readSignalDoc(ctx, signal.TargetRepo, signal.ID)
	if err != nil {
		return server.QueuedSignal{}, false, err
	}
	if doc.State != "pending" {
		return queuedSignalFromDoc(doc), false, nil
	}
	doc.State = "processing"
	updated, err := s.replaceSignalDoc(ctx, signal.TargetRepo, doc, etag)
	if isCosmosStatus(err, http.StatusPreconditionFailed) {
		return server.QueuedSignal{}, false, nil
	}
	if err != nil {
		return server.QueuedSignal{}, false, err
	}
	return queuedSignalFromDoc(updated), true, nil
}

func (s *Store) MarkSignalProcessed(ctx context.Context, signal server.QueuedSignal, decision string) (server.QueuedSignal, error) {
	doc, etag, err := s.readSignalDoc(ctx, signal.TargetRepo, signal.ID)
	if err != nil {
		return server.QueuedSignal{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc.State = "processed"
	doc.ProcessedAt = &now
	doc.ProcessedDecision = &decision
	updated, err := s.replaceSignalDoc(ctx, signal.TargetRepo, doc, etag)
	if err != nil {
		return server.QueuedSignal{}, err
	}
	return queuedSignalFromDoc(updated), nil
}

func (s *Store) MarkSignalFailed(ctx context.Context, signal server.QueuedSignal, reason string) error {
	doc, etag, err := s.readSignalDoc(ctx, signal.TargetRepo, signal.ID)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc.State = "failed"
	doc.ProcessedAt = &now
	if len(reason) > 500 {
		reason = reason[:500]
	}
	doc.FailureReason = &reason
	_, err = s.replaceSignalDoc(ctx, signal.TargetRepo, doc, etag)
	return err
}

func (s *Store) readSignalDoc(ctx context.Context, targetRepo, id string) (signalDoc, azcore.ETag, error) {
	pk := azcosmos.NewPartitionKeyString(targetRepo)
	resp, err := s.signals.ReadItem(ctx, pk, id, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return signalDoc{}, "", server.ErrNotFound
	}
	if err != nil {
		return signalDoc{}, "", err
	}
	var doc signalDoc
	if err := json.Unmarshal(resp.Value, &doc); err != nil {
		return signalDoc{}, "", err
	}
	return doc, resp.ETag, nil
}

func (s *Store) replaceSignalDoc(ctx context.Context, targetRepo string, doc signalDoc, etag azcore.ETag) (signalDoc, error) {
	payload, err := json.Marshal(doc)
	if err != nil {
		return signalDoc{}, err
	}
	pk := azcosmos.NewPartitionKeyString(targetRepo)
	resp, err := s.signals.ReplaceItem(ctx, pk, doc.ID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag})
	if err != nil {
		return signalDoc{}, err
	}
	var updated signalDoc
	if err := json.Unmarshal(resp.Value, &updated); err != nil {
		return signalDoc{}, err
	}
	return updated, nil
}

func queuedSignalFromDoc(doc signalDoc) server.QueuedSignal {
	return server.QueuedSignal{
		ID:         doc.ID,
		TargetType: doc.TargetType,
		TargetRepo: doc.TargetRepo,
		TargetID:   doc.TargetID,
		Source:     doc.Source,
		Payload:    mapOrEmpty(doc.Payload),
		State:      firstNonEmpty(doc.State, "pending"),
		EnqueuedAt: parseTimeOrZero(doc.EnqueuedAt),
	}
}

func (s *Store) FindRunForPR(ctx context.Context, repo string, prNumber int) (server.RunReplayData, error) {
	// Caller knows only (repo, pr_number); the runs container is
	// partitioned by /project so we fan out across every project
	// partition, then pick the most-recent doc by updated_at in Go.
	// The per-partition queries omit ORDER BY (which would force the
	// Go SDK down the unsupported query-plan path); ordering is
	// applied after the merge.
	projects, err := s.listProjectNames(ctx)
	if err != nil {
		return server.RunReplayData{}, err
	}
	var docs []runDoc
	if err := fanOutByProject(ctx, s.runs, projects,
		"SELECT * FROM c WHERE c.project = @project AND c.issue_repo = @repo AND c.pr_number = @pr",
		[]azcosmos.QueryParameter{
			{Name: "@repo", Value: repo},
			{Name: "@pr", Value: prNumber},
		},
		&docs,
	); err != nil {
		return server.RunReplayData{}, err
	}
	if len(docs) == 0 {
		return server.RunReplayData{}, server.ErrNotFound
	}
	sort.SliceStable(docs, func(i, j int) bool {
		return docs[i].UpdatedAt > docs[j].UpdatedAt
	})
	return s.ReadRunForReplay(ctx, docs[0].Project, docs[0].ID)
}

// ---- RunMutationStore implementation ----

// ReadRunIDForNumber resolves an issue-scoped run number to (runID, runRef).
func (s *Store) ReadRunIDForNumber(ctx context.Context, project string, issueNumber int, runNumber string) (string, string, error) {
	docs, err := s.issueRunDocs(ctx, project, issueNumber)
	if err != nil {
		return "", "", err
	}
	numbers := runNumberMap(docs)
	for _, doc := range docs {
		display := ""
		if doc.RunDisplayNumber != nil {
			display = strings.TrimSpace(*doc.RunDisplayNumber)
		}
		if (display != "" && display == strings.TrimSpace(runNumber)) ||
			fmt.Sprintf("%d", numbers[doc.ID]) == strings.TrimSpace(runNumber) {
			ref := publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), runDisplayNumber(doc, numbers[doc.ID]))
			return doc.ID, ref, nil
		}
	}
	return "", "", server.ErrNotFound
}

// ReadRunIDForCallbackToken resolves a run callback token to (runID, project, runRef).
func (s *Store) ReadRunIDForCallbackToken(ctx context.Context, token string) (string, string, string, error) {
	var docs []runDoc
	if err := crossPartitionQuery(
		ctx, s.runs,
		"SELECT * FROM c WHERE c.callback_token = @token",
		[]azcosmos.QueryParameter{{Name: "@token", Value: token}},
		&docs,
	); err != nil {
		return "", "", "", err
	}
	if len(docs) == 0 {
		return "", "", "", server.ErrNotFound
	}
	doc := docs[0]
	sibling, _ := s.issueRunDocs(ctx, doc.Project, doc.IssueNumber)
	numbers := runNumberMap(sibling)
	ref := publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), runDisplayNumber(doc, numbers[doc.ID]))
	return doc.ID, doc.Project, ref, nil
}

// AbortRunByID marks a run as aborted, best-effort releases issue/PR locks and
// any run slot lease.
func (s *Store) AbortRunByID(ctx context.Context, project, runID, reason string) (server.AbortRunResult, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return server.AbortRunResult{}, server.ErrNotFound
	}
	if err != nil {
		return server.AbortRunResult{}, err
	}

	var doc runDoc
	if err := json.Unmarshal(resp.Value, &doc); err != nil {
		return server.AbortRunResult{}, err
	}

	terminal := doc.State == "passed" || doc.State == "review_required" || doc.State == "aborted" || doc.State == "recycled"

	// Compute run_ref for the result.
	siblings, _ := s.issueRunDocs(ctx, project, doc.IssueNumber)
	numbers := runNumberMap(siblings)
	runRef := publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), runDisplayNumber(doc, numbers[doc.ID]))

	if terminal {
		slotLeaseReleased := s.releaseRunSlotLease(ctx, doc)
		return server.AbortRunResult{
			State:             "already_terminal",
			RunRef:            runRef,
			RunNumber:         doc.RunNumber,
			RunDisplayNumber:  doc.RunDisplayNumber,
			SlotLeaseReleased: slotLeaseReleased,
		}, nil
	}

	// Patch the run doc to aborted state.
	var raw map[string]any
	if err := json.Unmarshal(resp.Value, &raw); err != nil {
		return server.AbortRunResult{}, err
	}
	raw["state"] = "aborted"
	raw["abort_reason"] = reason
	delete(raw, "queue_state")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	raw["updated_at"] = now
	finalizeExecutionFailureRaw(raw, canonicalExecutionFailureReason(reason), now)

	payload, err := json.Marshal(raw)
	if err != nil {
		return server.AbortRunResult{}, err
	}
	etag := azcore.ETag(resp.ETag)
	if _, err := s.runs.ReplaceItem(ctx, pk, runID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag}); err != nil {
		return server.AbortRunResult{}, err
	}

	// Best-effort lock releases.
	var issueLockReleased, prLockReleased *bool
	if doc.IssueLockHolderID != nil && *doc.IssueLockHolderID != "" && doc.IssueNumber > 0 {
		released := s.pgLocks.ReleaseLock(ctx, "issue", fmt.Sprintf("%s#%d", project, doc.IssueNumber), *doc.IssueLockHolderID)
		issueLockReleased = &released
	}
	if doc.PRLockHolderID != nil && *doc.PRLockHolderID != "" && doc.PRNumber != nil && doc.IssueRepo != "" {
		released := s.pgLocks.ReleaseLock(ctx, "pr", fmt.Sprintf("%s#%d", doc.IssueRepo, *doc.PRNumber), *doc.PRLockHolderID)
		prLockReleased = &released
	}
	slotLeaseReleased := s.releaseRunSlotLease(ctx, doc)

	return server.AbortRunResult{
		State:             "aborted",
		RunRef:            runRef,
		RunNumber:         doc.RunNumber,
		RunDisplayNumber:  doc.RunDisplayNumber,
		IssueLockReleased: issueLockReleased,
		PRLockReleased:    prLockReleased,
		SlotLeaseReleased: slotLeaseReleased,
	}, nil
}

func (s *Store) releaseRunSlotLease(ctx context.Context, doc runDoc) *bool {
	ref := strings.TrimSpace(stringOrEmpty(doc.SlotLeaseRef))
	if ref == "" {
		return nil
	}
	_, err := s.CancelLeaseByRef(ctx, doc.Project, ref)
	released := err == nil
	return &released
}

// ---- NativeRunStore implementation ----

// GetNativeRunStatusByID returns the native run status for the latest in-progress k8s_job attempt.
func (s *Store) GetNativeRunStatusByID(ctx context.Context, project, runID string) (server.NativeRunStatusResponse, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return server.NativeRunStatusResponse{}, server.ErrNotFound
	}
	if err != nil {
		return server.NativeRunStatusResponse{}, err
	}

	var doc runDoc
	if err := json.Unmarshal(resp.Value, &doc); err != nil {
		return server.NativeRunStatusResponse{}, err
	}

	if len(doc.Attempts) == 0 {
		return server.NativeRunStatusResponse{}, server.ErrConflict
	}
	latest := doc.Attempts[len(doc.Attempts)-1]
	if latest.PhaseKind != "k8s_job" {
		return server.NativeRunStatusResponse{}, server.ErrConflict
	}

	siblings, _ := s.issueRunDocs(ctx, project, doc.IssueNumber)
	numbers := runNumberMap(siblings)
	runRef := publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), runDisplayNumber(doc, numbers[doc.ID]))

	return server.NativeRunStatusResponse{
		Project:           project,
		RunRef:            runRef,
		State:             doc.State,
		AttemptIndex:      latest.AttemptIndex,
		CancelRequested:   latest.CancelRequestedAt != nil && *latest.CancelRequestedAt != "",
		CancelRequestedAt: parseOptionalTime(stringOrEmpty(latest.CancelRequestedAt)),
		CancelReason:      latest.CancelReason,
	}, nil
}

// RecordNativeEventByID writes one idempotent native event for the run's latest in-progress attempt.
func (s *Store) RecordNativeEventByID(ctx context.Context, project, runID string, req server.NativeRunEventRequest) (server.NativeRunEventResult, error) {
	// Read run to get the latest attempt index + phase.
	pk := azcosmos.NewPartitionKeyString(project)
	resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return server.NativeRunEventResult{}, server.ErrNotFound
	}
	if err != nil {
		return server.NativeRunEventResult{}, err
	}

	var doc runDoc
	if err := json.Unmarshal(resp.Value, &doc); err != nil {
		return server.NativeRunEventResult{}, err
	}

	attemptIndex := 0
	phase := ""
	if len(doc.Attempts) > 0 {
		latest := doc.Attempts[len(doc.Attempts)-1]
		attemptIndex = latest.AttemptIndex
		phase = latest.Phase
	}
	if requestedAttempt, ok := nativeEventAttemptIndex(req); ok {
		found := false
		for _, attempt := range doc.Attempts {
			if attempt.AttemptIndex == requestedAttempt {
				attemptIndex = requestedAttempt
				phase = attempt.Phase
				found = true
				break
			}
		}
		if !found {
			return server.NativeRunEventResult{}, server.ValidationError{Message: fmt.Sprintf("attempt_index %d is not registered on run", requestedAttempt)}
		}
	}

	// Build the idempotency key.
	docID := fmt.Sprintf("%s::%04d::%s::%012d", runID, attemptIndex, req.JobID, req.Seq)

	stepSlug := ""
	if req.StepSlug != nil {
		stepSlug = *req.StepSlug
	}
	message := ""
	if req.Message != nil {
		message = *req.Message
	}

	// Stage 2c: write the event to pg via pgRunEvents instead of the
	// cosmos `runEvents` container. The cosmos-shaped nativeEventDoc is
	// kept for the in-process applyNativeEventExecutionState call below
	// (which mutates the runs document — runs is still in cosmos until
	// Stage 2e). Idempotency semantics match the cosmos contract: the
	// same PK with the same payload is accepted silently; the same PK
	// with a different payload returns ErrConflict.
	eventDoc := nativeEventDoc{
		ID:           docID,
		Project:      project,
		RunID:        runID,
		AttemptIndex: attemptIndex,
		Phase:        phase,
		JobID:        req.JobID,
		Seq:          req.Seq,
		Event:        req.Event,
		StepSlug:     stepSlug,
		Message:      message,
		ExitCode:     req.ExitCode,
		Metadata:     mapOrEmpty(req.Metadata),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	created, err := s.pgRunEvents.Insert(ctx, pgstore.RunEventRow{
		RunID:        runID,
		AttemptIndex: attemptIndex,
		JobID:        req.JobID,
		Seq:          req.Seq,
		Project:      project,
		Event:        req.Event,
		Phase:        phase,
		StepSlug:     stepSlug,
		Message:      message,
		ExitCode:     req.ExitCode,
		Metadata:     mapOrEmpty(req.Metadata),
		CreatedAt:    parseTimeOrZero(eventDoc.CreatedAt),
	})
	if err != nil {
		if errors.Is(err, pgstore.ErrRunEventConflict) {
			return server.NativeRunEventResult{}, server.ErrConflict
		}
		return server.NativeRunEventResult{}, err
	}
	if created {
		if err := s.applyNativeEventExecutionState(ctx, project, runID, eventDoc); err != nil {
			return server.NativeRunEventResult{}, err
		}
	}

	// Compute run_ref for the response.
	siblings, _ := s.issueRunDocs(ctx, project, doc.IssueNumber)
	numbers := runNumberMap(siblings)
	runRef := publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), runDisplayNumber(doc, numbers[doc.ID]))

	return server.NativeRunEventResult{
		RunRef:   runRef,
		JobID:    req.JobID,
		Seq:      req.Seq,
		Accepted: true,
	}, nil
}

// ListAllRunEventDocsForMigration reads every native-event document in
// the cosmos `run_events` container and returns it in the shape
// pg.RunEventsStore.Migrate consumes. Runs once per pod start (Insert
// is idempotent via ON CONFLICT DO NOTHING). Stage 2i removes this
// method along with the cosmos runEvents container client.
func (s *Store) ListAllRunEventDocsForMigration(ctx context.Context) ([]pgstore.RunEventRow, error) {
	if s == nil || s.runEvents == nil {
		return nil, nil
	}
	var docs []nativeEventDoc
	if err := crossPartitionQuery(ctx, s.runEvents, "SELECT * FROM c", nil, &docs); err != nil {
		return nil, err
	}
	out := make([]pgstore.RunEventRow, 0, len(docs))
	for _, doc := range docs {
		out = append(out, pgstore.RunEventRow{
			RunID:        doc.RunID,
			AttemptIndex: doc.AttemptIndex,
			JobID:        doc.JobID,
			Seq:          doc.Seq,
			Project:      doc.Project,
			Event:        doc.Event,
			Phase:        doc.Phase,
			StepSlug:     doc.StepSlug,
			Message:      doc.Message,
			ExitCode:     doc.ExitCode,
			Metadata:     mapOrEmpty(doc.Metadata),
			CreatedAt:    parseTimeOrZero(doc.CreatedAt),
		})
	}
	return out, nil
}

func (s *Store) applyNativeEventExecutionState(ctx context.Context, project, runID string, event nativeEventDoc) error {
	return s.mutateRunRaw(ctx, project, runID, func(doc runDoc, raw map[string]any) (bool, error) {
		var attempt attemptDoc
		found := false
		for _, candidate := range doc.Attempts {
			if candidate.AttemptIndex == event.AttemptIndex {
				attempt = candidate
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
		applyNativeEventToExecutionsRaw(raw, attempt, event)
		if event.Event == "phase_output_set" {
			if err := applyNativePhaseOutputSetRaw(raw, attempt, event); err != nil {
				return false, err
			}
		}
		raw["updated_at"] = event.CreatedAt
		return true, nil
	})
}

func applyNativePhaseOutputSetRaw(raw map[string]any, attempt attemptDoc, event nativeEventDoc) error {
	key := strings.TrimSpace(stringValue(event.Metadata["key"]))
	if key == "" {
		return server.ValidationError{Message: "phase_output_set event requires metadata.key"}
	}
	value := stringValue(event.Metadata["value"])
	attempts, _ := raw["attempts"].([]any)
	for i, rawAttempt := range attempts {
		attemptMap, ok := rawAttempt.(map[string]any)
		if !ok {
			continue
		}
		if attemptIndexFromRaw(attemptMap) != attempt.AttemptIndex {
			continue
		}
		outputs, _ := attemptMap["phase_outputs"].(map[string]any)
		if outputs == nil {
			outputs = map[string]any{}
		}
		if _, exists := outputs[key]; exists {
			return server.ValidationError{Message: fmt.Sprintf("phase output %q was already set for attempt %d", key, attempt.AttemptIndex)}
		}
		outputs[key] = value
		attemptMap["phase_outputs"] = outputs
		attempts[i] = attemptMap
		raw["attempts"] = attempts
		return nil
	}
	return nil
}

func attemptIndexFromRaw(attempt map[string]any) int {
	switch value := attempt["attempt_index"].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	default:
		return 0
	}
}

// ListNativeEventsByID returns ordered native events for a run.
func (s *Store) ListNativeEventsByID(ctx context.Context, project, runID string, attemptIndex *int, jobID *string, limit *int) (server.NativeRunLogsResponse, error) {
	// Validate that the run exists first.
	pk := azcosmos.NewPartitionKeyString(project)
	resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return server.NativeRunLogsResponse{}, server.ErrNotFound
	}
	if err != nil {
		return server.NativeRunLogsResponse{}, err
	}
	var doc runDoc
	if err := json.Unmarshal(resp.Value, &doc); err != nil {
		return server.NativeRunLogsResponse{}, err
	}

	// Stage 2c: read events from pg instead of the cosmos `runEvents`
	// container. pg.RunEventsStore.List returns rows in the canonical
	// sort order (attempt_index, job_id, seq, created_at) so no
	// post-sort is needed. Optional attemptIndex/jobID/limit are
	// pushed down to SQL.
	rows, err := s.pgRunEvents.List(ctx, runID, attemptIndex, jobID, limit)
	if err != nil {
		return server.NativeRunLogsResponse{}, err
	}

	siblings, _ := s.issueRunDocs(ctx, project, doc.IssueNumber)
	numbers := runNumberMap(siblings)
	runRef := publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), runDisplayNumber(doc, numbers[doc.ID]))

	events := make([]server.NativeRunLogEvent, 0, len(rows))
	for _, e := range rows {
		events = append(events, server.NativeRunLogEvent{
			Project:      project,
			RunRef:       runRef,
			AttemptIndex: e.AttemptIndex,
			Phase:        e.Phase,
			JobID:        e.JobID,
			Seq:          e.Seq,
			Event:        e.Event,
			StepSlug:     e.StepSlug,
			Message:      e.Message,
			ExitCode:     e.ExitCode,
			Metadata:     mapOrEmpty(e.Metadata),
			CreatedAt:    e.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}

	return server.NativeRunLogsResponse{
		Project:      project,
		RunRef:       runRef,
		AttemptIndex: attemptIndex,
		JobID:        jobID,
		Events:       events,
	}, nil
}

func nativeEventAttemptIndex(req server.NativeRunEventRequest) (int, bool) {
	if req.AttemptIndex != nil {
		return *req.AttemptIndex, true
	}
	if req.Metadata == nil {
		return 0, false
	}
	switch value := req.Metadata["attempt_index"].(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// RecordNativeJobCompletion records a single native job's terminal callback.
// It returns CompletionReady only for the transition where the final expected
// job for the current phase has completed; callers should run the decision
// engine only for that transition.
func (s *Store) RecordNativeJobCompletion(ctx context.Context, project, runID string, p server.CompletionPayload) (server.NativeJobCompletionResult, error) {
	jobID := ""
	if p.JobID != nil {
		jobID = strings.TrimSpace(*p.JobID)
	}
	if jobID == "" {
		return server.NativeJobCompletionResult{}, server.ValidationError{Message: "job_id required"}
	}

	pk := azcosmos.NewPartitionKeyString(project)
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
		if isCosmosStatus(err, http.StatusNotFound) {
			return server.NativeJobCompletionResult{}, server.ErrNotFound
		}
		if err != nil {
			return server.NativeJobCompletionResult{}, err
		}

		var doc runDoc
		if err := json.Unmarshal(resp.Value, &doc); err != nil {
			return server.NativeJobCompletionResult{}, err
		}
		var raw map[string]any
		if err := json.Unmarshal(resp.Value, &raw); err != nil {
			return server.NativeJobCompletionResult{}, err
		}
		attempts, _ := raw["attempts"].([]any)
		if len(doc.Attempts) == 0 || len(attempts) == 0 {
			return server.NativeJobCompletionResult{}, server.ErrConflict
		}

		idx := len(doc.Attempts) - 1
		if p.AttemptIndex != nil && *p.AttemptIndex >= 0 && *p.AttemptIndex < len(doc.Attempts) {
			idx = *p.AttemptIndex
		}
		attemptMap, ok := attempts[idx].(map[string]any)
		if !ok {
			return server.NativeJobCompletionResult{}, fmt.Errorf("invalid attempt at index %d", idx)
		}
		attempt := doc.Attempts[idx]
		if attempt.PhaseKind != "k8s_job" {
			return server.NativeJobCompletionResult{}, server.ErrConflict
		}

		expectedJobIDs, err := s.expectedNativeJobIDs(ctx, project, doc.Workflow, doc.WorkflowSchemaRef, attempt.Phase)
		if err != nil {
			return server.NativeJobCompletionResult{}, err
		}
		if !containsString(expectedJobIDs, jobID) {
			return server.NativeJobCompletionResult{}, server.ValidationError{
				Message: fmt.Sprintf("job_id %q is not registered on phase %q", jobID, attempt.Phase),
			}
		}

		completions := cloneJobCompletions(attempt.JobCompletions)
		if attempt.CompletedAt != "" {
			run, err := s.ReadRunForReplay(ctx, project, runID)
			if err != nil {
				return server.NativeJobCompletionResult{}, err
			}
			return nativeJobCompletionResult(run, expectedJobIDs, completions, true, false), nil
		}
		newCompletion := nativeJobCompletionDocFromPayload(jobID, p, time.Now().UTC().Format(time.RFC3339Nano))
		if existing, exists := completions[jobID]; exists {
			if !sameNativeJobCompletion(existing, newCompletion) {
				return server.NativeJobCompletionResult{}, server.ErrConflict
			}
			run, err := s.ReadRunForReplay(ctx, project, runID)
			if err != nil {
				return server.NativeJobCompletionResult{}, err
			}
			return nativeJobCompletionResult(run, expectedJobIDs, completions, attempt.CompletedAt != "" || allExpectedJobsCompleted(expectedJobIDs, completions), false), nil
		}
		if err := validateNativePhaseOutputKeys(jobID, newCompletion.PhaseOutputs, completions); err != nil {
			return server.NativeJobCompletionResult{}, err
		}

		completions[jobID] = newCompletion
		attemptMap["job_completions"] = completions
		attempts[idx] = attemptMap
		raw["attempts"] = attempts
		raw["updated_at"] = newCompletion.CompletedAt
		executionState, executionReason := nativeJobExecutionStateAndReason(newCompletion)
		markJobCompletionInExecutionsRaw(raw, attempt.Phase, jobID, executionState, executionReason, newCompletion.CompletedAt)

		payload, err := json.Marshal(raw)
		if err != nil {
			return server.NativeJobCompletionResult{}, err
		}
		etag := azcore.ETag(resp.ETag)
		if _, err := s.runs.ReplaceItem(ctx, pk, runID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag}); err != nil {
			if isCosmosStatus(err, http.StatusPreconditionFailed) {
				continue
			}
			return server.NativeJobCompletionResult{}, err
		}

		run, err := s.ReadRunForReplay(ctx, project, runID)
		if err != nil {
			return server.NativeJobCompletionResult{}, err
		}
		phaseComplete := allExpectedJobsCompleted(expectedJobIDs, completions)
		return nativeJobCompletionResult(run, expectedJobIDs, completions, phaseComplete, phaseComplete), nil
	}
	return server.NativeJobCompletionResult{}, fmt.Errorf("record native job completion: too many etag conflicts")
}

func validateNativePhaseOutputKeys(jobID string, outputs map[string]string, completions map[string]nativeJobCompletionDoc) error {
	if len(outputs) == 0 {
		return nil
	}
	for key := range outputs {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			return server.ValidationError{Message: fmt.Sprintf("job %q declared an empty phase output key", jobID)}
		}
		for existingJobID, completion := range completions {
			if _, exists := completion.PhaseOutputs[trimmed]; exists {
				return server.ValidationError{Message: fmt.Sprintf("phase output %q already set by job %q", trimmed, existingJobID)}
			}
		}
	}
	return nil
}

func (s *Store) expectedNativeJobIDs(ctx context.Context, project, workflowName, schemaRef, phaseName string) ([]string, error) {
	wf, err := s.workflowForRunExecution(ctx, project, workflowName, schemaRef)
	if err != nil {
		return nil, err
	}
	if wf == nil {
		return nil, server.ValidationError{Message: fmt.Sprintf("workflow %q is not registered", workflowName)}
	}
	for _, phase := range wf.Phases {
		if phase.Name != phaseName {
			continue
		}
		phase = server.CanonicalNativePhase(phase)
		if len(phase.Jobs) == 0 {
			return nil, server.ValidationError{Message: fmt.Sprintf("phase %q has no registered jobs", phaseName)}
		}
		ids := make([]string, 0, len(phase.Jobs))
		seen := map[string]bool{}
		for _, job := range phase.Jobs {
			id := strings.TrimSpace(job.ID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return nil, server.ValidationError{Message: fmt.Sprintf("phase %q has no registered job ids", phaseName)}
		}
		return ids, nil
	}
	return nil, server.ValidationError{Message: fmt.Sprintf("phase %q is not registered on workflow %q", phaseName, workflowName)}
}

func nativeJobCompletionDocFromPayload(jobID string, p server.CompletionPayload, completedAt string) nativeJobCompletionDoc {
	var verification *verificationDoc
	if p.VerificationStatus != "" {
		verification = &verificationDoc{
			Status:  p.VerificationStatus,
			Reasons: sliceOrEmpty(p.VerificationReasons),
			CostUSD: p.CostUSD,
		}
	}
	return nativeJobCompletionDoc{
		JobID:               jobID,
		CompletedAt:         completedAt,
		Conclusion:          p.Conclusion,
		Verification:        verification,
		SummaryMarkdown:     p.SummaryMarkdown,
		ScreenshotsMarkdown: p.ScreenshotsMarkdown,
		CostUSD:             p.CostUSD,
		PhaseOutputs:        stringMapOrEmpty(p.PhaseOutputs),
	}
}

func nativeJobExecutionStateAndReason(completion nativeJobCompletionDoc) (string, string) {
	if completion.Verification != nil {
		switch completion.Verification.Status {
		case "pass":
			return "succeeded", ""
		case "fail":
			return "failed", "verification_failed"
		case "error":
			return "failed", "verification_error"
		}
	}
	switch completion.Conclusion {
	case "success":
		return "succeeded", ""
	case "skipped":
		return "skipped", ""
	case "timed_out":
		return "failed", "timeout"
	case "cancelled":
		return "failed", "cancelled"
	default:
		return "failed", "job_failed"
	}
}

func cloneJobCompletions(values map[string]nativeJobCompletionDoc) map[string]nativeJobCompletionDoc {
	out := make(map[string]nativeJobCompletionDoc, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}

func (s *Store) mutateRunRaw(ctx context.Context, project, runID string, mutate func(runDoc, map[string]any) (bool, error)) error {
	pk := azcosmos.NewPartitionKeyString(project)
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
		if isCosmosStatus(err, http.StatusNotFound) {
			return server.ErrNotFound
		}
		if err != nil {
			return err
		}
		var doc runDoc
		if err := json.Unmarshal(resp.Value, &doc); err != nil {
			return err
		}
		var raw map[string]any
		if err := json.Unmarshal(resp.Value, &raw); err != nil {
			return err
		}
		changed, err := mutate(doc, raw)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		payload, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		etag := azcore.ETag(resp.ETag)
		if _, err := s.runs.ReplaceItem(ctx, pk, runID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag}); err != nil {
			if isCosmosStatus(err, http.StatusPreconditionFailed) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("mutate run: too many etag conflicts")
}

func (s *Store) RecordNativeJobsDispatched(ctx context.Context, project, runID, phase string, jobs map[string]string) error {
	if len(jobs) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return s.mutateRunRaw(ctx, project, runID, func(_ runDoc, raw map[string]any) (bool, error) {
		markPhaseJobsDispatchedRaw(raw, phase, jobs)
		raw["updated_at"] = now
		return true, nil
	})
}

func markPhaseDispatchingRaw(raw map[string]any, phaseName, phaseKind, now string) {
	phases, ok := raw["phase_executions"].([]any)
	if !ok {
		phases = []any{}
	}
	found := false
	for i, value := range phases {
		phase, ok := value.(map[string]any)
		if !ok || stringValue(phase["name"]) != phaseName {
			continue
		}
		found = true
		phase["state"] = "dispatching"
		phase["dispatched_at"] = now
		delete(phase, "reason")
		jobs, _ := phase["jobs"].([]any)
		for j, jobValue := range jobs {
			job, ok := jobValue.(map[string]any)
			if !ok {
				continue
			}
			if state := stringValue(job["state"]); state == "" || state == "not_started" {
				job["state"] = "dispatching"
				job["dispatched_at"] = now
				delete(job, "reason")
			}
			jobs[j] = job
		}
		phase["jobs"] = jobs
		phases[i] = phase
		break
	}
	if !found {
		phases = append(phases, map[string]any{
			"name":          phaseName,
			"kind":          firstNonEmpty(phaseKind, "k8s_job"),
			"state":         "dispatching",
			"created_at":    now,
			"dispatched_at": now,
			"jobs": []any{map[string]any{
				"id":            phaseName,
				"name":          phaseName,
				"state":         "dispatching",
				"created_at":    now,
				"dispatched_at": now,
				"steps": []any{map[string]any{
					"slug":       "workflow-run",
					"title":      "Workflow run",
					"state":      "not_started",
					"created_at": now,
				}},
			}},
		})
	}
	raw["phase_executions"] = phases
}

func markPhaseJobsDispatchedRaw(raw map[string]any, phaseName string, jobs map[string]string) {
	if len(jobs) == 0 {
		return
	}
	phases, ok := raw["phase_executions"].([]any)
	if !ok {
		return
	}
	for i, value := range phases {
		phase, ok := value.(map[string]any)
		if !ok || stringValue(phase["name"]) != phaseName {
			continue
		}
		jobValues, _ := phase["jobs"].([]any)
		for j, jobValue := range jobValues {
			job, ok := jobValue.(map[string]any)
			if !ok {
				continue
			}
			if name := jobs[stringValue(job["id"])]; name != "" {
				job["k8s_job_name"] = name
			}
			jobValues[j] = job
		}
		phase["jobs"] = jobValues
		phases[i] = phase
		break
	}
	raw["phase_executions"] = phases
}

func applyNativeEventToExecutionsRaw(raw map[string]any, attempt attemptDoc, event nativeEventDoc) {
	phases, ok := raw["phase_executions"].([]any)
	if !ok {
		return
	}
	now := event.CreatedAt
	for i, value := range phases {
		phase, ok := value.(map[string]any)
		if !ok || stringValue(phase["name"]) != attempt.Phase {
			continue
		}
		if state := stringValue(phase["state"]); state == "dispatching" || state == "not_started" {
			phase["state"] = "active"
			phase["started_at"] = now
			delete(phase, "reason")
		}
		jobs, _ := phase["jobs"].([]any)
		for j, jobValue := range jobs {
			job, ok := jobValue.(map[string]any)
			if !ok || stringValue(job["id"]) != event.JobID {
				continue
			}
			if state := stringValue(job["state"]); state == "dispatching" || state == "not_started" {
				job["state"] = "active"
				job["started_at"] = now
				delete(job, "reason")
			}
			steps, _ := job["steps"].([]any)
			for k, stepValue := range steps {
				step, ok := stepValue.(map[string]any)
				if !ok || event.StepSlug == "" || stringValue(step["slug"]) != event.StepSlug {
					continue
				}
				switch event.Event {
				case "step_started", "log":
					if state := stringValue(step["state"]); state == "not_started" || state == "dispatching" || state == "" {
						step["state"] = "active"
						step["started_at"] = now
						delete(step, "reason")
					}
				case "step_completed":
					step["state"] = "succeeded"
					step["completed_at"] = now
					delete(step, "reason")
					if event.ExitCode != nil {
						step["exit_code"] = *event.ExitCode
					}
				case "step_skipped":
					step["state"] = "skipped"
					step["completed_at"] = now
				case "step_failed":
					step["state"] = "failed"
					step["reason"] = "exit_nonzero"
					step["completed_at"] = now
					if event.ExitCode != nil {
						step["exit_code"] = *event.ExitCode
					}
					job["state"] = "failed"
					job["reason"] = "step_failed"
					job["completed_at"] = now
					phase["state"] = "failed"
					phase["reason"] = "job_failed"
					phase["completed_at"] = now
				}
				steps[k] = step
				break
			}
			job["steps"] = steps
			jobs[j] = job
			break
		}
		phase["jobs"] = jobs
		phases[i] = phase
		break
	}
	raw["phase_executions"] = phases
}

func markJobCompletionInExecutionsRaw(raw map[string]any, phaseName, jobID, state, reason, completedAt string) {
	phases, ok := raw["phase_executions"].([]any)
	if !ok {
		return
	}
	for i, value := range phases {
		phase, ok := value.(map[string]any)
		if !ok || stringValue(phase["name"]) != phaseName {
			continue
		}
		jobs, _ := phase["jobs"].([]any)
		allTerminal := len(jobs) > 0
		anyFailed := false
		for j, jobValue := range jobs {
			job, ok := jobValue.(map[string]any)
			if !ok {
				continue
			}
			if stringValue(job["id"]) == jobID {
				job["state"] = state
				job["completed_at"] = completedAt
				if reason != "" {
					job["reason"] = reason
				} else {
					delete(job, "reason")
				}
				steps, _ := job["steps"].([]any)
				for k, stepValue := range steps {
					step, ok := stepValue.(map[string]any)
					if !ok {
						continue
					}
					if state == "succeeded" && (stringValue(step["state"]) == "active" || stringValue(step["state"]) == "not_started" || stringValue(step["state"]) == "") {
						step["state"] = "succeeded"
						step["completed_at"] = completedAt
					}
					if state == "failed" && (stringValue(step["state"]) == "active" || stringValue(step["state"]) == "dispatching") {
						step["state"] = "failed"
						step["reason"] = firstNonEmpty(reason, "job_failed")
						step["completed_at"] = completedAt
					}
					steps[k] = step
				}
				job["steps"] = steps
			}
			jobState := stringValue(job["state"])
			if jobState != "succeeded" && jobState != "failed" && jobState != "skipped" {
				allTerminal = false
			}
			if jobState == "failed" {
				anyFailed = true
			}
			jobs[j] = job
		}
		phase["jobs"] = jobs
		if allTerminal {
			phase["completed_at"] = completedAt
			if anyFailed {
				phase["state"] = "failed"
				phase["reason"] = "job_failed"
			} else {
				phase["state"] = "succeeded"
				delete(phase, "reason")
			}
		}
		phases[i] = phase
		break
	}
	raw["phase_executions"] = phases
}

func finalizeExecutionFailureRaw(raw map[string]any, failureReason, now string) {
	phases, ok := raw["phase_executions"].([]any)
	if !ok {
		return
	}
	failedSeen := false
	failedAny := false
	for i, value := range phases {
		phase, ok := value.(map[string]any)
		if !ok {
			continue
		}
		phaseState := stringValue(phase["state"])
		if phaseState == "failed" {
			failedSeen = true
			failedAny = true
		} else if phaseState == "dispatching" || phaseState == "active" {
			phase["state"] = "failed"
			phase["reason"] = failureReason
			phase["completed_at"] = now
			failedSeen = true
			failedAny = true
			jobs, _ := phase["jobs"].([]any)
			for j, jobValue := range jobs {
				job, ok := jobValue.(map[string]any)
				if !ok {
					continue
				}
				jobState := stringValue(job["state"])
				if jobState == "dispatching" || jobState == "active" {
					job["state"] = "failed"
					job["reason"] = failureReason
					job["completed_at"] = now
				}
				jobs[j] = job
			}
			phase["jobs"] = jobs
		} else if failedSeen && phaseState == "not_started" {
			phase["state"] = "skipped"
			phase["completed_at"] = now
			jobs, _ := phase["jobs"].([]any)
			for j, jobValue := range jobs {
				job, ok := jobValue.(map[string]any)
				if !ok {
					continue
				}
				if stringValue(job["state"]) == "not_started" {
					job["state"] = "skipped"
					job["completed_at"] = now
					steps, _ := job["steps"].([]any)
					for k, stepValue := range steps {
						step, ok := stepValue.(map[string]any)
						if !ok {
							continue
						}
						if stringValue(step["state"]) == "not_started" {
							step["state"] = "skipped"
							step["completed_at"] = now
						}
						steps[k] = step
					}
					job["steps"] = steps
				}
				jobs[j] = job
			}
			phase["jobs"] = jobs
		}
		phases[i] = phase
	}
	if !failedAny {
		for i, value := range phases {
			phase, ok := value.(map[string]any)
			if !ok || stringValue(phase["state"]) == "skipped" || stringValue(phase["state"]) == "succeeded" {
				continue
			}
			phase["state"] = "failed"
			phase["reason"] = failureReason
			phase["completed_at"] = now
			jobs, _ := phase["jobs"].([]any)
			for j, jobValue := range jobs {
				job, ok := jobValue.(map[string]any)
				if !ok {
					continue
				}
				if stringValue(job["state"]) == "not_started" || stringValue(job["state"]) == "" {
					job["state"] = "failed"
					job["reason"] = failureReason
					job["completed_at"] = now
				}
				jobs[j] = job
			}
			phase["jobs"] = jobs
			phases[i] = phase
			failedAny = true
			failedSeen = true
			break
		}
	}
	if failedSeen {
		skipFollowing := false
		for i, value := range phases {
			phase, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if skipFollowing && stringValue(phase["state"]) == "not_started" {
				phase["state"] = "skipped"
				phase["completed_at"] = now
				jobs, _ := phase["jobs"].([]any)
				for j, jobValue := range jobs {
					job, ok := jobValue.(map[string]any)
					if !ok {
						continue
					}
					if stringValue(job["state"]) == "not_started" {
						job["state"] = "skipped"
						job["completed_at"] = now
						steps, _ := job["steps"].([]any)
						for k, stepValue := range steps {
							step, ok := stepValue.(map[string]any)
							if ok && stringValue(step["state"]) == "not_started" {
								step["state"] = "skipped"
								step["completed_at"] = now
								steps[k] = step
							}
						}
						job["steps"] = steps
					}
					jobs[j] = job
				}
				phase["jobs"] = jobs
				phases[i] = phase
			}
			if stringValue(phase["state"]) == "failed" {
				skipFollowing = true
			}
		}
	}
	raw["phase_executions"] = phases
}

func canonicalExecutionFailureReason(reason string) string {
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch {
	case strings.Contains(reason, "dispatch_timeout"):
		return "dispatch_timeout"
	case strings.Contains(reason, "timeout"), strings.Contains(reason, "timed_out"):
		return "timeout"
	case strings.Contains(reason, "cancel"):
		return "cancelled"
	case strings.Contains(reason, "native_dispatch_failed"):
		return "dispatch_failed"
	case strings.Contains(reason, "admission_failed"):
		return "admission_failed"
	case strings.Contains(reason, "verification"):
		return "verification_failed"
	default:
		return "job_failed"
	}
}

func sameNativeJobCompletion(a, b nativeJobCompletionDoc) bool {
	a.CompletedAt = ""
	b.CompletedAt = ""
	return reflect.DeepEqual(a, b)
}

func nativeJobCompletionResult(run server.RunReplayData, expected []string, completions map[string]nativeJobCompletionDoc, phaseComplete bool, completionReady bool) server.NativeJobCompletionResult {
	completed, pending, failed := nativeJobCompletionLists(expected, completions, phaseComplete)
	return server.NativeJobCompletionResult{
		Run:             run,
		PhaseComplete:   phaseComplete,
		CompletionReady: completionReady,
		CompletedJobIDs: completed,
		PendingJobIDs:   pending,
		FailedJobIDs:    failed,
		PhasePayload:    aggregateNativePhaseCompletion(expected, completions),
	}
}

func nativeJobCompletionLists(expected []string, completions map[string]nativeJobCompletionDoc, phaseComplete bool) ([]string, []string, []string) {
	if len(expected) == 0 {
		expected = sortedJobCompletionIDs(completions)
	}
	completed := make([]string, 0, len(expected))
	pending := make([]string, 0)
	failed := make([]string, 0)
	seen := map[string]bool{}
	for _, id := range expected {
		seen[id] = true
		completion, ok := completions[id]
		if !ok {
			if !phaseComplete {
				pending = append(pending, id)
			}
			continue
		}
		completed = append(completed, id)
		if completion.Conclusion != "" && completion.Conclusion != "success" {
			failed = append(failed, id)
		}
	}
	extras := make([]string, 0)
	for id := range completions {
		if !seen[id] {
			extras = append(extras, id)
		}
	}
	sort.Strings(extras)
	for _, id := range extras {
		completed = append(completed, id)
		if completions[id].Conclusion != "" && completions[id].Conclusion != "success" {
			failed = append(failed, id)
		}
	}
	return completed, pending, failed
}

func sortedJobCompletionIDs(completions map[string]nativeJobCompletionDoc) []string {
	ids := make([]string, 0, len(completions))
	for id := range completions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func allExpectedJobsCompleted(expected []string, completions map[string]nativeJobCompletionDoc) bool {
	if len(expected) == 0 {
		return len(completions) > 0
	}
	for _, id := range expected {
		if _, ok := completions[id]; !ok {
			return false
		}
	}
	return true
}

func aggregateNativePhaseCompletion(expected []string, completions map[string]nativeJobCompletionDoc) server.CompletionPayload {
	ids := expected
	if len(ids) == 0 {
		ids = sortedJobCompletionIDs(completions)
	}
	phaseOutputs := map[string]string{}
	summaries := make([]string, 0)
	screenshots := make([]string, 0)
	reasons := make([]string, 0)
	conclusion := "success"
	verificationStatus := ""
	for _, id := range ids {
		completion, ok := completions[id]
		if !ok {
			continue
		}
		if completion.Conclusion != "" && completion.Conclusion != "success" && conclusion == "success" {
			conclusion = completion.Conclusion
		}
		for key, value := range completion.PhaseOutputs {
			phaseOutputs[key] = value
		}
		if completion.SummaryMarkdown != nil && strings.TrimSpace(*completion.SummaryMarkdown) != "" {
			summaries = append(summaries, nativeJobMarkdownSection(id, *completion.SummaryMarkdown))
		}
		if completion.ScreenshotsMarkdown != nil && strings.TrimSpace(*completion.ScreenshotsMarkdown) != "" {
			screenshots = append(screenshots, nativeJobMarkdownSection(id, *completion.ScreenshotsMarkdown))
		}
		if completion.Verification != nil {
			verificationStatus = combineVerificationStatus(verificationStatus, completion.Verification.Status)
			for _, reason := range completion.Verification.Reasons {
				if strings.TrimSpace(reason) != "" {
					reasons = append(reasons, id+": "+reason)
				}
			}
		}
	}
	payload := server.CompletionPayload{
		Conclusion:          conclusion,
		VerificationStatus:  verificationStatus,
		VerificationReasons: reasons,
		CostUSD:             sumNativeJobCosts(completions),
		PhaseOutputs:        phaseOutputs,
	}
	if len(summaries) > 0 {
		joined := strings.Join(summaries, "\n\n")
		payload.SummaryMarkdown = &joined
	}
	if len(screenshots) > 0 {
		joined := strings.Join(screenshots, "\n\n")
		payload.ScreenshotsMarkdown = &joined
	}
	return payload
}

func nativeJobMarkdownSection(jobID, markdown string) string {
	return "### " + jobID + "\n\n" + strings.TrimSpace(markdown)
}

func combineVerificationStatus(current, next string) string {
	switch next {
	case "error":
		return "error"
	case "fail":
		if current != "error" {
			return "fail"
		}
	case "pass":
		if current == "" {
			return "pass"
		}
	}
	return current
}

func sumNativeJobCosts(completions map[string]nativeJobCompletionDoc) float64 {
	var total float64
	for _, completion := range completions {
		total += completion.CostUSD
	}
	return total
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// StampRunCompletion records completion data on the latest (or specified) attempt and
// increments the run's cumulative_cost_usd. Returns the updated run data.
func (s *Store) StampRunCompletion(ctx context.Context, project, runID string, p server.CompletionPayload) (server.RunReplayData, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
		if isCosmosStatus(err, http.StatusNotFound) {
			return server.RunReplayData{}, server.ErrNotFound
		}
		if err != nil {
			return server.RunReplayData{}, err
		}
		var raw map[string]any
		if err := json.Unmarshal(resp.Value, &raw); err != nil {
			return server.RunReplayData{}, err
		}
		attempts, _ := raw["attempts"].([]any)
		if len(attempts) == 0 {
			return server.RunReplayData{}, fmt.Errorf("run has no attempts")
		}

		idx := len(attempts) - 1
		if p.AttemptIndex != nil && *p.AttemptIndex >= 0 && *p.AttemptIndex < len(attempts) {
			idx = *p.AttemptIndex
		}
		attempt, ok := attempts[idx].(map[string]any)
		if !ok {
			return server.RunReplayData{}, fmt.Errorf("invalid attempt at index %d", idx)
		}

		now := time.Now().UTC().Format(time.RFC3339Nano)
		attempt["completed_at"] = now
		attempt["conclusion"] = p.Conclusion
		if p.SummaryMarkdown != nil {
			attempt["summary_markdown"] = *p.SummaryMarkdown
		}
		if p.PhaseOutputs != nil {
			attempt["phase_outputs"] = p.PhaseOutputs
		}
		if p.VerificationStatus != "" {
			attempt["verification"] = map[string]any{
				"status":   p.VerificationStatus,
				"reasons":  p.VerificationReasons,
				"cost_usd": p.CostUSD,
			}
		}
		attempt["cost_usd"] = p.CostUSD
		attempts[idx] = attempt
		raw["attempts"] = attempts

		// Increment cumulative cost.
		prior, _ := raw["cumulative_cost_usd"].(float64)
		raw["cumulative_cost_usd"] = prior + p.CostUSD
		raw["updated_at"] = now
		// First-arrival wins for screenshots_markdown.
		if p.ScreenshotsMarkdown != nil && (raw["screenshots_markdown"] == nil || raw["screenshots_markdown"] == "") {
			raw["screenshots_markdown"] = *p.ScreenshotsMarkdown
		}

		payload, err := json.Marshal(raw)
		if err != nil {
			return server.RunReplayData{}, err
		}
		etag := azcore.ETag(resp.ETag)
		if _, err := s.runs.ReplaceItem(ctx, pk, runID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag}); err != nil {
			if isCosmosStatus(err, http.StatusPreconditionFailed) {
				continue
			}
			return server.RunReplayData{}, err
		}
		// Re-read to return the updated RunReplayData.
		return s.ReadRunForReplay(ctx, project, runID)
	}
	return server.RunReplayData{}, fmt.Errorf("stamp run completion: too many etag conflicts")
}

// StampRunDecision stamps the decision string on the latest attempt of a run.
func (s *Store) StampRunDecision(ctx context.Context, project, runID, decision string) error {
	pk := azcosmos.NewPartitionKeyString(project)
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
		if isCosmosStatus(err, http.StatusNotFound) {
			return server.ErrNotFound
		}
		if err != nil {
			return err
		}
		var raw map[string]any
		if err := json.Unmarshal(resp.Value, &raw); err != nil {
			return err
		}
		attempts, _ := raw["attempts"].([]any)
		if len(attempts) == 0 {
			return nil
		}
		attempt, ok := attempts[len(attempts)-1].(map[string]any)
		if !ok {
			return nil
		}
		attempt["decision"] = decision
		attempts[len(attempts)-1] = attempt
		raw["attempts"] = attempts
		raw["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)

		payload, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		etag := azcore.ETag(resp.ETag)
		if _, err := s.runs.ReplaceItem(ctx, pk, runID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag}); err != nil {
			if isCosmosStatus(err, http.StatusPreconditionFailed) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("stamp run decision: too many etag conflicts")
}

// SetRunTerminalState sets the run's state (passed, review_required, or aborted) and
// best-effort releases issue/PR locks. Mirrors AbortRunByID but for non-abort terminal states.
func (s *Store) SetRunTerminalState(ctx context.Context, project, runID, state string, abortReason *string) (server.AbortRunResult, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return server.AbortRunResult{}, server.ErrNotFound
	}
	if err != nil {
		return server.AbortRunResult{}, err
	}
	var doc runDoc
	if err := json.Unmarshal(resp.Value, &doc); err != nil {
		return server.AbortRunResult{}, err
	}

	siblings, _ := s.issueRunDocs(ctx, project, doc.IssueNumber)
	numbers := runNumberMap(siblings)
	runRef := publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), runDisplayNumber(doc, numbers[doc.ID]))

	var raw map[string]any
	if err := json.Unmarshal(resp.Value, &raw); err != nil {
		return server.AbortRunResult{}, err
	}
	raw["state"] = state
	delete(raw, "queue_state")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	raw["updated_at"] = now
	if abortReason != nil {
		raw["abort_reason"] = *abortReason
	}
	if state == "aborted" {
		finalizeExecutionFailureRaw(raw, canonicalExecutionFailureReason(stringOrEmpty(abortReason)), now)
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return server.AbortRunResult{}, err
	}
	etag := azcore.ETag(resp.ETag)
	if _, err := s.runs.ReplaceItem(ctx, pk, runID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag}); err != nil {
		return server.AbortRunResult{}, err
	}

	var issueLockReleased, prLockReleased *bool
	if doc.IssueLockHolderID != nil && *doc.IssueLockHolderID != "" && doc.IssueNumber > 0 {
		released := s.pgLocks.ReleaseLock(ctx, "issue", fmt.Sprintf("%s#%d", project, doc.IssueNumber), *doc.IssueLockHolderID)
		issueLockReleased = &released
	}
	if doc.PRLockHolderID != nil && *doc.PRLockHolderID != "" && doc.PRNumber != nil && doc.IssueRepo != "" {
		released := s.pgLocks.ReleaseLock(ctx, "pr", fmt.Sprintf("%s#%d", doc.IssueRepo, *doc.PRNumber), *doc.PRLockHolderID)
		prLockReleased = &released
	}

	return server.AbortRunResult{
		State:             state,
		RunRef:            runRef,
		RunNumber:         doc.RunNumber,
		RunDisplayNumber:  doc.RunDisplayNumber,
		IssueLockReleased: issueLockReleased,
		PRLockReleased:    prLockReleased,
	}, nil
}

// AppendRunAttempt appends a new PhaseAttempt to an in-progress run before retry dispatch.
// Returns the new attempt's index.
func (s *Store) AppendRunAttempt(ctx context.Context, project, runID, phase, phaseKind, workflowFilename string) (int, error) {
	pk := azcosmos.NewPartitionKeyString(project)
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		resp, err := s.runs.ReadItem(ctx, pk, runID, nil)
		if isCosmosStatus(err, http.StatusNotFound) {
			return 0, server.ErrNotFound
		}
		if err != nil {
			return 0, err
		}
		var raw map[string]any
		if err := json.Unmarshal(resp.Value, &raw); err != nil {
			return 0, err
		}
		attempts, _ := raw["attempts"].([]any)
		nextIdx := len(attempts)
		if len(attempts) > 0 {
			if last, ok := attempts[len(attempts)-1].(map[string]any); ok {
				if ai, ok := last["attempt_index"].(float64); ok {
					nextIdx = int(ai) + 1
				}
			}
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		newAttempt := map[string]any{
			"attempt_index":     nextIdx,
			"phase":             phase,
			"phase_kind":        phaseKind,
			"workflow_filename": workflowFilename,
			"dispatched_at":     now,
		}
		raw["attempts"] = append(attempts, newAttempt)
		markPhaseDispatchingRaw(raw, phase, phaseKind, now)
		raw["updated_at"] = now

		payload, err := json.Marshal(raw)
		if err != nil {
			return 0, err
		}
		etag := azcore.ETag(resp.ETag)
		if _, err := s.runs.ReplaceItem(ctx, pk, runID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag}); err != nil {
			if isCosmosStatus(err, http.StatusPreconditionFailed) {
				continue
			}
			return 0, err
		}
		return nextIdx, nil
	}
	return 0, fmt.Errorf("append run attempt: too many etag conflicts")
}

func (s *Store) StartRunCycle(ctx context.Context, req server.StartRunCycleRequest) (int, error) {
	pk := azcosmos.NewPartitionKeyString(req.Project)
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		resp, err := s.runs.ReadItem(ctx, pk, req.RunID, nil)
		if isCosmosStatus(err, http.StatusNotFound) {
			return 0, server.ErrNotFound
		}
		if err != nil {
			return 0, err
		}
		var raw map[string]any
		if err := json.Unmarshal(resp.Value, &raw); err != nil {
			return 0, err
		}
		if stringValue(raw["state"]) != "queued" {
			return 0, server.ErrConflict
		}
		attempts, _ := raw["attempts"].([]any)
		for _, rawAttempt := range attempts {
			attemptMap, ok := rawAttempt.(map[string]any)
			if !ok || !boolValue(attemptMap["carry_forward"]) || stringValue(attemptMap["completed_at"]) == "" {
				return 0, server.ErrConflict
			}
		}
		nextIdx := len(attempts)
		if len(attempts) > 0 {
			if last, ok := attempts[len(attempts)-1].(map[string]any); ok {
				if ai, ok := last["attempt_index"].(float64); ok {
					nextIdx = int(ai) + 1
				}
			}
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		newAttempt := map[string]any{
			"attempt_index":     nextIdx,
			"phase":             req.PhaseName,
			"phase_kind":        req.PhaseKind,
			"workflow_filename": req.WorkflowFilename,
			"dispatched_at":     now,
		}
		raw["attempts"] = append(attempts, newAttempt)
		raw["state"] = "in_progress"
		raw["queue_state"] = "admitted"
		raw["slot_lease_ref"] = req.SlotLeaseRef
		delete(raw, "admission_error")
		markPhaseDispatchingRaw(raw, req.PhaseName, req.PhaseKind, now)
		raw["updated_at"] = now

		payload, err := json.Marshal(raw)
		if err != nil {
			return 0, err
		}
		etag := azcore.ETag(resp.ETag)
		if _, err := s.runs.ReplaceItem(ctx, pk, req.RunID, payload, &azcosmos.ItemOptions{IfMatchEtag: &etag}); err != nil {
			if isCosmosStatus(err, http.StatusPreconditionFailed) {
				continue
			}
			return 0, err
		}
		return nextIdx, nil
	}
	return 0, fmt.Errorf("start run cycle: too many etag conflicts")
}

// ---------------------------------------------------------------------------
// RunDispatchStore implementation
// ---------------------------------------------------------------------------

// ReadProjectGitHubRepo returns the githubRepo field for a registered project.
func (s *Store) ReadProjectGitHubRepo(ctx context.Context, project string) (string, error) {
	doc, err := s.readProjectDoc(ctx, project)
	if errors.Is(err, server.ErrNotFound) {
		return "", server.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return doc.GitHubRepo, nil
}

// ReadIssueForDispatch returns the minimal issue data needed to build dispatch metadata.
func (s *Store) ReadIssueForDispatch(ctx context.Context, project string, issueNumber int) (server.IssueDispatchData, error) {
	doc, err := s.readIssueByNumber(ctx, project, issueNumber)
	if errors.Is(err, server.ErrNotFound) {
		return server.IssueDispatchData{}, server.ErrNotFound
	}
	if err != nil {
		return server.IssueDispatchData{}, err
	}
	labels := doc.Labels
	if labels == nil {
		labels = []string{}
	}
	return server.IssueDispatchData{
		ID:     doc.ID,
		Title:  doc.Title,
		Body:   doc.Body,
		Labels: labels,
	}, nil
}

// ListProjectWorkflows returns all workflows registered for a project.
func (s *Store) ListProjectWorkflows(ctx context.Context, project string) ([]server.Workflow, error) {
	var docs []workflowDoc
	if err := singlePartitionQuery(
		ctx,
		s.workflows,
		azcosmos.NewPartitionKeyString(project),
		"SELECT * FROM c WHERE c.project = @p",
		[]azcosmos.QueryParameter{{Name: "@p", Value: project}},
		&docs,
	); err != nil {
		return nil, err
	}
	rows := make([]server.Workflow, 0, len(docs))
	for _, doc := range docs {
		if isWorkflowSchemaDoc(doc) {
			continue
		}
		rows = append(rows, workflowFromDoc(doc))
	}
	return rows, nil
}

// CreateRun creates a queued first cycle for a new issue run. The caller must
// hold the issue lock before calling this so cycle/run-number allocation is serialized.
func (s *Store) CreateRun(ctx context.Context, req server.CreateRunRequest) (server.CreatedRun, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Allocate the flat cycle ledger number and the logical run number under
	// the issue lock.
	docs, err := s.issueRunDocs(ctx, req.Project, req.IssueNumber)
	if err != nil {
		return server.CreatedRun{}, fmt.Errorf("query issue runs: %w", err)
	}
	numbers := runNumberMap(docs)
	cycleNumber := 1
	for _, n := range numbers {
		if n >= cycleNumber {
			cycleNumber = n + 1
		}
	}
	runNumber := 1
	for _, doc := range docs {
		if doc.RunNumber != nil && *doc.RunNumber >= runNumber {
			runNumber = *doc.RunNumber + 1
		}
	}
	runCycle := 1

	runID := uuid.New().String()
	callbackToken := uuid.New().String()[:32]
	runDisplay := fmt.Sprintf("%d.%d", runNumber, runCycle)
	budgetDoc := &budgetDoc{Total: req.Budget.Total}
	queueState := "queued"
	wf, err := s.workflowForRunExecution(ctx, req.Project, req.Workflow, req.WorkflowSchemaRef)
	if err != nil {
		return server.CreatedRun{}, err
	}

	originKind := "dispatch"
	if req.TriggerSource != nil {
		if k, ok := req.TriggerSource["kind"].(string); ok && k != "" {
			originKind = k
		}
	}

	doc := runDoc{
		ID:                runID,
		Project:           req.Project,
		Workflow:          req.Workflow,
		WorkflowSchemaRef: req.WorkflowSchemaRef,
		RunNumber:         &runNumber,
		CycleNumber:       &cycleNumber,
		RunCycleNumber:    &runCycle,
		RunDisplayNumber:  &runDisplay,
		RootRunID:         &runID,
		OriginKind:        &originKind,
		IsCycle:           true,
		IssueID:           req.IssueID,
		IssueRepo:         req.IssueRepo,
		IssueNumber:       req.IssueNumber,
		State:             "queued",
		QueueState:        &queueState,
		SlotLeaseRef:      optionalNonEmptyStringPtr(req.SlotLeaseRef),
		PhaseExecutions:   phaseExecutionDocsFromWorkflow(*wf, now, nil),
		Budget:            budgetDoc,
		Attempts:          []attemptDoc{},
		CumulativeCostUSD: 0.0,
		TriggerSource:     req.TriggerSource,
		CallbackToken:     &callbackToken,
		IssueLockHolderID: &req.IssueLockHolderID,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	payload, err := json.Marshal(doc)
	if err != nil {
		return server.CreatedRun{}, err
	}
	pk := azcosmos.NewPartitionKeyString(req.Project)
	if _, err := s.runs.CreateItem(ctx, pk, payload, nil); err != nil {
		return server.CreatedRun{}, fmt.Errorf("create run doc: %w", err)
	}
	return server.CreatedRun{
		ID:                   runID,
		RunNumber:            runNumber,
		CycleNumber:          cycleNumber,
		RunCycle:             runCycle,
		RunDisplay:           runDisplay,
		CallbackToken:        callbackToken,
		CarryForwardAttempts: carryForwardAttemptsFromDocs(doc.Attempts),
	}, nil
}

func carryForwardAttemptDocs(attempts []server.RunAttemptData, wf server.Workflow, now string) []attemptDoc {
	if len(attempts) == 0 {
		return []attemptDoc{}
	}
	phaseByName := map[string]server.PhaseSpec{}
	for _, phase := range wf.Phases {
		phaseByName[phase.Name] = phase
	}
	out := make([]attemptDoc, 0, len(attempts))
	for _, attempt := range attempts {
		phase, ok := phaseByName[attempt.Phase]
		if !ok {
			continue
		}
		kind := firstNonEmpty(phase.Kind, "k8s_job")
		workflowFilename := firstNonEmpty(phase.WorkflowFilename, fmt.Sprintf("%s:%s", kind, phase.Name))
		conclusion := firstNonEmpty(attempt.Conclusion, "success")
		decision := firstNonEmpty(attempt.Decision, "advance")
		out = append(out, attemptDoc{
			AttemptIndex:     len(out),
			Phase:            phase.Name,
			PhaseKind:        kind,
			WorkflowFilename: workflowFilename,
			DispatchedAt:     now,
			CompletedAt:      now,
			Conclusion:       &conclusion,
			Decision:         &decision,
			PhaseOutputs:     stringMapOrEmpty(attempt.PhaseOutputs),
			CarryForward:     true,
		})
	}
	return out
}

func carryForwardAttemptsFromDocs(docs []attemptDoc) []server.RunAttemptData {
	out := make([]server.RunAttemptData, 0, len(docs))
	for _, doc := range docs {
		if !doc.CarryForward {
			continue
		}
		out = append(out, server.RunAttemptData{
			AttemptIndex: doc.AttemptIndex,
			Phase:        doc.Phase,
			Conclusion:   stringOrEmpty(doc.Conclusion),
			Decision:     stringOrEmpty(doc.Decision),
			Completed:    doc.CompletedAt != "",
			CarryForward: true,
			PhaseOutputs: stringMapOrEmpty(doc.PhaseOutputs),
		})
	}
	return out
}

func (s *Store) CreateRecycleCycle(ctx context.Context, req server.CreateRecycleCycleRequest) (server.CreatedRun, error) {
	pk := azcosmos.NewPartitionKeyString(req.Parent.Project)
	parentResp, err := s.runs.ReadItem(ctx, pk, req.Parent.ID, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return server.CreatedRun{}, server.ErrNotFound
	}
	if err != nil {
		return server.CreatedRun{}, err
	}
	var parent runDoc
	if err := json.Unmarshal(parentResp.Value, &parent); err != nil {
		return server.CreatedRun{}, err
	}
	if parent.SlotLeaseRef == nil || *parent.SlotLeaseRef == "" {
		return server.CreatedRun{}, fmt.Errorf("parent cycle has no slot lease to recycle")
	}

	docs, err := s.issueRunDocs(ctx, parent.Project, parent.IssueNumber)
	if err != nil {
		return server.CreatedRun{}, fmt.Errorf("query issue runs: %w", err)
	}
	numbers := runNumberMap(docs)
	cycleNumber := 1
	for _, n := range numbers {
		if n >= cycleNumber {
			cycleNumber = n + 1
		}
	}
	runNumber := runLedgerNumber(parent)
	if parent.RunNumber != nil && *parent.RunNumber > 0 {
		runNumber = *parent.RunNumber
	}
	if runNumber <= 0 {
		runNumber = cycleNumber
	}
	runCycle := 1
	if parent.RunCycleNumber != nil && *parent.RunCycleNumber > 0 {
		runCycle = *parent.RunCycleNumber + 1
	} else if parent.CycleNumber != nil && parent.IsCycle && *parent.CycleNumber > 0 {
		runCycle = *parent.CycleNumber + 1
	} else if parent.RunDisplayNumber != nil {
		if parts := strings.Split(*parent.RunDisplayNumber, "."); len(parts) == 2 {
			if parsed, err := strconv.Atoi(parts[1]); err == nil && parsed > 0 {
				runCycle = parsed + 1
			}
		}
	}
	runDisplay := fmt.Sprintf("%d.%d", runNumber, runCycle)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	recycledState := "recycled"
	parent.State = recycledState
	parent.QueueState = nil
	parent.UpdatedAt = now
	parentPayload, err := json.Marshal(parent)
	if err != nil {
		return server.CreatedRun{}, err
	}
	parentETag := azcore.ETag(parentResp.ETag)
	if _, err := s.runs.ReplaceItem(ctx, pk, parent.ID, parentPayload, &azcosmos.ItemOptions{IfMatchEtag: &parentETag}); err != nil {
		return server.CreatedRun{}, err
	}

	runID := uuid.New().String()
	callbackToken := uuid.New().String()[:32]
	originKind := "recycle_policy"
	if req.TriggerSource != nil {
		if k, ok := req.TriggerSource["kind"].(string); ok && k != "" {
			originKind = k
		}
	}
	rootRunID := parent.ID
	if parent.RootRunID != nil && *parent.RootRunID != "" {
		rootRunID = *parent.RootRunID
	}
	queueState := "queued"
	wf, err := s.workflowForRunExecution(ctx, parent.Project, parent.Workflow, firstNonEmpty(req.WorkflowSchemaRef, parent.WorkflowSchemaRef))
	if err != nil {
		return server.CreatedRun{}, err
	}
	doc := runDoc{
		ID:                runID,
		Project:           parent.Project,
		Workflow:          parent.Workflow,
		WorkflowSchemaRef: firstNonEmpty(req.WorkflowSchemaRef, parent.WorkflowSchemaRef),
		RunNumber:         &runNumber,
		CycleNumber:       &cycleNumber,
		RunCycleNumber:    &runCycle,
		RunDisplayNumber:  &runDisplay,
		ParentRunID:       &parent.ID,
		RootRunID:         &rootRunID,
		OriginKind:        &originKind,
		IsCycle:           true,
		IssueID:           parent.IssueID,
		IssueRepo:         parent.IssueRepo,
		IssueNumber:       parent.IssueNumber,
		PRNumber:          parent.PRNumber,
		State:             "queued",
		QueueState:        &queueState,
		SlotLeaseRef:      parent.SlotLeaseRef,
		EntrypointPhase:   optionalNonEmptyStringPtr(req.TargetPhaseName),
		PhaseExecutions:   phaseExecutionDocsFromWorkflow(*wf, now, optionalNonEmptyStringPtr(req.TargetPhaseName)),
		Budget:            parent.Budget,
		Attempts:          carryForwardAttemptDocs(req.CarryForwardAttempts, *wf, now),
		CumulativeCostUSD: parent.CumulativeCostUSD,
		TriggerSource:     req.TriggerSource,
		CallbackToken:     &callbackToken,
		IssueLockHolderID: parent.IssueLockHolderID,
		PRLockHolderID:    parent.PRLockHolderID,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.CreatedRun{}, err
	}
	if _, err := s.runs.CreateItem(ctx, pk, payload, nil); err != nil {
		return server.CreatedRun{}, fmt.Errorf("create recycle cycle doc: %w", err)
	}
	return server.CreatedRun{
		ID:                   runID,
		RunNumber:            runNumber,
		CycleNumber:          cycleNumber,
		RunCycle:             runCycle,
		RunDisplay:           runDisplay,
		CallbackToken:        callbackToken,
		CarryForwardAttempts: carryForwardAttemptsFromDocs(doc.Attempts),
	}, nil
}

// ReadRunByNumber resolves a run by (project, issueNumber, runNumber display string)
// and returns the run ID. Returns server.ErrNotFound if no match.
func (s *Store) ReadRunByNumber(ctx context.Context, project string, issueNumber int, runNumber string) (string, error) {
	docs, err := s.issueRunDocs(ctx, project, issueNumber)
	if err != nil {
		return "", err
	}
	numbers := runNumberMap(docs)
	runNumber = strings.TrimSpace(runNumber)
	for _, doc := range docs {
		display := ""
		if doc.RunDisplayNumber != nil {
			display = strings.TrimSpace(*doc.RunDisplayNumber)
		}
		if display != "" && display == runNumber {
			return doc.ID, nil
		}
		if fmt.Sprintf("%d", numbers[doc.ID]) == runNumber {
			return doc.ID, nil
		}
	}
	return "", server.ErrNotFound
}

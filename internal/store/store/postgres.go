package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/publicids"
	"github.com/nelsong6/glimmung/internal/server"
	pgstore "github.com/nelsong6/glimmung/internal/store/pg"
)

// Store is the Postgres-backed data-access wrapper used by server-side
// handlers. Methods delegate to the per-cluster pg.*Store fields below while
// preserving the server-facing domain method set.
type Store struct {
	nativeProjectConcurrency int

	// pgLocks owns durable lock state.
	pgLocks *pgstore.LocksStore

	// pgRunEvents owns native-runner event storage.
	pgRunEvents *pgstore.RunEventsStore

	// pgProjects owns project rows and test-lease defaults.
	pgProjects *pgstore.ProjectsStore

	// pgWorkflows owns workflow registrations and workflow schemas.
	pgWorkflows *pgstore.WorkflowsStore

	// pgPortfolios owns design portfolio elements.
	pgPortfolios *pgstore.PortfoliosStore

	// pgPlaybooks owns operator-authored playbooks.
	pgPlaybooks *pgstore.PlaybooksStore

	// pgSignals owns the webhook signal queue.
	pgSignals *pgstore.SignalsStore

	// pgIssues owns issues and issue comments.
	pgIssues *pgstore.IssuesStore

	// pgSlots owns test-slot state and slot history.
	pgSlots *pgstore.SlotsStore

	// pgRuns owns durable run records.
	pgRuns *pgstore.RunsStore

	// pgTouchpoints owns operator-visible review touchpoints.
	pgTouchpoints *pgstore.TouchpointsStore

	// pgLeases owns native lease rows and lease counters.
	pgLeases *pgstore.LeasesStore

	// pgSlotInspections owns the durable ledger for free
	// (lease-scoped) inspections uploaded through POST /v1/inspections.
	pgSlotInspections *pgstore.SlotInspectionsStore
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

// SetPGProjects injects the Postgres-backed projects store. All
// project read/write methods on Store route through this field
// once set. Called by main.go at startup after pg.ProjectsStore is
// constructed.
func (s *Store) SetPGProjects(projects *pgstore.ProjectsStore) {
	s.pgProjects = projects
}

// SetPGWorkflows injects the Postgres-backed workflows store. All
// workflow read/write methods on Store route through this
// field once set.
func (s *Store) SetPGWorkflows(workflows *pgstore.WorkflowsStore) {
	s.pgWorkflows = workflows
}

// SetPGPortfolios injects the Postgres-backed portfolios store.
func (s *Store) SetPGPortfolios(portfolios *pgstore.PortfoliosStore) {
	s.pgPortfolios = portfolios
}

// SetPGPlaybooks injects the Postgres-backed playbooks store.
func (s *Store) SetPGPlaybooks(playbooks *pgstore.PlaybooksStore) {
	s.pgPlaybooks = playbooks
}

// SetPGSignals injects the Postgres-backed signals store.
func (s *Store) SetPGSignals(signals *pgstore.SignalsStore) {
	s.pgSignals = signals
}

// SetPGIssues injects the Postgres-backed issues store.
func (s *Store) SetPGIssues(issues *pgstore.IssuesStore) {
	s.pgIssues = issues
}

// SetPGSlots injects the Postgres-backed slots store.
func (s *Store) SetPGSlots(slots *pgstore.SlotsStore) {
	s.pgSlots = slots
}

// SetPGRuns injects the Postgres-backed runs store.
func (s *Store) SetPGRuns(runs *pgstore.RunsStore) {
	s.pgRuns = runs
}

// SetPGTouchpoints injects the Postgres-backed touchpoints store.
func (s *Store) SetPGTouchpoints(touchpoints *pgstore.TouchpointsStore) {
	s.pgTouchpoints = touchpoints
}

// SetPGLeases injects the Postgres-backed leases store.
func (s *Store) SetPGLeases(leases *pgstore.LeasesStore) {
	s.pgLeases = leases
}

// SetPGSlotInspections injects the Postgres-backed slot_inspections store.
func (s *Store) SetPGSlotInspections(slotInspections *pgstore.SlotInspectionsStore) {
	s.pgSlotInspections = slotInspections
}

// InsertSlotInspection records a free (lease-scoped) inspection.
func (s *Store) InsertSlotInspection(ctx context.Context, row server.SlotInspectionRecord) (server.SlotInspectionRecord, error) {
	if s == nil || s.pgSlotInspections == nil {
		return server.SlotInspectionRecord{}, errors.New("slot_inspections store not configured")
	}
	written, err := s.pgSlotInspections.Insert(ctx, pgstore.SlotInspectionRow{
		ID:                    row.ID,
		Project:               row.Project,
		Slot:                  row.Slot,
		LeaseID:               row.LeaseID,
		SessionID:             row.SessionID,
		RequestID:             row.RequestID,
		Scope:                 row.Scope,
		RunID:                 row.RunID,
		BlobPrefix:            row.BlobPrefix,
		ReportBlobPath:        row.ReportBlobPath,
		ScreenshotBlobPath:    row.ScreenshotBlobPath,
		ScreenshotContentType: row.ScreenshotContentType,
		ByteSizeScreenshot:    row.ByteSizeScreenshot,
		ByteSizeReport:        row.ByteSizeReport,
	})
	if err != nil {
		return server.SlotInspectionRecord{}, err
	}
	return slotInspectionRecordFromPGRow(written), nil
}

// LookupSlotInspectionByRequest returns a prior inspection if the same
// (lease_id, request_id) pair has already been recorded. Server-side
// idempotency for POST /v1/inspections.
func (s *Store) LookupSlotInspectionByRequest(ctx context.Context, leaseID, requestID string) (server.SlotInspectionRecord, error) {
	if s == nil || s.pgSlotInspections == nil {
		return server.SlotInspectionRecord{}, server.ErrSlotInspectionNotFound
	}
	row, err := s.pgSlotInspections.LookupByRequest(ctx, leaseID, requestID)
	if err != nil {
		if errors.Is(err, pgstore.ErrSlotInspectionNotFound) {
			return server.SlotInspectionRecord{}, server.ErrSlotInspectionNotFound
		}
		return server.SlotInspectionRecord{}, err
	}
	return slotInspectionRecordFromPGRow(row), nil
}

// DeleteSlotInspectionsByLease drops every ledger row for the lease and
// returns them so the sweep can delete the underlying blobs. Idempotent.
func (s *Store) DeleteSlotInspectionsByLease(ctx context.Context, leaseID string) ([]server.SlotInspectionRecord, error) {
	if s == nil || s.pgSlotInspections == nil {
		return nil, nil
	}
	rows, err := s.pgSlotInspections.DeleteByLease(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	out := make([]server.SlotInspectionRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, slotInspectionRecordFromPGRow(row))
	}
	return out, nil
}

// GetSlotInspectionByID returns one row by its public id, or
// ErrSlotInspectionNotFound when nothing matches.
func (s *Store) GetSlotInspectionByID(ctx context.Context, id string) (server.SlotInspectionRecord, error) {
	if s == nil || s.pgSlotInspections == nil {
		return server.SlotInspectionRecord{}, server.ErrSlotInspectionNotFound
	}
	row, err := s.pgSlotInspections.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgstore.ErrSlotInspectionNotFound) {
			return server.SlotInspectionRecord{}, server.ErrSlotInspectionNotFound
		}
		return server.SlotInspectionRecord{}, err
	}
	return slotInspectionRecordFromPGRow(row), nil
}

// ListSlotInspections returns rows newest-first, narrowed by the supplied
// filter.
func (s *Store) ListSlotInspections(ctx context.Context, filter server.SlotInspectionFilter) ([]server.SlotInspectionRecord, error) {
	if s == nil || s.pgSlotInspections == nil {
		return nil, nil
	}
	rows, err := s.pgSlotInspections.List(ctx, filter.Project, filter.LeaseID, filter.RunID, filter.Scope, filter.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]server.SlotInspectionRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, slotInspectionRecordFromPGRow(row))
	}
	return out, nil
}

func slotInspectionRecordFromPGRow(row pgstore.SlotInspectionRow) server.SlotInspectionRecord {
	scope := row.Scope
	if scope == "" {
		scope = "lease"
	}
	return server.SlotInspectionRecord{
		ID:                    row.ID,
		Project:               row.Project,
		Slot:                  row.Slot,
		LeaseID:               row.LeaseID,
		SessionID:             row.SessionID,
		RequestID:             row.RequestID,
		Scope:                 scope,
		RunID:                 row.RunID,
		BlobPrefix:            row.BlobPrefix,
		ReportBlobPath:        row.ReportBlobPath,
		ScreenshotBlobPath:    row.ScreenshotBlobPath,
		ScreenshotContentType: row.ScreenshotContentType,
		ByteSizeScreenshot:    row.ByteSizeScreenshot,
		ByteSizeReport:        row.ByteSizeReport,
		CreatedAt:             row.CreatedAt,
	}
}

// leaseDocFromPayload decodes a lease payload into the internal helper shape.
func leaseDocFromPayload(payload []byte) (leaseDoc, error) {
	var doc leaseDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		return leaseDoc{}, err
	}
	return doc, nil
}

// touchpointDocFromPayload decodes a touchpoint payload into the internal
// helper shape.
func touchpointDocFromPayload(payload []byte) (touchpointDoc, error) {
	var doc touchpointDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		return touchpointDoc{}, err
	}
	return doc, nil
}

// runDocFromPGRow unmarshals a pg runs row payload into the internal run shape.
func runDocFromPGRow(row pgstore.RunRow) (runDoc, error) {
	var doc runDoc
	if err := json.Unmarshal(row.Payload, &doc); err != nil {
		return runDoc{}, fmt.Errorf("run unmarshal: %w", err)
	}
	return doc, nil
}

// readRunDoc fetches a run by (project, runID) from pg and returns
// the unmarshaled runDoc plus its raw payload bytes (the caller may
// need to unmarshal into map[string]any for raw patches).
func (s *Store) readRunDoc(ctx context.Context, project, runID string) (runDoc, []byte, error) {
	row, err := s.pgRuns.Get(ctx, project, runID)
	if errors.Is(err, pgstore.ErrRunNotFound) {
		return runDoc{}, nil, server.ErrNotFound
	}
	if err != nil {
		return runDoc{}, nil, err
	}
	doc, err := runDocFromPGRow(row)
	if err != nil {
		return runDoc{}, nil, err
	}
	return doc, row.Payload, nil
}

// issueDocFromPGRow assembles the internal issue helper from a pg issues row
// plus the per-issue comment rows.
func (s *Store) issueDocFromPGRow(ctx context.Context, row pgstore.IssueRow) (issueDoc, error) {
	doc, err := issueDocFromPGPayload(row)
	if err != nil {
		return issueDoc{}, err
	}
	// archived_at column drives the partial index but doc.ClosedAt is
	// still the canonical source; both are kept in sync by PatchPayload.
	comments, err := s.pgIssues.ListComments(ctx, row.Project, row.Number)
	if err != nil {
		return issueDoc{}, err
	}
	doc.Comments = make([]issueCommentDoc, 0, len(comments))
	for _, c := range comments {
		var cd issueCommentDoc
		if err := json.Unmarshal(c.Payload, &cd); err != nil {
			return issueDoc{}, err
		}
		doc.Comments = append(doc.Comments, cd)
	}
	return doc, nil
}

func issueDocFromPGPayload(row pgstore.IssueRow) (issueDoc, error) {
	var doc issueDoc
	if err := json.Unmarshal(row.Payload, &doc); err != nil {
		return issueDoc{}, err
	}
	doc.Comments = nil
	return doc, nil
}

// issueDocsFromPGRows builds a slice of issueDocs from pg rows,
// without hydrating comments. List surfaces only need issue row fields;
// detail paths use issueDocFromPGRow when comments are part of the
// response.
func (s *Store) issueDocsFromPGRows(ctx context.Context, rows []pgstore.IssueRow) ([]issueDoc, error) {
	out := make([]issueDoc, 0, len(rows))
	for _, row := range rows {
		doc, err := issueDocFromPGPayload(row)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, nil
}

// signalDocFromPayload deserializes a pg signal row payload into the internal
// signal helper shape.
func signalDocFromPayload(payload []byte) (signalDoc, error) {
	var doc signalDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		return signalDoc{}, err
	}
	return doc, nil
}

// playbookDocFromPayload deserializes a pg playbook row payload into the
// internal playbook helper shape.
func playbookDocFromPayload(payload []byte) (playbookDoc, error) {
	var doc playbookDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		return playbookDoc{}, err
	}
	return doc, nil
}

// portfolioElementDocFromPayload deserializes a pg portfolio row payload into
// the server-facing helper shape.
func portfolioElementDocFromPayload(payload []byte) (portfolioElementDoc, error) {
	var doc portfolioElementDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		return portfolioElementDoc{}, err
	}
	return doc, nil
}

func encodeIssueDocPayload(doc issueDoc) ([]byte, error) {
	return json.Marshal(doc)
}

func encodeIssueCommentPayload(c issueCommentDoc) ([]byte, error) {
	return json.Marshal(c)
}

// workflowFromPayload unmarshals a pg workflow payload and converts it to the
// server-package Workflow type via workflowFromDoc.
func workflowFromPayload(payload []byte) (server.Workflow, error) {
	var doc workflowDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		return server.Workflow{}, err
	}
	return workflowFromDoc(doc), nil
}

// projectFromRecord converts a pg.ProjectRecord to the server-package Project
// type the delegation methods return. ID and Name are the same project key.
func projectFromRecord(rec pgstore.ProjectRecord) server.Project {
	return server.Project{
		ID:         rec.Name,
		Name:       rec.Name,
		GitHubRepo: rec.GitHubRepo,
		ArgoCDApp:  rec.ArgoCDApp,
		Metadata:   mapOrEmpty(rec.Metadata),
		CreatedAt:  rec.CreatedAt.UTC(),
	}
}

// testLeaseDefaultsFromRow converts a pg.TestLeaseDefaultsRow to the
// server-package TestLeaseDefaults type. Used by ReadTestLeaseDefaults
// and the SetGlobal* delegations.
func testLeaseDefaultsFromRow(row pgstore.TestLeaseDefaultsRow) server.TestLeaseDefaults {
	return server.TestLeaseDefaults{
		GlobalTTLSeconds:     row.GlobalTTLSeconds,
		HotSwapMinTTLSeconds: row.HotSwapMinTTLSeconds,
	}
}

const workflowSchemaKind = "workflow_schema"

// NewFromSettings constructs the store wrapper; callers wire in pg.*Store
// fields via SetPG* setters after constructing the Postgres pool.
func NewFromSettings(settings server.Settings) (*Store, error) {
	return &Store{
		nativeProjectConcurrency: settings.NativeRunnerProjectConcurrency,
	}, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]server.Project, error) {
	records, err := s.pgProjects.List(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]server.Project, 0, len(records))
	for _, rec := range records {
		rows = append(rows, projectFromRecord(rec))
	}
	return rows, nil
}

func (s *Store) listProjectNames(ctx context.Context) ([]string, error) {
	return s.pgProjects.ListNames(ctx)
}

func (s *Store) UpsertProject(ctx context.Context, req server.ProjectRegister) (server.Project, error) {
	rec, err := s.pgProjects.Upsert(ctx, pgstore.ProjectRegister{
		Name:       req.Name,
		GitHubRepo: req.GitHubRepo,
		Metadata:   req.Metadata,
	})
	if err != nil {
		return server.Project{}, err
	}
	return projectFromRecord(rec), nil
}

func (s *Store) SetProjectTestEnvironmentCount(ctx context.Context, project string, count int) (server.Project, error) {
	rec, err := s.pgProjects.SetTestEnvironmentCount(ctx, project, count)
	if errors.Is(err, pgstore.ErrProjectNotFound) {
		return server.Project{}, server.ErrNotFound
	}
	if err != nil {
		return server.Project{}, err
	}
	return projectFromRecord(rec), nil
}

func (s *Store) SetProjectNativeWorkloadIdentityStatus(ctx context.Context, project string, status server.NativeWorkloadIdentityStatus) (server.Project, error) {
	rec, err := s.pgProjects.SetNativeWorkloadIdentityStatus(ctx, project, status)
	if errors.Is(err, pgstore.ErrProjectNotFound) {
		return server.Project{}, server.ErrNotFound
	}
	if err != nil {
		return server.Project{}, err
	}
	return projectFromRecord(rec), nil
}

// SetProjectManagedAuthOriginStatus persists the auth.romaine.life
// origin reconciler result. Delegates to pg.ProjectsStore. See
// nelsong6/glimmung#142 stage 2.
func (s *Store) SetProjectManagedAuthOriginStatus(ctx context.Context, project string, status server.ManagedAuthOriginStatus) (server.Project, error) {
	rec, err := s.pgProjects.SetManagedAuthOriginStatus(ctx, project, status)
	if errors.Is(err, pgstore.ErrProjectNotFound) {
		return server.Project{}, server.ErrNotFound
	}
	if err != nil {
		return server.Project{}, err
	}
	return projectFromRecord(rec), nil
}

const (
	// testLeaseDefaultsDocKind is the persisted kind value for the singleton
	// settings row.
	testLeaseDefaultsDocKind = "test_lease_defaults"
)

func (s *Store) ReadTestLeaseDefaults(ctx context.Context) (server.TestLeaseDefaults, error) {
	row, err := s.pgProjects.ReadTestLeaseDefaults(ctx)
	if errors.Is(err, pgstore.ErrProjectNotFound) {
		return server.TestLeaseDefaults{}, server.ErrNotFound
	}
	if err != nil {
		return server.TestLeaseDefaults{}, err
	}
	return testLeaseDefaultsFromRow(row), nil
}

func (s *Store) SetGlobalTestLeaseDefaultTTL(ctx context.Context, ttlSeconds *int) (server.TestLeaseDefaults, error) {
	if ttlSeconds != nil && *ttlSeconds <= 0 {
		return server.TestLeaseDefaults{}, server.ValidationError{Message: "ttl_seconds must be positive"}
	}
	row, err := s.pgProjects.SetGlobalTestLeaseDefaultTTL(ctx, ttlSeconds)
	if err != nil {
		return server.TestLeaseDefaults{}, err
	}
	return testLeaseDefaultsFromRow(row), nil
}

func (s *Store) SetGlobalTestLeaseHotSwapMinTTL(ctx context.Context, ttlSeconds *int) (server.TestLeaseDefaults, error) {
	if ttlSeconds != nil && *ttlSeconds <= 0 {
		return server.TestLeaseDefaults{}, server.ValidationError{Message: "ttl_seconds must be positive"}
	}
	row, err := s.pgProjects.SetGlobalTestLeaseHotSwapMinTTL(ctx, ttlSeconds)
	if err != nil {
		return server.TestLeaseDefaults{}, err
	}
	return testLeaseDefaultsFromRow(row), nil
}

func (s *Store) SetProjectTestLeaseDefaultTTL(ctx context.Context, project string, ttlSeconds *int) (server.Project, error) {
	if ttlSeconds != nil && *ttlSeconds <= 0 {
		return server.Project{}, server.ValidationError{Message: "ttl_seconds must be positive"}
	}
	rec, err := s.pgProjects.SetTestLeaseDefaultTTL(ctx, project, ttlSeconds)
	if errors.Is(err, pgstore.ErrProjectNotFound) {
		return server.Project{}, server.ErrNotFound
	}
	if err != nil {
		return server.Project{}, err
	}
	return projectFromRecord(rec), nil
}

func (s *Store) SetProjectTestLeaseHotSwapMinTTL(ctx context.Context, project string, ttlSeconds *int) (server.Project, error) {
	if ttlSeconds != nil && *ttlSeconds <= 0 {
		return server.Project{}, server.ValidationError{Message: "ttl_seconds must be positive"}
	}
	rec, err := s.pgProjects.SetTestLeaseHotSwapMinTTL(ctx, project, ttlSeconds)
	if errors.Is(err, pgstore.ErrProjectNotFound) {
		return server.Project{}, server.ErrNotFound
	}
	if err != nil {
		return server.Project{}, err
	}
	return projectFromRecord(rec), nil
}

// StripProjectTestEnvironmentSlotsArray removes the embedded
// `metadata.native_standby_dns.slots[]` array from a project row. Called by
// the one-shot slot-storage cleanup in internal/server/slot_migration.go.
func (s *Store) StripProjectTestEnvironmentSlotsArray(ctx context.Context, project string) error {
	err := s.pgProjects.StripLegacySlotsArray(ctx, project)
	if errors.Is(err, pgstore.ErrProjectNotFound) {
		return server.ErrNotFound
	}
	return err
}

// ReadProject returns the project record. Project writes use row-level locking
// inside pg.ProjectsStore, so the returned Project has no external CAS token.
func (s *Store) ReadProject(ctx context.Context, project string) (server.Project, error) {
	rec, err := s.pgProjects.Read(ctx, project)
	if errors.Is(err, pgstore.ErrProjectNotFound) {
		return server.Project{}, server.ErrNotFound
	}
	if err != nil {
		return server.Project{}, err
	}
	return projectFromRecord(rec), nil
}

func (s *Store) ListWorkflows(ctx context.Context) ([]server.Workflow, error) {
	rows, err := s.pgWorkflows.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]server.Workflow, 0, len(rows))
	for _, row := range rows {
		w, err := workflowFromPayload(row.Payload)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

func (s *Store) UpsertWorkflow(ctx context.Context, req server.WorkflowRegister) (server.Workflow, error) {
	if _, err := s.pgProjects.Read(ctx, req.Project); err != nil {
		if errors.Is(err, pgstore.ErrProjectNotFound) {
			return server.Workflow{}, server.ValidationError{Message: "project " + req.Project + " does not exist; register it first"}
		}
		return server.Workflow{}, err
	}
	normalizeWorkflowRegister(&req)
	if err := validateWorkflowRegister(req); err != nil {
		return server.Workflow{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc := workflowDocFromRegister(req, now)
	doc.Kind = "workflow"
	doc.SchemaRef = workflowSchemaRef(doc)
	schemaDoc := workflowSchemaDocFromWorkflow(doc)

	workflowPayload, err := json.Marshal(doc)
	if err != nil {
		return server.Workflow{}, err
	}
	schemaPayload, err := json.Marshal(schemaDoc)
	if err != nil {
		return server.Workflow{}, err
	}
	row, err := s.pgWorkflows.Upsert(ctx,
		pgstore.WorkflowRow{
			Project:   req.Project,
			Name:      req.Name,
			SchemaRef: doc.SchemaRef,
			Payload:   workflowPayload,
		},
		pgstore.WorkflowSchemaRow{
			Project:   req.Project,
			SchemaRef: doc.SchemaRef,
			Payload:   schemaPayload,
		},
	)
	if err != nil {
		return server.Workflow{}, err
	}
	return workflowFromPayload(row.Payload)
}

func (s *Store) DeleteWorkflow(ctx context.Context, project string, name string) (server.Workflow, error) {
	row, err := s.pgWorkflows.Delete(ctx, project, name)
	if errors.Is(err, pgstore.ErrWorkflowNotFound) {
		return server.Workflow{}, server.ErrNotFound
	}
	if err != nil {
		return server.Workflow{}, err
	}
	return workflowFromPayload(row.Payload)
}

func (s *Store) PatchWorkflow(ctx context.Context, project string, name string, req server.WorkflowPatchRequest) (server.Workflow, error) {
	// Read the existing workflow, convert to a Register, apply patch,
	// then run through UpsertWorkflow so the schema_ref recomputation
	// stays in one place.
	row, err := s.pgWorkflows.GetByName(ctx, project, name)
	if errors.Is(err, pgstore.ErrWorkflowNotFound) {
		return server.Workflow{}, server.ErrNotFound
	}
	if err != nil {
		return server.Workflow{}, err
	}
	var doc workflowDoc
	if err := json.Unmarshal(row.Payload, &doc); err != nil {
		return server.Workflow{}, err
	}
	reg := workflowRegisterFromDoc(doc)
	if req.BudgetTotal != nil {
		reg.Budget.Total = *req.BudgetTotal
	}
	return s.UpsertWorkflow(ctx, reg)
}

func (s *Store) ListLeases(ctx context.Context) ([]server.Lease, error) {
	pgRows, err := s.pgLeases.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]server.Lease, 0, len(pgRows))
	for _, row := range pgRows {
		doc, derr := leaseDocFromPayload(row.Payload)
		if derr != nil {
			return nil, derr
		}
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
	releasedAt := now.Format(time.RFC3339Nano)
	patched, err := s.pgLeases.PatchPayload(ctx, doc.Project, doc.ID, func(payload map[string]any) error {
		payload["state"] = "released"
		payload["released_at"] = releasedAt
		return nil
	})
	if errors.Is(err, pgstore.ErrLeaseNotFound) {
		return server.Lease{}, server.ErrNotFound
	}
	if err != nil {
		return server.Lease{}, err
	}
	updated, err := leaseDocFromPayload(patched.Payload)
	if err != nil {
		return server.Lease{}, err
	}
	lease := leaseFromDoc(updated)
	if boolValue(lease.Metadata["native_k8s"]) && !boolValue(lease.Metadata["test_slot_checkout"]) {
		_ = s.releaseNativeSlotReservation(ctx, lease, now)
	}
	return lease, nil
}

func (s *Store) ListProjectRuns(ctx context.Context, project string, limit int) ([]server.RunReport, error) {
	rows, err := s.pgRuns.List(ctx, project, limit)
	if err != nil {
		return nil, err
	}
	docs := make([]runDoc, 0, len(rows))
	for _, row := range rows {
		doc, derr := runDocFromPGRow(row)
		if derr != nil {
			return nil, derr
		}
		docs = append(docs, doc)
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
	issues, err := s.listIssueDocs(ctx, filter.Project)
	if err != nil {
		return nil, err
	}
	runDocs, err := s.listRunDocs(ctx, filter.Project)
	if err != nil {
		return nil, err
	}
	// ListHeldByScope returns only currently-held + unexpired locks, so the
	// per-row check below collapses to a simple map lookup.
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
	// Audit-trail comment first so its created_at sorts before the
	// state change in the issues.updated_at history.
	note := capitalize(req.Action)
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		note = note + ": " + reason
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	commentDoc := issueCommentDoc{
		ID:        uuid.NewString(),
		Author:    req.Author,
		Body:      note,
		CreatedAt: now,
		UpdatedAt: now,
	}
	commentPayload, err := json.Marshal(commentDoc)
	if err != nil {
		return server.IssueDetail{}, err
	}
	if _, err := s.pgIssues.CreateComment(ctx, pgstore.IssueCommentRow{
		ID:          commentDoc.ID,
		Project:     req.Project,
		IssueNumber: req.Number,
		Payload:     commentPayload,
	}); err != nil {
		if errors.Is(err, pgstore.ErrIssueNotFound) {
			return server.IssueDetail{}, server.ErrNotFound
		}
		return server.IssueDetail{}, err
	}
	if _, err := s.pgIssues.PatchPayload(ctx, req.Project, req.Number, func(payload map[string]any) error {
		state, _ := payload["state"].(string)
		if state == "open" || state == "" {
			payload["state"] = "closed"
			payload["closed_at"] = now
		}
		payload["updated_at"] = now
		return nil
	}); err != nil {
		if errors.Is(err, pgstore.ErrIssueNotFound) {
			return server.IssueDetail{}, server.ErrNotFound
		}
		return server.IssueDetail{}, err
	}
	return s.GetIssueDetailByNumber(ctx, req.Project, req.Number)
}

const canonicalIssuePredicate = "IS_DEFINED(c.number) AND c.number > 0 AND (c.state = 'open' OR c.state = 'closed')"

// nextIssueNumber delegates to pgIssues, which seeds from
// MAX(issues.number)+1 on first call per-project and atomically
// increments on every subsequent call inside a transaction.
func (s *Store) nextIssueNumber(ctx context.Context, project string) (int, error) {
	return s.pgIssues.AllocateNextNumber(ctx, project)
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
		Comments:        []issueCommentDoc{},
		CreatedAt:       now,
		UpdatedAt:       now,
		PreserveTestEnv: req.PreserveTestEnv,
	}
	// Comments live in their own table; strip before marshaling payload.
	stripped := doc
	stripped.Comments = nil
	payload, err := json.Marshal(stripped)
	if err != nil {
		return server.IssueDetail{}, err
	}
	if _, err := s.pgIssues.Create(ctx, pgstore.IssueRow{
		Project: req.Project,
		Number:  number,
		Payload: payload,
	}); err != nil {
		return server.IssueDetail{}, err
	}
	return s.GetIssueDetailByNumber(ctx, req.Project, number)
}

func (s *Store) PatchIssueByNumber(ctx context.Context, req server.IssuePatch) (server.IssueDetail, error) {
	// Validate state value before opening the patch tx so we don't
	// abort mid-transaction with a ValidationError.
	if req.State != nil {
		switch strings.ToLower(*req.State) {
		case "closed", "open":
		default:
			return server.IssueDetail{}, server.ValidationError{Message: "state must be 'open' or 'closed'"}
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.pgIssues.PatchPayload(ctx, req.Project, req.Number, func(payload map[string]any) error {
		if req.Title != nil {
			payload["title"] = *req.Title
		}
		if req.Body != nil {
			payload["body"] = *req.Body
		}
		if req.Labels != nil {
			payload["labels"] = *req.Labels
		}
		if req.State != nil {
			target := strings.ToLower(*req.State)
			current, _ := payload["state"].(string)
			switch target {
			case "closed":
				if current != "closed" {
					payload["state"] = "closed"
					payload["closed_at"] = now
				}
			case "open":
				if current == "closed" {
					payload["state"] = "open"
					delete(payload, "closed_at")
				}
			}
		}
		if req.PreserveTestEnv != nil {
			payload["preserve_test_env"] = *req.PreserveTestEnv
		}
		payload["updated_at"] = now
		return nil
	})
	if errors.Is(err, pgstore.ErrIssueNotFound) {
		return server.IssueDetail{}, server.ErrNotFound
	}
	if err != nil {
		return server.IssueDetail{}, err
	}
	return s.GetIssueDetailByNumber(ctx, req.Project, req.Number)
}

func (s *Store) AddIssueComment(ctx context.Context, req server.IssueCommentAdd) (server.IssueComment, error) {
	if strings.TrimSpace(req.Body) == "" {
		return server.IssueComment{}, server.ValidationError{Message: "body required"}
	}
	// Verify the parent issue exists; CreateComment doesn't enforce
	// referential integrity at the FK level (issue_comments has no FK
	// to issues — we keep them in sync at the application layer).
	if _, err := s.pgIssues.GetByNumber(ctx, req.Project, req.Number); err != nil {
		if errors.Is(err, pgstore.ErrIssueNotFound) {
			return server.IssueComment{}, server.ErrNotFound
		}
		return server.IssueComment{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	comment := issueCommentDoc{
		ID:        uuid.NewString(),
		Author:    req.Author,
		Body:      req.Body,
		CreatedAt: now,
		UpdatedAt: now,
	}
	commentPayload, err := json.Marshal(comment)
	if err != nil {
		return server.IssueComment{}, err
	}
	if _, err := s.pgIssues.CreateComment(ctx, pgstore.IssueCommentRow{
		ID:          comment.ID,
		Project:     req.Project,
		IssueNumber: req.Number,
		Payload:     commentPayload,
	}); err != nil {
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
	if strings.TrimSpace(req.Body) == "" {
		return server.IssueComment{}, server.ValidationError{Message: "body required"}
	}
	// Pre-check author so we don't open a tx and roll back.
	existing, err := s.pgIssues.GetComment(ctx, req.CommentID)
	if errors.Is(err, pgstore.ErrIssueNotFound) {
		return server.IssueComment{}, server.ErrNotFound
	}
	if err != nil {
		return server.IssueComment{}, err
	}
	if existing.Project != req.Project || existing.IssueNumber != req.Number {
		// Comment does not belong to this issue.
		return server.IssueComment{}, server.ErrNotFound
	}
	var existingDoc issueCommentDoc
	if err := json.Unmarshal(existing.Payload, &existingDoc); err != nil {
		return server.IssueComment{}, err
	}
	if existingDoc.Author != req.Author {
		return server.IssueComment{}, server.ErrForbidden
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	patched, err := s.pgIssues.PatchComment(ctx, req.CommentID, func(payload map[string]any) error {
		payload["body"] = req.Body
		payload["updated_at"] = now
		return nil
	})
	if errors.Is(err, pgstore.ErrIssueNotFound) {
		return server.IssueComment{}, server.ErrNotFound
	}
	if err != nil {
		return server.IssueComment{}, err
	}
	var patchedDoc issueCommentDoc
	if err := json.Unmarshal(patched.Payload, &patchedDoc); err != nil {
		return server.IssueComment{}, err
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, patchedDoc.CreatedAt)
	updatedAt, _ := time.Parse(time.RFC3339Nano, patchedDoc.UpdatedAt)
	return server.IssueComment{
		ID:        patchedDoc.ID,
		Author:    patchedDoc.Author,
		Body:      patchedDoc.Body,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

func (s *Store) DeleteIssueComment(ctx context.Context, req server.IssueCommentDelete) (server.IssueDetail, error) {
	// Verify scope so a caller can't delete a comment by id from the
	// wrong issue. Match the materialized issue payload by 404ing on miss.
	existing, err := s.pgIssues.GetComment(ctx, req.CommentID)
	if errors.Is(err, pgstore.ErrIssueNotFound) {
		return server.IssueDetail{}, server.ErrNotFound
	}
	if err != nil {
		return server.IssueDetail{}, err
	}
	if existing.Project != req.Project || existing.IssueNumber != req.Number {
		return server.IssueDetail{}, server.ErrNotFound
	}
	if err := s.pgIssues.DeleteComment(ctx, req.CommentID); err != nil {
		if errors.Is(err, pgstore.ErrIssueNotFound) {
			return server.IssueDetail{}, server.ErrNotFound
		}
		return server.IssueDetail{}, err
	}
	return s.GetIssueDetailByNumber(ctx, req.Project, req.Number)
}

func (s *Store) readIssueByNumber(ctx context.Context, project string, number int) (issueDoc, error) {
	row, err := s.pgIssues.GetByNumber(ctx, project, number)
	if errors.Is(err, pgstore.ErrIssueNotFound) {
		return issueDoc{}, server.ErrNotFound
	}
	if err != nil {
		return issueDoc{}, err
	}
	doc, err := s.issueDocFromPGRow(ctx, row)
	if err != nil {
		return issueDoc{}, err
	}
	if !isCanonicalIssueDoc(doc) {
		return issueDoc{}, server.ErrNotFound
	}
	return doc, nil
}

func (s *Store) listIssueDocs(ctx context.Context, project string) ([]issueDoc, error) {
	rows, err := s.pgIssues.List(ctx, project)
	if err != nil {
		return nil, err
	}
	docs, err := s.issueDocsFromPGRows(ctx, rows)
	if err != nil {
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

func (s *Store) listRunDocs(ctx context.Context, project string) ([]runDoc, error) {
	var (
		rows []pgstore.RunRow
		err  error
	)
	if project != "" {
		rows, err = s.pgRuns.List(ctx, project, 0)
	} else {
		rows, err = s.pgRuns.ListAll(ctx)
	}
	if err != nil {
		return nil, err
	}
	docs := make([]runDoc, 0, len(rows))
	for _, row := range rows {
		doc, err := runDocFromPGRow(row)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func (s *Store) latestRunForIssue(ctx context.Context, issue issueDoc) (*runDoc, []runDoc, error) {
	// Try issue_id (payload->>'issue_id') first; fall back to
	// (project, issue_number) which is indexed.
	var docs []runDoc
	if issue.ID != "" {
		all, err := s.pgRuns.List(ctx, issue.Project, 0)
		if err != nil {
			return nil, nil, err
		}
		for _, row := range all {
			doc, err := runDocFromPGRow(row)
			if err != nil {
				return nil, nil, err
			}
			if doc.IssueID == issue.ID {
				docs = append(docs, doc)
			}
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
	rows, err := s.pgRuns.ListByIssue(ctx, project, issueNumber)
	if err != nil {
		return nil, err
	}
	docs := make([]runDoc, 0, len(rows))
	for _, row := range rows {
		doc, derr := runDocFromPGRow(row)
		if derr != nil {
			return nil, derr
		}
		docs = append(docs, doc)
	}
	// Defensive re-sort: pg ordering is created_at ASC, but the
	// payload's own CreatedAt may differ on legacy migrated rows.
	sort.SliceStable(docs, func(i, j int) bool {
		return docs[i].CreatedAt < docs[j].CreatedAt
	})
	return docs, nil
}

func (s *Store) readLeaseDocByCallbackToken(ctx context.Context, token string) (leaseDoc, error) {
	row, err := s.pgLeases.GetByCallbackToken(ctx, token)
	if errors.Is(err, pgstore.ErrLeaseNotFound) {
		return leaseDoc{}, server.ErrNotFound
	}
	if err != nil {
		return leaseDoc{}, err
	}
	return leaseDocFromPayload(row.Payload)
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
	ID                   string                       `json:"id"`
	Project              string                       `json:"project"`
	Workflow             string                       `json:"workflow"`
	WorkflowSchemaRef    string                       `json:"workflow_schema_ref,omitempty"`
	RunNumber            *int                         `json:"run_number"`
	RunCycleNumber       *int                         `json:"run_cycle_number,omitempty"`
	RunDisplayNumber     *string                      `json:"run_display_number"`
	ParentRunID          *string                      `json:"parent_run_id"`
	RootRunID            *string                      `json:"root_run_id"`
	OriginKind           *string                      `json:"origin_kind"`
	IsCycle              bool                         `json:"is_cycle"`
	CycleNumber          *int                         `json:"cycle_number"`
	IssueID              string                       `json:"issue_id"`
	IssueRepo            string                       `json:"issue_repo"`
	IssueNumber          int                          `json:"issue_number"`
	PRNumber             *int                         `json:"pr_number"`
	State                string                       `json:"state"`
	QueueState           *string                      `json:"queue_state,omitempty"`
	AdmissionError       *string                      `json:"admission_error,omitempty"`
	SlotLeaseRef         *string                      `json:"slot_lease_ref,omitempty"`
	Attempts             []attemptDoc                 `json:"attempts"`
	PhaseExecutions      []phaseExecutionDoc          `json:"phase_executions,omitempty"`
	CumulativeCostUSD    float64                      `json:"cumulative_cost_usd"`
	Budget               *budgetDoc                   `json:"budget,omitempty"`
	ValidationURL        *string                      `json:"validation_url"`
	ScreenshotsMarkdown  *string                      `json:"screenshots_markdown"`
	EvidenceRequirements []server.EvidenceRequirement `json:"evidence_requirements,omitempty"`
	AbortReason          *string                      `json:"abort_reason"`
	EntrypointPhase      *string                      `json:"entrypoint_phase,omitempty"`
	TriggerSource        map[string]any               `json:"trigger_source"`
	CreatedAt            string                       `json:"created_at"`
	UpdatedAt            string                       `json:"updated_at"`
	// Fields used by mutation operations.
	CallbackToken     *string `json:"callback_token,omitempty"`
	IssueLockHolderID *string `json:"issue_lock_holder_id,omitempty"`
	PRLockHolderID    *string `json:"pr_lock_holder_id,omitempty"`
	// PreserveTestEnv is the immutable snapshot of the originating issue's
	// flag at dispatch time. Default false. The cleanup_early phase
	// consults this to decide execute vs `skipped`.
	PreserveTestEnv bool `json:"preserve_test_env,omitempty"`
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
	ID              string            `json:"id"`
	Number          int               `json:"number"`
	Project         string            `json:"project"`
	Title           string            `json:"title"`
	Body            string            `json:"body"`
	Labels          []string          `json:"labels"`
	State           string            `json:"state"`
	Metadata        issueMetadataDoc  `json:"metadata"`
	Comments        []issueCommentDoc `json:"comments"`
	CreatedAt       string            `json:"created_at"`
	UpdatedAt       string            `json:"updated_at"`
	ClosedAt        *string           `json:"closed_at,omitempty"`
	PreserveTestEnv bool              `json:"preserve_test_env,omitempty"`
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
	Status       string                    `json:"status"`
	Reasons      []string                  `json:"reasons"`
	EvidenceRefs []string                  `json:"evidence_refs"`
	Evidence     []server.EvidenceArtifact `json:"evidence,omitempty"`
	CostUSD      float64                   `json:"cost_usd"`
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
	SkipWhenPreserveTestEnv  bool              `json:"skipWhenPreserveTestEnv,omitempty"`
}

type recyclePolicyDoc struct {
	MaxAttempts int      `json:"maxAttempts"`
	On          []string `json:"on"`
	LandsAt     string   `json:"landsAt"`
}

type nativeJobDoc struct {
	ID               string              `json:"id"`
	Name             *string             `json:"name"`
	Primitive        string              `json:"primitive,omitempty"`
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
		Ref:             publicids.IssueRef(doc.Project, &number),
		Project:         doc.Project,
		Repo:            nil,
		Number:          &number,
		Title:           doc.Title,
		Body:            doc.Body,
		State:           firstNonEmpty(doc.State, "open"),
		Labels:          sliceOrEmpty(doc.Labels),
		HTMLURL:         nil,
		Comments:        comments,
		PreserveTestEnv: doc.PreserveTestEnv,
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
		ID:                   doc.ID,
		Ref:                  runRef + "/report",
		Project:              doc.Project,
		RunRef:               runRef,
		RunNumber:            doc.RunNumber,
		RunDisplayNumber:     optionalNonEmptyStringPtr(display),
		ParentRunRef:         refPtr(lineageByID, parentID),
		RootRunRef:           refPtr(lineageByID, rootID),
		OriginKind:           emptyStringNil(originKind),
		EntrypointPhase:      emptyStringNil(doc.EntrypointPhase),
		TriggerSource:        mapOrEmpty(doc.TriggerSource),
		IsCycle:              doc.IsCycle,
		CycleNumber:          doc.CycleNumber,
		RunCycleNumber:       doc.RunCycleNumber,
		WorkflowSchemaRef:    doc.WorkflowSchemaRef,
		QueueState:           emptyStringNil(doc.QueueState),
		AdmissionError:       emptyStringNil(doc.AdmissionError),
		SlotLeaseRef:         emptyStringNil(doc.SlotLeaseRef),
		Workflow:             doc.Workflow,
		IssueRef:             optionalNonEmptyStringPtr(publicids.IssueRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber))),
		IssueRepo:            optionalNonEmptyStringPtr(doc.IssueRepo),
		IssueNumber:          positiveIssueNumberPtr(doc.IssueNumber),
		State:                firstNonEmpty(doc.State, "in_progress"),
		CurrentPhase:         currentPhase,
		AttemptsCount:        len(doc.Attempts),
		PhaseExecutions:      runPhaseExecutionsFromDocs(doc.PhaseExecutions),
		CumulativeCostUSD:    doc.CumulativeCostUSD,
		ValidationURL:        emptyStringNil(doc.ValidationURL),
		ScreenshotsMarkdown:  emptyStringNil(doc.ScreenshotsMarkdown),
		EvidenceRequirements: sliceOrEmpty(doc.EvidenceRequirements),
		AbortReason:          emptyStringNil(doc.AbortReason),
		StartedAt:            parseTimeOrNow(doc.CreatedAt),
		CompletedAt:          completed,
		UpdatedAt:            parseTimeOrNow(doc.UpdatedAt),
		Attempts:             attempts,
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
		evidenceRefs = appendMissingStrings(evidenceRefs, server.EvidenceRefsFromArtifacts(doc.Verification.Evidence)...)
		if doc.CostUSD == nil {
			cost = &doc.Verification.CostUSD
		}
	}
	evidenceRefs = appendMissingStrings(evidenceRefs, evidenceRefsFromPhaseOutputs(doc.PhaseOutputs)...)
	evidence := evidenceArtifactsForAttempt(doc)
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
		CarryForward:       doc.CarryForward,
		DispatchedAt:       parseTimeOrNow(doc.DispatchedAt),
		CompletedAt:        parseOptionalTime(doc.CompletedAt),
		Conclusion:         emptyStringNil(doc.Conclusion),
		VerificationStatus: verificationStatus,
		EvidenceRefs:       evidenceRefs,
		Evidence:           evidence,
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
	evidenceRefs := []string{}
	if doc.Verification != nil {
		verificationStatus = optionalNonEmptyStringPtr(doc.Verification.Status)
		verificationReasons = sliceOrEmpty(doc.Verification.Reasons)
		evidenceRefs = sliceOrEmpty(doc.Verification.EvidenceRefs)
		evidenceRefs = appendMissingStrings(evidenceRefs, server.EvidenceRefsFromArtifacts(doc.Verification.Evidence)...)
	}
	return server.RunAttemptJobCompletion{
		JobID:               doc.JobID,
		CompletedAt:         parseOptionalTime(doc.CompletedAt),
		Conclusion:          doc.Conclusion,
		VerificationStatus:  verificationStatus,
		VerificationReasons: verificationReasons,
		EvidenceRefs:        evidenceRefs,
		Evidence:            sliceOrEmpty(doc.VerificationEvidence()),
		CostUSD:             doc.CostUSD,
		PhaseOutputs:        mapStringOrEmpty(doc.PhaseOutputs),
	}
}

func (doc nativeJobCompletionDoc) VerificationEvidence() []server.EvidenceArtifact {
	if doc.Verification == nil {
		return nil
	}
	return doc.Verification.Evidence
}

func evidenceArtifactsForAttempt(doc attemptDoc) []server.EvidenceArtifact {
	out := make([]server.EvidenceArtifact, 0)
	if doc.Verification != nil {
		out = append(out, doc.Verification.Evidence...)
	}
	for _, artifact := range server.EvidenceArtifactsFromVerificationOutput(doc.PhaseOutputs["verification"]) {
		artifact = evidenceArtifactWithSource(artifact, doc.Phase, doc.AttemptIndex)
		out = appendEvidenceArtifact(out, artifact)
	}
	for i := range out {
		out[i] = evidenceArtifactWithSource(out[i], doc.Phase, doc.AttemptIndex)
	}
	return out
}

func appendEvidenceArtifact(values []server.EvidenceArtifact, artifact server.EvidenceArtifact) []server.EvidenceArtifact {
	ref := strings.TrimSpace(firstNonEmpty(artifact.Ref, artifact.ArtifactPath, artifact.URL))
	if ref == "" {
		return values
	}
	key := artifact.Kind + "\x00" + ref
	for _, existing := range values {
		existingRef := strings.TrimSpace(firstNonEmpty(existing.Ref, existing.ArtifactPath, existing.URL))
		if existing.Kind+"\x00"+existingRef == key {
			return values
		}
	}
	return append(values, artifact)
}

func evidenceArtifactWithSource(artifact server.EvidenceArtifact, phase string, attemptIndex int) server.EvidenceArtifact {
	if strings.TrimSpace(artifact.SourcePhase) == "" {
		artifact.SourcePhase = phase
	}
	if artifact.SourceAttemptIndex == nil {
		artifact.SourceAttemptIndex = &attemptIndex
	}
	return artifact
}

func evidenceRefsFromPhaseOutputs(outputs map[string]string) []string {
	raw := strings.TrimSpace(outputs["verification"])
	if raw == "" {
		return nil
	}
	var payload struct {
		EvidenceRefs []string `json:"evidence_refs"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil
	}
	return appendMissingStrings(cleanStringRefs(payload.EvidenceRefs), server.EvidenceRefsFromArtifacts(server.EvidenceArtifactsFromVerificationOutput(raw))...)
}

func cleanStringRefs(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func appendMissingStrings(values []string, additions ...string) []string {
	out := append([]string{}, values...)
	for _, addition := range additions {
		addition = strings.TrimSpace(addition)
		if addition == "" || containsString(out, addition) {
			continue
		}
		out = append(out, addition)
	}
	return out
}

func phaseExecutionDocsFromWorkflow(wf server.Workflow, createdAt string, entrypointPhase *string) []phaseExecutionDoc {
	wf = server.CanonicalWorkflow(wf)
	out := make([]phaseExecutionDoc, 0, len(wf.Phases))
	beforeEntrypoint := strings.TrimSpace(stringOrEmpty(entrypointPhase)) != ""
	for _, phase := range wf.Phases {
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
			canonical := server.CanonicalWorkflow(*wf)
			return &canonical, nil
		}
	}
	wf, err := s.GetWorkflowByName(ctx, project, workflowName)
	if err != nil {
		return nil, err
	}
	if wf == nil {
		return nil, server.ValidationError{Message: fmt.Sprintf("workflow %q is not registered", workflowName)}
	}
	canonical := server.CanonicalWorkflow(*wf)
	return &canonical, nil
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
		SkipWhenPreserveTestEnv:  phase.SkipWhenPreserveTestEnv,
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
		Primitive:        job.Primitive,
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
	return prDoc{RecyclePolicy: recyclePolicyDocFromSpec(pr.RecyclePolicy)}
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

func validateWorkflowRegister(req server.WorkflowRegister) error {
	return server.ValidateWorkflowRegister(req)
}

func normalizeWorkflowRegister(req *server.WorkflowRegister) {
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
		for j := range req.Phases[i].Jobs {
			req.Phases[i].Jobs[j].Primitive = strings.TrimSpace(req.Phases[i].Jobs[j].Primitive)
		}
		req.Phases[i] = server.CanonicalNativePhase(req.Phases[i])
	}
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
		SkipWhenPreserveTestEnv:  doc.SkipWhenPreserveTestEnv,
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
		Primitive:        doc.Primitive,
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
	return server.PrPrimitive{RecyclePolicy: recyclePolicyFromDoc(doc.RecyclePolicy)}
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
	ID            string                      `json:"id"`
	Project       string                      `json:"project"`
	Repo          string                      `json:"repo"`
	Number        int                         `json:"number"`
	Title         string                      `json:"title"`
	Body          string                      `json:"body"`
	State         string                      `json:"state"`
	Branch        string                      `json:"branch"`
	BaseRef       string                      `json:"base_ref"`
	HeadSHA       string                      `json:"head_sha"`
	HTMLURL       string                      `json:"html_url"`
	LinkedIssueID *string                     `json:"linked_issue_id"`
	LinkedRunID   *string                     `json:"linked_run_id"`
	MergedAt      *string                     `json:"merged_at"`
	MergedBy      *string                     `json:"merged_by"`
	Comments      []map[string]any            `json:"comments"`
	Reviews       []map[string]any            `json:"reviews"`
	Evidence      []server.TouchpointEvidence `json:"evidence"`
	CreatedAt     string                      `json:"created_at"`
	UpdatedAt     string                      `json:"updated_at"`
}

func (s *Store) ListTouchpoints(ctx context.Context, filter server.TouchpointListFilter) ([]server.TouchpointRow, error) {
	// pg.TouchpointsStore.List handles both the single-project and
	// cross-project cases via a single SELECT against the unified
	// touchpoints table.
	limit := 0
	if filter.Limit != nil {
		limit = *filter.Limit
	}
	pgRows, err := s.pgTouchpoints.List(ctx, filter.Project, filter.Repo, filter.State, 0)
	if err != nil {
		return nil, err
	}
	touchpointDocs := make([]touchpointDoc, 0, len(pgRows))
	for _, row := range pgRows {
		doc, derr := touchpointDocFromPayload(row.Payload)
		if derr != nil {
			return nil, derr
		}
		touchpointDocs = append(touchpointDocs, doc)
	}
	_ = limit

	// Enrich with issue and run data.
	issueDocs, _ := s.listIssueDocs(ctx, filter.Project)
	runDocs, _ := s.listRunDocs(ctx, filter.Project)
	heldPRLocks, _ := s.pgLocks.ListHeldByScope(ctx, "pr")
	prLockByKey := make(map[string]bool, len(heldPRLocks))
	for key := range heldPRLocks {
		prLockByKey[key] = true
	}

	issueRefByID, issueNumberByID := buildIssueIndexes(issueDocs)
	runRefByID, runByLinkedIssueID, runByRepoPR := buildRunIndexes(runDocs)

	now := time.Now().UTC()
	out := make([]server.TouchpointRow, 0, len(touchpointDocs))
	for _, doc := range touchpointDocs {
		row := touchpointRowFromDoc(doc, issueRefByID, issueNumberByID, runRefByID, runByLinkedIssueID, runByRepoPR, prLockByKey, now)
		out = append(out, row)
	}
	if filter.Limit != nil && *filter.Limit < len(out) {
		out = out[:*filter.Limit]
	}
	return out, nil
}

func (s *Store) GetTouchpointForIssue(ctx context.Context, project string, issueNumber int) (server.TouchpointDetail, error) {
	issueDoc, err := s.readIssueByNumber(ctx, project, issueNumber)
	if err != nil {
		return server.TouchpointDetail{}, server.ErrNotFound
	}
	row, err := s.pgTouchpoints.FindByLinkedIssueID(ctx, project, issueDoc.ID)
	if errors.Is(err, pgstore.ErrTouchpointNotFound) {
		return server.TouchpointDetail{}, server.ErrNotFound
	}
	if err != nil {
		return server.TouchpointDetail{}, err
	}
	doc, err := touchpointDocFromPayload(row.Payload)
	if err != nil {
		return server.TouchpointDetail{}, err
	}
	return s.buildTouchpointDetail(ctx, doc)
}

func (s *Store) EnsureTouchpoint(ctx context.Context, req server.TouchpointCreate) (server.TouchpointDetail, error) {
	// Resolve linked refs.
	var linkedIssueID *string
	if req.LinkedIssueRef != "" {
		linkedIssueID = s.resolveIssueIDByRef(ctx, req.Project, req.LinkedIssueRef)
	}
	var linkedRunID *string
	if req.LinkedRunRef != "" {
		linkedRunID = s.resolveRunIDByRef(ctx, req.Project, req.LinkedRunRef)
	}

	// 1) If we have a linked issue, check for an existing touchpoint
	//    for that issue and patch linkages if provided.
	if linkedIssueID != nil {
		row, err := s.pgTouchpoints.FindByLinkedIssueID(ctx, req.Project, *linkedIssueID)
		if err == nil {
			doc, derr := touchpointDocFromPayload(row.Payload)
			if derr != nil {
				return server.TouchpointDetail{}, derr
			}
			shouldPatch := false
			if linkedRunID != nil && (doc.LinkedRunID == nil || *doc.LinkedRunID != *linkedRunID) {
				shouldPatch = true
			}
			if req.EvidenceSet && !reflect.DeepEqual(sliceOrEmpty(doc.Evidence), sliceOrEmpty(req.Evidence)) {
				shouldPatch = true
			}
			if shouldPatch {
				patched, perr := s.pgTouchpoints.PatchPayload(ctx, doc.Project, doc.Number, func(payload map[string]any) error {
					if linkedRunID != nil {
						payload["linked_run_id"] = *linkedRunID
					}
					if req.EvidenceSet {
						payload["evidence"] = sliceOrEmpty(req.Evidence)
					}
					payload["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
					return nil
				})
				if perr != nil {
					return server.TouchpointDetail{}, perr
				}
				updated, uerr := touchpointDocFromPayload(patched.Payload)
				if uerr != nil {
					return server.TouchpointDetail{}, uerr
				}
				doc = updated
			}
			return s.buildTouchpointDetail(ctx, doc)
		}
	}

	// 2) Fall back to (repo, number) idempotency key.
	if row, err := s.pgTouchpoints.FindByRepoNumber(ctx, req.Repo, req.Number); err == nil {
		doc, derr := touchpointDocFromPayload(row.Payload)
		if derr != nil {
			return server.TouchpointDetail{}, derr
		}
		updated := false
		if linkedIssueID != nil && doc.LinkedIssueID == nil {
			updated = true
		}
		if linkedRunID != nil && (doc.LinkedRunID == nil || *doc.LinkedRunID != *linkedRunID) {
			updated = true
		}
		if req.EvidenceSet && !reflect.DeepEqual(sliceOrEmpty(doc.Evidence), sliceOrEmpty(req.Evidence)) {
			updated = true
		}
		if updated {
			patched, perr := s.pgTouchpoints.PatchPayload(ctx, doc.Project, doc.Number, func(payload map[string]any) error {
				if linkedIssueID != nil {
					if _, ok := payload["linked_issue_id"].(string); !ok || payload["linked_issue_id"] == nil {
						payload["linked_issue_id"] = *linkedIssueID
					}
				}
				if linkedRunID != nil {
					current, _ := payload["linked_run_id"].(string)
					if current != *linkedRunID {
						payload["linked_run_id"] = *linkedRunID
					}
				}
				if req.EvidenceSet {
					payload["evidence"] = sliceOrEmpty(req.Evidence)
				}
				payload["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
				return nil
			})
			if perr != nil {
				return server.TouchpointDetail{}, perr
			}
			updatedDoc, uerr := touchpointDocFromPayload(patched.Payload)
			if uerr != nil {
				return server.TouchpointDetail{}, uerr
			}
			doc = updatedDoc
		}
		return s.buildTouchpointDetail(ctx, doc)
	}

	// 3) Create a new touchpoint.
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
		Evidence:      sliceOrEmpty(req.Evidence),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.TouchpointDetail{}, err
	}
	if _, err := s.pgTouchpoints.Create(ctx, pgstore.TouchpointRow{
		Project:     req.Project,
		IssueNumber: req.Number,
		Payload:     payload,
	}); err != nil {
		return server.TouchpointDetail{}, err
	}
	return s.buildTouchpointDetail(ctx, doc)
}

func (s *Store) buildTouchpointDetail(ctx context.Context, doc touchpointDoc) (server.TouchpointDetail, error) {
	// Look up linked run by id. Touchpoints and their linked runs share a
	// project.
	var run *runDoc
	if doc.LinkedRunID != nil && *doc.LinkedRunID != "" {
		runRow, err := s.pgRuns.Get(ctx, doc.Project, *doc.LinkedRunID)
		if err == nil {
			rd, derr := runDocFromPGRow(runRow)
			if derr == nil {
				run = &rd
			}
		}
	}
	if run == nil {
		// Fall back to latest run by (repo, pr_number) scoped to this
		// project by filtering project runs ordered by created_at DESC.
		rows, err := s.pgRuns.List(ctx, doc.Project, 0)
		if err == nil {
			for _, row := range rows {
				rd, derr := runDocFromPGRow(row)
				if derr != nil {
					continue
				}
				if rd.IssueRepo != doc.Repo {
					continue
				}
				if rd.PRNumber == nil || *rd.PRNumber != doc.Number {
					continue
				}
				if run == nil || rd.CreatedAt > run.CreatedAt {
					tmp := rd
					run = &tmp
				}
			}
		}
	}

	// Look up linked issue by its payload UUID. The (project, number) primary
	// key is preferred but the touchpoint payload carries this id reference.
	var linkedIssueRef *string
	var linkedIssueNumber *int
	var linkedIssueTitle *string
	if doc.LinkedIssueID != nil && *doc.LinkedIssueID != "" {
		row, err := s.pgIssues.GetByPayloadID(ctx, doc.Project, *doc.LinkedIssueID)
		if err == nil {
			issue, derr := s.issueDocFromPGRow(ctx, row)
			if derr == nil && isCanonicalIssueDoc(issue) {
				ref := publicids.IssueRef(issue.Project, &issue.Number)
				linkedIssueRef = &ref
				linkedIssueNumber = &issue.Number
				linkedIssueTitle = &issue.Title
			}
		}
	}

	// PR lock state lives in pg.LocksStore.
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
		Evidence:       sliceOrEmpty(doc.Evidence),
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
	// Whole-doc replace, used by callers (currently none after the
	// EnsureTouchpoint refactor) that build the new touchpointDoc
	// shape externally. We honor the legacy semantics by routing
	// through pgTouchpoints.PatchPayload — set every top-level key
	// in the mutator, preserving created_at on the row.
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = s.pgTouchpoints.PatchPayload(ctx, doc.Project, doc.Number, func(p map[string]any) error {
		var fresh map[string]any
		if err := json.Unmarshal(payload, &fresh); err != nil {
			return err
		}
		for k, v := range fresh {
			p[k] = v
		}
		return nil
	})
	if errors.Is(err, pgstore.ErrTouchpointNotFound) {
		return server.ErrNotFound
	}
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
		Evidence: sliceOrEmpty(doc.Evidence),
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
			entry["evidence"] = a.Verification.Evidence
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
	rows, err := s.pgPlaybooks.List(ctx, filter.Project, filter.State, filter.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]server.PlaybookPublic, 0, len(rows))
	for _, row := range rows {
		doc, err := playbookDocFromPayload(row.Payload)
		if err != nil {
			return nil, err
		}
		out = append(out, s.playbookToPublic(ctx, doc))
	}
	return out, nil
}

func (s *Store) GetPlaybook(ctx context.Context, project, ref string) (server.PlaybookPublic, error) {
	rows, err := s.pgPlaybooks.List(ctx, project, "", nil)
	if err != nil {
		return server.PlaybookPublic{}, err
	}
	for _, row := range rows {
		doc, err := playbookDocFromPayload(row.Payload)
		if err != nil {
			return server.PlaybookPublic{}, err
		}
		if playbookPublicRef(doc) == ref {
			return s.playbookToPublic(ctx, doc), nil
		}
	}
	return server.PlaybookPublic{}, server.ErrNotFound
}

func (s *Store) CreatePlaybook(ctx context.Context, req server.PlaybookCreate) (server.PlaybookPublic, error) {
	// Verify project exists.
	if _, err := s.pgProjects.Read(ctx, req.Project); err != nil {
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
	if _, err := s.pgPlaybooks.Create(ctx, pgstore.PlaybookRow{
		Project: req.Project,
		Name:    doc.ID,
		Payload: payload,
	}); err != nil {
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
		// Resolve created_issue_ref from created_issue_id. Issues live in pg,
		// keyed by (project, number); GetByPayloadID returns the same row by
		// its payload->>'id' field.
		if e.CreatedIssueID != nil && *e.CreatedIssueID != "" {
			row, err := s.pgIssues.GetByPayloadID(ctx, doc.Project, *e.CreatedIssueID)
			if err == nil {
				issue, derr := s.issueDocFromPGRow(ctx, row)
				if derr == nil && isCanonicalIssueDoc(issue) {
					ref := publicids.IssueRef(issue.Project, &issue.Number)
					pub.CreatedIssueRef = &ref
				}
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
	doc, _, err := s.readRunDoc(ctx, project, *runID)
	if err != nil {
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
	rows, err := s.pgPortfolios.List(ctx, filter.Project, filter.Status, filter.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]server.PortfolioElementPublic, 0, len(rows))
	for _, row := range rows {
		doc, err := portfolioElementDocFromPayload(row.Payload)
		if err != nil {
			return nil, err
		}
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
	doc := portfolioElementDoc{
		ID:               portfolioElementDocID(req.Project, req.Route, req.ElementID),
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
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.PortfolioElementPublic{}, err
	}
	row, err := s.pgPortfolios.Upsert(ctx, pgstore.PortfolioRow{
		Project:   req.Project,
		Route:     req.Route,
		ElementID: req.ElementID,
		Payload:   payload,
	})
	if err != nil {
		return server.PortfolioElementPublic{}, err
	}
	finalDoc, err := portfolioElementDocFromPayload(row.Payload)
	if err != nil {
		return server.PortfolioElementPublic{}, err
	}
	runRef := s.resolveLastTouchedRunRef(ctx, finalDoc.Project, finalDoc.LastTouchedRunID)
	return portfolioElementDocToPublic(finalDoc, runRef), nil
}

func (s *Store) PatchPortfolioElement(ctx context.Context, project, ref string, req server.PortfolioElementPatch) (server.PortfolioElementPublic, error) {
	// The portfolio ref encodes (route, element_id). The server-side
	// helper that parses it lives in publicids; we re-derive here by
	// scanning project rows for a matching ref, then patch by
	// (project, route, element_id).
	rows, err := s.pgPortfolios.List(ctx, project, "", nil)
	if err != nil {
		return server.PortfolioElementPublic{}, err
	}
	var targetRoute, targetElementID string
	for _, row := range rows {
		if server.PortfolioElementRef(row.Route, row.ElementID) == ref {
			targetRoute = row.Route
			targetElementID = row.ElementID
			break
		}
	}
	if targetRoute == "" {
		return server.PortfolioElementPublic{}, server.ErrNotFound
	}

	// Resolve the run ref outside the mutator so the error path is clean.
	var newRunID *string
	if req.LastTouchedRunRef != nil {
		newRunID, err = s.resolveRunRefToID(ctx, project, req.LastTouchedRunRef)
		if err != nil {
			return server.PortfolioElementPublic{}, err
		}
	}

	row, err := s.pgPortfolios.PatchPayload(ctx, project, targetRoute, targetElementID, func(payload map[string]any) error {
		if req.Title != nil {
			payload["title"] = *req.Title
		}
		if req.ScreenshotURL != nil {
			payload["screenshot_url"] = *req.ScreenshotURL
		}
		if req.PreviewURL != nil {
			payload["preview_url"] = *req.PreviewURL
		}
		if req.Status != nil {
			payload["status"] = *req.Status
		}
		if req.Notes != nil {
			payload["notes"] = *req.Notes
		}
		if req.Metadata != nil {
			payload["metadata"] = *req.Metadata
		}
		if req.LastTouchedRunRef != nil {
			if newRunID == nil {
				delete(payload, "last_touched_run_id")
			} else {
				payload["last_touched_run_id"] = *newRunID
			}
		}
		payload["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	if errors.Is(err, pgstore.ErrPortfolioNotFound) {
		return server.PortfolioElementPublic{}, server.ErrNotFound
	}
	if err != nil {
		return server.PortfolioElementPublic{}, err
	}
	doc, err := portfolioElementDocFromPayload(row.Payload)
	if err != nil {
		return server.PortfolioElementPublic{}, err
	}
	runRef := s.resolveLastTouchedRunRef(ctx, doc.Project, doc.LastTouchedRunID)
	return portfolioElementDocToPublic(doc, runRef), nil
}

// â”€â”€ Playbook gate â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func (s *Store) PatchPlaybookEntryGate(ctx context.Context, project, ref, entryID string, manualGate bool) (server.PlaybookPublic, error) {
	// Resolve the playbook name (= doc.ID) by ref. Playbook public refs
	// are derived from doc metadata, not the row's primary key, so we
	// scan the project's playbooks to find the matching row name.
	rows, err := s.pgPlaybooks.List(ctx, project, "", nil)
	if err != nil {
		return server.PlaybookPublic{}, err
	}
	var name string
	for _, row := range rows {
		doc, err := playbookDocFromPayload(row.Payload)
		if err != nil {
			return server.PlaybookPublic{}, err
		}
		if playbookPublicRef(doc) == ref {
			name = row.Name
			break
		}
	}
	if name == "" {
		return server.PlaybookPublic{}, server.ErrNotFound
	}
	patched, err := s.pgPlaybooks.PatchPayload(ctx, project, name, func(payload map[string]any) error {
		entries, ok := payload["entries"].([]any)
		if !ok {
			return server.ErrNotFound
		}
		found := false
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, _ := entry["id"].(string)
			if id != entryID {
				continue
			}
			entry["manual_gate"] = manualGate
			md, _ := entry["metadata"].(map[string]any)
			if md == nil {
				md = map[string]any{}
				entry["metadata"] = md
			}
			md["manual_gate_updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			found = true
			break
		}
		if !found {
			return server.ErrNotFound
		}
		payload["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	if errors.Is(err, pgstore.ErrPlaybookNotFound) {
		return server.PlaybookPublic{}, server.ErrNotFound
	}
	if err != nil {
		return server.PlaybookPublic{}, err
	}
	doc, err := playbookDocFromPayload(patched.Payload)
	if err != nil {
		return server.PlaybookPublic{}, err
	}
	return s.playbookToPublic(ctx, doc), nil
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
	rows, err := s.pgPlaybooks.List(ctx, project, "", nil)
	if err != nil {
		return err
	}
	for _, row := range rows {
		doc, derr := playbookDocFromPayload(row.Payload)
		if derr != nil {
			return derr
		}
		if !playbookReferencesRun(doc, runID, runRef) {
			continue
		}
		if err := s.advancePlaybookDoc(ctx, &doc, dispatch); err != nil {
			return err
		}
		if err := s.replacePlaybookDoc(ctx, &doc); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readPlaybookDocByRef(ctx context.Context, project, ref string) (*playbookDoc, error) {
	rows, err := s.pgPlaybooks.List(ctx, project, "", nil)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		doc, derr := playbookDocFromPayload(row.Payload)
		if derr != nil {
			return nil, derr
		}
		if playbookPublicRef(doc) == ref {
			return &doc, nil
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
	_, err = s.pgPlaybooks.PatchPayload(ctx, doc.Project, doc.ID, func(payload map[string]any) error {
		var fresh map[string]any
		if err := json.Unmarshal(data, &fresh); err != nil {
			return err
		}
		// Wholesale replace: clear existing keys then copy fresh
		// values. Preserves jsonb-level atomicity inside the
		// SELECT FOR UPDATE tx that PatchPayload runs.
		for k := range payload {
			delete(payload, k)
		}
		for k, v := range fresh {
			payload[k] = v
		}
		return nil
	})
	if errors.Is(err, pgstore.ErrPlaybookNotFound) {
		return server.ErrNotFound
	}
	if err != nil {
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
	doc, _, err := s.readRunDoc(ctx, project, runID)
	if err != nil {
		return runDoc{}, false
	}
	return doc, true
}

func (s *Store) runRefByID(ctx context.Context, project, runID string) (string, error) {
	doc, _, err := s.readRunDoc(ctx, project, runID)
	if err != nil {
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
	var leaseExpires *time.Time
	if doc.TTLSeconds > 0 {
		exp := now.Add(time.Duration(doc.TTLSeconds) * time.Second)
		leaseExpires = &exp
	}
	if _, err := s.pgLeases.Create(ctx, pgstore.LeaseRow{
		ID:            leaseID,
		Project:       req.Project,
		CallbackToken: callbackToken,
		Payload:       payload,
		ExpiresAt:     leaseExpires,
	}); err != nil {
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
	// lease another slot. pg.LeasesStore.ListClaimedNative returns the
	// claimed native-k8s leases for this project; selectAvailableNativeSlot
	// then computes the first ready slot not currently held.
	pgRows, err := s.pgLeases.ListClaimedNative(ctx, project)
	if err != nil {
		return nil, err
	}
	docs := make([]leaseDoc, 0, len(pgRows))
	for _, row := range pgRows {
		doc, derr := leaseDocFromPayload(row.Payload)
		if derr != nil {
			return nil, derr
		}
		docs = append(docs, doc)
	}
	readySlots := s.nativeReadySlots(ctx, project)
	return selectAvailableNativeSlot(project, readySlots, docs, s.nativeProjectCap()), nil
}

// nativeReadySlots returns the slot indices that are currently in the
// `provisioned` state and therefore eligible to receive a new lease.
//
// Slot state lives in the pg slots table. The project metadata count still
// bounds the result so a stale slot row cannot extend capacity past the
// declared scale.
func (s *Store) nativeReadySlots(ctx context.Context, project string) []int {
	rec, err := s.pgProjects.Read(ctx, project)
	if err != nil {
		return nil
	}
	standby, ok := rec.Metadata["native_standby_dns"].(map[string]any)
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
// from nativeReadySlots so the selection contract is testable without a live
// database round trip.
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
	rec, err := s.pgProjects.Read(ctx, project)
	if err == nil {
		if standby, ok := rec.Metadata["native_standby_dns"].(map[string]any); ok {
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
	docs, err := s.listLeaseDocsForProject(ctx, project)
	if err != nil {
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

	releasedAt := time.Now().UTC().Format(time.RFC3339Nano)
	patched, err := s.pgLeases.PatchPayload(ctx, project, found.ID, func(payload map[string]any) error {
		payload["state"] = "released"
		payload["released_at"] = releasedAt
		return nil
	})
	if errors.Is(err, pgstore.ErrLeaseNotFound) {
		return server.CancelLeaseResult{}, server.ErrNotFound
	}
	if err != nil {
		return server.CancelLeaseResult{}, fmt.Errorf("release lease: %w", err)
	}
	updated, err := leaseDocFromPayload(patched.Payload)
	if err != nil {
		return server.CancelLeaseResult{}, err
	}
	lease := leaseFromDoc(updated)
	if boolValue(lease.Metadata["native_k8s"]) && !boolValue(lease.Metadata["test_slot_checkout"]) {
		_ = s.releaseNativeSlotReservation(ctx, lease, time.Now().UTC())
	}
	return server.CancelLeaseResult{
		State:    "no_active_run",
		LeaseRef: publicRef,
	}, nil
}

// ListLeasesForExpirySweep returns every lease across all projects with
// just the durable fields the expire-stale-leases sweep needs (state,
// expires_at, identity). Satisfies server.StaleLeaseStore; see
// server.ExpireStaleLeases for the contract and the lease-orphan sources
// the sweep recovers.
func (s *Store) ListLeasesForExpirySweep(ctx context.Context) ([]server.StaleLeaseExpiryRow, error) {
	if s == nil || s.pgLeases == nil {
		return nil, nil
	}
	rows, err := s.pgLeases.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	out := make([]server.StaleLeaseExpiryRow, 0, len(rows))
	for _, row := range rows {
		doc, derr := leaseDocFromPayload(row.Payload)
		if derr != nil {
			// Skip rows whose payload no longer decodes; the sweep is
			// recovery, not validation. A separate audit query is the
			// right tool for decode failures.
			continue
		}
		out = append(out, server.StaleLeaseExpiryRow{
			ID:        row.ID,
			Project:   row.Project,
			State:     doc.State,
			ExpiresAt: row.ExpiresAt,
		})
	}
	return out, nil
}

// PatchLeasePayload exposes pgLeases.PatchPayload as a server-package
// interface method for server.ExpireStaleLeases. The mutate closure runs
// inside a SELECT FOR UPDATE transaction so it can re-check the live
// state and skip overwrites when a concurrent release/cancel/callback
// has already terminalized the lease.
func (s *Store) PatchLeasePayload(ctx context.Context, project, id string, mutate func(payload map[string]any) error) error {
	if s == nil || s.pgLeases == nil {
		return fmt.Errorf("leases store not configured")
	}
	_, err := s.pgLeases.PatchPayload(ctx, project, id, mutate)
	return err
}

// listLeaseDocsForProject is a small helper used by ref-based methods
// that scan all leases in a project to find the one matching a public
// ref. Pulls the rows from pg + decodes payload into leaseDoc.
func (s *Store) listLeaseDocsForProject(ctx context.Context, project string) ([]leaseDoc, error) {
	rows, err := s.pgLeases.List(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]leaseDoc, 0, len(rows))
	for _, row := range rows {
		doc, derr := leaseDocFromPayload(row.Payload)
		if derr != nil {
			return nil, derr
		}
		out = append(out, doc)
	}
	return out, nil
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
	docs, err := s.listLeaseDocsForProject(ctx, project)
	if err != nil {
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
	var notClaimed bool
	patched, err := s.pgLeases.PatchPayload(ctx, project, docID, func(payload map[string]any) error {
		state, _ := payload["state"].(string)
		if state != "claimed" {
			notClaimed = true
			return errAbortPatch
		}
		payload["ttl_seconds"] = ttlSeconds
		return nil
	})
	if notClaimed {
		return server.Lease{}, server.ErrConflict
	}
	if errors.Is(err, pgstore.ErrLeaseNotFound) {
		return server.Lease{}, server.ErrNotFound
	}
	if err != nil {
		return server.Lease{}, fmt.Errorf("update lease TTL: %w", err)
	}
	doc, err := leaseDocFromPayload(patched.Payload)
	if err != nil {
		return server.Lease{}, err
	}
	return leaseFromDoc(doc), nil
}

func (s *Store) leaseDocIDByPublicRef(ctx context.Context, project, ref string) (string, error) {
	docs, err := s.listLeaseDocsForProject(ctx, project)
	if err != nil {
		return "", fmt.Errorf("query leases: %w", err)
	}
	found := selectLeaseDocByPublicRef(docs, ref)
	if found == nil {
		return "", server.ErrNotFound
	}
	return found.ID, nil
}

func (s *Store) AppendTestSlotHotSwapHistory(ctx context.Context, project, ref string, entry server.TestSlotHotSwapHistoryEntry) (server.Lease, error) {
	docs, err := s.listLeaseDocsForProject(ctx, project)
	if err != nil {
		return server.Lease{}, fmt.Errorf("query leases: %w", err)
	}
	found := selectLeaseDocByPublicRef(docs, ref)
	if found == nil {
		return server.Lease{}, server.ErrNotFound
	}
	entryBytes, err := json.Marshal(entry)
	if err != nil {
		return server.Lease{}, err
	}
	var entryMap map[string]any
	if err := json.Unmarshal(entryBytes, &entryMap); err != nil {
		return server.Lease{}, err
	}
	patched, err := s.pgLeases.PatchPayload(ctx, project, found.ID, func(payload map[string]any) error {
		metadata, _ := payload["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
			payload["metadata"] = metadata
		}
		history := anySliceValue(metadata["test_slot_hot_swap_history"])
		history = append(history, entryMap)
		if len(history) > 20 {
			history = history[len(history)-20:]
		}
		metadata["test_slot_hot_swap_history"] = history
		return nil
	})
	if errors.Is(err, pgstore.ErrLeaseNotFound) {
		return server.Lease{}, server.ErrNotFound
	}
	if err != nil {
		return server.Lease{}, fmt.Errorf("append hot-swap history: %w", err)
	}
	doc, err := leaseDocFromPayload(patched.Payload)
	if err != nil {
		return server.Lease{}, err
	}
	return leaseFromDoc(doc), nil
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

// nextLeaseNumber delegates to pgLeases, which seeds the counter
// from MAX(payload->>'leaseNumber')+1 on first call per-project and
// atomically increments on every subsequent call inside a tx.
func (s *Store) nextLeaseNumber(ctx context.Context, project string) (int, error) {
	return s.pgLeases.AllocateNextNumber(ctx, project)
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
	row, err := s.pgWorkflows.GetByName(ctx, project, name)
	if errors.Is(err, pgstore.ErrWorkflowNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w, err := workflowFromPayload(row.Payload)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) GetWorkflowBySchemaRef(ctx context.Context, project, schemaRef string) (*server.Workflow, error) {
	if strings.TrimSpace(schemaRef) == "" {
		return nil, nil
	}
	row, err := s.pgWorkflows.GetSchemaByRef(ctx, project, schemaRef)
	if errors.Is(err, pgstore.ErrWorkflowNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w, err := workflowFromPayload(row.Payload)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// ReadRunForReplay reads a run document and returns the minimal fields
// needed by the replay decision engine.
func (s *Store) ReadRunForReplay(ctx context.Context, project, runID string) (server.RunReplayData, error) {
	doc, _, err := s.readRunDoc(ctx, project, runID)
	if err != nil {
		return server.RunReplayData{}, err
	}
	return runReplayDataFromDoc(doc), nil
}

func (s *Store) ListQueuedRunCycles(ctx context.Context, project string, limit int) ([]server.RunReplayData, error) {
	rows, err := s.pgRuns.List(ctx, project, 0)
	if err != nil {
		return nil, err
	}
	docs := make([]runDoc, 0, len(rows))
	for _, row := range rows {
		doc, derr := runDocFromPGRow(row)
		if derr != nil {
			return nil, derr
		}
		if doc.State != "queued" || stringOrEmpty(doc.QueueState) != "queued" {
			continue
		}
		docs = append(docs, doc)
	}
	// Preserve ascending creation order.
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].CreatedAt < docs[j].CreatedAt })
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
				Status:       a.Verification.Status,
				Reasons:      a.Verification.Reasons,
				EvidenceRefs: sliceOrEmpty(a.Verification.EvidenceRefs),
				Evidence:     sliceOrEmpty(a.Verification.Evidence),
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
		ID:                   doc.ID,
		Project:              doc.Project,
		WorkflowName:         doc.Workflow,
		WorkflowSchemaRef:    doc.WorkflowSchemaRef,
		Attempts:             attempts,
		CumulativeCostUSD:    doc.CumulativeCostUSD,
		Budget:               bdg,
		IssueNumber:          doc.IssueNumber,
		RunNumber:            doc.RunNumber,
		CycleNumber:          doc.CycleNumber,
		RunCycleNumber:       doc.RunCycleNumber,
		RunDisplayNumber:     doc.RunDisplayNumber,
		IssueRepo:            doc.IssueRepo,
		ValidationURL:        doc.ValidationURL,
		ScreenshotsMarkdown:  doc.ScreenshotsMarkdown,
		EvidenceRequirements: sliceOrEmpty(doc.EvidenceRequirements),
		CallbackToken:        doc.CallbackToken,
		IssueLockHolderID:    doc.IssueLockHolderID,
		PRNumber:             doc.PRNumber,
		PRLockHolderID:       doc.PRLockHolderID,
		SlotLeaseRef:         doc.SlotLeaseRef,
		EntrypointPhase:      doc.EntrypointPhase,
		TriggerSource:        mapOrEmpty(doc.TriggerSource),
		PreserveTestEnv:      doc.PreserveTestEnv,
		State:                doc.State,
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
	if _, err := s.pgSignals.Create(ctx, pgstore.SignalRow{
		ID:         doc.ID,
		TargetRepo: doc.TargetRepo,
		Payload:    data,
	}); err != nil {
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
	state := strings.TrimSpace(filter.State)
	rows, err := s.pgSignals.List(ctx, state, "", 0)
	if err != nil {
		return nil, err
	}
	signals := make([]server.GraphSignal, 0, len(rows))
	for _, row := range rows {
		doc, err := signalDocFromPayload(row.Payload)
		if err != nil {
			return nil, err
		}
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
	rows, err := s.pgSignals.List(ctx, "pending", "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]server.QueuedSignal, 0, len(rows))
	for _, row := range rows {
		doc, err := signalDocFromPayload(row.Payload)
		if err != nil {
			return nil, err
		}
		out = append(out, queuedSignalFromDoc(doc))
	}
	return out, nil
}

func (s *Store) MarkSignalProcessing(ctx context.Context, signal server.QueuedSignal) (server.QueuedSignal, bool, error) {
	// Use a transactional read-modify-write: SELECT FOR UPDATE prevents
	// the state-pending check from racing another dispatcher claiming
	// the same signal. The mutator returns errSignalAlreadyClaimed when
	// state != pending; that becomes the "claimed=false" return.
	var alreadyClaimed bool
	patched, err := s.pgSignals.PatchPayload(ctx, signal.ID, func(payload map[string]any) error {
		state, _ := payload["state"].(string)
		if state != "pending" {
			alreadyClaimed = true
			return errSignalAlreadyClaimed
		}
		payload["state"] = "processing"
		return nil
	})
	if alreadyClaimed {
		// Re-read the current row so we can return its current state.
		row, getErr := s.pgSignals.GetByID(ctx, signal.ID)
		if getErr != nil {
			return server.QueuedSignal{}, false, getErr
		}
		doc, derr := signalDocFromPayload(row.Payload)
		if derr != nil {
			return server.QueuedSignal{}, false, derr
		}
		return queuedSignalFromDoc(doc), false, nil
	}
	if errors.Is(err, pgstore.ErrSignalNotFound) {
		return server.QueuedSignal{}, false, server.ErrNotFound
	}
	if err != nil {
		return server.QueuedSignal{}, false, err
	}
	doc, err := signalDocFromPayload(patched.Payload)
	if err != nil {
		return server.QueuedSignal{}, false, err
	}
	return queuedSignalFromDoc(doc), true, nil
}

func (s *Store) MarkSignalProcessed(ctx context.Context, signal server.QueuedSignal, decision string) (server.QueuedSignal, error) {
	patched, err := s.pgSignals.PatchPayload(ctx, signal.ID, func(payload map[string]any) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		payload["state"] = "processed"
		payload["processed_at"] = now
		payload["processed_decision"] = decision
		return nil
	})
	if errors.Is(err, pgstore.ErrSignalNotFound) {
		return server.QueuedSignal{}, server.ErrNotFound
	}
	if err != nil {
		return server.QueuedSignal{}, err
	}
	doc, err := signalDocFromPayload(patched.Payload)
	if err != nil {
		return server.QueuedSignal{}, err
	}
	return queuedSignalFromDoc(doc), nil
}

func (s *Store) MarkSignalFailed(ctx context.Context, signal server.QueuedSignal, reason string) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	_, err := s.pgSignals.PatchPayload(ctx, signal.ID, func(payload map[string]any) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		payload["state"] = "failed"
		payload["processed_at"] = now
		payload["failure_reason"] = reason
		return nil
	})
	if errors.Is(err, pgstore.ErrSignalNotFound) {
		return server.ErrNotFound
	}
	return err
}

// errSignalAlreadyClaimed is returned from the MarkSignalProcessing
// mutator when state != "pending", so the outer call can distinguish
// "claim lost" from a real database error.
var errSignalAlreadyClaimed = errors.New("signal already claimed")

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
	// pg.RunsStore.FindByPR returns the most-recently-updated row
	// across all projects via payload->>'issue_repo' + payload->>'pr_number'.
	// A single SELECT against the runs table handles the cross-project view.
	row, err := s.pgRuns.FindByPR(ctx, repo, prNumber)
	if errors.Is(err, pgstore.ErrRunNotFound) {
		return server.RunReplayData{}, server.ErrNotFound
	}
	if err != nil {
		return server.RunReplayData{}, err
	}
	return s.ReadRunForReplay(ctx, row.Project, row.ID)
}

func (s *Store) LinkRunPullRequest(ctx context.Context, project, runID string, prNumber int) error {
	if prNumber < 1 {
		return server.ValidationError{Message: "pr number must be positive"}
	}
	_, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		raw["pr_number"] = prNumber
		raw["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	if errors.Is(err, pgstore.ErrRunNotFound) {
		return server.ErrNotFound
	}
	return err
}

func (s *Store) NormalizeRunReviewFacts(ctx context.Context, project, runID string, facts server.RunReviewFacts) (server.RunReplayData, error) {
	if facts.ValidationURL == nil || strings.TrimSpace(*facts.ValidationURL) == "" {
		return s.ReadRunForReplay(ctx, project, runID)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err := s.mutateRunRaw(ctx, project, runID, func(_ runDoc, raw map[string]any) (bool, error) {
		changed := promoteRunReviewOutputRaw(raw, "validation_url", *facts.ValidationURL)
		if changed {
			raw["updated_at"] = now
		}
		return changed, nil
	})
	if err != nil {
		return server.RunReplayData{}, err
	}
	return s.ReadRunForReplay(ctx, project, runID)
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
	// Scan all runs in pg and match by payload->>'callback_token'. The
	// expected runs row count is small (active runs across all
	// projects) so a full scan is acceptable here. If this becomes a
	// hot path, add a partial index on (payload->>'callback_token')
	// with WHERE the value is non-empty.
	rows, err := s.pgRuns.ListAll(ctx)
	if err != nil {
		return "", "", "", err
	}
	var found *runDoc
	for _, row := range rows {
		doc, derr := runDocFromPGRow(row)
		if derr != nil {
			return "", "", "", derr
		}
		if stringOrEmpty(doc.CallbackToken) == token {
			found = &doc
			break
		}
	}
	if found == nil {
		return "", "", "", server.ErrNotFound
	}
	sibling, _ := s.issueRunDocs(ctx, found.Project, found.IssueNumber)
	numbers := runNumberMap(sibling)
	ref := publicids.RunRef(found.Project, positiveIssueNumberPtr(found.IssueNumber), runDisplayNumber(*found, numbers[found.ID]))
	return found.ID, found.Project, ref, nil
}

// AbortRunByID marks a run as aborted, best-effort releases issue/PR locks and
// any run slot lease.
func (s *Store) AbortRunByID(ctx context.Context, project, runID, reason string) (server.AbortRunResult, error) {
	doc, _, err := s.readRunDoc(ctx, project, runID)
	if err != nil {
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

	// Patch the run doc to aborted state inside a SELECT FOR UPDATE
	// transaction — replaces the previous ETag retry loop.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		raw["state"] = "aborted"
		raw["abort_reason"] = reason
		delete(raw, "queue_state")
		raw["updated_at"] = now
		finalizeExecutionFailureRaw(raw, canonicalExecutionFailureReason(reason), now)
		return nil
	}); err != nil {
		if errors.Is(err, pgstore.ErrRunNotFound) {
			return server.AbortRunResult{}, server.ErrNotFound
		}
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
	doc, _, err := s.readRunDoc(ctx, project, runID)
	if err != nil {
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
	doc, _, err := s.readRunDoc(ctx, project, runID)
	if err != nil {
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

	// Write the event to pg via pgRunEvents. Idempotency is based on the
	// natural event key: the same key with the same payload is accepted
	// silently; the same key with a different payload returns ErrConflict.
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
		promoteRunReviewOutputRaw(raw, key, value)
		return nil
	}
	return nil
}

func promoteRunReviewOutputsRaw(raw map[string]any, outputs map[string]string) bool {
	changed := false
	for key, value := range outputs {
		if promoteRunReviewOutputRaw(raw, key, value) {
			changed = true
		}
	}
	return changed
}

func promoteRunReviewOutputRaw(raw map[string]any, key, value string) bool {
	switch strings.TrimSpace(key) {
	case "validation_url":
		return setRunReviewStringRaw(raw, "validation_url", value)
	default:
		return false
	}
}

func setRunReviewStringRaw(raw map[string]any, key, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	current, _ := raw[key].(string)
	if strings.TrimSpace(current) == value {
		return false
	}
	raw[key] = value
	return true
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
	doc, _, err := s.readRunDoc(ctx, project, runID)
	if err != nil {
		return server.NativeRunLogsResponse{}, err
	}

	// Read events from pg. pg.RunEventsStore.List returns rows in the canonical
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

	// pg's SELECT FOR UPDATE inside PatchPayload owns serialization.
	// Early-return paths (already terminal /
	// duplicate idempotent completion) are emitted as sentinel
	// errors from the mutator and translated outside.
	var (
		completedIdempotent   bool
		duplicateCompletion   bool
		earlyExpectedJobIDs   []string
		earlyCompletions      map[string]nativeJobCompletionDoc
		writtenCompletions    map[string]nativeJobCompletionDoc
		writtenExpectedJobIDs []string
		validationErr         error
		conflictMsg           error
	)
	_, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		// Re-marshal to get a runDoc for typed access.
		bytes, mErr := json.Marshal(raw)
		if mErr != nil {
			return mErr
		}
		var doc runDoc
		if uErr := json.Unmarshal(bytes, &doc); uErr != nil {
			return uErr
		}
		attempts, _ := raw["attempts"].([]any)
		if len(doc.Attempts) == 0 || len(attempts) == 0 {
			conflictMsg = server.ErrConflict
			return errAbortPatch
		}
		idx := len(doc.Attempts) - 1
		if p.AttemptIndex != nil && *p.AttemptIndex >= 0 && *p.AttemptIndex < len(doc.Attempts) {
			idx = *p.AttemptIndex
		}
		attemptMap, ok := attempts[idx].(map[string]any)
		if !ok {
			validationErr = fmt.Errorf("invalid attempt at index %d", idx)
			return errAbortPatch
		}
		attempt := doc.Attempts[idx]
		if attempt.PhaseKind != "k8s_job" {
			conflictMsg = server.ErrConflict
			return errAbortPatch
		}
		expectedJobIDs, eErr := s.expectedNativeJobIDs(ctx, project, doc.Workflow, doc.WorkflowSchemaRef, attempt.Phase)
		if eErr != nil {
			validationErr = eErr
			return errAbortPatch
		}
		if !containsString(expectedJobIDs, jobID) {
			validationErr = server.ValidationError{Message: fmt.Sprintf("job_id %q is not registered on phase %q", jobID, attempt.Phase)}
			return errAbortPatch
		}
		completions := cloneJobCompletions(attempt.JobCompletions)
		if attempt.CompletedAt != "" {
			completedIdempotent = true
			earlyExpectedJobIDs = expectedJobIDs
			earlyCompletions = completions
			return errAbortPatch
		}
		newCompletion := nativeJobCompletionDocFromPayload(jobID, p, time.Now().UTC().Format(time.RFC3339Nano))
		if existing, exists := completions[jobID]; exists {
			if !sameNativeJobCompletion(existing, newCompletion) {
				conflictMsg = server.ErrConflict
				return errAbortPatch
			}
			duplicateCompletion = true
			earlyExpectedJobIDs = expectedJobIDs
			earlyCompletions = completions
			return errAbortPatch
		}
		if vErr := validateNativePhaseOutputKeys(jobID, newCompletion.PhaseOutputs, completions); vErr != nil {
			validationErr = vErr
			return errAbortPatch
		}
		completions[jobID] = newCompletion
		attemptMap["job_completions"] = completions
		attempts[idx] = attemptMap
		raw["attempts"] = attempts
		raw["updated_at"] = newCompletion.CompletedAt
		executionState, executionReason := nativeJobExecutionStateAndReason(newCompletion)
		markJobCompletionInExecutionsRaw(raw, attempt.Phase, jobID, executionState, executionReason, newCompletion.CompletedAt)
		writtenCompletions = completions
		writtenExpectedJobIDs = expectedJobIDs
		return nil
	})
	if conflictMsg != nil {
		return server.NativeJobCompletionResult{}, conflictMsg
	}
	if validationErr != nil {
		return server.NativeJobCompletionResult{}, validationErr
	}
	if completedIdempotent || duplicateCompletion {
		run, err := s.ReadRunForReplay(ctx, project, runID)
		if err != nil {
			return server.NativeJobCompletionResult{}, err
		}
		phaseComplete := completedIdempotent || allExpectedJobsCompleted(earlyExpectedJobIDs, earlyCompletions)
		return nativeJobCompletionResult(run, earlyExpectedJobIDs, earlyCompletions, phaseComplete, false), nil
	}
	if errors.Is(err, pgstore.ErrRunNotFound) {
		return server.NativeJobCompletionResult{}, server.ErrNotFound
	}
	if err != nil {
		return server.NativeJobCompletionResult{}, err
	}
	run, err := s.ReadRunForReplay(ctx, project, runID)
	if err != nil {
		return server.NativeJobCompletionResult{}, err
	}
	phaseComplete := allExpectedJobsCompleted(writtenExpectedJobIDs, writtenCompletions)
	return nativeJobCompletionResult(run, writtenExpectedJobIDs, writtenCompletions, phaseComplete, phaseComplete), nil
}

// errAbortPatch is returned by mutators that want to short-circuit
// pgRuns.PatchPayload without committing — the caller then inspects
// captured sentinels to decide the outer return value.
var errAbortPatch = errors.New("abort patch")

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
	if p.VerificationStatus != "" || len(p.EvidenceRefs) > 0 || len(p.Evidence) > 0 {
		verification = &verificationDoc{
			Status:       p.VerificationStatus,
			Reasons:      sliceOrEmpty(p.VerificationReasons),
			EvidenceRefs: sliceOrEmpty(p.EvidenceRefs),
			Evidence:     sliceOrEmpty(p.Evidence),
			CostUSD:      p.CostUSD,
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
	// SELECT FOR UPDATE inside PatchPayload's tx serializes concurrent
	// writers on the row lock. The mutator still receives both
	// the typed runDoc and the raw map[string]any so existing call
	// sites continue to work unchanged.
	var noChange bool
	_, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		// Re-marshal the raw map to bytes so we can unmarshal it into
		// the typed runDoc for the caller. Avoids a separate query.
		bytes, mErr := json.Marshal(raw)
		if mErr != nil {
			return mErr
		}
		var doc runDoc
		if uErr := json.Unmarshal(bytes, &doc); uErr != nil {
			return uErr
		}
		changed, mErr := mutate(doc, raw)
		if mErr != nil {
			return mErr
		}
		if !changed {
			noChange = true
			return errMutateNoChange
		}
		return nil
	})
	if noChange {
		return nil
	}
	if errors.Is(err, pgstore.ErrRunNotFound) {
		return server.ErrNotFound
	}
	return err
}

// errMutateNoChange is the sentinel used by mutateRunRaw to abort
// the patch transaction when the mutator reports "nothing changed".
// PatchPayload rolls the tx back; the outer call returns nil.
var errMutateNoChange = errors.New("run mutate: no change")

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
	case strings.Contains(reason, "forward_dispatch_failed"),
		strings.Contains(reason, "retry_dispatch_failed"),
		strings.Contains(reason, "teardown_dispatch_failed"),
		strings.Contains(reason, "cleanup_dispatch_failed"):
		return "dispatch_failed"
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
	evidenceRefs := make([]string, 0)
	evidenceArtifacts := make([]server.EvidenceArtifact, 0)
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
			evidenceRefs = appendMissingStrings(evidenceRefs, completion.Verification.EvidenceRefs...)
			evidenceRefs = appendMissingStrings(evidenceRefs, server.EvidenceRefsFromArtifacts(completion.Verification.Evidence)...)
			for _, artifact := range completion.Verification.Evidence {
				evidenceArtifacts = appendEvidenceArtifact(evidenceArtifacts, artifact)
			}
		}
	}
	payload := server.CompletionPayload{
		Conclusion:          conclusion,
		VerificationStatus:  verificationStatus,
		VerificationReasons: reasons,
		EvidenceRefs:        evidenceRefs,
		Evidence:            evidenceArtifacts,
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
	var noAttempts, invalidIdx bool
	_, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		attempts, _ := raw["attempts"].([]any)
		if len(attempts) == 0 {
			noAttempts = true
			return errAbortPatch
		}
		idx := len(attempts) - 1
		if p.AttemptIndex != nil && *p.AttemptIndex >= 0 && *p.AttemptIndex < len(attempts) {
			idx = *p.AttemptIndex
		}
		attempt, ok := attempts[idx].(map[string]any)
		if !ok {
			invalidIdx = true
			return errAbortPatch
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		attempt["completed_at"] = now
		attempt["conclusion"] = p.Conclusion
		if p.SummaryMarkdown != nil {
			attempt["summary_markdown"] = *p.SummaryMarkdown
		}
		if p.PhaseOutputs != nil {
			attempt["phase_outputs"] = p.PhaseOutputs
			promoteRunReviewOutputsRaw(raw, p.PhaseOutputs)
		}
		if p.VerificationStatus != "" || len(p.EvidenceRefs) > 0 || len(p.Evidence) > 0 {
			attempt["verification"] = map[string]any{
				"status":        p.VerificationStatus,
				"reasons":       p.VerificationReasons,
				"evidence_refs": sliceOrEmpty(p.EvidenceRefs),
				"evidence":      sliceOrEmpty(p.Evidence),
				"cost_usd":      p.CostUSD,
			}
		}
		attempt["cost_usd"] = p.CostUSD
		attempts[idx] = attempt
		raw["attempts"] = attempts
		prior, _ := raw["cumulative_cost_usd"].(float64)
		raw["cumulative_cost_usd"] = prior + p.CostUSD
		raw["updated_at"] = now
		if p.ScreenshotsMarkdown != nil && (raw["screenshots_markdown"] == nil || raw["screenshots_markdown"] == "") {
			raw["screenshots_markdown"] = *p.ScreenshotsMarkdown
		}
		return nil
	})
	if noAttempts {
		return server.RunReplayData{}, fmt.Errorf("run has no attempts")
	}
	if invalidIdx {
		return server.RunReplayData{}, fmt.Errorf("invalid attempt index")
	}
	if errors.Is(err, pgstore.ErrRunNotFound) {
		return server.RunReplayData{}, server.ErrNotFound
	}
	if err != nil {
		return server.RunReplayData{}, err
	}
	return s.ReadRunForReplay(ctx, project, runID)
}

// StampRunDecision stamps the decision string on the latest attempt of a run.
func (s *Store) StampRunDecision(ctx context.Context, project, runID, decision string) error {
	var skipNoAttempts bool
	_, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		attempts, _ := raw["attempts"].([]any)
		if len(attempts) == 0 {
			skipNoAttempts = true
			return errAbortPatch
		}
		attempt, ok := attempts[len(attempts)-1].(map[string]any)
		if !ok {
			skipNoAttempts = true
			return errAbortPatch
		}
		attempt["decision"] = decision
		attempts[len(attempts)-1] = attempt
		raw["attempts"] = attempts
		raw["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	if skipNoAttempts {
		return nil
	}
	if errors.Is(err, pgstore.ErrRunNotFound) {
		return server.ErrNotFound
	}
	return err
}

// SetRunReviewRequired puts a run into the review_required sub-state. This is
// NOT a terminal transition: the run is parked at a touchpoint_gate phase
// waiting for an approve signal. Locks stay held; the slot lease may still
// be alive (preserve_test_env). The run advances to true terminal only after
// the gate is released (approve) or the workflow is aborted.
func (s *Store) SetRunReviewRequired(ctx context.Context, project, runID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		raw["state"] = "review_required"
		delete(raw, "queue_state")
		raw["updated_at"] = now
		return nil
	}); err != nil {
		if errors.Is(err, pgstore.ErrRunNotFound) {
			return server.ErrNotFound
		}
		return err
	}
	return nil
}

// SetRunInProgress flips a run out of the review_required sub-state back to
// in_progress, used when an approve signal releases a touchpoint_gate and the
// pr_merge job is about to launch.
func (s *Store) SetRunInProgress(ctx context.Context, project, runID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		raw["state"] = "in_progress"
		raw["updated_at"] = now
		return nil
	}); err != nil {
		if errors.Is(err, pgstore.ErrRunNotFound) {
			return server.ErrNotFound
		}
		return err
	}
	return nil
}

// SetRunTerminalState sets the run's state (passed, review_required, or aborted) and
// best-effort releases issue/PR locks. Mirrors AbortRunByID but for non-abort terminal states.
func (s *Store) SetRunTerminalState(ctx context.Context, project, runID, state string, abortReason *string) (server.AbortRunResult, error) {
	doc, _, err := s.readRunDoc(ctx, project, runID)
	if err != nil {
		return server.AbortRunResult{}, err
	}

	siblings, _ := s.issueRunDocs(ctx, project, doc.IssueNumber)
	numbers := runNumberMap(siblings)
	runRef := publicids.RunRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber), runDisplayNumber(doc, numbers[doc.ID]))

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		raw["state"] = state
		delete(raw, "queue_state")
		raw["updated_at"] = now
		if abortReason != nil {
			raw["abort_reason"] = *abortReason
		}
		if state == "aborted" {
			finalizeExecutionFailureRaw(raw, canonicalExecutionFailureReason(stringOrEmpty(abortReason)), now)
		}
		return nil
	}); err != nil {
		if errors.Is(err, pgstore.ErrRunNotFound) {
			return server.AbortRunResult{}, server.ErrNotFound
		}
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
	var nextIdx int
	_, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		attempts, _ := raw["attempts"].([]any)
		nextIdx = len(attempts)
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
		return nil
	})
	if errors.Is(err, pgstore.ErrRunNotFound) {
		return 0, server.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return nextIdx, nil
}

// StampLatestAttemptSkipped marks the latest attempt on a run as completed
// with conclusion="skipped" and decision="advance" without launching any
// jobs. Used when an always-run phase with SkipWhenPreserveTestEnv is
// dispatched against a run whose preserve_test_env snapshot is true: the
// phase appears in run history with a deliberate "skipped" outcome and the
// workflow advances past it like a success. The skip is durable in the
// attempt record so projections render the dedicated "skipped" pill.
func (s *Store) StampLatestAttemptSkipped(ctx context.Context, project, runID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.pgRuns.PatchPayload(ctx, project, runID, func(raw map[string]any) error {
		attempts, _ := raw["attempts"].([]any)
		if len(attempts) == 0 {
			return fmt.Errorf("run has no attempts to stamp")
		}
		last, ok := attempts[len(attempts)-1].(map[string]any)
		if !ok {
			return fmt.Errorf("malformed attempt record")
		}
		skipped := "skipped"
		advance := "advance"
		last["conclusion"] = skipped
		last["decision"] = advance
		last["completed_at"] = now
		attempts[len(attempts)-1] = last
		raw["attempts"] = attempts
		raw["updated_at"] = now
		return nil
	})
	if errors.Is(err, pgstore.ErrRunNotFound) {
		return server.ErrNotFound
	}
	return err
}

func (s *Store) StartRunCycle(ctx context.Context, req server.StartRunCycleRequest) (int, error) {
	var nextIdx int
	var conflictReason error
	_, err := s.pgRuns.PatchPayload(ctx, req.Project, req.RunID, func(raw map[string]any) error {
		if stringValue(raw["state"]) != "queued" {
			conflictReason = server.ErrConflict
			return errAbortPatch
		}
		attempts, _ := raw["attempts"].([]any)
		for _, rawAttempt := range attempts {
			attemptMap, ok := rawAttempt.(map[string]any)
			if !ok || !boolValue(attemptMap["carry_forward"]) || stringValue(attemptMap["completed_at"]) == "" {
				conflictReason = server.ErrConflict
				return errAbortPatch
			}
		}
		nextIdx = len(attempts)
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
		return nil
	})
	if conflictReason != nil {
		return 0, conflictReason
	}
	if errors.Is(err, pgstore.ErrRunNotFound) {
		return 0, server.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return nextIdx, nil
}

// ---------------------------------------------------------------------------
// RunDispatchStore implementation
// ---------------------------------------------------------------------------

// ReadProjectGitHubRepo returns the github_repo field for a registered project.
func (s *Store) ReadProjectGitHubRepo(ctx context.Context, project string) (string, error) {
	repo, err := s.pgProjects.ReadGitHubRepo(ctx, project)
	if errors.Is(err, pgstore.ErrProjectNotFound) {
		return "", server.ErrNotFound
	}
	return repo, err
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
		ID:              doc.ID,
		Title:           doc.Title,
		Body:            doc.Body,
		Labels:          labels,
		PreserveTestEnv: doc.PreserveTestEnv,
	}, nil
}

// ListProjectWorkflows returns all workflows registered for a project.
func (s *Store) ListProjectWorkflows(ctx context.Context, project string) ([]server.Workflow, error) {
	rows, err := s.pgWorkflows.ListByProject(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]server.Workflow, 0, len(rows))
	for _, row := range rows {
		w, err := workflowFromPayload(row.Payload)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
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
		ID:                   runID,
		Project:              req.Project,
		Workflow:             req.Workflow,
		WorkflowSchemaRef:    req.WorkflowSchemaRef,
		RunNumber:            &runNumber,
		CycleNumber:          &cycleNumber,
		RunCycleNumber:       &runCycle,
		RunDisplayNumber:     &runDisplay,
		RootRunID:            &runID,
		OriginKind:           &originKind,
		IsCycle:              true,
		IssueID:              req.IssueID,
		IssueRepo:            req.IssueRepo,
		IssueNumber:          req.IssueNumber,
		State:                "queued",
		QueueState:           &queueState,
		SlotLeaseRef:         optionalNonEmptyStringPtr(req.SlotLeaseRef),
		PhaseExecutions:      phaseExecutionDocsFromWorkflow(*wf, now, nil),
		Budget:               budgetDoc,
		Attempts:             []attemptDoc{},
		CumulativeCostUSD:    0.0,
		EvidenceRequirements: sliceOrEmpty(req.EvidenceRequirements),
		TriggerSource:        req.TriggerSource,
		CallbackToken:        &callbackToken,
		IssueLockHolderID:    &req.IssueLockHolderID,
		PreserveTestEnv:      req.PreserveTestEnv,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	payload, err := json.Marshal(doc)
	if err != nil {
		return server.CreatedRun{}, err
	}
	issueNum := req.IssueNumber
	if _, err := s.pgRuns.Create(ctx, pgstore.RunRow{
		ID:          runID,
		Project:     req.Project,
		IssueNumber: &issueNum,
		Payload:     payload,
	}); err != nil {
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

func firstEvidenceRequirements(values ...[]server.EvidenceRequirement) []server.EvidenceRequirement {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func (s *Store) CreateRecycleCycle(ctx context.Context, req server.CreateRecycleCycleRequest) (server.CreatedRun, error) {
	parent, _, err := s.readRunDoc(ctx, req.Parent.Project, req.Parent.ID)
	if err != nil {
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
	// Mark parent recycled via pgRuns.PatchPayload — atomically holds
	// a row lock so a concurrent reader sees either the pre-recycle
	// or the recycled doc, not a torn write.
	if _, err := s.pgRuns.PatchPayload(ctx, parent.Project, parent.ID, func(raw map[string]any) error {
		raw["state"] = "recycled"
		delete(raw, "queue_state")
		raw["updated_at"] = now
		return nil
	}); err != nil {
		if errors.Is(err, pgstore.ErrRunNotFound) {
			return server.CreatedRun{}, server.ErrNotFound
		}
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
		ID:                   runID,
		Project:              parent.Project,
		Workflow:             parent.Workflow,
		WorkflowSchemaRef:    firstNonEmpty(req.WorkflowSchemaRef, parent.WorkflowSchemaRef),
		RunNumber:            &runNumber,
		CycleNumber:          &cycleNumber,
		RunCycleNumber:       &runCycle,
		RunDisplayNumber:     &runDisplay,
		ParentRunID:          &parent.ID,
		RootRunID:            &rootRunID,
		OriginKind:           &originKind,
		IsCycle:              true,
		IssueID:              parent.IssueID,
		IssueRepo:            parent.IssueRepo,
		IssueNumber:          parent.IssueNumber,
		PRNumber:             parent.PRNumber,
		State:                "queued",
		QueueState:           &queueState,
		SlotLeaseRef:         parent.SlotLeaseRef,
		EntrypointPhase:      optionalNonEmptyStringPtr(req.TargetPhaseName),
		PhaseExecutions:      phaseExecutionDocsFromWorkflow(*wf, now, optionalNonEmptyStringPtr(req.TargetPhaseName)),
		Budget:               parent.Budget,
		Attempts:             carryForwardAttemptDocs(req.CarryForwardAttempts, *wf, now),
		CumulativeCostUSD:    parent.CumulativeCostUSD,
		EvidenceRequirements: sliceOrEmpty(firstEvidenceRequirements(req.EvidenceRequirements, parent.EvidenceRequirements)),
		TriggerSource:        req.TriggerSource,
		CallbackToken:        &callbackToken,
		IssueLockHolderID:    parent.IssueLockHolderID,
		PRLockHolderID:       parent.PRLockHolderID,
		PreserveTestEnv:      parent.PreserveTestEnv,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.CreatedRun{}, err
	}
	issueNum := parent.IssueNumber
	if _, err := s.pgRuns.Create(ctx, pgstore.RunRow{
		ID:          runID,
		Project:     parent.Project,
		IssueNumber: &issueNum,
		Payload:     payload,
	}); err != nil {
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

package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
	"github.com/google/uuid"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/publicids"
	"github.com/nelsong6/glimmung/internal/server"
)

type Store struct {
	projects  *azcosmos.ContainerClient
	workflows *azcosmos.ContainerClient
	hosts     *azcosmos.ContainerClient
	leases    *azcosmos.ContainerClient
	runs      *azcosmos.ContainerClient
	issues    *azcosmos.ContainerClient
	locks     *azcosmos.ContainerClient
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
	runs, err := client.NewContainer(settings.CosmosDatabase, "runs")
	if err != nil {
		return nil, fmt.Errorf("create runs container client: %w", err)
	}
	issues, err := client.NewContainer(settings.CosmosDatabase, "issues")
	if err != nil {
		return nil, fmt.Errorf("create issues container client: %w", err)
	}
	locks, err := client.NewContainer(settings.CosmosDatabase, "locks")
	if err != nil {
		return nil, fmt.Errorf("create locks container client: %w", err)
	}
	return &Store{projects: projects, workflows: workflows, hosts: hosts, leases: leases, runs: runs, issues: issues, locks: locks}, nil
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
	if err := queryAll(ctx, s.workflows, &docs); err != nil {
		return nil, err
	}
	rows := make([]server.Workflow, 0, len(docs))
	for _, doc := range docs {
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
	if err := validateWorkflowForProject(projectDoc, req); err != nil {
		return server.Workflow{}, err
	}

	doc := workflowDocFromRegister(req, time.Now().UTC().Format(time.RFC3339Nano))
	pk := azcosmos.NewPartitionKeyString(req.Project)
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
	var doc map[string]any
	if err := json.Unmarshal(read.Value, &doc); err != nil {
		return server.Workflow{}, err
	}
	if req.PREnabled != nil {
		pr, _ := doc["pr"].(map[string]any)
		if pr == nil {
			pr = map[string]any{}
		}
		pr["enabled"] = *req.PREnabled
		doc["pr"] = pr
	}
	if req.BudgetTotal != nil {
		budget, _ := doc["budget"].(map[string]any)
		if budget == nil {
			budget = map[string]any{}
		}
		budget["total"] = *req.BudgetTotal
		doc["budget"] = budget
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Workflow{}, err
	}
	if _, err := s.workflows.ReplaceItem(ctx, pk, name, payload, nil); err != nil {
		return server.Workflow{}, err
	}
	return workflowFromMap(doc)
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
	if doc.Host != nil && *doc.Host != "" && *doc.Host != "native-k8s" {
		if err := s.touchHostHeartbeat(ctx, *doc.Host); err != nil {
			return server.Lease{}, err
		}
	}
	return leaseFromDoc(doc), nil
}

func (s *Store) ReleaseLeaseByCallbackToken(ctx context.Context, token string) (server.Lease, error) {
	doc, err := s.readLeaseDocByCallbackToken(ctx, token)
	if err != nil {
		return server.Lease{}, err
	}
	if doc.State == "released" || doc.State == "expired" {
		return leaseFromDoc(doc), nil
	}
	if boolValue(doc.Metadata["test_slot_checkout"]) {
		return server.Lease{}, server.ErrUnsupported
	}
	if doc.Host != nil && *doc.Host != "" && *doc.Host != "native-k8s" {
		if err := s.clearHostLease(ctx, *doc.Host, doc.ID); err != nil {
			return server.Lease{}, err
		}
	}

	doc.State = "released"
	doc.ReleasedAt = time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(doc)
	if err != nil {
		return server.Lease{}, err
	}
	partitionKey := azcosmos.NewPartitionKeyString(doc.Project)
	if _, err := s.leases.ReplaceItem(ctx, partitionKey, doc.ID, payload, nil); err != nil {
		return server.Lease{}, err
	}
	return leaseFromDoc(doc), nil
}

func (s *Store) ListProjectRuns(ctx context.Context, project string, limit int) ([]server.RunReport, error) {
	var docs []runDoc
	if err := queryAllWhere(
		ctx,
		s.runs,
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
	locks, err := s.listIssueLockDocs(ctx)
	if err != nil {
		return nil, err
	}
	runContext := issueRunContext(runDocs)
	locksByKey := map[string]lockDoc{}
	for _, lock := range locks {
		locksByKey[lock.Key] = lock
	}

	now := time.Now().UTC()
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
		if lock, ok := locksByKey[fmt.Sprintf("%s#%d", issue.Project, issue.Number)]; ok {
			expiresAt := parseOptionalTime(lock.ExpiresAt)
			row.IssueLockHeld = lock.State == "held" && expiresAt != nil && expiresAt.After(now)
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
	held, err := s.issueLockHeld(ctx, issue.Project, issue.Number)
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

func (s *Store) readIssueByNumber(ctx context.Context, project string, number int) (issueDoc, error) {
	var docs []issueDoc
	if err := queryAllWhere(
		ctx,
		s.issues,
		"SELECT * FROM c WHERE c.project = @project AND c.number = @number",
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
	if err := queryAll(ctx, s.issues, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}

func (s *Store) listRunDocs(ctx context.Context) ([]runDoc, error) {
	var docs []runDoc
	if err := queryAll(ctx, s.runs, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}

func (s *Store) listIssueLockDocs(ctx context.Context) ([]lockDoc, error) {
	var docs []lockDoc
	if err := queryAllWhere(
		ctx,
		s.locks,
		"SELECT * FROM c WHERE c.scope = @scope",
		[]azcosmos.QueryParameter{{Name: "@scope", Value: "issue"}},
		&docs,
	); err != nil {
		return nil, err
	}
	return docs, nil
}

func (s *Store) latestRunForIssue(ctx context.Context, issue issueDoc) (*runDoc, []runDoc, error) {
	var docs []runDoc
	if issue.ID != "" {
		if err := queryAllWhere(
			ctx,
			s.runs,
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

func (s *Store) issueLockHeld(ctx context.Context, project string, number int) (bool, error) {
	var docs []lockDoc
	if err := queryAllWhere(
		ctx,
		s.locks,
		"SELECT * FROM c WHERE c.scope = @scope AND c.key = @key",
		[]azcosmos.QueryParameter{
			{Name: "@scope", Value: "issue"},
			{Name: "@key", Value: fmt.Sprintf("%s#%d", project, number)},
		},
		&docs,
	); err != nil {
		return false, err
	}
	now := time.Now().UTC()
	for _, doc := range docs {
		expiresAt := parseOptionalTime(doc.ExpiresAt)
		if doc.State == "held" && expiresAt != nil && expiresAt.After(now) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) issueRunDocs(ctx context.Context, project string, issueNumber int) ([]runDoc, error) {
	var docs []runDoc
	if err := queryAllWhere(
		ctx,
		s.runs,
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
	if err := queryAllWhere(
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

func (s *Store) touchHostHeartbeat(ctx context.Context, hostName string) error {
	partitionKey := azcosmos.NewPartitionKeyString(hostName)
	response, err := s.hosts.ReadItem(ctx, partitionKey, hostName, nil)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(response.Value, &doc); err != nil {
		return err
	}
	doc["lastHeartbeat"] = time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = s.hosts.ReplaceItem(ctx, partitionKey, hostName, payload, nil)
	return err
}

func (s *Store) clearHostLease(ctx context.Context, hostName string, leaseID string) error {
	partitionKey := azcosmos.NewPartitionKeyString(hostName)
	response, err := s.hosts.ReadItem(ctx, partitionKey, hostName, nil)
	if isCosmosStatus(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(response.Value, &doc); err != nil {
		return err
	}
	if stringValue(doc["currentLeaseId"]) != leaseID {
		return nil
	}
	doc["currentLeaseId"] = nil
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = s.hosts.ReplaceItem(ctx, partitionKey, hostName, payload, nil)
	return err
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

type runDoc struct {
	ID                  string         `json:"id"`
	Project             string         `json:"project"`
	Workflow            string         `json:"workflow"`
	RunNumber           *int           `json:"run_number"`
	RunDisplayNumber    *string        `json:"run_display_number"`
	ParentRunID         *string        `json:"parent_run_id"`
	RootRunID           *string        `json:"root_run_id"`
	OriginKind          *string        `json:"origin_kind"`
	IsCycle             bool           `json:"is_cycle"`
	CycleNumber         *int           `json:"cycle_number"`
	IssueID             string         `json:"issue_id"`
	IssueRepo           string         `json:"issue_repo"`
	IssueNumber         int            `json:"issue_number"`
	State               string         `json:"state"`
	Attempts            []attemptDoc   `json:"attempts"`
	CumulativeCostUSD   float64        `json:"cumulative_cost_usd"`
	ValidationURL       *string        `json:"validation_url"`
	ScreenshotsMarkdown *string        `json:"screenshots_markdown"`
	AbortReason         *string        `json:"abort_reason"`
	ClonedFromRunID     *string        `json:"cloned_from_run_id"`
	TriggerSource       map[string]any `json:"trigger_source"`
	CreatedAt           string         `json:"created_at"`
	UpdatedAt           string         `json:"updated_at"`
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
	ID        string `json:"id"`
	Scope     string `json:"scope"`
	Key       string `json:"key"`
	State     string `json:"state"`
	ExpiresAt string `json:"expires_at"`
}

type attemptDoc struct {
	AttemptIndex     int              `json:"attempt_index"`
	Phase            string           `json:"phase"`
	PhaseKind        string           `json:"phase_kind"`
	WorkflowFilename string           `json:"workflow_filename"`
	WorkflowRunID    *int64           `json:"workflow_run_id"`
	DispatchedAt     string           `json:"dispatched_at"`
	CompletedAt      string           `json:"completed_at"`
	Conclusion       *string          `json:"conclusion"`
	Verification     *verificationDoc `json:"verification"`
	SummaryMarkdown  *string          `json:"summary_markdown"`
	CostUSD          *float64         `json:"cost_usd"`
	Decision         *string          `json:"decision"`
	LogArchiveURL    *string          `json:"log_archive_url"`
	SkippedFromRunID *string          `json:"skipped_from_run_id"`
}

type verificationDoc struct {
	Status       string   `json:"status"`
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
		if doc.RunNumber == nil || *doc.RunNumber <= 0 || used[*doc.RunNumber] {
			continue
		}
		assigned[doc.ID] = *doc.RunNumber
		used[*doc.RunNumber] = true
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
	if parentID == nil || *parentID == "" {
		parentID = doc.ClonedFromRunID
	}
	rootID := doc.RootRunID
	if (rootID == nil || *rootID == "") && parentID != nil && *parentID != "" {
		rootID = parentID
	}
	originKind := doc.OriginKind
	if (originKind == nil || *originKind == "") && parentID != nil && *parentID != "" {
		if value := stringValue(doc.TriggerSource["kind"]); value != "" {
			originKind = optionalNonEmptyStringPtr(value)
		} else {
			originKind = optionalNonEmptyStringPtr("resume")
		}
	}
	return server.RunReport{
		Ref:                 runRef + "/report",
		Project:             doc.Project,
		RunRef:              runRef,
		RunNumber:           doc.RunNumber,
		RunDisplayNumber:    optionalNonEmptyStringPtr(display),
		ParentRunRef:        refPtr(lineageByID, parentID),
		RootRunRef:          refPtr(lineageByID, rootID),
		OriginKind:          emptyStringNil(originKind),
		IsCycle:             doc.IsCycle,
		CycleNumber:         doc.CycleNumber,
		Workflow:            doc.Workflow,
		IssueRef:            optionalNonEmptyStringPtr(publicids.IssueRef(doc.Project, positiveIssueNumberPtr(doc.IssueNumber))),
		IssueRepo:           optionalNonEmptyStringPtr(doc.IssueRepo),
		IssueNumber:         positiveIssueNumberPtr(doc.IssueNumber),
		State:               firstNonEmpty(doc.State, "in_progress"),
		CurrentPhase:        currentPhase,
		AttemptsCount:       len(doc.Attempts),
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
	return server.RunReportAttempt{
		AttemptIndex:       doc.AttemptIndex,
		Phase:              doc.Phase,
		PhaseKind:          firstNonEmpty(doc.PhaseKind, "gha_dispatch"),
		WorkflowFilename:   doc.WorkflowFilename,
		WorkflowRunID:      doc.WorkflowRunID,
		DispatchedAt:       parseTimeOrNow(doc.DispatchedAt),
		CompletedAt:        parseOptionalTime(doc.CompletedAt),
		Conclusion:         emptyStringNil(doc.Conclusion),
		VerificationStatus: verificationStatus,
		EvidenceRefs:       evidenceRefs,
		SummaryMarkdown:    emptyStringNil(doc.SummaryMarkdown),
		Decision:           emptyStringNil(doc.Decision),
		CostUSD:            cost,
		LogArchiveURL:      emptyStringNil(doc.LogArchiveURL),
		SkippedFromRunRef:  refPtr(lineageByID, doc.SkippedFromRunID),
	}
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
		TriggerLabel:        req.TriggerLabel,
		DefaultRequirements: mapOrEmpty(req.DefaultRequirements),
		Metadata:            mapOrEmpty(req.Metadata),
		CreatedAt:           createdAt,
	}
}

func phaseDocFromSpec(phase server.PhaseSpec) phaseDoc {
	jobs := make([]nativeJobDoc, 0, len(phase.Jobs))
	for _, job := range phase.Jobs {
		jobs = append(jobs, nativeJobDocFromSpec(job))
	}
	return phaseDoc{
		Name:                     phase.Name,
		Kind:                     firstNonEmpty(phase.Kind, "gha_dispatch"),
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
		steps = append(steps, nativeStepDoc{Slug: step.Slug, Title: step.Title})
	}
	return nativeJobDoc{
		ID:             job.ID,
		Name:           job.Name,
		Image:          job.Image,
		Command:        sliceOrEmpty(job.Command),
		Args:           sliceOrEmpty(job.Args),
		Env:            stringMapOrEmpty(job.Env),
		Steps:          steps,
		TimeoutSeconds: job.TimeoutSeconds,
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
	if projectRequiresNativeWorkflows(project) {
		ghaPhases := make([]string, 0)
		for _, phase := range req.Phases {
			if firstNonEmpty(phase.Kind, "gha_dispatch") == "gha_dispatch" {
				ghaPhases = append(ghaPhases, phase.Name)
			}
		}
		if len(ghaPhases) > 0 {
			return server.ValidationError{
				Message: "project is marked native_webapp; workflow phases must use kind='k8s_job' (gha_dispatch phases: " + strings.Join(ghaPhases, ", ") + ")",
			}
		}
	}

	hasEntry := false
	hasTesting := false
	hasCleanup := false
	for _, phase := range req.Phases {
		if len(phase.DependsOn) == 0 {
			hasEntry = true
		}
		if phase.Verify || phase.EvidenceVerificationGate {
			hasTesting = true
		}
		if phase.Always {
			hasCleanup = true
		}
	}
	missing := make([]string, 0)
	if !hasEntry {
		missing = append(missing, "prepare")
	}
	if !hasTesting {
		missing = append(missing, "testing")
	}
	if !hasCleanup {
		missing = append(missing, "cleanup")
	}
	if len(missing) > 0 {
		return server.ValidationError{Message: "workflow " + req.Name + " is missing required phases: " + strings.Join(missing, ", ")}
	}
	return nil
}

func projectRequiresNativeWorkflows(project projectDoc) bool {
	metadata := project.Metadata
	if boolValue(metadata["native_webapp"]) || boolValue(metadata["nativeWebapp"]) {
		return true
	}
	appKind := strings.ToLower(strings.TrimSpace(stringValue(metadata["app_kind"])))
	return appKind == "native_webapp" || appKind == "native-webapp" || appKind == "native webapp"
}

func boolValue(value any) bool {
	typed, ok := value.(bool)
	return ok && typed
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

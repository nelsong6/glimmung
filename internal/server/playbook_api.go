package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

type PlaybookStore interface {
	ListPlaybooks(ctx context.Context, filter PlaybookListFilter) ([]PlaybookPublic, error)
	GetPlaybook(ctx context.Context, project, ref string) (PlaybookPublic, error)
	CreatePlaybook(ctx context.Context, req PlaybookCreate) (PlaybookPublic, error)
	PatchPlaybookEntryGate(ctx context.Context, project, ref, entryID string, manualGate bool) (PlaybookPublic, error)
}

type PlaybookRunStore interface {
	PlaybookStore
	AdvancePlaybook(ctx context.Context, project, ref string, dispatch PlaybookEntryDispatcher) (PlaybookPublic, error)
	AdvancePlaybooksForRun(ctx context.Context, project, runID string, dispatch PlaybookEntryDispatcher) error
}

type PlaybookEntryDispatcher func(ctx context.Context, entry PlaybookEntryDispatch) (PlaybookEntryDispatchResult, error)

type PlaybookListFilter struct {
	Project string
	State   string
	Limit   *int
}

type PlaybookIssueSpec struct {
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	Labels   []string       `json:"labels"`
	Workflow *string        `json:"workflow"`
	Metadata map[string]any `json:"metadata"`
}

type PlaybookEntryPublic struct {
	ID              string            `json:"id"`
	Title           *string           `json:"title"`
	Issue           PlaybookIssueSpec `json:"issue"`
	DependsOn       []string          `json:"depends_on"`
	ManualGate      bool              `json:"manual_gate"`
	State           string            `json:"state"`
	CreatedIssueRef *string           `json:"created_issue_ref"`
	RunRef          *string           `json:"run_ref"`
	CompletedAt     *string           `json:"completed_at"`
	Metadata        map[string]any    `json:"metadata"`
}

type PlaybookEntryDispatch struct {
	Project             string
	PlaybookID          string
	PlaybookRef         string
	EntryID             string
	Issue               PlaybookIssueSpec
	CreatedIssueRef     *string
	IntegrationStrategy string
	WorkContext         map[string]string
}

type PlaybookEntryDispatchResult struct {
	State           string
	Detail          *string
	CreatedIssueRef *string
	RunID           *string
	RunRef          *string
}

type PlaybookPublic struct {
	SchemaVersion       int                   `json:"schema_version"`
	Ref                 string                `json:"ref"`
	Project             string                `json:"project"`
	Title               string                `json:"title"`
	Description         string                `json:"description"`
	Entries             []PlaybookEntryPublic `json:"entries"`
	ConcurrencyLimit    *int                  `json:"concurrency_limit"`
	IntegrationStrategy string                `json:"integration_strategy"`
	State               string                `json:"state"`
	Metadata            map[string]any        `json:"metadata"`
	CreatedAt           string                `json:"created_at"`
	UpdatedAt           string                `json:"updated_at"`
}

type PlaybookEntryCreate struct {
	ID         string            `json:"id"`
	Title      *string           `json:"title"`
	Issue      PlaybookIssueSpec `json:"issue"`
	DependsOn  []string          `json:"depends_on"`
	ManualGate bool              `json:"manual_gate"`
	Metadata   map[string]any    `json:"metadata"`
}

type PlaybookCreate struct {
	Project             string                `json:"project"`
	Title               string                `json:"title"`
	Description         string                `json:"description"`
	Entries             []PlaybookEntryCreate `json:"entries"`
	ConcurrencyLimit    *int                  `json:"concurrency_limit"`
	IntegrationStrategy string                `json:"integration_strategy"`
	Metadata            map[string]any        `json:"metadata"`
}

func listPlaybooks(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pbStore, ok := store.(PlaybookStore)
		if !ok || pbStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "playbook store not configured")
			return
		}
		limit, ok := parseOptionalIssueLimit(w, r)
		if !ok {
			return
		}
		filter := PlaybookListFilter{
			Project: r.URL.Query().Get("project"),
			State:   r.URL.Query().Get("state"),
			Limit:   limit,
		}
		rows, err := pbStore.ListPlaybooks(r.Context(), filter)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "list playbooks failed")
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func getPlaybook(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pbStore, ok := store.(PlaybookStore)
		if !ok || pbStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "playbook store not configured")
			return
		}
		pb, err := pbStore.GetPlaybook(r.Context(), r.PathValue("project"), r.PathValue("playbook_ref"))
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "playbook not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "get playbook failed")
			return
		}
		writeJSON(w, http.StatusOK, pb)
	}
}

func createPlaybook(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pbStore, ok := store.(PlaybookStore)
		if !ok || pbStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "playbook store not configured")
			return
		}
		var body PlaybookCreate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if body.Project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		if strings.TrimSpace(body.Title) == "" {
			writeProblem(w, http.StatusBadRequest, "title required")
			return
		}
		// Validate unique entry IDs.
		seen := map[string]bool{}
		for _, e := range body.Entries {
			if seen[e.ID] {
				writeProblem(w, http.StatusUnprocessableEntity, "playbook entry ids must be unique")
				return
			}
			seen[e.ID] = true
		}
		// Validate depends_on references.
		for _, e := range body.Entries {
			for _, dep := range e.DependsOn {
				if !seen[dep] {
					writeProblem(w, http.StatusUnprocessableEntity, "entry "+e.ID+" depends on unknown entry "+dep)
					return
				}
			}
		}
		if body.ConcurrencyLimit != nil && *body.ConcurrencyLimit < 1 {
			writeProblem(w, http.StatusUnprocessableEntity, "concurrency_limit must be >= 1")
			return
		}
		if body.IntegrationStrategy == "rolling_main" && (body.ConcurrencyLimit == nil || *body.ConcurrencyLimit != 1) {
			writeProblem(w, http.StatusUnprocessableEntity, "rolling_main playbooks must be serial; set concurrency_limit to 1")
			return
		}
		pb, err := pbStore.CreatePlaybook(r.Context(), body)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusBadRequest, "project not registered")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "create playbook failed")
			return
		}
		writeJSON(w, http.StatusOK, pb)
	}
}

type PlaybookEntryGateRequest struct {
	ManualGate bool `json:"manual_gate"`
}

func patchPlaybookEntryGate(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pbStore, ok := store.(PlaybookStore)
		if !ok || pbStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "playbook store not configured")
			return
		}
		project := r.PathValue("project")
		playbookRef := r.PathValue("playbook_ref")
		entryID := r.PathValue("entry_id")
		var body PlaybookEntryGateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		pb, err := pbStore.PatchPlaybookEntryGate(r.Context(), project, playbookRef, entryID, body.ManualGate)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "playbook or entry not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "patch playbook entry gate failed")
			return
		}
		writeJSON(w, http.StatusOK, pb)
	}
}

func runPlaybook(store ReadStore, nativeLauncher NativeLauncher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pbStore, ok := store.(PlaybookRunStore)
		if !ok || pbStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "playbook run store not configured")
			return
		}
		dispatcher, ok := playbookEntryDispatcher(store, nativeLauncher)
		if !ok {
			writeProblem(w, http.StatusServiceUnavailable, "playbook dispatch dependencies not configured")
			return
		}
		pb, err := pbStore.AdvancePlaybook(r.Context(), r.PathValue("project"), r.PathValue("playbook_ref"), dispatcher)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "playbook not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "run playbook failed")
			return
		}
		writeJSON(w, http.StatusOK, pb)
	}
}

func playbookEntryDispatcher(store ReadStore, nativeLauncher NativeLauncher) (PlaybookEntryDispatcher, bool) {
	issueStore, ok := store.(IssueStore)
	if !ok || issueStore == nil {
		return nil, false
	}
	dispatchStore, ok := store.(RunDispatchStore)
	if !ok || dispatchStore == nil {
		return nil, false
	}
	if nativeLauncher == nil {
		return nil, false
	}
	return func(ctx context.Context, entry PlaybookEntryDispatch) (PlaybookEntryDispatchResult, error) {
		issueRef := entry.CreatedIssueRef
		var issueNumber int
		if issueRef != nil && *issueRef != "" {
			parsed, ok := issueNumberFromRef(entry.Project, *issueRef)
			if !ok {
				return PlaybookEntryDispatchResult{}, ValidationError{Message: "playbook entry has invalid created issue ref"}
			}
			issueNumber = parsed
		} else {
			issue, err := issueStore.CreateIssue(ctx, IssueCreate{
				Project:  entry.Project,
				Title:    entry.Issue.Title,
				Body:     entry.Issue.Body,
				Labels:   entry.Issue.Labels,
				Workflow: entry.Issue.Workflow,
			})
			if err != nil {
				return PlaybookEntryDispatchResult{}, err
			}
			issueRef = &issue.Ref
			if issue.Number == nil {
				return PlaybookEntryDispatchResult{}, ValidationError{Message: "created issue has no issue number"}
			}
			issueNumber = *issue.Number
		}
		workflow := ""
		if entry.Issue.Workflow != nil {
			workflow = *entry.Issue.Workflow
		}
		triggerSource := map[string]any{
			"kind":                 "playbook",
			"playbook_id":          entry.PlaybookID,
			"playbook_ref":         entry.PlaybookRef,
			"playbook_entry_id":    entry.EntryID,
			"integration_strategy": entry.IntegrationStrategy,
			"work_context":         entry.WorkContext,
		}
		result, problem := dispatchRun(ctx, dispatchStore, nativeLauncher, DispatchRunRequest{
			Project:       entry.Project,
			IssueNumber:   issueNumber,
			WorkflowName:  workflow,
			TriggerSource: triggerSource,
		})
		if problem != nil {
			return PlaybookEntryDispatchResult{
				State:           "failed",
				Detail:          &problem.message,
				CreatedIssueRef: issueRef,
			}, errors.New(problem.message)
		}
		return PlaybookEntryDispatchResult{
			State:           result.State,
			Detail:          result.Detail,
			CreatedIssueRef: issueRef,
			RunID:           result.RunID,
			RunRef:          result.RunRef,
		}, nil
	}, true
}

func issueNumberFromRef(project, ref string) (int, bool) {
	prefix := project + "#"
	if !strings.HasPrefix(ref, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(ref, prefix))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

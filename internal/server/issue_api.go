package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"
)

type IssueStore interface {
	ListIssues(ctx context.Context, filter IssueListFilter) ([]IssueRow, error)
	GetIssueDetailByNumber(ctx context.Context, project string, number int) (IssueDetail, error)
}

type IssueListFilter struct {
	Project        string
	State          string
	Workflow       string
	NeedsAttention bool
	Limit          *int
}

type IssueRow struct {
	Ref                string   `json:"ref"`
	Project            string   `json:"project"`
	Workflow           *string  `json:"workflow"`
	Repo               *string  `json:"repo"`
	Number             *int     `json:"number"`
	Title              string   `json:"title"`
	State              string   `json:"state"`
	Labels             []string `json:"labels"`
	HTMLURL            *string  `json:"html_url"`
	LastRunRef         *string  `json:"last_run_ref"`
	LastRunNumber      *int     `json:"last_run_number"`
	LastRunState       *string  `json:"last_run_state"`
	LastRunAbortReason *string  `json:"last_run_abort_reason"`
	IssueLockHeld      bool     `json:"issue_lock_held"`
}

type IssueComment struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type IssueDetail struct {
	Ref           string         `json:"ref"`
	Project       string         `json:"project"`
	Repo          *string        `json:"repo"`
	Number        *int           `json:"number"`
	Title         string         `json:"title"`
	Body          string         `json:"body"`
	State         string         `json:"state"`
	Labels        []string       `json:"labels"`
	HTMLURL       *string        `json:"html_url"`
	Comments      []IssueComment `json:"comments"`
	LastRunRef    *string        `json:"last_run_ref"`
	LastRunNumber *int           `json:"last_run_number"`
	LastRunState  *string        `json:"last_run_state"`
	IssueLockHeld bool           `json:"issue_lock_held"`
}

func listIssues(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		issueStore, ok := store.(IssueStore)
		if !ok || issueStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "issue store not configured")
			return
		}
		if r.URL.Query().Get("repo") != "" {
			writeProblem(w, http.StatusGone, "GitHub Issue repo filters are disabled; filter by project")
			return
		}
		limit, ok := parseOptionalIssueLimit(w, r)
		if !ok {
			return
		}
		filter := IssueListFilter{
			Project:        r.URL.Query().Get("project"),
			State:          firstNonEmpty(r.URL.Query().Get("state"), "open"),
			Workflow:       r.URL.Query().Get("workflow"),
			NeedsAttention: r.URL.Query().Get("needs_attention") == "true",
			Limit:          limit,
		}
		rows, err := issueStore.ListIssues(r.Context(), filter)
		var validationErr ValidationError
		switch {
		case errors.As(err, &validationErr):
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "list issues failed")
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func issueDetailByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		issueStore, ok := store.(IssueStore)
		if !ok || issueStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "issue store not configured")
			return
		}
		number, err := strconv.Atoi(r.PathValue("issue_number"))
		if err != nil || number < 1 {
			writeProblem(w, http.StatusBadRequest, "issue_number must be a positive integer")
			return
		}
		detail, err := issueStore.GetIssueDetailByNumber(r.Context(), r.PathValue("project"), number)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "issue not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "get issue detail failed")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}

func parseOptionalIssueLimit(w http.ResponseWriter, r *http.Request) (*int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return nil, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 500 {
		writeProblem(w, http.StatusBadRequest, "limit must be between 1 and 500")
		return nil, false
	}
	return &limit, true
}

package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"
)

type IssueStore interface {
	GetIssueDetailByNumber(ctx context.Context, project string, number int) (IssueDetail, error)
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

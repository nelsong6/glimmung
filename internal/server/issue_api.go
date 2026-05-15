package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type IssueStore interface {
	ListIssues(ctx context.Context, filter IssueListFilter) ([]IssueRow, error)
	GetIssueDetailByNumber(ctx context.Context, project string, number int) (IssueDetail, error)
	ArchiveIssueByNumber(ctx context.Context, req IssueArchive) (IssueDetail, error)
	CreateIssue(ctx context.Context, req IssueCreate) (IssueDetail, error)
	PatchIssueByNumber(ctx context.Context, req IssuePatch) (IssueDetail, error)
	AddIssueComment(ctx context.Context, req IssueCommentAdd) (IssueComment, error)
	UpdateIssueComment(ctx context.Context, req IssueCommentUpdate) (IssueComment, error)
	DeleteIssueComment(ctx context.Context, req IssueCommentDelete) (IssueDetail, error)
}

type IssueListFilter struct {
	Project        string
	State          string
	Workflow       string
	NeedsAttention bool
	Limit          *int
}

type IssueArchive struct {
	Project string
	Number  int
	Action  string
	Reason  string
	Author  string
}

type IssueArchiveRequest struct {
	Reason string `json:"reason"`
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

func archiveIssueByNumber(store ReadStore, action string) http.HandlerFunc {
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
		var body IssueArchiveRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		user, _ := adminUser(r.Context())
		author := firstNonEmpty(user.Email, user.Name, user.Sub, "admin")
		detail, err := issueStore.ArchiveIssueByNumber(r.Context(), IssueArchive{
			Project: r.PathValue("project"),
			Number:  number,
			Action:  action,
			Reason:  body.Reason,
			Author:  author,
		})
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "issue not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "archive issue failed")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}

// IssueCreate is the store-level request for creating a new issue.
type IssueCreate struct {
	Project  string
	Title    string
	Body     string
	Labels   []string
	Workflow *string
}

// IssuePatch is the store-level request for patching an issue.
type IssuePatch struct {
	Project string
	Number  int
	Title   *string
	Body    *string
	Labels  *[]string
	State   *string
}

// IssueCommentAdd is the store-level request for adding a comment.
type IssueCommentAdd struct {
	Project string
	Number  int
	Author  string
	Body    string
}

// IssueCommentUpdate is the store-level request for editing a comment.
type IssueCommentUpdate struct {
	Project   string
	Number    int
	CommentID string
	Author    string
	Body      string
}

// IssueCommentDelete is the store-level request for deleting a comment.
type IssueCommentDelete struct {
	Project   string
	Number    int
	CommentID string
}

// HTTP request bodies

type IssueCreateRequest struct {
	Project  string   `json:"project"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Labels   []string `json:"labels"`
	Workflow *string  `json:"workflow"`
}

type IssuePatchRequest struct {
	Title  *string   `json:"title"`
	Body   *string   `json:"body"`
	Labels *[]string `json:"labels"`
	State  *string   `json:"state"`
}

type IssueCommentRequest struct {
	Body string `json:"body"`
}

func createIssue(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		issueStore, ok := store.(IssueStore)
		if !ok || issueStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "issue store not configured")
			return
		}
		var body IssueCreateRequest
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
		detail, err := issueStore.CreateIssue(r.Context(), IssueCreate{
			Project:  body.Project,
			Title:    body.Title,
			Body:     body.Body,
			Labels:   body.Labels,
			Workflow: body.Workflow,
		})
		var validationErr ValidationError
		switch {
		case errors.As(err, &validationErr):
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "create issue failed")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}

func patchIssueByNumber(store ReadStore) http.HandlerFunc {
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
		var body IssuePatchRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		detail, err := issueStore.PatchIssueByNumber(r.Context(), IssuePatch{
			Project: r.PathValue("project"),
			Number:  number,
			Title:   body.Title,
			Body:    body.Body,
			Labels:  body.Labels,
			State:   body.State,
		})
		var validationErr ValidationError
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "issue not found")
			return
		case errors.As(err, &validationErr):
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "patch issue failed")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}

func createIssueComment(store ReadStore) http.HandlerFunc {
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
		var body IssueCommentRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		user, _ := adminUser(r.Context())
		author := firstNonEmpty(user.Email, user.Name, user.Sub, "admin")
		comment, err := issueStore.AddIssueComment(r.Context(), IssueCommentAdd{
			Project: r.PathValue("project"),
			Number:  number,
			Author:  author,
			Body:    body.Body,
		})
		var validationErr ValidationError
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "issue not found")
			return
		case errors.As(err, &validationErr):
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "add comment failed")
			return
		}
		writeJSON(w, http.StatusOK, comment)
	}
}

func updateIssueComment(store ReadStore) http.HandlerFunc {
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
		var body IssueCommentRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		user, _ := adminUser(r.Context())
		author := firstNonEmpty(user.Email, user.Name, user.Sub, "admin")
		comment, err := issueStore.UpdateIssueComment(r.Context(), IssueCommentUpdate{
			Project:   r.PathValue("project"),
			Number:    number,
			CommentID: r.PathValue("comment_id"),
			Author:    author,
			Body:      body.Body,
		})
		var validationErr ValidationError
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "comment not found")
			return
		case errors.Is(err, ErrForbidden):
			writeProblem(w, http.StatusForbidden, "cannot edit another author's comment")
			return
		case errors.As(err, &validationErr):
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "update comment failed")
			return
		}
		writeJSON(w, http.StatusOK, comment)
	}
}

func deleteIssueComment(store ReadStore) http.HandlerFunc {
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
		detail, err := issueStore.DeleteIssueComment(r.Context(), IssueCommentDelete{
			Project:   r.PathValue("project"),
			Number:    number,
			CommentID: r.PathValue("comment_id"),
		})
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "issue or comment not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "delete comment failed")
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

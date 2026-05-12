package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

type PortfolioStore interface {
	ListPortfolioElements(ctx context.Context, filter PortfolioListFilter) ([]PortfolioElementPublic, error)
	UpsertPortfolioElement(ctx context.Context, req PortfolioElementUpsert) (PortfolioElementPublic, error)
	PatchPortfolioElement(ctx context.Context, project, ref string, req PortfolioElementPatch) (PortfolioElementPublic, error)
}

type PortfolioDispatchStore interface {
	PortfolioStore
	RunDispatchStore
	CreateIssue(ctx context.Context, req IssueCreate) (IssueDetail, error)
}

type PortfolioListFilter struct {
	Project string
	Status  string
	Limit   *int
}

type PortfolioElementUpsert struct {
	Project           string         `json:"project"`
	Route             string         `json:"route"`
	ElementID         string         `json:"element_id"`
	Title             string         `json:"title"`
	ScreenshotURL     *string        `json:"screenshot_url"`
	PreviewURL        *string        `json:"preview_url"`
	Status            string         `json:"status"`
	Notes             *string        `json:"notes"`
	LastTouchedRunRef *string        `json:"last_touched_run_ref"`
	Metadata          map[string]any `json:"metadata"`
}

type PortfolioElementPatch struct {
	Title             *string         `json:"title"`
	ScreenshotURL     *string         `json:"screenshot_url"`
	PreviewURL        *string         `json:"preview_url"`
	Status            *string         `json:"status"`
	Notes             *string         `json:"notes"`
	LastTouchedRunRef *string         `json:"last_touched_run_ref"`
	Metadata          *map[string]any `json:"metadata"`
}

type PortfolioElementPublic struct {
	Ref               string         `json:"ref"`
	Project           string         `json:"project"`
	Route             string         `json:"route"`
	ElementID         string         `json:"element_id"`
	Title             string         `json:"title"`
	ScreenshotURL     *string        `json:"screenshot_url"`
	PreviewURL        *string        `json:"preview_url"`
	Status            string         `json:"status"`
	Notes             *string        `json:"notes"`
	LastTouchedRunRef *string        `json:"last_touched_run_ref"`
	Metadata          map[string]any `json:"metadata"`
	CreatedAt         string         `json:"created_at"`
	UpdatedAt         string         `json:"updated_at"`
}

type PortfolioElementsDispatchRequest struct {
	Project  string  `json:"project"`
	Status   string  `json:"status"`
	Route    *string `json:"route"`
	Limit    *int    `json:"limit"`
	Title    *string `json:"title"`
	Workflow *string `json:"workflow"`
}

var nonAlphaNum = regexp.MustCompile(`[^a-zA-Z0-9]+`)
var nonAlphaNumExt = regexp.MustCompile(`[^a-zA-Z0-9_.\-]+`)

func PortfolioElementRef(route, elementID string) string {
	routeSlug := strings.Trim(nonAlphaNum.ReplaceAllString(route, "-"), "-")
	if routeSlug == "" {
		routeSlug = "root"
	}
	elemSlug := strings.Trim(nonAlphaNumExt.ReplaceAllString(elementID, "-"), "-")
	if elemSlug == "" {
		elemSlug = "element"
	}
	return routeSlug + "--" + elemSlug
}

func listPortfolioElements(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ps, ok := store.(PortfolioStore)
		if !ok || ps == nil {
			writeProblem(w, http.StatusServiceUnavailable, "portfolio store not configured")
			return
		}
		limit, ok := parseOptionalIssueLimit(w, r)
		if !ok {
			return
		}
		filter := PortfolioListFilter{
			Project: r.URL.Query().Get("project"),
			Status:  r.URL.Query().Get("status"),
			Limit:   limit,
		}
		rows, err := ps.ListPortfolioElements(r.Context(), filter)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "list portfolio elements failed")
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func upsertPortfolioElement(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ps, ok := store.(PortfolioStore)
		if !ok || ps == nil {
			writeProblem(w, http.StatusServiceUnavailable, "portfolio store not configured")
			return
		}
		var req PortfolioElementUpsert
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if strings.TrimSpace(req.Project) == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		if strings.TrimSpace(req.Route) == "" {
			writeProblem(w, http.StatusBadRequest, "route required")
			return
		}
		if strings.TrimSpace(req.ElementID) == "" {
			writeProblem(w, http.StatusBadRequest, "element_id required")
			return
		}
		pub, err := ps.UpsertPortfolioElement(r.Context(), req)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusUnprocessableEntity, "run ref not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "upsert portfolio element failed")
			return
		}
		writeJSON(w, http.StatusOK, pub)
	}
}

func patchPortfolioElement(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ps, ok := store.(PortfolioStore)
		if !ok || ps == nil {
			writeProblem(w, http.StatusServiceUnavailable, "portfolio store not configured")
			return
		}
		project := r.PathValue("project")
		elementRef := r.PathValue("element_ref")
		var req PortfolioElementPatch
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		pub, err := ps.PatchPortfolioElement(r.Context(), project, elementRef, req)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "portfolio element not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "patch portfolio element failed")
			return
		}
		writeJSON(w, http.StatusOK, pub)
	}
}

func dispatchPortfolioElements(store ReadStore, ghDispatch GHADispatchClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dispatchStore, ok := store.(PortfolioDispatchStore)
		if !ok || dispatchStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "portfolio dispatch store not configured")
			return
		}
		var req PortfolioElementsDispatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		project := strings.TrimSpace(req.Project)
		if project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		status := firstNonEmpty(strings.TrimSpace(req.Status), "needs_review")
		if !validPortfolioReviewStatus(status) {
			writeProblem(w, http.StatusBadRequest, "status must be unreviewed, needs_review, approved, or needs_work")
			return
		}
		if req.Limit != nil && (*req.Limit < 1 || *req.Limit > 500) {
			writeProblem(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}

		rows, err := dispatchStore.ListPortfolioElements(r.Context(), PortfolioListFilter{
			Project: project,
			Status:  status,
			Limit:   req.Limit,
		})
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "list portfolio elements failed")
			return
		}
		route := ""
		if req.Route != nil {
			route = strings.TrimSpace(*req.Route)
		}
		if route != "" {
			filtered := rows[:0]
			for _, row := range rows {
				if row.Route == route {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
		}
		if len(rows) == 0 {
			routeHint := ""
			if route != "" {
				routeHint = fmt.Sprintf(" for route %q", route)
			}
			writeProblem(w, http.StatusBadRequest, fmt.Sprintf("no %s portfolio elements in %s%s", status, project, routeHint))
			return
		}

		title := strings.TrimSpace(stringPtrValue(req.Title))
		if title == "" {
			title = portfolioReviewIssueTitle(rows, status)
		}
		workflow := trimmedStringPtr(req.Workflow)
		issue, err := dispatchStore.CreateIssue(r.Context(), IssueCreate{
			Project:  project,
			Title:    title,
			Body:     portfolioReviewIssueBody(rows, status),
			Labels:   []string{"design-portfolio", status},
			Workflow: workflow,
		})
		var validationErr ValidationError
		switch {
		case errors.As(err, &validationErr):
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "create portfolio review issue failed")
			return
		}
		if issue.Number == nil || *issue.Number <= 0 {
			writeProblem(w, http.StatusInternalServerError, "created issue did not receive a project issue number")
			return
		}

		result, problem := dispatchRun(r.Context(), dispatchStore, ghDispatch, DispatchRunRequest{
			Project:     project,
			IssueNumber: *issue.Number,
			Workflow:    stringPtrValue(workflow),
			TriggerSource: map[string]any{
				"kind":          "portfolio_review",
				"status":        status,
				"route":         route,
				"element_count": len(rows),
			},
		})
		if problem != nil {
			writeProblem(w, problem.status, problem.message)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func validPortfolioReviewStatus(status string) bool {
	switch status {
	case "unreviewed", "needs_review", "approved", "needs_work":
		return true
	default:
		return false
	}
}

func portfolioReviewIssueTitle(rows []PortfolioElementPublic, status string) string {
	sample := firstNonEmpty(rows[0].Title, rows[0].ElementID)
	if len(rows) == 1 {
		return "Review portfolio element: " + sample
	}
	return fmt.Sprintf("Review %d portfolio elements marked %s", len(rows), status)
}

func portfolioReviewIssueBody(rows []PortfolioElementPublic, status string) string {
	var b strings.Builder
	b.WriteString("Portfolio review dispatch for `")
	b.WriteString(rows[0].Project)
	b.WriteString("`.\n\nSelected status: `")
	b.WriteString(status)
	b.WriteString("`\n\n")
	for _, row := range rows {
		label := firstNonEmpty(row.Title, row.ElementID)
		b.WriteString("- `")
		b.WriteString(row.Route)
		b.WriteString("` / `")
		b.WriteString(row.ElementID)
		b.WriteString("`: ")
		b.WriteString(label)
		b.WriteString("\n")
		if row.Notes != nil && *row.Notes != "" {
			b.WriteString("  Notes: ")
			b.WriteString(*row.Notes)
			b.WriteString("\n")
		}
		if row.PreviewURL != nil && *row.PreviewURL != "" {
			b.WriteString("  Preview: ")
			b.WriteString(*row.PreviewURL)
			b.WriteString("\n")
		}
		if row.ScreenshotURL != nil && *row.ScreenshotURL != "" {
			b.WriteString("  Screenshot: ")
			b.WriteString(*row.ScreenshotURL)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func trimmedStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

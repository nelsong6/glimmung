package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

type PortfolioStore interface {
	ListPortfolioElements(ctx context.Context, filter PortfolioListFilter) ([]PortfolioElementPublic, error)
	UpsertPortfolioElement(ctx context.Context, req PortfolioElementUpsert) (PortfolioElementPublic, error)
	PatchPortfolioElement(ctx context.Context, project, ref string, req PortfolioElementPatch) (PortfolioElementPublic, error)
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

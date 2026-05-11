package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakeIssueStore struct {
	fakeReadStore
	detail  IssueDetail
	rows    []IssueRow
	err     error
	project string
	number  int
	filter  IssueListFilter
	archive IssueArchive
}

func (s *fakeIssueStore) ListIssues(_ context.Context, filter IssueListFilter) ([]IssueRow, error) {
	s.filter = filter
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func (s *fakeIssueStore) GetIssueDetailByNumber(_ context.Context, project string, number int) (IssueDetail, error) {
	s.project = project
	s.number = number
	if s.err != nil {
		return IssueDetail{}, s.err
	}
	return s.detail, nil
}

func (s *fakeIssueStore) ArchiveIssueByNumber(_ context.Context, req IssueArchive) (IssueDetail, error) {
	s.archive = req
	if s.err != nil {
		return IssueDetail{}, s.err
	}
	detail := s.detail
	detail.State = "closed"
	return detail, nil
}

func TestListIssues(t *testing.T) {
	store := &fakeIssueStore{rows: []IssueRow{{
		Ref:     "glimmung#17",
		Project: "glimmung",
		Number:  intPtr(17),
		Title:   "Fix dashboard",
		State:   "open",
		Labels:  []string{"bug"},
	}}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/issues?project=glimmung&workflow=issue-agent&limit=10&needs_attention=true", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.filter.Project != "glimmung" || store.filter.Workflow != "issue-agent" || store.filter.Limit == nil || *store.filter.Limit != 10 || !store.filter.NeedsAttention {
		t.Fatalf("filter=%#v", store.filter)
	}
	if !strings.Contains(rec.Body.String(), `"ref":"glimmung#17"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestListIssuesValidatesFilters(t *testing.T) {
	handler := NewWithStore(Settings{}, &fakeIssueStore{})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/issues?repo=nelsong6/glimmung", nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/issues?limit=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestIssueDetailByNumber(t *testing.T) {
	store := &fakeIssueStore{detail: IssueDetail{
		Ref:     "glimmung#17",
		Project: "glimmung",
		Number:  intPtr(17),
		Title:   "Fix dashboard",
		Body:    "details",
		State:   "open",
		Labels:  []string{"bug"},
	}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/issues/by-number/glimmung/17", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.project != "glimmung" || store.number != 17 {
		t.Fatalf("project=%q number=%d", store.project, store.number)
	}
	if !strings.Contains(rec.Body.String(), `"ref":"glimmung#17"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestIssueDetailByNumberMapsErrors(t *testing.T) {
	handler := NewWithStore(Settings{}, &fakeIssueStore{err: ErrNotFound})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/issues/by-number/glimmung/17", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/issues/by-number/glimmung/zero", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestIssueDetailByNumberRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/issues/by-number/glimmung/17", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestArchiveIssueByNumberRequiresAdminAndAddsAuthor(t *testing.T) {
	store := &fakeIssueStore{detail: IssueDetail{
		Ref:     "glimmung#17",
		Project: "glimmung",
		Number:  intPtr(17),
		Title:   "Fix dashboard",
		State:   "open",
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/issues/by-number/glimmung/17/archive", strings.NewReader(`{"reason":"done"}`))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.archive.Project != "glimmung" || store.archive.Number != 17 || store.archive.Action != "archived" || store.archive.Reason != "done" {
		t.Fatalf("archive=%#v", store.archive)
	}
	if store.archive.Author != "admin@example.com" {
		t.Fatalf("author=%q", store.archive.Author)
	}
}

func TestDiscardIssueByNumberUsesDiscardedAction(t *testing.T) {
	store := &fakeIssueStore{detail: IssueDetail{Ref: "glimmung#17", Project: "glimmung", Number: intPtr(17), Title: "Fix", State: "open"}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/issues/by-number/glimmung/17/discard", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.archive.Action != "discarded" {
		t.Fatalf("archive=%#v", store.archive)
	}
}

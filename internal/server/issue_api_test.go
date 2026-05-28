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
	detail     IssueDetail
	rows       []IssueRow
	err        error
	project    string
	number     int
	filter     IssueListFilter
	archive    IssueArchive
	lastCreate IssueCreate
	lastPatch  IssuePatch
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

func (s *fakeIssueStore) CreateIssue(_ context.Context, req IssueCreate) (IssueDetail, error) {
	s.lastCreate = req
	if s.err != nil {
		return IssueDetail{}, s.err
	}
	return IssueDetail{Project: req.Project, Title: req.Title, Body: req.Body, State: "open", PreserveTestEnv: req.PreserveTestEnv}, nil
}

func (s *fakeIssueStore) PatchIssueByNumber(_ context.Context, req IssuePatch) (IssueDetail, error) {
	s.lastPatch = req
	if s.err != nil {
		return IssueDetail{}, s.err
	}
	detail := s.detail
	if req.Title != nil {
		detail.Title = *req.Title
	}
	if req.PreserveTestEnv != nil {
		detail.PreserveTestEnv = *req.PreserveTestEnv
	}
	return detail, nil
}

func (s *fakeIssueStore) AddIssueComment(_ context.Context, req IssueCommentAdd) (IssueComment, error) {
	if s.err != nil {
		return IssueComment{}, s.err
	}
	return IssueComment{ID: "c1", Author: req.Author, Body: req.Body}, nil
}

func (s *fakeIssueStore) UpdateIssueComment(_ context.Context, req IssueCommentUpdate) (IssueComment, error) {
	if s.err != nil {
		return IssueComment{}, s.err
	}
	return IssueComment{ID: req.CommentID, Author: req.Author, Body: req.Body}, nil
}

func (s *fakeIssueStore) DeleteIssueComment(_ context.Context, req IssueCommentDelete) (IssueDetail, error) {
	if s.err != nil {
		return IssueDetail{}, s.err
	}
	return s.detail, nil
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

func TestCreateIssueRequiresAdmin(t *testing.T) {
	handler := NewWithStore(Settings{}, &fakeIssueStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/issues", strings.NewReader(`{"project":"glimmung","title":"New issue"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateIssue(t *testing.T) {
	store := &fakeIssueStore{}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/issues", strings.NewReader(`{"project":"glimmung","title":"New issue","body":"details","labels":["bug"]}`))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"title":"New issue"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCreateIssueValidatesBody(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeIssueStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	for _, body := range []string{
		`{"title":"missing project"}`,
		`{"project":"glimmung"}`,
		`not json`,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/issues", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%q status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestPatchIssueSetsPreserveTestEnv(t *testing.T) {
	store := &fakeIssueStore{detail: IssueDetail{Ref: "glimmung#17", Project: "glimmung", Number: intPtr(17), Title: "Old title", State: "open"}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/issues/by-number/glimmung/17", strings.NewReader(`{"preserve_test_env":true}`))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.lastPatch.PreserveTestEnv == nil || *store.lastPatch.PreserveTestEnv != true {
		t.Fatalf("lastPatch.PreserveTestEnv=%v, want true ptr", store.lastPatch.PreserveTestEnv)
	}
	if !strings.Contains(rec.Body.String(), `"preserve_test_env":true`) {
		t.Fatalf("body=%s, want preserve_test_env=true in response", rec.Body.String())
	}
}

func TestPatchIssueLeavesPreserveTestEnvAlone(t *testing.T) {
	store := &fakeIssueStore{detail: IssueDetail{Ref: "glimmung#17", Project: "glimmung", Number: intPtr(17), Title: "Old", State: "open", PreserveTestEnv: true}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/issues/by-number/glimmung/17", strings.NewReader(`{"title":"Renamed"}`))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.lastPatch.PreserveTestEnv != nil {
		t.Fatalf("PATCH omitting preserve_test_env should leave it nil; got %v", *store.lastPatch.PreserveTestEnv)
	}
}

func TestCreateIssueAcceptsPreserveTestEnv(t *testing.T) {
	store := &fakeIssueStore{}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/issues", strings.NewReader(`{"project":"glimmung","title":"keep env alive","preserve_test_env":true}`))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !store.lastCreate.PreserveTestEnv {
		t.Fatalf("lastCreate.PreserveTestEnv=false, want true")
	}
	if !strings.Contains(rec.Body.String(), `"preserve_test_env":true`) {
		t.Fatalf("body=%s, want preserve_test_env=true", rec.Body.String())
	}
}

func TestPatchIssueByNumber(t *testing.T) {
	store := &fakeIssueStore{detail: IssueDetail{Ref: "glimmung#17", Project: "glimmung", Number: intPtr(17), Title: "Old title", State: "open"}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/issues/by-number/glimmung/17", strings.NewReader(`{"title":"New title"}`))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"title":"New title"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCreateIssueComment(t *testing.T) {
	store := &fakeIssueStore{}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/issues/by-number/glimmung/17/comments", strings.NewReader(`{"body":"great issue"}`))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"body":"great issue"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestUpdateIssueComment(t *testing.T) {
	store := &fakeIssueStore{}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/issues/by-number/glimmung/17/comments/c1", strings.NewReader(`{"body":"edited"}`))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"body":"edited"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestDeleteIssueComment(t *testing.T) {
	store := &fakeIssueStore{detail: IssueDetail{Ref: "glimmung#17", Project: "glimmung", Number: intPtr(17), Title: "Fix", State: "open"}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/issues/by-number/glimmung/17/comments/c1", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

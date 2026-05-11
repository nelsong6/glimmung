package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeIssueStore struct {
	fakeReadStore
	detail  IssueDetail
	err     error
	project string
	number  int
}

func (s *fakeIssueStore) GetIssueDetailByNumber(_ context.Context, project string, number int) (IssueDetail, error) {
	s.project = project
	s.number = number
	if s.err != nil {
		return IssueDetail{}, s.err
	}
	return s.detail, nil
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

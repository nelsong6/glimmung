package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeRunStore struct {
	fakeReadStore
	rows        []RunReport
	err         error
	project     string
	limit       int
	issueNumber int
	runNumber   string
}

func (s *fakeRunStore) ListProjectRuns(_ context.Context, project string, limit int) ([]RunReport, error) {
	s.project = project
	s.limit = limit
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func (s *fakeRunStore) GetRunReportByNumber(_ context.Context, project string, issueNumber int, runNumber string) (RunReport, error) {
	s.project = project
	s.issueNumber = issueNumber
	s.runNumber = runNumber
	if s.err != nil {
		return RunReport{}, s.err
	}
	if len(s.rows) == 0 {
		return RunReport{}, ErrNotFound
	}
	return s.rows[0], nil
}

func TestListProjectRuns(t *testing.T) {
	now := time.Date(2026, 5, 11, 5, 15, 0, 0, time.UTC)
	store := &fakeRunStore{rows: []RunReport{{
		Ref:               "glimmung#141/runs/1/report",
		Project:           "glimmung",
		RunRef:            "glimmung#141/runs/1",
		RunNumber:         intPtr(1),
		Workflow:          "issue-agent",
		IssueRef:          stringPtr("glimmung#141"),
		IssueRepo:         stringPtr("nelsong6/glimmung"),
		IssueNumber:       intPtr(141),
		State:             "in_progress",
		AttemptsCount:     0,
		CumulativeCostUSD: 0,
		StartedAt:         now,
		UpdatedAt:         now,
	}}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/glimmung/runs?limit=25", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.project != "glimmung" || store.limit != 25 {
		t.Fatalf("project=%q limit=%d", store.project, store.limit)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"run_ref":"glimmung#141/runs/1"`) {
		t.Fatalf("body=%s", body)
	}
}

func TestListProjectRunsDefaultsAndValidatesLimit(t *testing.T) {
	store := &fakeRunStore{}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/glimmung/runs", nil))
	if rec.Code != http.StatusOK || store.limit != 100 {
		t.Fatalf("status=%d limit=%d body=%s", rec.Code, store.limit, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/glimmung/runs?limit=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListProjectRunsRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/glimmung/runs", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetRunReportByNumber(t *testing.T) {
	now := time.Date(2026, 5, 11, 5, 30, 0, 0, time.UTC)
	store := &fakeRunStore{rows: []RunReport{{
		Ref:           "glimmung#141/runs/1/report",
		Project:       "glimmung",
		RunRef:        "glimmung#141/runs/1",
		RunNumber:     intPtr(1),
		Workflow:      "issue-agent",
		IssueRef:      stringPtr("glimmung#141"),
		IssueNumber:   intPtr(141),
		State:         "passed",
		AttemptsCount: 0,
		StartedAt:     now,
		CompletedAt:   &now,
		UpdatedAt:     now,
	}}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/glimmung/issues/141/runs/1/report", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.project != "glimmung" || store.issueNumber != 141 || store.runNumber != "1" {
		t.Fatalf("project=%q issue=%d run=%q", store.project, store.issueNumber, store.runNumber)
	}
	if !strings.Contains(rec.Body.String(), `"ref":"glimmung#141/runs/1/report"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestGetRunReportByNumberMapsNotFoundAndBadIssueNumber(t *testing.T) {
	handler := NewWithStore(Settings{}, &fakeRunStore{})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/glimmung/issues/141/runs/1/report", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/glimmung/issues/zero/runs/1/report", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakeTouchpointStore struct {
	fakeReadStore
	rows   []TouchpointRow
	detail TouchpointDetail
	err    error
}

func (s *fakeTouchpointStore) ListTouchpoints(_ context.Context, _ TouchpointListFilter) ([]TouchpointRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func (s *fakeTouchpointStore) GetTouchpointForIssue(_ context.Context, _ string, _ int) (TouchpointDetail, error) {
	if s.err != nil {
		return TouchpointDetail{}, s.err
	}
	return s.detail, nil
}

func (s *fakeTouchpointStore) EnsureTouchpoint(_ context.Context, req TouchpointCreate) (TouchpointDetail, error) {
	if s.err != nil {
		return TouchpointDetail{}, s.err
	}
	return TouchpointDetail{
		Ref:      req.Repo + "#" + itoa(req.Number),
		Project:  req.Project,
		Repo:     req.Repo,
		PRNumber: req.Number,
		Title:    req.Title,
		State:    "ready",
	}, nil
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Replace(" "+string(rune('0'+n)), " ", "", -1))
}

func TestListTouchpoints(t *testing.T) {
	store := &fakeTouchpointStore{rows: []TouchpointRow{{
		Ref:      "nelsong6/glimmung#42",
		Project:  "glimmung",
		Repo:     "nelsong6/glimmung",
		PRNumber: 42,
		Title:    "Fix dashboard",
		State:    "ready",
	}}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/touchpoints?project=glimmung", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ref":"nelsong6/glimmung#42"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestIssueTouchpointDetail(t *testing.T) {
	store := &fakeTouchpointStore{detail: TouchpointDetail{
		Ref: "nelsong6/glimmung#42", Project: "glimmung", Repo: "nelsong6/glimmung", PRNumber: 42, Title: "Fix dashboard", State: "ready",
	}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/glimmung/issues/17/touchpoint", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateTouchpoint(t *testing.T) {
	store := &fakeTouchpointStore{}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	body := `{"project":"glimmung","repo":"nelsong6/glimmung","number":42,"title":"Fix","branch":"fix-branch"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/touchpoints", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"title":"Fix"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCreateTouchpointValidates(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeTouchpointStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	cases := []struct {
		body string
		desc string
	}{
		{`{"repo":"nelsong6/glimmung","number":1,"title":"t","branch":"b"}`, "missing project"},
		{`{"project":"glimmung","number":1,"title":"t","branch":"b"}`, "missing repo"},
		{`{"project":"glimmung","repo":"nelsong6/glimmung","title":"t","branch":"b"}`, "missing number"},
		{`{"project":"glimmung","repo":"nelsong6/glimmung","number":1,"branch":"b"}`, "missing title"},
		{`{"project":"glimmung","repo":"nelsong6/glimmung","number":1,"title":"t"}`, "missing branch"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/touchpoints", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer admin")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", tc.desc, rec.Code, rec.Body.String())
		}
	}
}

func TestTouchpointRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	for _, path := range []string{"/v1/touchpoints", "/v1/projects/glimmung/issues/1/touchpoint"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("path=%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

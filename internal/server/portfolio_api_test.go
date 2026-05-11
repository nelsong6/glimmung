package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakePortfolioStore struct {
	fakeReadStore
	rows   []PortfolioElementPublic
	detail PortfolioElementPublic
	err    error
}

func (s *fakePortfolioStore) ListPortfolioElements(_ context.Context, _ PortfolioListFilter) ([]PortfolioElementPublic, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func (s *fakePortfolioStore) UpsertPortfolioElement(_ context.Context, _ PortfolioElementUpsert) (PortfolioElementPublic, error) {
	if s.err != nil {
		return PortfolioElementPublic{}, s.err
	}
	return s.detail, nil
}

func (s *fakePortfolioStore) PatchPortfolioElement(_ context.Context, _, _ string, _ PortfolioElementPatch) (PortfolioElementPublic, error) {
	if s.err != nil {
		return PortfolioElementPublic{}, s.err
	}
	return s.detail, nil
}

func TestListPortfolioElements(t *testing.T) {
	store := &fakePortfolioStore{rows: []PortfolioElementPublic{{
		Ref:       "about--hero",
		Project:   "myproject",
		Route:     "/about",
		ElementID: "hero",
		Status:    "needs_review",
	}}}
	handler := NewWithStore(Settings{}, store)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/portfolio/elements?project=myproject", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ref":"about--hero"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestUpsertPortfolioElement(t *testing.T) {
	store := &fakePortfolioStore{detail: PortfolioElementPublic{
		Ref:       "about--hero",
		Project:   "myproject",
		Route:     "/about",
		ElementID: "hero",
		Status:    "needs_review",
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"project":"myproject","route":"/about","element_id":"hero","title":"Hero","status":"needs_review"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/portfolio/elements", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ref":"about--hero"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestUpsertPortfolioElementValidates(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakePortfolioStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	cases := []struct {
		body   string
		desc   string
	}{
		{`{"route":"/about","element_id":"hero","title":"t"}`, "missing project"},
		{`{"project":"p","element_id":"hero","title":"t"}`, "missing route"},
		{`{"project":"p","route":"/about","title":"t"}`, "missing element_id"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/portfolio/elements", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer admin")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", tc.desc, rec.Code, rec.Body.String())
		}
	}
}

func TestPatchPortfolioElement(t *testing.T) {
	store := &fakePortfolioStore{detail: PortfolioElementPublic{
		Ref:    "about--hero",
		Status: "approved",
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"status":"approved"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/portfolio/elements/myproject/about--hero", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPatchPortfolioElementNotFound(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakePortfolioStore{err: ErrNotFound}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"status":"approved"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/portfolio/elements/myproject/nonexistent--el", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPortfolioRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/portfolio/elements", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPortfolioElementRef(t *testing.T) {
	cases := []struct {
		route     string
		elementID string
		want      string
	}{
		{"/about", "hero", "about--hero"},
		{"/", "main-cta", "root--main-cta"},
		{"//double//slash", "my element", "double-slash--my-element"},
	}
	for _, tc := range cases {
		got := PortfolioElementRef(tc.route, tc.elementID)
		if got != tc.want {
			t.Fatalf("PortfolioElementRef(%q,%q) = %q, want %q", tc.route, tc.elementID, got, tc.want)
		}
	}
}

func TestPatchPlaybookEntryGate(t *testing.T) {
	store := &fakePlaybookStore{detail: PlaybookPublic{
		Ref:     "my-playbook-20260101000000",
		Project: "myproject",
		Title:   "My Playbook",
		State:   "draft",
		Entries: []PlaybookEntryPublic{{ID: "step-1", ManualGate: false}},
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"manual_gate":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/playbooks/myproject/my-playbook-20260101000000/entries/step-1/gate", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

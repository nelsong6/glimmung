package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakePlaybookStore struct {
	fakeReadStore
	rows   []PlaybookPublic
	detail PlaybookPublic
	err    error
}

func (s *fakePlaybookStore) ListPlaybooks(_ context.Context, _ PlaybookListFilter) ([]PlaybookPublic, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func (s *fakePlaybookStore) GetPlaybook(_ context.Context, _, _ string) (PlaybookPublic, error) {
	if s.err != nil {
		return PlaybookPublic{}, s.err
	}
	return s.detail, nil
}

func (s *fakePlaybookStore) PatchPlaybookEntryGate(_ context.Context, _, _, _ string, _ bool) (PlaybookPublic, error) {
	if s.err != nil {
		return PlaybookPublic{}, s.err
	}
	return s.detail, nil
}

func (s *fakePlaybookStore) CreatePlaybook(_ context.Context, req PlaybookCreate) (PlaybookPublic, error) {
	if s.err != nil {
		return PlaybookPublic{}, s.err
	}
	return PlaybookPublic{
		SchemaVersion: 1,
		Ref:           "my-playbook-20260101000000",
		Project:       req.Project,
		Title:         req.Title,
		State:         "draft",
		Entries:       []PlaybookEntryPublic{},
		Metadata:      map[string]any{},
	}, nil
}

func TestListPlaybooks(t *testing.T) {
	store := &fakePlaybookStore{rows: []PlaybookPublic{{
		SchemaVersion: 1,
		Ref:           "fix-dashboard-20260101120000",
		Project:       "glimmung",
		Title:         "Fix dashboard",
		State:         "draft",
	}}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/playbooks?project=glimmung", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ref":"fix-dashboard-20260101120000"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestGetPlaybook(t *testing.T) {
	store := &fakePlaybookStore{detail: PlaybookPublic{
		SchemaVersion: 1,
		Ref:           "fix-dashboard-20260101120000",
		Project:       "glimmung",
		Title:         "Fix dashboard",
		State:         "draft",
	}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/playbooks/glimmung/fix-dashboard-20260101120000", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"title":"Fix dashboard"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestGetPlaybookNotFound(t *testing.T) {
	handler := NewWithStore(Settings{}, &fakePlaybookStore{err: ErrNotFound})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/playbooks/glimmung/nonexistent", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreatePlaybook(t *testing.T) {
	store := &fakePlaybookStore{}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	rec := httptest.NewRecorder()
	body := `{"project":"glimmung","title":"My Playbook","entries":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/playbooks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"title":"My Playbook"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCreatePlaybookValidates(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakePlaybookStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	cases := []struct {
		body   string
		status int
		desc   string
	}{
		{`{"title":"t"}`, http.StatusBadRequest, "missing project"},
		{`{"project":"p"}`, http.StatusBadRequest, "missing title"},
		{`{"project":"p","title":"t","entries":[{"id":"a","issue":{"title":"x"}},{"id":"a","issue":{"title":"y"}}]}`, http.StatusUnprocessableEntity, "duplicate entry IDs"},
		{`{"project":"p","title":"t","entries":[{"id":"a","issue":{"title":"x"},"depends_on":["missing"]}]}`, http.StatusUnprocessableEntity, "unknown dep"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/playbooks", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer admin")
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.desc, rec.Code, rec.Body.String())
		}
	}
}

func TestPlaybookRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	for _, path := range []string{"/v1/playbooks", "/v1/playbooks/glimmung/foo"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("path=%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

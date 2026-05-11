package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeWorkflowWriteStore struct {
	fakeReadStore
	workflow Workflow
	project  string
	name     string
	patchReq WorkflowPatchRequest
	err      error
}

func (s *fakeWorkflowWriteStore) DeleteWorkflow(_ context.Context, project string, name string) (Workflow, error) {
	s.project = project
	s.name = name
	if s.err != nil {
		return Workflow{}, s.err
	}
	return s.workflow, nil
}

func (s *fakeWorkflowWriteStore) PatchWorkflow(_ context.Context, project string, name string, req WorkflowPatchRequest) (Workflow, error) {
	s.project = project
	s.name = name
	s.patchReq = req
	if s.err != nil {
		return Workflow{}, s.err
	}
	return s.workflow, nil
}

func TestPatchWorkflowRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowWriteStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(`{"pr_enabled":true}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestPatchWorkflowPatchesAndReturnsWorkflow(t *testing.T) {
	store := &fakeWorkflowWriteStore{workflow: Workflow{
		ID:        "agent-run",
		Project:   "ambience",
		Name:      "agent-run",
		CreatedAt: time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC),
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(`{"pr_enabled":true,"budget_total":50}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.project != "ambience" || store.name != "agent-run" {
		t.Fatalf("project=%q name=%q", store.project, store.name)
	}
	if store.patchReq.PREnabled == nil || *store.patchReq.PREnabled != true {
		t.Fatalf("pr_enabled=%v", store.patchReq.PREnabled)
	}
	if store.patchReq.BudgetTotal == nil || *store.patchReq.BudgetTotal != 50 {
		t.Fatalf("budget_total=%v", store.patchReq.BudgetTotal)
	}
}

func TestPatchWorkflowMapsMissingTo404(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: ErrNotFound},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/missing", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestPatchWorkflowStoreErrorsReturn500(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: errors.New("boom")},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestDeleteWorkflowRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowWriteStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/agent-run", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestDeleteWorkflowDeletesAndReturnsWorkflow(t *testing.T) {
	store := &fakeWorkflowWriteStore{workflow: Workflow{
		ID:        "agent-run",
		Project:   "ambience",
		Name:      "agent-run",
		CreatedAt: time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC),
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/agent-run", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.project != "ambience" || store.name != "agent-run" {
		t.Fatalf("project=%q name=%q", store.project, store.name)
	}
	if !strings.Contains(rec.Body.String(), `"name":"agent-run"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestDeleteWorkflowMapsMissingTo404(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: ErrNotFound},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/missing", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestDeleteWorkflowStoreErrorsReturn500(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: errors.New("boom")},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/agent-run", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakeProjectScalerStore struct {
	fakeReadStore
	project      Project
	name         string
	count        int
	slotStatuses []TestEnvironmentSlotStatus
	status       *NativeAuthRedirectStatus
	wiStatus     *NativeWorkloadIdentityStatus
	statusErr    error
	err          error
}

func (s *fakeProjectScalerStore) SetProjectTestEnvironmentCount(_ context.Context, project string, count int) (Project, error) {
	s.name = project
	s.count = count
	if s.err != nil {
		return Project{}, s.err
	}
	if s.project.Metadata == nil {
		s.project.Metadata = map[string]any{}
	}
	standby, _ := s.project.Metadata["native_standby_dns"].(map[string]any)
	if standby == nil {
		standby = map[string]any{}
	}
	standby["count"] = count
	standby["slots"] = pruneFakeTestSlots(standby["slots"], count)
	s.project.Metadata["native_standby_dns"] = standby
	if workloadIdentity, ok := s.project.Metadata["native_standby_workload_identity"].(map[string]any); ok {
		workloadIdentity["count"] = count
		s.project.Metadata["native_standby_workload_identity"] = workloadIdentity
	}
	return s.project, nil
}

func (s *fakeProjectScalerStore) SetProjectTestEnvironmentSlotStatus(_ context.Context, project string, status TestEnvironmentSlotStatus) (Project, error) {
	s.name = project
	s.slotStatuses = append(s.slotStatuses, status)
	if s.project.Metadata == nil {
		s.project.Metadata = map[string]any{}
	}
	standby, _ := s.project.Metadata["native_standby_dns"].(map[string]any)
	if standby == nil {
		standby = map[string]any{}
	}
	slots, _ := standby["slots"].([]any)
	replaced := false
	for i, raw := range slots {
		slot, _ := raw.(map[string]any)
		if slot == nil {
			continue
		}
		if index, ok := positiveIntFromMap(slot, "slot_index"); ok && index == status.SlotIndex {
			slots[i] = testSlotStatusMap(status)
			replaced = true
		}
	}
	if !replaced {
		slots = append(slots, testSlotStatusMap(status))
	}
	standby["slots"] = slots
	s.project.Metadata["native_standby_dns"] = standby
	return s.project, nil
}

func testSlotStatusMap(status TestEnvironmentSlotStatus) map[string]any {
	slot := map[string]any{
		"slot_index": float64(status.SlotIndex),
		"slot_name":  status.SlotName,
		"state":      status.State,
	}
	if !status.UpdatedAt.IsZero() {
		slot["updated_at"] = status.UpdatedAt.Format(time.RFC3339Nano)
	}
	if status.Detail != nil {
		slot["detail"] = *status.Detail
	}
	if status.ReadyAt != nil {
		slot["ready_at"] = status.ReadyAt.Format(time.RFC3339Nano)
	}
	if status.ActivationAttempt != nil {
		slot["activation_attempt"] = float64(*status.ActivationAttempt)
	}
	if status.ActivationState != nil {
		slot["activation_state"] = *status.ActivationState
	}
	if status.ActivationStartedAt != nil {
		slot["activation_started_at"] = status.ActivationStartedAt.Format(time.RFC3339Nano)
	}
	if status.ActivationCompletedAt != nil {
		slot["activation_completed_at"] = status.ActivationCompletedAt.Format(time.RFC3339Nano)
	}
	if status.ActivationJobName != nil {
		slot["activation_job_name"] = *status.ActivationJobName
	}
	if status.ActivationError != nil {
		slot["activation_error"] = *status.ActivationError
	}
	if status.CleanupState != nil {
		slot["cleanup_state"] = *status.CleanupState
	}
	if status.CleanupStartedAt != nil {
		slot["cleanup_started_at"] = status.CleanupStartedAt.Format(time.RFC3339Nano)
	}
	if status.CleanupCompletedAt != nil {
		slot["cleanup_completed_at"] = status.CleanupCompletedAt.Format(time.RFC3339Nano)
	}
	if status.CleanupError != nil {
		slot["cleanup_error"] = *status.CleanupError
	}
	return slot
}

func pruneFakeTestSlots(raw any, count int) []any {
	slots, _ := raw.([]any)
	pruned := make([]any, 0, len(slots))
	for _, rawSlot := range slots {
		slot, _ := rawSlot.(map[string]any)
		if slot == nil {
			continue
		}
		index, ok := positiveIntFromMap(slot, "slot_index")
		if !ok {
			index, ok = positiveIntFromMap(slot, "slotIndex")
		}
		if ok && index <= count {
			pruned = append(pruned, slot)
		}
	}
	return pruned
}

func (s *fakeProjectScalerStore) SetProjectNativeAuthRedirectStatus(_ context.Context, project string, status NativeAuthRedirectStatus) (Project, error) {
	if s.statusErr != nil {
		return Project{}, s.statusErr
	}
	s.status = &status
	s.project.Metadata["native_auth_redirects_status"] = status
	return s.project, nil
}

func (s *fakeProjectScalerStore) SetProjectNativeWorkloadIdentityStatus(_ context.Context, project string, status NativeWorkloadIdentityStatus) (Project, error) {
	if s.statusErr != nil {
		return Project{}, s.statusErr
	}
	s.wiStatus = &status
	s.project.Metadata["native_standby_workload_identity_status"] = status
	return s.project, nil
}

type fakeNativeAuthRedirectReconciler struct {
	status NativeAuthRedirectStatus
	err    error
}

func (r fakeNativeAuthRedirectReconciler) ReconcileNativeAuthRedirects(context.Context, Project) (NativeAuthRedirectStatus, error) {
	return r.status, r.err
}

type fakeNativeWorkloadIdentityReconciler struct {
	status NativeWorkloadIdentityStatus
	err    error
}

func (r fakeNativeWorkloadIdentityReconciler) ReconcileNativeWorkloadIdentities(context.Context, Project) (NativeWorkloadIdentityStatus, error) {
	return r.status, r.err
}

func TestScaleProjectTestEnvironmentsRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeProjectScalerStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/projects/ambience/test-environments/count", strings.NewReader(`{"count":2}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestScaleProjectTestEnvironmentsUpdatesCount(t *testing.T) {
	created := time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC)
	store := &fakeProjectScalerStore{project: Project{
		ID:         "ambience",
		Name:       "ambience",
		GitHubRepo: "nelsong6/ambience",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{"count": float64(3)},
		},
		CreatedAt: created,
	}}
	handler := NewWithDependencies(
		Settings{},
		store,
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	var project Project
	patchJSON(t, handler, "/v1/projects/ambience/test-environments/count", `{"count":3}`, &project)

	if store.name != "ambience" || store.count != 3 {
		t.Fatalf("name=%q count=%d", store.name, store.count)
	}
	if project.Metadata["native_standby_dns"] == nil {
		t.Fatalf("metadata=%#v", project.Metadata)
	}
}

func TestScaleProjectTestEnvironmentsPersistsAuthRedirectStatus(t *testing.T) {
	store := &fakeProjectScalerStore{project: Project{
		ID:         "tank",
		Name:       "tank",
		GitHubRepo: "nelsong6/tank-operator",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{"count": float64(4)},
		},
	}}
	handler := newHandler(
		Settings{},
		store,
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
		nil,
		fakeNativeAuthRedirectReconciler{status: NativeAuthRedirectStatus{
			State:               NativeAuthRedirectStatusOK,
			DesiredCount:        4,
			ManagedRedirectURIs: []string{"https://tank-slot-1.tank.dev.romaine.life/"},
		}},
		nil,
	)

	var project Project
	patchJSON(t, handler, "/v1/projects/tank/test-environments/count", `{"count":4}`, &project)

	if store.status == nil || store.status.State != NativeAuthRedirectStatusOK {
		t.Fatalf("status=%#v", store.status)
	}
	if project.Metadata["native_auth_redirects_status"] == nil {
		t.Fatalf("metadata=%#v", project.Metadata)
	}
}

func TestScaleProjectTestEnvironmentsPersistsWorkloadIdentityStatus(t *testing.T) {
	store := &fakeProjectScalerStore{project: Project{
		ID:         "tank",
		Name:       "tank",
		GitHubRepo: "nelsong6/tank-operator",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{"count": float64(4)},
			"native_standby_workload_identity": map[string]any{
				"enabled": true,
				"count":   float64(4),
			},
		},
	}}
	handler := newHandlerWithReconcilers(
		Settings{},
		store,
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
		nil,
		nil,
		fakeNativeWorkloadIdentityReconciler{status: NativeWorkloadIdentityStatus{
			State:        NativeWorkloadIdentityStatusOK,
			DesiredCount: 6,
			ManagedCredentials: []NativeWorkloadIdentityCredentialStatus{{
				IdentityName:   "tank-session-identity",
				CredentialName: "tank-slot-1-session",
				Subject:        "system:serviceaccount:tank-slot-1-sessions:tank-slot-1-session",
			}},
		}},
		nil,
	)

	var project Project
	patchJSON(t, handler, "/v1/projects/tank/test-environments/count", `{"count":6}`, &project)

	if store.wiStatus == nil || store.wiStatus.State != NativeWorkloadIdentityStatusOK {
		t.Fatalf("status=%#v", store.wiStatus)
	}
	standbyWI := project.Metadata["native_standby_workload_identity"].(map[string]any)
	if count, ok := positiveIntFromMap(standbyWI, "count"); !ok || count != 6 {
		t.Fatalf("workload identity count=%#v", standbyWI["count"])
	}
	if project.Metadata["native_standby_workload_identity_status"] == nil {
		t.Fatalf("metadata=%#v", project.Metadata)
	}
}

func TestScaleProjectTestEnvironmentsPreparesSlotsBeforeReady(t *testing.T) {
	store := &fakeProjectScalerStore{project: Project{
		ID:         "tank",
		Name:       "tank",
		GitHubRepo: "nelsong6/tank-operator",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{
				"count":       float64(2),
				"slot_prefix": "tank-slot",
			},
		},
	}}
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(
		Settings{},
		store,
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
		nil,
		nil,
		preparer,
	)

	var project Project
	patchJSON(t, handler, "/v1/projects/tank/test-environments/count", `{"count":2}`, &project)

	if !preparer.preliminaries {
		t.Fatal("expected scale to prepare test slot preliminaries")
	}
	if preparer.activated {
		t.Fatal("scale should not activate test slot runtime")
	}
	if len(store.slotStatuses) != 4 {
		t.Fatalf("statuses=%#v", store.slotStatuses)
	}
	if store.slotStatuses[0].State != "warming" || store.slotStatuses[1].State != "ready" {
		t.Fatalf("slot 1 statuses=%#v", store.slotStatuses[:2])
	}
	if store.slotStatuses[2].SlotIndex != 2 || store.slotStatuses[3].State != "ready" {
		t.Fatalf("slot 2 statuses=%#v", store.slotStatuses[2:])
	}
	standby := project.Metadata["native_standby_dns"].(map[string]any)
	slots := standby["slots"].([]any)
	if len(slots) != 2 {
		t.Fatalf("slots=%#v", slots)
	}
}

func TestScaleProjectTestEnvironmentsDeprovisionsRemovedSlots(t *testing.T) {
	project := Project{
		ID:         "tank",
		Name:       "tank",
		GitHubRepo: "nelsong6/tank-operator",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{
				"count":       float64(3),
				"slot_prefix": "tank-slot",
				"slots": []any{
					map[string]any{"slot_index": float64(1), "slot_name": "tank-slot-1", "state": "ready"},
					map[string]any{"slot_index": float64(2), "slot_name": "tank-slot-2", "state": "ready"},
					map[string]any{"slot_index": float64(3), "slot_name": "tank-slot-3", "state": "error"},
				},
			},
		},
	}
	store := &fakeProjectScalerStore{
		fakeReadStore: fakeReadStore{projects: []Project{project}},
		project:       project,
	}
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(
		Settings{},
		store,
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
		nil,
		nil,
		preparer,
	)

	var updated Project
	patchJSON(t, handler, "/v1/projects/tank/test-environments/count", `{"count":1}`, &updated)

	if got, want := strings.Join(preparer.deprovisioned, ","), "tank-slot-2,tank-slot-3"; got != want {
		t.Fatalf("deprovisioned=%q, want %q", got, want)
	}
	if preparer.preliminaries || preparer.activated {
		t.Fatal("scale down should not warm or activate removed slots")
	}
	standby := updated.Metadata["native_standby_dns"].(map[string]any)
	slots := standby["slots"].([]any)
	if len(slots) != 1 {
		t.Fatalf("slots=%#v", slots)
	}
}

func TestScaleProjectTestEnvironmentsValidatesCount(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeProjectScalerStore{},
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/projects/ambience/test-environments/count", strings.NewReader(`{"count":51}`)))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", rec.Code)
	}
}

func TestScaleProjectTestEnvironmentsMapsNotFound(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeProjectScalerStore{err: ErrNotFound},
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/projects/missing/test-environments/count", strings.NewReader(`{"count":1}`)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestScaleProjectTestEnvironmentsStoreErrorsReturn500(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeProjectScalerStore{err: errors.New("boom")},
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/projects/ambience/test-environments/count", strings.NewReader(`{"count":1}`)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func patchJSON(t *testing.T, handler http.Handler, path string, body string, target any) {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

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

// fakeProjectScalerStore is the project-scale-handler test double.
//
// Embeds fakeLeaseStore so the scaler tests inherit a working SlotStore
// + SlotHistoryStore implementation — production code that calls
// `slotStoreFromReadStore(store).ListSlotsByProject(ctx, project)` etc.
// sees the same per-row slot data the test seeds. This is the post-PR-518
// shape; the legacy `SetProjectTestEnvironmentSlotStatus` write surface
// and its embedded-array maintenance helpers were deleted with the
// surrounding production code per docs/migration-policy.md.
type fakeProjectScalerStore struct {
	fakeLeaseStore
	project   Project
	name      string
	count     int
	wiStatus  *NativeWorkloadIdentityStatus
	statusErr error
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
	s.project.Metadata["native_standby_dns"] = standby
	if workloadIdentity, ok := s.project.Metadata["native_standby_workload_identity"].(map[string]any); ok {
		workloadIdentity["count"] = count
		s.project.Metadata["native_standby_workload_identity"] = workloadIdentity
	}
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
		fakeNativeWorkloadIdentityReconciler{status: NativeWorkloadIdentityStatus{
			State:        NativeWorkloadIdentityStatusOK,
			DesiredCount: 6,
			ManagedCredentials: []NativeWorkloadIdentityCredentialStatus{{
				IdentityName:   "tank-session-identity",
				CredentialName: "tank-slot-1-session",
				Subject:        "system:serviceaccount:tank-slot-1-sessions:tank-slot-1-session",
			}},
		}},
		nil, // managed-origins reconciler not under test here
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

func TestScaleProjectTestEnvironmentsDoesNotWarmSynchronously(t *testing.T) {
	// Warming is durable reconciler work. PATCH count must return without
	// running EnsureTestSlotPreliminaries — otherwise a crash mid-warm leaves
	// the project doc permanently inconsistent and the handler's success
	// signal is a lie about what's stored.
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
		preparer,
	)

	var project Project
	patchJSON(t, handler, "/v1/projects/tank/test-environments/count", `{"count":2}`, &project)

	if preparer.preliminaries {
		t.Fatal("PATCH count must not run preliminary reconciliation; that is the reconciler's job")
	}
	if preparer.activated {
		t.Fatal("scale should not activate test slot runtime")
	}
	if len(store.slotStatuses) != 0 {
		t.Fatalf("PATCH count must not write slot statuses: %#v", store.slotStatuses)
	}
	standby := project.Metadata["native_standby_dns"].(map[string]any)
	if count, ok := positiveIntFromMap(standby, "count"); !ok || count != 2 {
		t.Fatalf("count=%v, want 2", standby["count"])
	}
	// Slots themselves moved to their own Cosmos container post PR #518;
	// PATCH no longer keeps ANY embedded slot data on the project doc.
	// The boot migration deletes the legacy array; this assertion
	// confirms the write-path does not resurrect it. The check walks
	// the metadata keys rather than indexing the retired field name
	// directly so it doesn't trip the migration guard.
	for key := range standby {
		if key == "slots" {
			t.Fatalf("PATCH count must not write a slots field on project metadata: %#v", standby)
		}
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
		fakeLeaseStore: fakeLeaseStore{fakeReadStore: fakeReadStore{projects: []Project{project}}},
		project:       project,
	}
	seedSlotsFromLegacyMetadata(t, store, store, "tank")
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(
		Settings{},
		store,
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
		nil,
		preparer,
	)

	var updated Project
	patchJSON(t, handler, "/v1/projects/tank/test-environments/count", `{"count":1}`, &updated)

	if got, want := strings.Join(preparer.deprovisioned, ","), "tank-slot-2,tank-slot-3"; got != want {
		t.Fatalf("deprovisioned=%q, want %q", got, want)
	}
	if got, want := strings.Join(preparer.deprovisionedSessions, ","), "tank-slot-2-sessions,tank-slot-3-sessions"; got != want {
		t.Fatalf("deprovisioned sessions=%q, want %q", got, want)
	}
	if preparer.preliminaries || preparer.activated {
		t.Fatal("scale down should not warm or activate removed slots")
	}
	// Post-PR-518 the slot rows live in the SlotStore, not in
	// `native_standby_dns.slots[]`. Assert via the SlotStore.
	remaining, err := store.ListSlotsByProject(context.Background(), "tank")
	if err != nil {
		t.Fatalf("ListSlotsByProject: %v", err)
	}
	if len(remaining) != 1 || remaining[0].SlotIndex != 1 {
		t.Fatalf("remaining slots=%#v, want 1 row with index 1", remaining)
	}
}

func TestScaleProjectTestEnvironmentsRejectsRemovingActiveSlot(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
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
					map[string]any{"slot_index": float64(3), "slot_name": "tank-slot-3", "state": SlotStateRunning},
				},
			},
		},
	}
	store := &fakeProjectScalerStore{
		fakeLeaseStore: fakeLeaseStore{
			fakeReadStore: fakeReadStore{projects: []Project{project}},
			leases: []Lease{{
				Project:     "tank",
				LeaseNumber: intPtr(3),
				State:       "claimed",
				Metadata: map[string]any{
					"test_slot_checkout": true,
					"native_slot_index":  "3",
					"native_slot_name":   "tank-slot-3",
				},
				RequestedAt: now,
			}},
		},
		project: project,
	}
	seedSlotsFromLegacyMetadata(t, store, store, "tank")
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(
		Settings{},
		store,
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
		nil,
		preparer,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/projects/tank/test-environments/count", strings.NewReader(`{"count":2}`))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(preparer.deprovisioned) > 0 {
		t.Fatalf("deprovisioned=%#v, want none", preparer.deprovisioned)
	}
	if store.count != 0 {
		t.Fatalf("count=%d, want unchanged", store.count)
	}
}

func TestScaleProjectTestEnvironmentsRequiresLeaseVisibilityWhenRemovingSlots(t *testing.T) {
	project := Project{
		ID:         "tank",
		Name:       "tank",
		GitHubRepo: "nelsong6/tank-operator",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{
				"count": float64(2),
				"slots": []any{
					map[string]any{"slot_index": float64(1), "slot_name": "tank-slot-1", "state": "ready"},
					map[string]any{"slot_index": float64(2), "slot_name": "tank-slot-2", "state": "ready"},
				},
			},
		},
	}
	store := &fakeProjectScalerStore{
		fakeLeaseStore: fakeLeaseStore{
			fakeReadStore: fakeReadStore{projects: []Project{project}},
			leaseErr:      errors.New("cosmos unavailable"),
		},
		project: project,
	}
	seedSlotsFromLegacyMetadata(t, store, store, "tank")
	handler := newHandler(
		Settings{},
		store,
		fakeAdminAuthenticator{user: auth.User{Sub: "admin"}},
		nil,
		&fakeTestSlotPreparer{},
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/projects/tank/test-environments/count", strings.NewReader(`{"count":1}`))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.count != 0 {
		t.Fatalf("count=%d, want unchanged", store.count)
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
		&fakeProjectScalerStore{fakeLeaseStore: fakeLeaseStore{err: ErrNotFound}},
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
		&fakeProjectScalerStore{fakeLeaseStore: fakeLeaseStore{err: errors.New("boom")}},
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

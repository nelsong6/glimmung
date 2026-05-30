package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

func TestRepairProjectTestEnvironmentRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeLeaseStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/projects/tank/test-environments/tank-slot-1/repair", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestRepairProjectTestEnvironmentRepairsProvisionedSlot(t *testing.T) {
	project := repairTestProject()
	store := &fakeLeaseStore{fakeReadStore: fakeReadStore{projects: []Project{project}}, leases: []Lease{}}
	seedProvisionedRepairSlot(t, store, "tank", 2, "tank-slot-2")
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	var body TestEnvironmentRepairResponse
	postRepairJSON(t, handler, "/v1/projects/tank/test-environments/tank-slot-2/repair", &body)

	if !preparer.repaired {
		t.Fatal("repair endpoint did not run preliminary repair")
	}
	if got := strings.Join(preparer.repairedSlots, ","); got != "tank-slot-2" {
		t.Fatalf("repaired slots=%q", got)
	}
	if preparer.activated {
		t.Fatal("repair endpoint must not activate runtime")
	}
	slot, err := store.GetSlot(context.Background(), "tank", 2)
	if err != nil {
		t.Fatalf("GetSlot: %v", err)
	}
	if slot.State != SlotStateProvisioned {
		t.Fatalf("slot state=%q, want provisioned", slot.State)
	}
	if slot.ProvisionedAt == nil {
		t.Fatalf("slot ProvisionedAt missing after repair: %#v", slot)
	}
	if body.Project != "tank" || body.SlotIndex != 2 || body.SlotName != "tank-slot-2" || body.State != SlotStateProvisioned {
		t.Fatalf("response=%#v", body)
	}
}

func TestRepairProjectTestEnvironmentRetriesPreliminaryError(t *testing.T) {
	project := repairTestProject()
	store := &fakeLeaseStore{fakeReadStore: fakeReadStore{projects: []Project{project}}, leases: []Lease{}}
	slot := NewUnseededSlot("tank", 1, "tank-slot-1", fixedTime)
	slot.State = SlotStateError
	slot.Detail = strPtr("previous warm failure")
	if _, err := store.CreateSlot(context.Background(), slot); err != nil {
		t.Fatalf("CreateSlot: %v", err)
	}
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	var body TestEnvironmentRepairResponse
	postRepairJSON(t, handler, "/v1/projects/tank/test-environments/tank-slot-1/repair", &body)

	if !preparer.repaired {
		t.Fatal("repair endpoint did not retry preliminary error")
	}
	repaired, err := store.GetSlot(context.Background(), "tank", 1)
	if err != nil {
		t.Fatalf("GetSlot: %v", err)
	}
	if repaired.State != SlotStateProvisioned || repaired.Detail != nil {
		t.Fatalf("repaired slot=%#v, want clean provisioned", repaired)
	}
}

func TestRepairProjectTestEnvironmentRejectsActiveLease(t *testing.T) {
	project := repairTestProject()
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{project}},
		leases: []Lease{{
			Project:     "tank",
			LeaseNumber: intPtr(8),
			State:       "claimed",
			Metadata: map[string]any{
				"test_slot_checkout": true,
				"native_slot_index":  "2",
				"native_slot_name":   "tank-slot-2",
			},
		}},
	}
	seedProvisionedRepairSlot(t, store, "tank", 2, "tank-slot-2")
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/tank/test-environments/tank-slot-2/repair", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if preparer.repaired {
		t.Fatal("repair should not run for an active leased slot")
	}
}

func TestRepairProjectTestEnvironmentClearsOrphanedReservation(t *testing.T) {
	project := repairTestProject()
	// A lease that once held the slot, terminalized to expired by the
	// stale-lease startup sweep but whose slot reservation was never
	// released — the orphan that strands the slot in provisioned forever.
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{project}},
		leases: []Lease{{
			Project:     "tank",
			LeaseNumber: intPtr(33),
			State:       "expired",
			Metadata: map[string]any{
				"native_k8s":        true,
				"native_slot_index": "2",
				"native_slot_name":  "tank-slot-2",
			},
		}},
	}
	seedProvisionedRepairSlotWithRef(t, store, "tank", 2, "tank-slot-2", "tank#33")
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	var body TestEnvironmentRepairResponse
	postRepairJSON(t, handler, "/v1/projects/tank/test-environments/tank-slot-2/repair", &body)

	if !preparer.repaired {
		t.Fatal("repair should run for a slot whose only lease is terminal (orphaned reservation)")
	}
	slot, err := store.GetSlot(context.Background(), "tank", 2)
	if err != nil {
		t.Fatalf("GetSlot: %v", err)
	}
	if slot.State != SlotStateProvisioned {
		t.Fatalf("slot state=%q, want provisioned", slot.State)
	}
	if slot.ActiveLeaseRef != nil {
		t.Fatalf("orphaned active_lease_ref not cleared: %q", *slot.ActiveLeaseRef)
	}
	if body.State != SlotStateProvisioned {
		t.Fatalf("response state=%q, want provisioned", body.State)
	}
}

func TestRepairProjectTestEnvironmentRejectsActiveRunLease(t *testing.T) {
	project := repairTestProject()
	// A live native *run* lease (env-prep and later phases), not a checkout
	// lease. Repair must refuse: the slot is genuinely in use, and clearing
	// its reservation would yank the slot out from under an in-progress run.
	store := &fakeLeaseStore{
		fakeReadStore: fakeReadStore{projects: []Project{project}},
		leases: []Lease{{
			Project:     "tank",
			LeaseNumber: intPtr(39),
			State:       "claimed",
			Metadata: map[string]any{
				"native_k8s":        true,
				"native_slot_index": "2",
				"native_slot_name":  "tank-slot-2",
			},
		}},
	}
	seedProvisionedRepairSlotWithRef(t, store, "tank", 2, "tank-slot-2", "tank#39")
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/tank/test-environments/tank-slot-2/repair", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if preparer.repaired {
		t.Fatal("repair must not run while a live run lease holds the slot")
	}
}

func TestRepairProjectTestEnvironmentRejectsSlotOutsideConfiguredCount(t *testing.T) {
	project := repairTestProject()
	store := &fakeLeaseStore{fakeReadStore: fakeReadStore{projects: []Project{project}}, leases: []Lease{}}
	preparer := &fakeTestSlotPreparer{}
	handler := newHandler(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, preparer)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/tank/test-environments/tank-slot-4/repair", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	if preparer.repaired {
		t.Fatal("repair should not run for a slot outside configured count")
	}
}

func repairTestProject() Project {
	return Project{
		ID:   "tank",
		Name: "tank",
		Metadata: map[string]any{"native_standby_dns": map[string]any{
			"count":       float64(3),
			"slot_prefix": "tank-slot",
			"record_base": "tank.dev.romaine.life",
		}},
	}
}

func seedProvisionedRepairSlot(t *testing.T, store *fakeLeaseStore, project string, slotIndex int, slotName string) {
	t.Helper()
	slot := NewUnseededSlot(project, slotIndex, slotName, fixedTime)
	slot.State = SlotStateProvisioned
	provisionedAt := fixedTime
	slot.ProvisionedAt = &provisionedAt
	if _, err := store.CreateSlot(context.Background(), slot); err != nil {
		t.Fatalf("CreateSlot: %v", err)
	}
}

func seedProvisionedRepairSlotWithRef(t *testing.T, store *fakeLeaseStore, project string, slotIndex int, slotName, leaseRef string) {
	t.Helper()
	slot := NewUnseededSlot(project, slotIndex, slotName, fixedTime)
	slot.State = SlotStateProvisioned
	provisionedAt := fixedTime
	slot.ProvisionedAt = &provisionedAt
	ref := leaseRef
	slot.ActiveLeaseRef = &ref
	if _, err := store.CreateSlot(context.Background(), slot); err != nil {
		t.Fatalf("CreateSlot: %v", err)
	}
}

func postRepairJSON(t *testing.T, handler http.Handler, path string, target any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

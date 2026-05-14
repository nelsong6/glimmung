package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakeLeaseStore struct {
	fakeReadStore
	lease        Lease
	leases       []Lease
	result       CancelLeaseResult
	leaseReq     LeaseAcquireRequest
	slotStatuses []TestEnvironmentSlotStatus
	cancelledRef string
	err          error
}

func (s *fakeLeaseStore) AcquireLease(_ context.Context, req LeaseAcquireRequest) (Lease, error) {
	s.leaseReq = req
	if s.err != nil {
		return Lease{}, s.err
	}
	return s.lease, nil
}

func (s *fakeLeaseStore) CancelLeaseByRef(_ context.Context, _, ref string) (CancelLeaseResult, error) {
	s.cancelledRef = ref
	if s.err != nil {
		return CancelLeaseResult{}, s.err
	}
	return s.result, nil
}

func (s *fakeLeaseStore) ListLeases(context.Context) ([]Lease, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.leases != nil {
		return s.leases, nil
	}
	return []Lease{s.lease}, nil
}

func (s *fakeLeaseStore) SetProjectTestEnvironmentSlotStatus(_ context.Context, project string, status TestEnvironmentSlotStatus) (Project, error) {
	s.slotStatuses = append(s.slotStatuses, status)
	for i := range s.projects {
		if s.projects[i].Name != project && s.projects[i].ID != project {
			continue
		}
		if s.projects[i].Metadata == nil {
			s.projects[i].Metadata = map[string]any{}
		}
		standby, _ := s.projects[i].Metadata["native_standby_dns"].(map[string]any)
		if standby == nil {
			standby = map[string]any{}
		}
		slots, _ := standby["slots"].([]any)
		replaced := false
		for j, raw := range slots {
			slot, _ := raw.(map[string]any)
			if slot == nil {
				continue
			}
			if index, ok := positiveIntFromMap(slot, "slot_index"); ok && index == status.SlotIndex {
				slots[j] = testSlotStatusMap(status)
				replaced = true
			}
		}
		if !replaced {
			slots = append(slots, testSlotStatusMap(status))
		}
		standby["slots"] = slots
		s.projects[i].Metadata["native_standby_dns"] = standby
		return s.projects[i], nil
	}
	return Project{}, ErrNotFound
}

func TestCreateLeaseRouteRetired(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeLeaseStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/lease", strings.NewReader(`{"project":"myproject"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCancelLeaseByRef(t *testing.T) {
	store := &fakeLeaseStore{
		result: CancelLeaseResult{
			State:    "cancelled",
			LeaseRef: "myproject/leases/3",
		},
	}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"project":"myproject","lease_ref":"myproject/leases/3"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/leases/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state":"cancelled"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCancelLeaseByRefNotFound(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeLeaseStore{err: ErrNotFound}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})
	body := `{"project":"myproject","lease_ref":"myproject/leases/99"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/leases/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCancelLeaseByRefRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/leases/cancel", strings.NewReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

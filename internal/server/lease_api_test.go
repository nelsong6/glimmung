package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
	// mu guards slotStatuses and the project mutations performed by
	// SetProjectTestEnvironmentSlotStatus. The reconciler now warms multiple
	// slots concurrently, so several goroutines may write through this fake at
	// the same time. Without the mutex appends race and the test sees a short
	// count or corrupted slice header.
	mu sync.Mutex
	// etag tracks the optimistic-concurrency cursor for the project doc.
	// Bumped on every write; ReadProject captures the current value;
	// SetProjectTestEnvironmentSlotStatusIfMatch returns ErrPreconditionFailed
	// when the caller's etag is stale. Lets multi-replica cleanup-claim
	// races be exercised in unit tests without a real Cosmos.
	etag int
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
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeSlotStatusLocked(project, status, "")
}

// SetProjectTestEnvironmentSlotStatusIfMatch implements
// ProjectTestEnvironmentSlotStatusClaimer for the fake. When `ifMatchEtag`
// is non-empty and disagrees with the store's current etag, returns
// ErrPreconditionFailed without mutating state — simulating the Cosmos
// 412 path that makes the multi-replica cleanup-claim race safe.
func (s *fakeLeaseStore) SetProjectTestEnvironmentSlotStatusIfMatch(_ context.Context, project string, status TestEnvironmentSlotStatus, ifMatchEtag string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeSlotStatusLocked(project, status, ifMatchEtag)
}

func (s *fakeLeaseStore) writeSlotStatusLocked(project string, status TestEnvironmentSlotStatus, ifMatchEtag string) (Project, error) {
	if ifMatchEtag != "" && ifMatchEtag != s.currentEtagLocked() {
		return Project{}, ErrPreconditionFailed
	}
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
		s.etag++
		return s.projects[i].WithETag(s.currentEtagLocked()), nil
	}
	return Project{}, ErrNotFound
}

// ReadProject implements ProjectReader for the fake. Returns the project
// with the current etag attached so callers can attempt etag-conditional
// writes.
func (s *fakeLeaseStore) ReadProject(_ context.Context, project string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.projects {
		if p.Name == project || p.ID == project {
			return p.WithETag(s.currentEtagLocked()), nil
		}
	}
	return Project{}, ErrNotFound
}

func (s *fakeLeaseStore) currentEtagLocked() string {
	return "etag-" + strconv.Itoa(s.etag)
}

// snapshotSlotStatuses returns a stable copy of the recorded status writes
// under the same lock that protects them. Tests that read slotStatuses while
// concurrent warmup goroutines are still appending must use this helper.
func (s *fakeLeaseStore) snapshotSlotStatuses() []TestEnvironmentSlotStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TestEnvironmentSlotStatus, len(s.slotStatuses))
	copy(out, s.slotStatuses)
	return out
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

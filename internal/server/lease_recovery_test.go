package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakeStaleLeaseStore struct {
	rows          []StaleLeaseExpiryRow
	docs          map[string]map[string]any
	patches       int
	listErr       error
	releasedSlots []string
	releaseErr    error
}

func newFakeStaleLeaseStore() *fakeStaleLeaseStore {
	return &fakeStaleLeaseStore{docs: map[string]map[string]any{}}
}

func (s *fakeStaleLeaseStore) seed(row StaleLeaseExpiryRow) {
	key := row.Project + "|" + row.ID
	doc := map[string]any{"state": row.State}
	s.docs[key] = doc
	s.rows = append(s.rows, row)
}

func (s *fakeStaleLeaseStore) ListLeasesForExpirySweep(_ context.Context) ([]StaleLeaseExpiryRow, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]StaleLeaseExpiryRow, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

func (s *fakeStaleLeaseStore) PatchLeasePayload(_ context.Context, project, id string, mutate func(payload map[string]any) error) error {
	key := project + "|" + id
	doc, ok := s.docs[key]
	if !ok {
		return errors.New("lease not found")
	}
	// Round-trip through JSON to mirror the real store's payload shape
	// (jsonb-decoded map[string]any with json.Number-like scalars).
	raw, _ := json.Marshal(doc)
	var working map[string]any
	if err := json.Unmarshal(raw, &working); err != nil {
		return err
	}
	if err := mutate(working); err != nil {
		return err
	}
	s.docs[key] = working
	s.patches++
	return nil
}

func (s *fakeStaleLeaseStore) ReleaseExpiredNativeSlotReservation(_ context.Context, project, id string) error {
	if s.releaseErr != nil {
		return s.releaseErr
	}
	s.releasedSlots = append(s.releasedSlots, project+"|"+id)
	return nil
}

func TestExpireStaleLeases_TransitionsExpiredActiveAndClaimed(t *testing.T) {
	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	past := now.Add(-7 * 24 * time.Hour)
	future := now.Add(time.Hour)

	store := newFakeStaleLeaseStore()
	// Active, expired → expired.
	store.seed(StaleLeaseExpiryRow{ID: "a1", Project: "ambience", State: "active", ExpiresAt: &past})
	// Claimed, expired → expired.
	store.seed(StaleLeaseExpiryRow{ID: "c1", Project: "ambience", State: "claimed", ExpiresAt: &past})
	// Released, expired → left alone (already terminal).
	store.seed(StaleLeaseExpiryRow{ID: "r1", Project: "ambience", State: "released", ExpiresAt: &past})
	// Expired, expired → left alone (already terminal).
	store.seed(StaleLeaseExpiryRow{ID: "e1", Project: "ambience", State: "expired", ExpiresAt: &past})
	// Active, future deadline → left alone (lease still legitimate).
	store.seed(StaleLeaseExpiryRow{ID: "a2", Project: "tank-operator", State: "active", ExpiresAt: &future})
	// Claimed, no deadline → left alone (open-ended legitimate claim).
	store.seed(StaleLeaseExpiryRow{ID: "c2", Project: "tank-operator", State: "claimed", ExpiresAt: nil})

	count, err := ExpireStaleLeases(context.Background(), store, now, nil)
	if err != nil {
		t.Fatalf("ExpireStaleLeases err=%v", err)
	}
	if count != 2 {
		t.Fatalf("expired count=%d, want 2", count)
	}

	// Reservation release is attempted exactly for the leases the sweep
	// terminalized — never for rows it left alone (already-terminal,
	// future-deadline, or no-deadline).
	gotReleased := map[string]bool{}
	for _, key := range store.releasedSlots {
		gotReleased[key] = true
	}
	wantReleased := map[string]bool{"ambience|a1": true, "ambience|c1": true}
	if len(store.releasedSlots) != len(wantReleased) {
		t.Fatalf("released slots=%v, want keys %v", store.releasedSlots, wantReleased)
	}
	for key := range wantReleased {
		if !gotReleased[key] {
			t.Errorf("expected reservation release for %s, got %v", key, store.releasedSlots)
		}
	}

	wantExpired := map[string]bool{"ambience|a1": true, "ambience|c1": true}
	for key, doc := range store.docs {
		state, _ := doc["state"].(string)
		switch key {
		case "ambience|a1", "ambience|c1":
			if state != "expired" {
				t.Errorf("%s state=%s, want expired", key, state)
			}
			if reason, _ := doc["expiry_reason"].(string); reason != "stale_at_startup" {
				t.Errorf("%s expiry_reason=%s, want stale_at_startup", key, reason)
			}
			if doc["expired_at"] == nil {
				t.Errorf("%s missing expired_at", key)
			}
		case "ambience|r1":
			if state != "released" {
				t.Errorf("%s state=%s, want released (untouched)", key, state)
			}
		case "ambience|e1":
			if state != "expired" {
				t.Errorf("%s state=%s, want expired (untouched)", key, state)
			}
			if doc["expiry_reason"] != nil {
				t.Errorf("%s expiry_reason should not be set on already-expired row", key)
			}
		case "tank-operator|a2":
			if state != "active" {
				t.Errorf("%s state=%s, want active (untouched, future expires_at)", key, state)
			}
		case "tank-operator|c2":
			if state != "claimed" {
				t.Errorf("%s state=%s, want claimed (untouched, no expires_at)", key, state)
			}
		}
		_ = wantExpired
	}
}

func TestExpireStaleLeases_ReservationReleaseFailureDoesNotFailSweep(t *testing.T) {
	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)

	store := newFakeStaleLeaseStore()
	store.seed(StaleLeaseExpiryRow{ID: "a1", Project: "ambience", State: "active", ExpiresAt: &past})
	store.releaseErr = errors.New("slot row CAS exhausted")

	count, err := ExpireStaleLeases(context.Background(), store, now, nil)
	if err != nil {
		t.Fatalf("ExpireStaleLeases err=%v, want nil (release failure is best-effort)", err)
	}
	if count != 1 {
		t.Fatalf("expired count=%d, want 1", count)
	}
	if state, _ := store.docs["ambience|a1"]["state"].(string); state != "expired" {
		t.Fatalf("lease state=%s, want expired even though reservation release failed", state)
	}
}

func TestExpireStaleLeases_NilStoreIsNoOp(t *testing.T) {
	count, err := ExpireStaleLeases(context.Background(), nil, time.Now(), nil)
	if err != nil || count != 0 {
		t.Fatalf("nil store: count=%d err=%v, want (0, nil)", count, err)
	}
}

func TestExpireStaleLeases_ConcurrentRaceLeavesLeaseAlone(t *testing.T) {
	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)

	store := newFakeStaleLeaseStore()
	store.seed(StaleLeaseExpiryRow{ID: "raced", Project: "ambience", State: "active", ExpiresAt: &past})
	// Simulate a callback release racing the sweep: by the time
	// PatchLeasePayload's SELECT FOR UPDATE materializes, the live
	// payload already says "released".
	store.docs["ambience|raced"]["state"] = "released"
	store.docs["ambience|raced"]["released_at"] = "2026-05-29T00:00:00Z"

	count, err := ExpireStaleLeases(context.Background(), store, now, nil)
	if err != nil {
		t.Fatalf("ExpireStaleLeases err=%v", err)
	}
	if count != 0 {
		t.Fatalf("expired count=%d, want 0 (sweep should defer to concurrent release)", count)
	}
	if state, _ := store.docs["ambience|raced"]["state"].(string); state != "released" {
		t.Fatalf("sweep overwrote released state: state=%s", state)
	}
}

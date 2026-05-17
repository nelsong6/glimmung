package cosmos

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nelsong6/glimmung/internal/server"
)

// liveCosmosStore returns a *Store wired against a real Cosmos endpoint,
// or skips the calling test if `GLIMMUNG_TEST_COSMOS=live` isn't set.
// Mirrors the env gating used by TestLiveCosmosLockLifecycle.
func liveCosmosStore(t *testing.T) *Store {
	t.Helper()
	if strings.ToLower(os.Getenv("GLIMMUNG_TEST_COSMOS")) != "live" {
		t.Skip("set GLIMMUNG_TEST_COSMOS=live to run live Cosmos smoke")
	}
	endpoint := os.Getenv("COSMOS_ENDPOINT")
	if endpoint == "" {
		t.Fatal("COSMOS_ENDPOINT is required for live Cosmos smoke")
	}
	database := os.Getenv("COSMOS_DATABASE")
	if database == "" {
		database = "glimmung"
	}
	store, err := NewFromSettings(server.Settings{
		CosmosEndpoint: endpoint,
		CosmosDatabase: database,
	})
	if err != nil {
		t.Fatalf("create Cosmos store: %v", err)
	}
	return store
}

// liveSmokeProject returns a sanitized project name unique to this run
// so concurrent live-smoke invocations don't collide.
func liveSmokeProject(t *testing.T, suffix string) string {
	t.Helper()
	prefix := strings.TrimSpace(os.Getenv("GLIMMUNG_TEST_PREFIX"))
	if prefix == "" {
		prefix = "test-" + uuid.NewString()
	}
	return sanitizeLiveSmokeName(prefix) + "-" + suffix
}

func TestLiveCosmosSlotCreateGetUpdateDelete(t *testing.T) {
	store := liveCosmosStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	project := liveSmokeProject(t, "slot-crud")
	defer func() {
		// Best-effort cleanup of every slot index touched by this test.
		for _, idx := range []int{1, 2, 3} {
			_ = store.DeleteSlot(context.Background(), project, idx)
		}
	}()

	now := time.Now().UTC()
	created, err := store.CreateSlot(ctx, server.NewUnseededSlot(project, 1, project+"-slot-1", now))
	if err != nil {
		t.Fatalf("CreateSlot: %v", err)
	}
	if created.ETag() == "" {
		t.Fatal("CreateSlot returned slot without etag")
	}

	got, err := store.GetSlot(ctx, project, 1)
	if err != nil {
		t.Fatalf("GetSlot: %v", err)
	}
	if got.State != server.SlotStateUnseeded {
		t.Errorf("GetSlot.State=%q, want unseeded", got.State)
	}
	if got.ETag() == "" {
		t.Fatal("GetSlot returned slot without etag")
	}

	// CreateSlot is idempotent — re-create returns the existing doc
	// without bumping the etag.
	reCreated, err := store.CreateSlot(ctx, server.NewUnseededSlot(project, 1, project+"-slot-1", now))
	if err != nil {
		t.Fatalf("idempotent CreateSlot: %v", err)
	}
	if reCreated.ETag() != got.ETag() {
		t.Errorf("idempotent re-create bumped etag: %q -> %q", got.ETag(), reCreated.ETag())
	}

	// UpdateIfMatch happy path.
	updated, err := store.UpdateIfMatch(ctx, project, 1, func(s server.Slot) (server.Slot, error) {
		return s.MarkProvisioning(time.Now().UTC())
	})
	if err != nil {
		t.Fatalf("UpdateIfMatch: %v", err)
	}
	if updated.State != server.SlotStateProvisioning {
		t.Errorf("UpdateIfMatch.State=%q, want provisioning", updated.State)
	}
	if updated.ETag() == got.ETag() {
		t.Error("etag should advance after successful UpdateIfMatch")
	}

	// DeleteSlot then GetSlot returns ErrNotFound.
	if err := store.DeleteSlot(ctx, project, 1); err != nil {
		t.Fatalf("DeleteSlot: %v", err)
	}
	if _, err := store.GetSlot(ctx, project, 1); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("post-Delete GetSlot=%v, want ErrNotFound", err)
	}
	// Idempotent: delete-of-missing returns nil.
	if err := store.DeleteSlot(ctx, project, 1); err != nil {
		t.Fatalf("idempotent DeleteSlot: %v", err)
	}
}

func TestLiveCosmosSlotUpdateIfMatchSurfacesPreconditionFailed(t *testing.T) {
	store := liveCosmosStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	project := liveSmokeProject(t, "slot-cas")
	defer func() { _ = store.DeleteSlot(context.Background(), project, 1) }()

	if _, err := store.CreateSlot(ctx, server.NewUnseededSlot(project, 1, project+"-slot-1", time.Now().UTC())); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Two goroutines both read and try to UpdateIfMatch from unseeded ->
	// provisioning. Exactly one wins on Cosmos's etag check; the other
	// gets ErrPreconditionFailed.
	var (
		successes  int32 = 0
		precondErr int32 = 0
		otherErr   int32 = 0
	)
	var wg sync.WaitGroup
	mu := sync.Mutex{}
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_, err := store.UpdateIfMatch(ctx, project, 1, func(s server.Slot) (server.Slot, error) {
				// Hold each goroutine briefly so they overlap.
				time.Sleep(50 * time.Millisecond)
				return s.MarkProvisioning(time.Now().UTC())
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, server.ErrPreconditionFailed):
				precondErr++
			default:
				otherErr++
				t.Logf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if otherErr > 0 {
		t.Fatalf("got %d unexpected errors (see logs above)", otherErr)
	}
	if successes != 1 || precondErr != 1 {
		// Note: both goroutines might succeed if their reads happen
		// strictly sequentially with the first's write committing in
		// between. The test biases against that by sleeping inside the
		// mutate function; if the test is flaky on a slow Cosmos
		// instance, increase the sleep.
		t.Fatalf("successes=%d precondFailed=%d, want exactly 1 and 1 (cross-replica CAS contract)", successes, precondErr)
	}
}

func TestLiveCosmosSlotListByProjectSingleParition(t *testing.T) {
	store := liveCosmosStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	project := liveSmokeProject(t, "slot-list")
	defer func() {
		for _, idx := range []int{1, 2, 3, 4, 5} {
			_ = store.DeleteSlot(context.Background(), project, idx)
		}
	}()

	for _, idx := range []int{3, 1, 5, 2} {
		if _, err := store.CreateSlot(ctx, server.NewUnseededSlot(project, idx, project+"-slot", time.Now().UTC())); err != nil {
			t.Fatalf("seed slot %d: %v", idx, err)
		}
	}

	got, err := store.ListSlotsByProject(ctx, project)
	if err != nil {
		t.Fatalf("ListSlotsByProject: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len(got)=%d, want 4", len(got))
	}
	want := []int{1, 2, 3, 5}
	for i, s := range got {
		if s.SlotIndex != want[i] {
			t.Errorf("got[%d].SlotIndex=%d, want %d", i, s.SlotIndex, want[i])
		}
	}
}

func TestLiveCosmosSlotHistoryAppendAndList(t *testing.T) {
	store := liveCosmosStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	project := liveSmokeProject(t, "slot-history")
	defer func() {
		// History entries inherit the project as partition key; we
		// don't have a clean delete-all primitive, so we accept that
		// the test's entries linger in Cosmos under the unique project
		// prefix. The prefix is unique per test run via uuid.
	}()

	one := 1
	two := 2
	for _, e := range []server.SlotHistoryEntry{
		{Project: project, SlotIndex: &one, Event: "return", LeaseRef: project + "/leases/1", Source: "test_slot_return", CreatedAt: time.Now().UTC()},
		{Project: project, SlotIndex: &two, Event: "return", LeaseRef: project + "/leases/2", Source: "test_slot_return", CreatedAt: time.Now().UTC().Add(time.Second)},
		{Project: project, SlotIndex: &one, Event: "error", LeaseRef: project + "/leases/3", Source: "test_slot_return", CreatedAt: time.Now().UTC().Add(2 * time.Second)},
	} {
		if _, err := store.AppendSlotHistory(ctx, e); err != nil {
			t.Fatalf("AppendSlotHistory: %v", err)
		}
	}

	all, err := store.ListSlotHistory(ctx, project, nil)
	if err != nil {
		t.Fatalf("ListSlotHistory all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len(all)=%d, want 3", len(all))
	}

	slotOne, err := store.ListSlotHistory(ctx, project, &one)
	if err != nil {
		t.Fatalf("ListSlotHistory slot 1: %v", err)
	}
	if len(slotOne) != 2 {
		t.Fatalf("len(slot 1)=%d, want 2", len(slotOne))
	}
}

package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
	"github.com/google/uuid"

	"github.com/nelsong6/glimmung/internal/server"
)

func TestLiveCosmosLockLifecycle(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := strings.TrimSpace(os.Getenv("GLIMMUNG_TEST_PREFIX"))
	if prefix == "" {
		prefix = "test-" + uuid.NewString()
	}
	project := sanitizeLiveSmokeName(prefix) + "-locks"
	issueNumber := 1
	holderID := "ci-holder"
	key := fmt.Sprintf("%s#%d", project, issueNumber)
	docID := lockDocID("issue", key)
	pk := azcosmos.NewPartitionKeyString("issue")

	cleanup := func() {
		if _, err := store.locks.DeleteItem(context.Background(), pk, docID, nil); err != nil && !isCosmosStatus(err, http.StatusNotFound) {
			t.Logf("cleanup lock %s: %v", docID, err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	if err := store.ClaimIssueLock(ctx, project, issueNumber, holderID, 300); err != nil {
		t.Fatalf("claim issue lock: %v", err)
	}
	if err := store.ClaimIssueLock(ctx, project, issueNumber, "other-holder", 300); !errors.Is(err, server.ErrAlreadyRunning) {
		t.Fatalf("second claim err=%v, want ErrAlreadyRunning", err)
	}

	store.ReleaseIssueLock(ctx, project, issueNumber, holderID)

	read, err := store.locks.ReadItem(ctx, pk, docID, nil)
	if err != nil {
		t.Fatalf("read released lock: %v", err)
	}
	var doc lockDoc
	if err := json.Unmarshal(read.Value, &doc); err != nil {
		t.Fatalf("decode released lock: %v", err)
	}
	if doc.State != "released" {
		t.Fatalf("lock state=%q, want released", doc.State)
	}
	if doc.HeldBy == nil || *doc.HeldBy != holderID {
		t.Fatalf("held_by=%v, want %q", doc.HeldBy, holderID)
	}
}

func sanitizeLiveSmokeName(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "test"
	}
	return out
}

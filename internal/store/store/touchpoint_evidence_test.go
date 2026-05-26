package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nelsong6/glimmung/internal/server"
)

func TestTouchpointDocPreservesStructuredEvidence(t *testing.T) {
	doc := touchpointDoc{
		ID:      "tp-1",
		Project: "proj",
		Repo:    "owner/repo",
		Number:  123,
		Title:   "review",
		State:   "ready",
		Evidence: []server.TouchpointEvidence{{
			Kind:         "screenshot",
			Ref:          "blob://artifacts/runs/proj/run-1/screenshots/default.png",
			Label:        "default",
			URL:          "/v1/artifacts/runs/proj/run-1/screenshots/default.png",
			ArtifactPath: "runs/proj/run-1/screenshots/default.png",
		}},
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := touchpointDocFromPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	row := touchpointRowFromDoc(decoded, nil, nil, nil, nil, nil, nil, time.Now().UTC())
	if len(row.Evidence) != 1 {
		t.Fatalf("row evidence=%#v", row.Evidence)
	}
	if row.Evidence[0].Kind != "screenshot" || row.Evidence[0].ArtifactPath != "runs/proj/run-1/screenshots/default.png" {
		t.Fatalf("row evidence=%#v", row.Evidence[0])
	}
}

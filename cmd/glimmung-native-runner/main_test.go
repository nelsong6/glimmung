package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseOutputFileAcceptsKeyValueAndJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outputs.txt")
	if err := os.WriteFile(path, []byte("alpha=one\n{\"beta\":\"two\"}\n{\"key\":\"gamma\",\"value\":3}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := parseOutputFile(path)
	if err != nil {
		t.Fatalf("parseOutputFile: %v", err)
	}
	if got["alpha"] != "one" || got["beta"] != "two" || got["gamma"] != "3" {
		t.Fatalf("outputs=%#v", got)
	}
}

func TestParseOutputFileRejectsDuplicateKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outputs.txt")
	if err := os.WriteFile(path, []byte("alpha=one\nalpha=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := parseOutputFile(path); err == nil {
		t.Fatal("expected duplicate output error")
	}
}

func TestNativeRunnerExecutesStepsAndPublishesOutputs(t *testing.T) {
	var events []nativeEventRequest
	var completion completedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			var event nativeEventRequest
			if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
				t.Errorf("decode event: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			events = append(events, event)
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
		case "/completed":
			if err := json.NewDecoder(r.Body).Decode(&completion); err != nil {
				t.Errorf("decode completion: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"decision": "done"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	workspace := t.TempDir()
	r := &nativeRunner{
		cfg: runnerConfig{
			JobID:        "test",
			EventsURL:    server.URL + "/events",
			CompletedURL: server.URL + "/completed",
			Workspace:    workspace,
			Job: jobSpec{
				WorkingDirectory: workspace,
				Shell:            "sh",
				Steps: []stepSpec{{
					Slug: "write-output",
					Type: "run",
					Run:  "printf 'preview_url=https://example.test\\n' >> \"$GLIMMUNG_OUTPUT_FILE\"",
				}},
			},
		},
		client:  server.Client(),
		outputs: map[string]string{},
	}

	if err := r.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if completion.Conclusion != "success" || completion.Outputs["preview_url"] != "https://example.test" {
		t.Fatalf("completion=%#v", completion)
	}
	if !sawEvent(events, "phase_output_set") || !sawEvent(events, "step_completed") {
		t.Fatalf("events=%#v", events)
	}
}

func sawEvent(events []nativeEventRequest, event string) bool {
	for _, candidate := range events {
		if candidate.Event == event {
			return true
		}
	}
	return false
}

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
					Run:  "printf '{\"type\":\"result\",\"total_cost_usd\":1.25}\\n'\nprintf 'preview_url=https://example.test\\n' >> \"$GLIMMUNG_OUTPUT_FILE\"\nprintf '{\"summary_markdown\":\"done\",\"verification\":{\"status\":\"pass\"}}' > \"$GLIMMUNG_COMPLETION_FILE\"",
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
	if completion.SummaryMarkdown == nil || *completion.SummaryMarkdown != "done" {
		t.Fatalf("summary=%#v", completion.SummaryMarkdown)
	}
	if completion.Verification["status"] != "pass" {
		t.Fatalf("verification=%#v", completion.Verification)
	}
	if completion.CostUSD != 1.25 {
		t.Fatalf("cost=%v", completion.CostUSD)
	}
	if !sawEvent(events, "phase_output_set") || !sawEvent(events, "step_completed") {
		t.Fatalf("events=%#v", events)
	}
}

// TestNativeRunnerHaltsRemainingStepsOnAbortReason pins the fail-closed
// abort short-circuit: when a step emits a non-empty abort_reason phase
// output, the runner must stop and NOT run later steps in the phase, and
// must report conclusion=aborted carrying the reason. This is the
// spirelens env-prep guard contract — a probe that finds the host
// unavailable / an unexpected mod must not let downstream steps run.
func TestNativeRunnerHaltsRemainingStepsOnAbortReason(t *testing.T) {
	var events []nativeEventRequest
	var completion completedRequest
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			var event nativeEventRequest
			_ = json.NewDecoder(r.Body).Decode(&event)
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
		case "/completed":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&completion)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"decision": "done"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	workspace := t.TempDir()
	sentinel := filepath.Join(workspace, "second-step-ran")
	r := &nativeRunner{
		cfg: runnerConfig{
			JobID:        "test-abort",
			EventsURL:    server.URL + "/events",
			CompletedURL: server.URL + "/completed",
			Workspace:    workspace,
			Job: jobSpec{
				WorkingDirectory: workspace,
				Shell:            "sh",
				Steps: []stepSpec{
					{
						Slug: "probe-ssh",
						Type: "run",
						Run:  "printf 'abort_reason=host_unavailable\\n' >> \"$GLIMMUNG_OUTPUT_FILE\"",
					},
					{
						Slug: "probe-mod-set",
						Type: "run",
						Run:  "touch \"" + sentinel + "\"",
					},
				},
			},
		},
		client:  server.Client(),
		outputs: map[string]string{},
	}

	if err := r.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if completion.Conclusion != "aborted" {
		t.Fatalf("conclusion=%q, want aborted", completion.Conclusion)
	}
	if completion.Outputs["abort_reason"] != "host_unavailable" {
		t.Fatalf("outputs=%#v, want abort_reason=host_unavailable", completion.Outputs)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("second step ran despite abort_reason from the first step")
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawEvent(events, "phase_aborted") {
		t.Fatalf("expected a phase_aborted event, got %#v", events)
	}
	for _, e := range events {
		if e.Event == "step_started" && e.StepSlug != nil && *e.StepSlug == "probe-mod-set" {
			t.Fatalf("probe-mod-set step was started despite abort")
		}
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

// TestNativeRunnerPostsTimedOutOnContextCancel pins the SIGTERM-handler
// behaviour: when the context is cancelled mid-step (modelling the
// kubelet sending SIGTERM after activeDeadlineSeconds), the runner must
// deliver a /completed callback with conclusion=timed_out before
// returning, rather than leaving the run with no terminal signal.
//
// The previous behaviour — no signal handler, so SIGTERM killed the
// process before any callback fired — is what hung ambience#170/runs/1.1
// even after the rest of verify.sh would have been able to clean up.
func TestNativeRunnerPostsTimedOutOnContextCancel(t *testing.T) {
	var events []nativeEventRequest
	var completion completedRequest
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			var event nativeEventRequest
			_ = json.NewDecoder(r.Body).Decode(&event)
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
		case "/completed":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&completion)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"decision": "done"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	workspace := t.TempDir()
	r := &nativeRunner{
		cfg: runnerConfig{
			JobID:        "test-shutdown",
			EventsURL:    server.URL + "/events",
			CompletedURL: server.URL + "/completed",
			Workspace:    workspace,
			Job: jobSpec{
				WorkingDirectory: workspace,
				Shell:            "sh",
				Steps: []stepSpec{{
					Slug: "long-running",
					Type: "run",
					// Sleep longer than the cancel timer. The
					// exec.CommandContext will kill it when ctx
					// cancels; the runner then has to choose between
					// "failure" and "timed_out". The signal-aware
					// helper picks timed_out.
					Run: "sleep 30",
				}},
			},
		},
		client:  server.Client(),
		outputs: map[string]string{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	if err := r.run(ctx); err == nil {
		t.Fatal("expected run to return an error after cancellation")
	}

	mu.Lock()
	got := completion
	mu.Unlock()
	if got.Conclusion != "timed_out" {
		t.Fatalf("conclusion=%q, want timed_out", got.Conclusion)
	}
	if got.SummaryMarkdown == nil || !strings.Contains(*got.SummaryMarkdown, "shutdown") {
		t.Fatalf("summary=%v, want it to mention shutdown", got.SummaryMarkdown)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawEvent(events, "runner_failed") {
		t.Fatalf("expected runner_failed event; events=%v", events)
	}
}

// TestNativeRunnerEmitsInnerJobRegisteredEventFromMarker pins the
// inner-Job observation contract (docs/inner-job-observation.md): when
// a phase script prints the marker line on stdout, the runner forwards
// it as an inner_job_registered event with the parsed metadata, so
// glimmung records the child Job alongside the outer one. Without this
// the dashboard has no record of inner Jobs and operators cannot
// discover their logs.
func TestNativeRunnerEmitsInnerJobRegisteredEventFromMarker(t *testing.T) {
	var (
		mu       sync.Mutex
		events   []nativeEventRequest
		complete completedRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			var event nativeEventRequest
			_ = json.NewDecoder(r.Body).Decode(&event)
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
		case "/completed":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&complete)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"decision": "done"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	workspace := t.TempDir()
	marker := `===GLIMMUNG-INNER-JOB=== {"namespace":"ambience-slot-3","job_name":"agent-ve-2","intent":"verification_agent","label":"verify-agent"}`
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
					Slug: "spawn-child",
					Type: "run",
					// Print marker + an unrelated log line + an
					// invalid marker (missing job_name). The runner
					// should emit one inner_job_registered for the
					// valid marker and a runner_warning for the
					// invalid one.
					Run: "printf '%s\\n' " + shellQuote(marker) +
						" && printf 'a normal log line\\n'" +
						" && printf '%s\\n' " + shellQuote(`===GLIMMUNG-INNER-JOB=== {"namespace":"ns"}`),
				}},
			},
		},
		client:  server.Client(),
		outputs: map[string]string{},
	}

	if err := r.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if complete.Conclusion != "success" {
		t.Fatalf("conclusion=%q", complete.Conclusion)
	}

	mu.Lock()
	defer mu.Unlock()

	var (
		registered *nativeEventRequest
		warning    *nativeEventRequest
	)
	for i := range events {
		switch events[i].Event {
		case "inner_job_registered":
			ev := events[i]
			registered = &ev
		case "runner_warning":
			ev := events[i]
			warning = &ev
		}
	}
	if registered == nil {
		t.Fatalf("expected inner_job_registered event; events=%+v", events)
	}
	if got := registered.Metadata["namespace"]; got != "ambience-slot-3" {
		t.Fatalf("registered.namespace=%v", got)
	}
	if got := registered.Metadata["job_name"]; got != "agent-ve-2" {
		t.Fatalf("registered.job_name=%v", got)
	}
	if got := registered.Metadata["intent"]; got != "verification_agent" {
		t.Fatalf("registered.intent=%v", got)
	}
	if warning == nil {
		t.Fatalf("expected runner_warning for invalid marker; events=%+v", events)
	}
	if msg := warning.Message; msg == nil || !strings.Contains(*msg, "job_name") {
		t.Fatalf("warning.message=%v", warning.Message)
	}
}

func TestNativeRunnerSuppressesEvidenceTarPayloadLogs(t *testing.T) {
	var (
		mu     sync.Mutex
		events []nativeEventRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var event nativeEventRequest
		_ = json.NewDecoder(r.Body).Decode(&event)
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
	}))
	defer server.Close()

	workspace := t.TempDir()
	r := &nativeRunner{
		cfg: runnerConfig{
			EventsURL: server.URL + "/events",
			Workspace: workspace,
			Job: jobSpec{
				WorkingDirectory: workspace,
				Shell:            "sh",
			},
		},
		client:  server.Client(),
		outputs: map[string]string{},
	}

	script := strings.Join([]string{
		"printf '" + evidenceTarStartMarker + "\\n'",
		"i=0; while [ \"$i\" -lt 25 ]; do printf 'payload-line-%s\\n' \"$i\"; i=$((i + 1)); done",
		"printf '" + evidenceTarEndMarker + "\\n'",
		"printf 'after-payload\\n'",
	}, "\n")
	exitCode, err := r.executeStep(context.Background(), stepSpec{Slug: "collect", Run: script}, filepath.Join(workspace, "out"), filepath.Join(workspace, "completion"))
	if err != nil || exitCode != 0 {
		t.Fatalf("executeStep exit=%d err=%v", exitCode, err)
	}

	var sawSummary bool
	deadline := time.Now().Add(1 * time.Second)
	for {
		mu.Lock()
		sawSummary = false
		payloadLeaked := ""
		for _, event := range events {
			if event.Message == nil {
				continue
			}
			if strings.Contains(*event.Message, "payload-line-") {
				payloadLeaked = *event.Message
				break
			}
			if strings.Contains(*event.Message, "omitted 25 payload lines") {
				sawSummary = true
			}
		}
		snapshot := append([]nativeEventRequest(nil), events...)
		mu.Unlock()
		if payloadLeaked != "" {
			t.Fatalf("evidence payload leaked into log event: %q", payloadLeaked)
		}
		if sawSummary {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected omitted payload summary event, got %+v", snapshot)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNativeRunnerDoesNotWaitForeverForBlockedLogEvent(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		once.Do(func() { close(requestStarted) })
		<-releaseRequest
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
	}))
	defer server.Close()

	workspace := t.TempDir()
	r := &nativeRunner{
		cfg: runnerConfig{
			EventsURL: server.URL + "/events",
			Workspace: workspace,
			Job: jobSpec{
				WorkingDirectory: workspace,
				Shell:            "sh",
			},
		},
		client:  server.Client(),
		outputs: map[string]string{},
	}

	type result struct {
		exitCode int
		err      error
	}
	done := make(chan result, 1)
	go func() {
		exitCode, err := r.executeStep(context.Background(), stepSpec{Slug: "run", Run: "printf 'blocked-log\\n'"}, filepath.Join(workspace, "out"), filepath.Join(workspace, "completion"))
		done <- result{exitCode: exitCode, err: err}
	}()

	select {
	case <-requestStarted:
	case <-time.After(1 * time.Second):
		close(releaseRequest)
		t.Fatal("log event request was not started")
	}

	select {
	case got := <-done:
		if got.err != nil || got.exitCode != 0 {
			close(releaseRequest)
			t.Fatalf("executeStep exit=%d err=%v", got.exitCode, got.err)
		}
	case <-time.After(1 * time.Second):
		close(releaseRequest)
		t.Fatal("executeStep waited for blocked log drain after child exit")
	}
	close(releaseRequest)
}

// shellQuote produces a POSIX-shell-safe single-quoted form of s.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

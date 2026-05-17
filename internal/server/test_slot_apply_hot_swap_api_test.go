package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
	
)

func newApplyHotSwapStore(t *testing.T) *fakeLeaseStore {
	t.Helper()
	return &fakeLeaseStore{
		fakeReadStore: fakeReadStore{
			projects: []Project{
				{
					Name:       "tank-operator",
					GitHubRepo: "nelsong6/tank-operator",
					Metadata: map[string]any{
						"test_slot_hot_swap": map[string]any{
							"enabled": true,
							"agent_runner": map[string]any{
								"enabled":       true,
								"source":        "agent-runner/dist",
								"target":        "/var/run/agent-runner-hot/dist",
								"build_command": "cd agent-runner && npm run build",
								"pod_selector":  "tank-operator/session-id",
								"container":     "agent-runner",
								"restart":       "SIGHUP",
								"builder_image": "node:20-alpine",
							},
						},
					},
				},
			},
		},
		leases: []Lease{
			{
				ID:      "tank-operator-slot-1",
				Project: "tank-operator",
				State:   "claimed",
				Metadata: map[string]any{
					"test_slot_checkout": true,
					"native_slot_name":   "tank-operator-slot-1",
					"native_slot_index":  "1",
				},
			},
		},
	}
}

// TestApplyTestSlotHotSwapHappyPathResolves pins the end-to-end happy path:
// lease resolved by slot_name → contract read from project metadata →
// performer invoked with the right options → history entry recorded.
func TestApplyTestSlotHotSwapHappyPathResolves(t *testing.T) {
	store := newApplyHotSwapStore(t)
	var seen ApplyHotSwapOptions
	performer := func(_ context.Context, opts ApplyHotSwapOptions) (ApplyHotSwapResult, error) {
		seen = opts
		return ApplyHotSwapResult{
			JobName:      "apply-hot-swap-abc",
			ArtifactKind: opts.ArtifactKind,
			GitRef:       opts.GitRef,
			Outcome:      "persisted",
			Timings:      map[string]string{"total": "42s"},
		}, nil
	}

	handler := http.HandlerFunc(applyTestSlotHotSwap(store, performer))
	body := `{"project":"tank-operator","slot_name":"tank-operator-slot-1","artifact_kind":"agent_runner","git_ref":"feat/durable-stop-request"}`
	req := authedApplyRequest(t, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var result TestSlotApplyHotSwapResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if result.Apply.Outcome != "persisted" {
		t.Fatalf("apply.outcome = %q, want persisted", result.Apply.Outcome)
	}
	if result.Lease == "" {
		t.Fatalf("result.lease should be populated")
	}

	// Performer received the right options
	if seen.ArtifactKind != "agent_runner" {
		t.Fatalf("performer ArtifactKind = %q", seen.ArtifactKind)
	}
	if seen.GitRef != "feat/durable-stop-request" {
		t.Fatalf("performer GitRef = %q", seen.GitRef)
	}
	// Target namespace derived from slot_name convention
	if seen.TargetNamespace != "tank-operator-slot-1-sessions" {
		t.Fatalf("performer TargetNamespace = %q, want tank-operator-slot-1-sessions", seen.TargetNamespace)
	}
	// RepoURL derived from project.github_repo
	if seen.RepoURL != "https://github.com/nelsong6/tank-operator.git" {
		t.Fatalf("performer RepoURL = %q", seen.RepoURL)
	}
	// Contract.AgentRunner.BuilderImage flowed through
	if seen.Contract.AgentRunner.BuilderImage != "node:20-alpine" {
		t.Fatalf("contract not flowed correctly: %#v", seen.Contract.AgentRunner)
	}

	// History was recorded with status=persisted (the fakeLeaseStore
	// writes the last hot-swap status to leases[0].Metadata).
	if got := store.leases[0].Metadata["last_hot_swap_status"]; got != "persisted" {
		t.Fatalf("history not recorded with persisted; got %v", got)
	}
}

// TestApplyTestSlotHotSwapRecordsFailureHistory pins that on apply
// failure, the endpoint still appends a history entry with the failure
// status. Durable state in the system, regardless of the response.
func TestApplyTestSlotHotSwapRecordsFailureHistory(t *testing.T) {
	store := newApplyHotSwapStore(t)
	performer := func(_ context.Context, _ ApplyHotSwapOptions) (ApplyHotSwapResult, error) {
		return ApplyHotSwapResult{
			JobName:       "apply-hot-swap-xyz",
			Outcome:       "build_failed",
			BuildLogsTail: "npm ERR! missing script: build",
			Error:         "init container exited 1",
			Timings:       map[string]string{"total": "12s"},
		}, errors.New("apply failed")
	}

	handler := http.HandlerFunc(applyTestSlotHotSwap(store, performer))
	body := `{"project":"tank-operator","slot_index":1,"artifact_kind":"agent_runner","git_ref":"feat/x"}`
	req := authedApplyRequest(t, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Endpoint returns 200 with the structured failure (caller decodes
	// Outcome to present cleanly).
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := store.leases[0].Metadata["last_hot_swap_status"]; got != "build_failed" {
		t.Fatalf("history should record build_failed; got %v", got)
	}
}

// TestApplyTestSlotHotSwapClampsTimeout pins the request-timeout clamping:
// caller asks for 9999s, server clamps to applyHotSwapTimeoutMax (600s).
func TestApplyTestSlotHotSwapClampsTimeout(t *testing.T) {
	store := newApplyHotSwapStore(t)
	var seenTimeout int
	performer := func(_ context.Context, opts ApplyHotSwapOptions) (ApplyHotSwapResult, error) {
		seenTimeout = int(opts.Timeout.Seconds())
		return ApplyHotSwapResult{Outcome: "persisted", Timings: map[string]string{}}, nil
	}

	handler := http.HandlerFunc(applyTestSlotHotSwap(store, performer))
	body := `{"project":"tank-operator","slot_name":"tank-operator-slot-1","artifact_kind":"agent_runner","git_ref":"feat/x","timeout_seconds":9999}`
	req := authedApplyRequest(t, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if seenTimeout != int(applyHotSwapTimeoutMax.Seconds()) {
		t.Fatalf("performer Timeout = %ds, want clamped to %ds", seenTimeout, int(applyHotSwapTimeoutMax.Seconds()))
	}
}

// TestApplyTestSlotHotSwapRejectsBackendWithoutBuilderImage pins the
// request-time guard: backend kind needs builder_image, which is
// optional at Contract.Validate time but required for the apply path.
func TestApplyTestSlotHotSwapRejectsBackendWithoutBuilderImage(t *testing.T) {
	store := newApplyHotSwapStore(t)
	// Patch project to have backend enabled but no builder_image
	store.projects[0].Metadata["test_slot_hot_swap"].(map[string]any)["backend"] = map[string]any{
		"enabled":       true,
		"strategy":      "supervisor",
		"build_command": "go build",
		"artifact":      "/tmp/app",
		"target":        "/var/run/app-hot/app",
		"health_path":   "/healthz",
		// builder_image intentionally absent
	}

	performer := func(_ context.Context, _ ApplyHotSwapOptions) (ApplyHotSwapResult, error) {
		t.Fatal("performer should not be invoked when validation fails")
		return ApplyHotSwapResult{}, nil
	}

	handler := http.HandlerFunc(applyTestSlotHotSwap(store, performer))
	body := `{"project":"tank-operator","slot_name":"tank-operator-slot-1","artifact_kind":"backend","git_ref":"feat/x"}`
	req := authedApplyRequest(t, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "builder_image") {
		t.Fatalf("response should name builder_image; got %s", rec.Body.String())
	}
}

// TestApplyTestSlotHotSwapRejectsMissingFields pins the basic input
// validation: project, artifact_kind, git_ref all required.
func TestApplyTestSlotHotSwapRejectsMissingFields(t *testing.T) {
	store := newApplyHotSwapStore(t)
	performer := func(_ context.Context, _ ApplyHotSwapOptions) (ApplyHotSwapResult, error) {
		return ApplyHotSwapResult{}, nil
	}
	handler := http.HandlerFunc(applyTestSlotHotSwap(store, performer))

	bodies := []string{
		`{"slot_name":"tank-operator-slot-1","artifact_kind":"agent_runner","git_ref":"x"}`,  // missing project
		`{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"x"}`,        // missing artifact_kind
		`{"project":"tank-operator","slot_name":"tank-operator-slot-1","artifact_kind":"x"}`, // missing git_ref
	}
	for i, body := range bodies {
		req := authedApplyRequest(t, body)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("case %d: status = %d body = %s", i, rec.Code, rec.Body.String())
		}
	}
}

func authedApplyRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/test-slots/apply-hot-swap", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// The endpoint is registered behind requireAdmin in server.go, but
	// we test the handler directly, so no auth header is needed at this
	// layer. The handler itself doesn't read auth.
	_ = auth.User{}
	return req
}

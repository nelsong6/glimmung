package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/server"
	pgstore "github.com/nelsong6/glimmung/internal/store/pg"
)

func TestNativeEventAttemptIndexAcceptsExplicitOrMetadataValue(t *testing.T) {
	explicit := 3
	if got, ok := nativeEventAttemptIndex(server.NativeRunEventRequest{AttemptIndex: &explicit}); !ok || got != 3 {
		t.Fatalf("explicit attempt index=(%d,%t), want (3,true)", got, ok)
	}
	if got, ok := nativeEventAttemptIndex(server.NativeRunEventRequest{Metadata: map[string]any{"attempt_index": "2"}}); !ok || got != 2 {
		t.Fatalf("metadata attempt index=(%d,%t), want (2,true)", got, ok)
	}
	if _, ok := nativeEventAttemptIndex(server.NativeRunEventRequest{Metadata: map[string]any{"attempt_index": "bad"}}); ok {
		t.Fatal("invalid metadata attempt_index should not be accepted")
	}
}

func TestTerminalStateReleasesSlotLease(t *testing.T) {
	cases := []struct {
		name            string
		state           string
		preserveTestEnv bool
		want            bool
	}{
		// An aborted run tears everything down and releases the slot lease
		// regardless of preserve_test_env — this is the teardown-then-abort
		// path that previously stranded the lease "claimed".
		{name: "aborted releases", state: "aborted", preserveTestEnv: false, want: true},
		{name: "aborted releases even when preserve set", state: "aborted", preserveTestEnv: true, want: true},
		// A passed run releases unless preserve_test_env keeps the env alive.
		{name: "passed releases", state: "passed", preserveTestEnv: false, want: true},
		{name: "passed preserves when preserve set", state: "passed", preserveTestEnv: true, want: false},
		// Non-terminal / non-slot states never release here.
		{name: "review_required does not release", state: "review_required", preserveTestEnv: false, want: false},
		{name: "recycled does not release", state: "recycled", preserveTestEnv: false, want: false},
		{name: "empty state does not release", state: "", preserveTestEnv: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalStateReleasesSlotLease(tc.state, tc.preserveTestEnv); got != tc.want {
				t.Fatalf("terminalStateReleasesSlotLease(%q, %t)=%t, want %t", tc.state, tc.preserveTestEnv, got, tc.want)
			}
		})
	}
}

func TestNativeJobExecutionStateVerificationControl(t *testing.T) {
	completion := nativeJobCompletionDoc{
		Conclusion:   "success",
		Verification: &verificationDoc{Status: "fail"},
	}

	state, reason := nativeJobExecutionStateAndReason(completion, false)
	if state != "succeeded" || reason != "" {
		t.Fatalf("non-controlling verification state=(%q,%q), want succeeded with no reason", state, reason)
	}

	state, reason = nativeJobExecutionStateAndReason(completion, true)
	if state != "failed" || reason != "verification_failed" {
		t.Fatalf("controlling verification state=(%q,%q), want failed verification_failed", state, reason)
	}
}

func TestApplyNativePhaseOutputSetRawStoresAndRejectsDuplicateKeys(t *testing.T) {
	raw := map[string]any{
		"attempts": []any{
			map[string]any{
				"attempt_index": float64(2),
				"phase":         "env-prep",
			},
		},
	}
	attempt := attemptDoc{AttemptIndex: 2, Phase: "env-prep"}
	event := nativeEventDoc{
		Event:    "phase_output_set",
		StepSlug: "publish",
		Metadata: map[string]any{
			"key":   "validation_url",
			"value": "https://preview.example",
		},
	}

	if err := applyNativePhaseOutputSetRaw(raw, attempt, event); err != nil {
		t.Fatalf("applyNativePhaseOutputSetRaw: %v", err)
	}
	attempts := raw["attempts"].([]any)
	outputs := attempts[0].(map[string]any)["phase_outputs"].(map[string]any)
	if outputs["validation_url"] != "https://preview.example" {
		t.Fatalf("outputs=%#v", outputs)
	}
	if raw["validation_url"] != "https://preview.example" {
		t.Fatalf("validation_url was not promoted: %#v", raw)
	}

	err := applyNativePhaseOutputSetRaw(raw, attempt, event)
	if err == nil || !strings.Contains(err.Error(), "already set") {
		t.Fatalf("duplicate error=%v", err)
	}
}

func TestPromoteRunReviewOutputsRaw(t *testing.T) {
	raw := map[string]any{}
	if !promoteRunReviewOutputsRaw(raw, map[string]string{"validation_url": " https://slot.example "}) {
		t.Fatalf("expected validation_url promotion")
	}
	if raw["validation_url"] != "https://slot.example" {
		t.Fatalf("raw=%#v", raw)
	}
	if promoteRunReviewOutputsRaw(raw, map[string]string{"validation_url": "https://slot.example"}) {
		t.Fatalf("same validation_url should be a no-op")
	}
	if promoteRunReviewOutputsRaw(raw, map[string]string{"other": "value", "validation_url": ""}) {
		t.Fatalf("unsupported or empty outputs should be a no-op")
	}
}

func TestCarryForwardAttemptDocsPreserveOutputsForRecycleEntry(t *testing.T) {
	now := "2026-05-21T05:00:00Z"
	docs := carryForwardAttemptDocs([]server.RunAttemptData{{
		Phase:        "env-prep",
		Conclusion:   "success",
		Decision:     "advance",
		Completed:    true,
		CarryForward: true,
		PhaseOutputs: map[string]string{"validation_url": "https://slot.example"},
	}}, server.Workflow{Phases: []server.PhaseSpec{
		{Name: "env-prep", Kind: "k8s_job"},
		{Name: "llm-work", Kind: "k8s_job", DependsOn: []string{"env-prep"}},
	}}, now)

	if len(docs) != 1 {
		t.Fatalf("docs=%#v", docs)
	}
	doc := docs[0]
	if !doc.CarryForward || doc.AttemptIndex != 0 || doc.CompletedAt != now || doc.PhaseOutputs["validation_url"] != "https://slot.example" {
		t.Fatalf("carry doc=%#v", doc)
	}
	roundTrip := carryForwardAttemptsFromDocs(docs)
	if len(roundTrip) != 1 || !roundTrip[0].CarryForward || roundTrip[0].PhaseOutputs["validation_url"] != "https://slot.example" {
		t.Fatalf("roundTrip=%#v", roundTrip)
	}
}

func TestExecutionRawHelpersDriveCanonicalState(t *testing.T) {
	now := "2026-05-20T12:00:00Z"
	raw := map[string]any{
		"phase_executions": []any{
			map[string]any{
				"name":       "env-prep",
				"kind":       "k8s_job",
				"state":      "not_started",
				"created_at": now,
				"jobs": []any{map[string]any{
					"id":         "prepare",
					"state":      "not_started",
					"created_at": now,
					"steps": []any{map[string]any{
						"slug":       "checkout",
						"state":      "not_started",
						"created_at": now,
					}},
				}},
			},
			map[string]any{
				"name":       "agent-execute",
				"kind":       "k8s_job",
				"state":      "not_started",
				"created_at": now,
				"jobs": []any{map[string]any{
					"id":         "agent",
					"state":      "not_started",
					"created_at": now,
					"steps": []any{map[string]any{
						"slug":       "work",
						"state":      "not_started",
						"created_at": now,
					}},
				}},
			},
		},
	}

	markPhaseDispatchingRaw(raw, "env-prep", "k8s_job", now)
	env := rawPhase(t, raw, "env-prep")
	if got := stringValue(env["state"]); got != "dispatching" {
		t.Fatalf("env state=%q", got)
	}
	if got := stringValue(rawJob(t, env, "prepare")["state"]); got != "dispatching" {
		t.Fatalf("prepare state=%q", got)
	}

	applyNativeEventToExecutionsRaw(raw, attemptDoc{Phase: "env-prep"}, nativeEventDoc{
		JobID:     "prepare",
		Event:     "step_started",
		StepSlug:  "checkout",
		CreatedAt: now,
	})
	env = rawPhase(t, raw, "env-prep")
	prepare := rawJob(t, env, "prepare")
	if got := stringValue(env["state"]); got != "active" {
		t.Fatalf("env state after event=%q", got)
	}
	if got := stringValue(rawStep(t, prepare, "checkout")["state"]); got != "active" {
		t.Fatalf("checkout state=%q", got)
	}

	markJobCompletionInExecutionsRaw(raw, "env-prep", "prepare", "succeeded", "", now)
	env = rawPhase(t, raw, "env-prep")
	if got := stringValue(env["state"]); got != "succeeded" {
		t.Fatalf("env state after completion=%q", got)
	}

	markPhaseDispatchingRaw(raw, "agent-execute", "k8s_job", now)
	finalizeExecutionFailureRaw(raw, "dispatch_timeout", now)
	execute := rawPhase(t, raw, "agent-execute")
	if got := stringValue(execute["state"]); got != "failed" {
		t.Fatalf("execute state after timeout=%q", got)
	}
	if got := stringValue(rawJob(t, execute, "agent")["reason"]); got != "dispatch_timeout" {
		t.Fatalf("agent reason=%q", got)
	}
}

func TestJobCompletionFailurePreservesUnstartedSteps(t *testing.T) {
	now := "2026-05-25T04:26:41Z"
	raw := map[string]any{
		"phase_executions": []any{
			map[string]any{
				"name":  "env-prep",
				"kind":  "k8s_job",
				"state": "active",
				"jobs": []any{map[string]any{
					"id":    "env-prep",
					"state": "active",
					"steps": []any{
						map[string]any{"slug": "check-validation-env", "state": "active"},
						map[string]any{"slug": "emit-env-outputs", "state": "not_started"},
					},
				}},
			},
		},
	}

	markJobCompletionInExecutionsRaw(raw, "env-prep", "env-prep", "failed", "step_failed", now)

	job := rawJob(t, rawPhase(t, raw, "env-prep"), "env-prep")
	check := rawStep(t, job, "check-validation-env")
	if got := stringValue(check["state"]); got != "failed" {
		t.Fatalf("check-validation-env state=%q", got)
	}
	if got := stringValue(check["reason"]); got != "step_failed" {
		t.Fatalf("check-validation-env reason=%q", got)
	}
	emit := rawStep(t, job, "emit-env-outputs")
	if got := stringValue(emit["state"]); got != "not_started" {
		t.Fatalf("emit-env-outputs state=%q", got)
	}
	if _, ok := emit["reason"]; ok {
		t.Fatalf("emit-env-outputs should not have failure reason: %#v", emit)
	}
	if _, ok := emit["completed_at"]; ok {
		t.Fatalf("emit-env-outputs should not be completed: %#v", emit)
	}
}

func TestFinalizeExecutionFailureClassifiesForwardDispatchFailure(t *testing.T) {
	now := "2026-05-25T07:32:14Z"
	raw := map[string]any{
		"phase_executions": []any{
			map[string]any{
				"name":  "env-prep",
				"kind":  "k8s_job",
				"state": "succeeded",
				"jobs": []any{map[string]any{
					"id":    "env-prep",
					"state": "succeeded",
				}},
			},
			map[string]any{
				"name":  "llm-work",
				"kind":  "k8s_job",
				"state": "not_started",
				"jobs": []any{
					map[string]any{
						"id":    "llm-test-plan",
						"state": "not_started",
						"steps": []any{
							map[string]any{"slug": "clone", "state": "not_started"},
							map[string]any{"slug": "run-test-plan", "state": "not_started"},
						},
					},
					map[string]any{
						"id":    "llm-implement",
						"state": "not_started",
						"steps": []any{
							map[string]any{"slug": "clone", "state": "not_started"},
							map[string]any{"slug": "run-implementation", "state": "not_started"},
						},
					},
				},
			},
			map[string]any{
				"name":  "llm-verify",
				"kind":  "k8s_job",
				"state": "not_started",
				"jobs": []any{map[string]any{
					"id":    "llm-verify",
					"state": "not_started",
					"steps": []any{map[string]any{"slug": "clone", "state": "not_started"}},
				}},
			},
		},
	}

	reason := canonicalExecutionFailureReason(`forward_dispatch_failed: phase "llm-work" input "claude_ca_namespace" refs phase "env-prep" which has no captured outputs on this run`)
	finalizeExecutionFailureRaw(raw, reason, now)

	work := rawPhase(t, raw, "llm-work")
	if got := stringValue(work["state"]); got != "failed" {
		t.Fatalf("llm-work state=%q", got)
	}
	if got := stringValue(work["reason"]); got != "dispatch_failed" {
		t.Fatalf("llm-work reason=%q", got)
	}
	for _, id := range []string{"llm-test-plan", "llm-implement"} {
		job := rawJob(t, work, id)
		if got := stringValue(job["state"]); got != "failed" {
			t.Fatalf("%s state=%q", id, got)
		}
		if got := stringValue(job["reason"]); got != "dispatch_failed" {
			t.Fatalf("%s reason=%q", id, got)
		}
		for _, stepValue := range job["steps"].([]any) {
			step := stepValue.(map[string]any)
			if got := stringValue(step["state"]); got != "not_started" {
				t.Fatalf("%s step %#v should remain not_started", id, step)
			}
			if _, ok := step["reason"]; ok {
				t.Fatalf("%s step should not have failure reason: %#v", id, step)
			}
			if _, ok := step["completed_at"]; ok {
				t.Fatalf("%s step should not be completed: %#v", id, step)
			}
		}
	}
	verify := rawPhase(t, raw, "llm-verify")
	if got := stringValue(verify["state"]); got != "skipped" {
		t.Fatalf("llm-verify state=%q", got)
	}
}

func rawPhase(t *testing.T, raw map[string]any, name string) map[string]any {
	t.Helper()
	for _, value := range raw["phase_executions"].([]any) {
		phase := value.(map[string]any)
		if stringValue(phase["name"]) == name {
			return phase
		}
	}
	t.Fatalf("missing phase %s", name)
	return nil
}

func rawJob(t *testing.T, phase map[string]any, id string) map[string]any {
	t.Helper()
	for _, value := range phase["jobs"].([]any) {
		job := value.(map[string]any)
		if stringValue(job["id"]) == id {
			return job
		}
	}
	t.Fatalf("missing job %s", id)
	return nil
}

func rawStep(t *testing.T, job map[string]any, slug string) map[string]any {
	t.Helper()
	for _, value := range job["steps"].([]any) {
		step := value.(map[string]any)
		if stringValue(step["slug"]) == slug {
			return step
		}
	}
	t.Fatalf("missing step %s", slug)
	return nil
}

func TestWorkflowFromDocConvertsNestedShape(t *testing.T) {
	raw := []byte(`{
		"id": "issue-agent",
		"project": "ambience",
		"name": "issue-agent",
		"phases": [
			{
				"name": "plan",
				"kind": "k8s_job",
				"workflowRef": "main",
				"outputs": ["plan"],
				"jobs": [
					{
						"id": "plan",
						"name": "Plan",
						"image": "python:3.12",
						"command": ["python"],
						"args": ["-V"],
						"env": {"A": "B"},
						"steps": [{"slug": "run", "title": "Run"}],
						"timeoutSeconds": 60
					}
				]
			},
			{
				"name": "agent",
				"kind": "k8s_job",
				"workflowFilename": "k8s_job:agent",
				"dependsOn": ["plan"],
				"verify": true,
				"recyclePolicy": {"maxAttempts": 4, "on": ["verify_fail"], "landsAt": "self"}
			},
			{
				"name": "cleanup",
				"runOn": "always",
				"purpose": "teardown",
				"dependsOn": ["agent"]
			}
		],
		"pr": {"enabled": true},
		"budget": {"total": 40},
		"defaultRequirements": {"gpu": "none"},
		"metadata": {"kind": "primary"},
		"createdAt": "2026-05-11T03:00:00Z"
	}`)
	var doc workflowDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode doc: %v", err)
	}

	workflow := workflowFromDoc(doc)

	if workflow.Budget.Total != 40 {
		t.Fatalf("Budget=%#v", workflow.Budget)
	}
	if len(workflow.Phases) != 3 {
		t.Fatalf("len(phases)=%d", len(workflow.Phases))
	}
	if workflow.Phases[1].DependsOn[0] != "plan" {
		t.Fatalf("agent depends_on=%#v", workflow.Phases[1].DependsOn)
	}
	if len(workflow.Phases[2].DependsOn) != 1 || workflow.Phases[2].DependsOn[0] != "agent" {
		t.Fatalf("cleanup depends_on=%#v", workflow.Phases[2].DependsOn)
	}
	if workflow.Phases[1].RecyclePolicy.MaxAttempts != 4 {
		t.Fatalf("RecyclePolicy=%#v", workflow.Phases[1].RecyclePolicy)
	}
	if workflow.Phases[0].Jobs[0].Steps[0].Slug != "run" {
		t.Fatalf("jobs=%#v", workflow.Phases[0].Jobs)
	}
}

func TestWorkflowFromDocDoesNotInferDependsOn(t *testing.T) {
	doc := workflowDoc{
		ID:      "parallel",
		Project: "ambience",
		Name:    "parallel",
		Phases: []phaseDoc{
			{Name: "a"},
			{Name: "b"},
			{Name: "verify", DependsOn: []string{"a", "b"}},
		},
	}

	workflow := workflowFromDoc(doc)

	if len(workflow.Phases[1].DependsOn) != 0 {
		t.Fatalf("workflowFromDoc should not infer b depends_on: %#v", workflow.Phases[1].DependsOn)
	}
	if len(workflow.Phases[2].DependsOn) != 2 {
		t.Fatalf("verify depends_on=%#v", workflow.Phases[2].DependsOn)
	}
}

func TestNormalizeWorkflowRegisterForProjectDefaultsToK8sJob(t *testing.T) {
	req := server.WorkflowRegister{
		Project: "glimmung",
		Name:    "agent-run",
		Phases: []server.PhaseSpec{
			{Name: "prepare"},
			{Name: "test", Verify: true, DependsOn: []string{"prepare"}},
			{Name: "cleanup_early", RunOn: server.PhaseRunOnAlways, Purpose: server.PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"test"}, Jobs: []server.NativeJobSpec{{ID: "cleanup-early"}}},
			{Name: "touchpoint", RunOn: server.PhaseRunOnSuccess, Purpose: server.PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []server.NativeJobSpec{{ID: "pr-touchpoint", Primitive: "pr_touchpoint"}}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", DependsOn: []string{"touchpoint"}, Jobs: []server.NativeJobSpec{{ID: "pr-merge", Primitive: "pr_merge"}}},
			{Name: "cleanup_final", RunOn: server.PhaseRunOnAlways, Purpose: server.PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []server.NativeJobSpec{{ID: "cleanup-final"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	for _, phase := range req.Phases {
		// touchpoint_gate is the second valid kind alongside k8s_job
		// (introduced by the gated-workflow migration).
		if phase.Kind != "k8s_job" && phase.Kind != "touchpoint_gate" {
			t.Fatalf("phase %q kind=%q, want k8s_job or touchpoint_gate", phase.Name, phase.Kind)
		}
	}
	if err := validateWorkflowRegister(req); err != nil {
		t.Fatalf("validateWorkflowRegister: %v", err)
	}
}

func TestValidateWorkflowRegisterRejectsNonNativeKind(t *testing.T) {
	req := server.WorkflowRegister{
		Project: "glimmung",
		Name:    "agent-run",
		Phases: []server.PhaseSpec{
			{Name: "prepare", Kind: "container"},
			{Name: "test", Kind: "k8s_job", Verify: true, DependsOn: []string{"prepare"}},
			{Name: "cleanup", Kind: "k8s_job", RunOn: server.PhaseRunOnAlways, Purpose: server.PhasePurposeTeardown, DependsOn: []string{"test"}},
		},
	}

	if err := validateWorkflowRegister(req); err == nil {
		t.Fatal("validateWorkflowRegister succeeded, want error")
	}
}

func TestLeaseFromDocConvertsStateSnapshotShape(t *testing.T) {
	raw := []byte(`{
		"id": "lease-1",
		"leaseNumber": 17,
		"project": "ambience",
		"workflow": "agent-run",
		"host": "runner-1",
		"state": "claimed",
		"requirements": {"size": "large"},
		"metadata": {
			"native_slot_name": "ambience-slot-1",
			"requester": {
				"consumer": "glimmung",
				"kind": "run",
				"ref": "glimmung#1/runs/2",
				"metadata": {"run_id": "2"}
			}
		},
		"requestedAt": "2026-05-11T03:00:00Z",
		"assignedAt": "2026-05-11T03:01:00Z",
		"releasedAt": null,
		"ttlSeconds": 900
	}`)
	var doc leaseDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode doc: %v", err)
	}

	lease := leaseFromDoc(doc)

	if lease.LeaseNumber == nil || *lease.LeaseNumber != 17 {
		t.Fatalf("LeaseNumber=%v", lease.LeaseNumber)
	}
	if lease.AssignedAt == nil || lease.ReleasedAt != nil {
		t.Fatalf("lease times=%#v", lease)
	}
	if lease.Metadata["native_slot_name"] != "ambience-slot-1" {
		t.Fatalf("metadata=%#v", lease.Metadata)
	}
}

func TestSetNativeSlotMetadataUsesDeterministicQueueName(t *testing.T) {
	metadata := map[string]any{
		"native_slot_name": "ambience-slot-99",
	}

	setNativeSlotMetadata(metadata, "ambience", 2, "ambience-slot")

	if metadata["native_slot_index"] != "2" {
		t.Fatalf("native_slot_index=%#v", metadata["native_slot_index"])
	}
	if metadata["native_slot_name"] != "ambience-slot-2" {
		t.Fatalf("metadata=%#v", metadata)
	}
}

func TestValidateNativeLeaseSlotIdentityRejectsCallerSuppliedFields(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{
			name:     "top-level slot index",
			metadata: map[string]any{"native_slot_index": "2"},
			want:     "native_slot_index",
		},
		{
			name:     "top-level slot name",
			metadata: map[string]any{"native_slot_name": "tank-slot-2"},
			want:     "native_slot_name",
		},
		{
			name:     "top-level slot prefix",
			metadata: map[string]any{"native_slot_prefix": "tank-slot"},
			want:     "native_slot_prefix",
		},
		{
			name:     "phase input preferred slot",
			metadata: map[string]any{"phase_inputs": map[string]any{"validation_slot_index": "2"}},
			want:     "phase_inputs.validation_slot_index",
		},
		{
			name:     "test slot phase inputs",
			metadata: map[string]any{"test_slot_checkout": true, "phase_inputs": map[string]any{"target": "provision"}},
			want:     "test-slot checkout lease requests may not include phase_inputs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNativeLeaseSlotIdentity(tc.metadata)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			if _, ok := err.(server.ValidationError); !ok {
				t.Fatalf("err type=%T, want server.ValidationError", err)
			}
		})
	}
}

func TestListedLeaseFromDocSkipsLeaseNumberCounters(t *testing.T) {
	cases := []leaseDoc{
		{
			ID:      leaseCounterPrefix + "ambience",
			Kind:    "lease_number_counter",
			Project: "ambience",
		},
		{
			ID:      leaseCounterPrefix + "ambience",
			Project: "ambience",
		},
		{
			ID:      "old-counter",
			Kind:    "lease_number_counter",
			Project: "ambience",
		},
	}
	for _, tc := range cases {
		if lease, ok := listedLeaseFromDoc(tc); ok {
			t.Fatalf("counter doc listed as lease: %#v", lease)
		}
	}

	lease, ok := listedLeaseFromDoc(leaseDoc{
		ID:           "lease-1",
		LeaseNumber:  intPtr(1),
		Project:      "ambience",
		State:        "claimed",
		RequestedAt:  "2026-05-11T03:00:00Z",
		Requirements: map[string]any{},
		Metadata:     map[string]any{},
	})
	if !ok || lease.ID != "lease-1" {
		t.Fatalf("real lease not listed: ok=%v lease=%#v", ok, lease)
	}
}

func TestRunReportsFromDocsBuildsPublicRefsAndAttempts(t *testing.T) {
	cost := 1.25
	completed := "2026-05-11T03:05:00Z"
	docs := []runDoc{
		{
			ID:                "new",
			Project:           "glimmung",
			Workflow:          "issue-agent",
			RunNumber:         intPtr(2),
			IssueRepo:         "nelsong6/glimmung",
			IssueNumber:       141,
			State:             "passed",
			CumulativeCostUSD: 3.5,
			CreatedAt:         "2026-05-11T03:00:00Z",
			UpdatedAt:         "2026-05-11T03:06:00Z",
			Attempts: []attemptDoc{{
				AttemptIndex:     0,
				Phase:            "implement",
				PhaseKind:        "k8s_job",
				WorkflowFilename: "k8s_job:implement",
				DispatchedAt:     "2026-05-11T03:01:00Z",
				CompletedAt:      completed,
				Conclusion:       stringPtr("success"),
				Verification:     &verificationDoc{Status: "pass", EvidenceRefs: []string{"blob://evidence"}, CostUSD: 2.5},
				CostUSD:          &cost,
				JobCompletions: map[string]nativeJobCompletionDoc{
					"agent": {
						JobID:       "agent",
						CompletedAt: completed,
						Conclusion:  "success",
						Verification: &verificationDoc{
							Status:  "pass",
							Reasons: []string{"tests passed"},
						},
						CostUSD:      1.1,
						PhaseOutputs: map[string]string{"validation_url": "https://preview.example"},
					},
				},
			}},
		},
		{
			ID:          "old",
			Project:     "glimmung",
			Workflow:    "issue-agent",
			IssueRepo:   "nelsong6/glimmung",
			IssueNumber: 141,
			State:       "in_progress",
			CreatedAt:   "2026-05-11T02:00:00Z",
			UpdatedAt:   "2026-05-11T02:00:00Z",
		},
	}

	reports := runReportsFromDocs(docs)

	if reports[0].RunRef != "glimmung#141/runs/2" || reports[0].Ref != "glimmung#141/runs/2/report" {
		t.Fatalf("new refs=%#v", reports[0])
	}
	if reports[1].RunRef != "glimmung#141/runs/1" {
		t.Fatalf("numberless fallback ref=%#v", reports[1])
	}
	if reports[0].AttemptsCount != 1 || reports[0].CurrentPhase == nil || *reports[0].CurrentPhase != "implement" {
		t.Fatalf("attempt summary=%#v", reports[0])
	}
	if reports[0].Attempts[0].VerificationStatus == nil || *reports[0].Attempts[0].VerificationStatus != "pass" {
		t.Fatalf("verification=%#v", reports[0].Attempts[0])
	}
	if reports[0].CompletedAt == nil {
		t.Fatalf("completed_at missing: %#v", reports[0])
	}
	if len(reports[0].Attempts[0].JobCompletions) != 1 || reports[0].Attempts[0].JobCompletions[0].JobID != "agent" {
		t.Fatalf("job completions=%#v", reports[0].Attempts[0].JobCompletions)
	}
	if reports[0].Attempts[0].JobCompletions[0].VerificationStatus == nil || *reports[0].Attempts[0].JobCompletions[0].VerificationStatus != "pass" {
		t.Fatalf("job verification=%#v", reports[0].Attempts[0].JobCompletions[0])
	}
}

func TestRunReportAttemptFallsBackToVerificationPhaseOutputEvidenceRefs(t *testing.T) {
	completed := "2026-05-11T03:05:00Z"
	docs := []runDoc{{
		ID:          "run-1",
		Project:     "ambience",
		Workflow:    "default",
		RunNumber:   intPtr(4),
		IssueRepo:   "nelsong6/ambience",
		IssueNumber: 171,
		State:       "review_required",
		CreatedAt:   "2026-05-11T03:00:00Z",
		UpdatedAt:   "2026-05-11T03:06:00Z",
		Attempts: []attemptDoc{{
			AttemptIndex:     2,
			Phase:            "llm-verify",
			PhaseKind:        "k8s_job",
			WorkflowFilename: "k8s_job:llm-verify",
			DispatchedAt:     "2026-05-11T03:01:00Z",
			CompletedAt:      completed,
			Conclusion:       stringPtr("success"),
			Verification:     &verificationDoc{Status: "pass"},
			PhaseOutputs: map[string]string{
				"verification": `{"schema_version":1,"status":"pass","evidence_refs":["screenshots/default.png",""]}`,
			},
		}},
	}}

	reports := runReportsFromDocs(docs)

	refs := reports[0].Attempts[0].EvidenceRefs
	if len(refs) != 1 || refs[0] != "screenshots/default.png" {
		t.Fatalf("evidence refs=%#v", refs)
	}
}

func TestAggregateNativePhaseCompletionPreservesEvidenceRefs(t *testing.T) {
	payload := aggregateNativePhaseCompletion([]string{"verify"}, map[string]nativeJobCompletionDoc{
		"verify": {
			JobID:      "verify",
			Conclusion: "success",
			Verification: &verificationDoc{
				Status:       "pass",
				EvidenceRefs: []string{"screenshots/default.png"},
			},
		},
	})

	if len(payload.EvidenceRefs) != 1 || payload.EvidenceRefs[0] != "screenshots/default.png" {
		t.Fatalf("evidence refs=%#v", payload.EvidenceRefs)
	}
}

func TestIssueDetailFromDocBuildsPublicShape(t *testing.T) {
	doc := issueDoc{
		ID:      "01KISSUE",
		Number:  17,
		Project: "glimmung",
		Title:   "Fix dashboard",
		Body:    "details",
		Labels:  []string{"bug"},
		State:   "open",
		Comments: []issueCommentDoc{{
			ID:        "comment-1",
			Author:    "admin@example.com",
			Body:      "looking",
			CreatedAt: "2026-05-11T05:00:00Z",
			UpdatedAt: "2026-05-11T05:00:00Z",
		}},
	}

	detail := issueDetailFromDoc(doc)

	if detail.Ref != "glimmung#17" || detail.Number == nil || *detail.Number != 17 {
		t.Fatalf("detail refs=%#v", detail)
	}
	if detail.Repo != nil || detail.HTMLURL != nil {
		t.Fatalf("github fields should be nil: %#v", detail)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].ID != "comment-1" {
		t.Fatalf("comments=%#v", detail.Comments)
	}
}

func TestIssueDocFromPGPayloadStripsCommentsForListPath(t *testing.T) {
	payload, err := json.Marshal(issueDoc{
		ID:       "issue-17",
		Number:   17,
		Project:  "glimmung",
		Title:    "Fix dashboard",
		State:    "open",
		Comments: []issueCommentDoc{{ID: "comment-1", Body: "detail-only"}},
	})
	if err != nil {
		t.Fatalf("marshal issue payload: %v", err)
	}

	doc, err := issueDocFromPGPayload(pgstore.IssueRow{Payload: payload})
	if err != nil {
		t.Fatalf("issueDocFromPGPayload: %v", err)
	}

	if len(doc.Comments) != 0 {
		t.Fatalf("list payload should not hydrate comments: %#v", doc.Comments)
	}
	if doc.ID != "issue-17" || doc.Project != "glimmung" || doc.Number != 17 {
		t.Fatalf("doc=%#v", doc)
	}
}

func TestCanonicalIssueDocsRequiresIssueShape(t *testing.T) {
	docs := []issueDoc{
		{ID: "issue-17", Project: "ambience", Number: 17, Title: "real issue", State: "open"},
		{ID: "__counter:issue-number:ambience", Project: "ambience"},
		{ID: "portfolio-element", Project: "ambience"},
		{ID: "missing-project", Number: 18},
		{ID: "numbered-non-issue", Project: "ambience", Number: 19, State: "pending"},
	}

	filtered := canonicalIssueDocs(docs)

	if len(filtered) != 1 || filtered[0].ID != "issue-17" {
		t.Fatalf("filtered=%#v", filtered)
	}
}

func TestIssueRunContextMapsLatestRunAndNeedsAttention(t *testing.T) {
	runNumber := 2
	docs := []runDoc{
		{
			ID:          "old",
			Project:     "glimmung",
			Workflow:    "issue-agent",
			IssueID:     "issue-1",
			IssueNumber: 17,
			State:       "in_progress",
			CreatedAt:   "2026-05-11T04:00:00Z",
		},
		{
			ID:          "new",
			Project:     "glimmung",
			Workflow:    "issue-agent",
			RunNumber:   &runNumber,
			IssueID:     "issue-1",
			IssueNumber: 17,
			State:       "review_required",
			CreatedAt:   "2026-05-11T05:00:00Z",
		},
	}

	ctx := issueRunContext(docs)
	latest := ctx.latestByIssueID["issue-1"]
	if latest == nil || latest.ID != "new" {
		t.Fatalf("latest=%#v", latest)
	}
	row := serverIssueRowForTest("glimmung#17", "review_required")
	if !issueRowNeedsAttention(row) {
		t.Fatalf("row should need attention: %#v", row)
	}
	row.IssueLockHeld = true
	if issueRowNeedsAttention(row) {
		t.Fatalf("locked row should not need attention: %#v", row)
	}
}

func TestCancelLeaseCandidateRankPrefersActiveLease(t *testing.T) {
	claimed := leaseDoc{State: "claimed"}
	released := leaseDoc{State: "released"}
	waiting := leaseDoc{State: "waiting"}

	if cancelLeaseCandidateRank(claimed) >= cancelLeaseCandidateRank(released) {
		t.Fatal("claimed lease should rank ahead of released lease")
	}
	if cancelLeaseCandidateRank(waiting) >= cancelLeaseCandidateRank(released) {
		t.Fatal("waiting lease should rank ahead of released lease")
	}
}

func TestNativeProjectConcurrencyCapDefaultAndOverride(t *testing.T) {
	store := &Store{}
	if got := store.nativeProjectCap(); got != 5 {
		t.Fatalf("project cap=%d, want 5", got)
	}

	store.nativeProjectConcurrency = 2
	if got := store.nativeProjectCap(); got != 2 {
		t.Fatalf("project cap override=%d, want 2", got)
	}
}

func TestSelectLeaseDocByPublicRefSkipsCountersAndPrefersActive(t *testing.T) {
	docs := []leaseDoc{
		{
			ID:      leaseCounterPrefix + "ambience",
			Kind:    "lease_number_counter",
			Project: "ambience",
		},
		{
			ID:         "released-old",
			Project:    "ambience",
			State:      "released",
			ReleasedAt: "2026-05-11T04:00:00Z",
		},
		{
			ID:          "claimed-old",
			Project:     "ambience",
			State:       "claimed",
			RequestedAt: "2026-05-11T05:00:00Z",
		},
	}

	found := selectLeaseDocByPublicRef(docs, "ambience/lease")

	if found == nil || found.ID != "claimed-old" {
		t.Fatalf("selected=%#v, want claimed lease", found)
	}
}

func intPtr(value int) *int {
	return &value
}

func stringPtr(value string) *string {
	return &value
}

func serverIssueRowForTest(ref string, state string) server.IssueRow {
	return server.IssueRow{
		Ref:          ref,
		Project:      "glimmung",
		Number:       intPtr(17),
		Title:        "Fix",
		State:        "open",
		LastRunRef:   &ref,
		LastRunState: &state,
	}
}

func TestAppendInnerJobRegistrationDeduplicates(t *testing.T) {
	phase := map[string]any{"name": "verify"}
	doc := nativeEventDoc{
		JobID:    "llm-verify",
		Event:    "inner_job_registered",
		StepSlug: "run-verification",
		Metadata: map[string]any{
			"namespace": "ambience-slot-3",
			"job_name":  "agent-ve-2",
			"intent":    "verification_agent",
			"label":     "verify-agent",
		},
		CreatedAt: "2026-05-29T01:00:00Z",
	}

	appendInnerJobRegistration(phase, doc)
	appendInnerJobRegistration(phase, doc) // idempotent replay

	innerJobs, _ := phase["inner_jobs"].([]any)
	if len(innerJobs) != 1 {
		t.Fatalf("inner_jobs=%d, want 1 after dedupe", len(innerJobs))
	}
	first := innerJobs[0].(map[string]any)
	if first["namespace"] != "ambience-slot-3" || first["job_name"] != "agent-ve-2" {
		t.Fatalf("inner job=%#v", first)
	}
	if first["intent"] != "verification_agent" {
		t.Fatalf("intent=%q", first["intent"])
	}
	if first["state"] != "active" {
		t.Fatalf("initial state=%q, want active", first["state"])
	}
	if first["parent_step_slug"] != "run-verification" {
		t.Fatalf("parent_step_slug=%q", first["parent_step_slug"])
	}
}

func TestUpdateInnerJobTerminationMatchesExistingRegistration(t *testing.T) {
	phase := map[string]any{"name": "verify"}
	regDoc := nativeEventDoc{
		JobID:    "llm-verify",
		Event:    "inner_job_registered",
		StepSlug: "run-verification",
		Metadata: map[string]any{
			"namespace": "ambience-slot-3",
			"job_name":  "agent-ve-2",
			"intent":    "verification_agent",
		},
		CreatedAt: "2026-05-29T01:00:00Z",
	}
	appendInnerJobRegistration(phase, regDoc)

	termDoc := nativeEventDoc{
		JobID: "llm-verify",
		Event: "inner_job_terminated",
		Metadata: map[string]any{
			"namespace":    "ambience-slot-3",
			"job_name":     "agent-ve-2",
			"state":        "succeeded",
			"reason":       "",
			"completed_at": "2026-05-29T01:05:00Z",
		},
		CreatedAt: "2026-05-29T01:05:05Z",
	}
	updateInnerJobTermination(phase, termDoc)

	innerJobs, _ := phase["inner_jobs"].([]any)
	if len(innerJobs) != 1 {
		t.Fatalf("inner_jobs=%d, want 1", len(innerJobs))
	}
	ref := innerJobs[0].(map[string]any)
	if ref["state"] != "succeeded" {
		t.Fatalf("state=%q, want succeeded", ref["state"])
	}
	if ref["completed_at"] != "2026-05-29T01:05:00Z" {
		t.Fatalf("completed_at=%q", ref["completed_at"])
	}
}

func TestUpdateInnerJobTerminationSynthesizesStubWhenUnregistered(t *testing.T) {
	phase := map[string]any{"name": "verify"}
	termDoc := nativeEventDoc{
		JobID: "llm-verify",
		Event: "inner_job_terminated",
		Metadata: map[string]any{
			"namespace":    "ambience-slot-3",
			"job_name":     "agent-zombie",
			"state":        "failed",
			"reason":       "deadline_exceeded",
			"completed_at": "2026-05-29T01:05:00Z",
		},
		CreatedAt: "2026-05-29T01:05:05Z",
	}
	updateInnerJobTermination(phase, termDoc)

	innerJobs, _ := phase["inner_jobs"].([]any)
	if len(innerJobs) != 1 {
		t.Fatalf("inner_jobs=%d, want stub", len(innerJobs))
	}
	stub := innerJobs[0].(map[string]any)
	if stub["state"] != "failed" || stub["reason"] != "deadline_exceeded" {
		t.Fatalf("stub=%#v", stub)
	}
	if stub["intent"] != "unknown" {
		t.Fatalf("stub intent=%q, want unknown", stub["intent"])
	}
}

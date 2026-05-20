package cosmos

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/server"
)

func TestProjectFromDocConvertsCamelCaseFields(t *testing.T) {
	raw := []byte(`{
		"id": "ambience",
		"name": "ambience",
		"githubRepo": "nelsong6/ambience",
		"argocdApp": "ambience",
		"metadata": {"tier": "app"},
		"createdAt": "2026-05-11T03:00:00Z"
	}`)
	var doc projectDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode doc: %v", err)
	}

	project := projectFromDoc(doc)

	if project.GitHubRepo != "nelsong6/ambience" {
		t.Fatalf("GitHubRepo=%q", project.GitHubRepo)
	}
	if project.Metadata["tier"] != "app" {
		t.Fatalf("metadata=%#v", project.Metadata)
	}
	if project.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be populated")
	}
}

func TestSortNativeEventDocsOrdersInMemory(t *testing.T) {
	docs := []nativeEventDoc{
		{AttemptIndex: 1, JobID: "verify", Seq: 2, CreatedAt: "2026-05-19T00:00:04Z"},
		{AttemptIndex: 0, JobID: "implement", Seq: 2, CreatedAt: "2026-05-19T00:00:03Z"},
		{AttemptIndex: 0, JobID: "implement", Seq: 1, CreatedAt: "2026-05-19T00:00:02Z"},
		{AttemptIndex: 0, JobID: "plan", Seq: 1, CreatedAt: "2026-05-19T00:00:01Z"},
	}

	sortNativeEventDocs(docs)

	got := []string{
		fmt.Sprintf("%s:%d", docs[0].JobID, docs[0].Seq),
		fmt.Sprintf("%s:%d", docs[1].JobID, docs[1].Seq),
		fmt.Sprintf("%s:%d", docs[2].JobID, docs[2].Seq),
		fmt.Sprintf("%s:%d", docs[3].JobID, docs[3].Seq),
	}
	want := []string{"implement:1", "implement:2", "plan:1", "verify:2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order=%v, want %v", got, want)
	}
}

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
				"always": true,
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
			{Name: "cleanup", Always: true, DependsOn: []string{"test"}},
		},
	}
	project := projectDoc{
		Name:     "glimmung",
		Metadata: map[string]any{"app_type": "native_web_app"},
	}

	normalizeWorkflowRegisterForProjectDoc(&req, project)

	for _, phase := range req.Phases {
		if phase.Kind != "k8s_job" {
			t.Fatalf("phase %q kind=%q, want k8s_job", phase.Name, phase.Kind)
		}
	}
	if err := validateWorkflowForProject(project, req); err != nil {
		t.Fatalf("validateWorkflowForProject: %v", err)
	}
}

func TestValidateWorkflowForProjectRejectsNonNativeKind(t *testing.T) {
	req := server.WorkflowRegister{
		Project: "glimmung",
		Name:    "agent-run",
		Phases: []server.PhaseSpec{
			{Name: "prepare", Kind: "container"},
			{Name: "test", Kind: "k8s_job", Verify: true, DependsOn: []string{"prepare"}},
			{Name: "cleanup", Kind: "k8s_job", Always: true, DependsOn: []string{"test"}},
		},
	}
	project := projectDoc{
		Name:     "glimmung",
		Metadata: map[string]any{"app_type": "native_web_app"},
	}

	if err := validateWorkflowForProject(project, req); err == nil {
		t.Fatal("validateWorkflowForProject succeeded, want error")
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

func TestNativeConcurrencyCapsDefaultAndOverride(t *testing.T) {
	store := &Store{}
	if got := store.nativeGlobalCap(); got != 5 {
		t.Fatalf("global cap=%d, want 5", got)
	}
	if got := store.nativeProjectCap(); got != 5 {
		t.Fatalf("project cap=%d, want 5", got)
	}

	store.nativeGlobalConcurrency = 7
	store.nativeProjectConcurrency = 2
	if got := store.nativeGlobalCap(); got != 7 {
		t.Fatalf("global cap override=%d, want 7", got)
	}
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

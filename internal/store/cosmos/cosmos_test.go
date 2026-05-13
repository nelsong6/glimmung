package cosmos

import (
	"encoding/json"
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

func TestWorkflowFromDocConvertsNestedShapeAndInfersSequentialDependsOn(t *testing.T) {
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
				"kind": "gha_dispatch",
				"workflowFilename": "agent.yml",
				"verify": true,
				"recyclePolicy": {"maxAttempts": 4, "on": ["verify_fail"], "landsAt": "self"}
			},
			{
				"name": "cleanup",
				"always": true
			}
		],
		"pr": {"enabled": true},
		"budget": {"total": 40},
		"triggerLabel": "agent",
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
	if workflow.TriggerLabel == nil || *workflow.TriggerLabel != "agent" {
		t.Fatalf("TriggerLabel=%v", workflow.TriggerLabel)
	}
	if len(workflow.Phases) != 3 {
		t.Fatalf("len(phases)=%d", len(workflow.Phases))
	}
	if workflow.Phases[1].DependsOn[0] != "plan" {
		t.Fatalf("agent depends_on=%#v", workflow.Phases[1].DependsOn)
	}
	if len(workflow.Phases[2].DependsOn) != 2 {
		t.Fatalf("cleanup depends_on=%#v", workflow.Phases[2].DependsOn)
	}
	if workflow.Phases[1].RecyclePolicy.MaxAttempts != 4 {
		t.Fatalf("RecyclePolicy=%#v", workflow.Phases[1].RecyclePolicy)
	}
	if workflow.Phases[0].Jobs[0].Steps[0].Slug != "run" {
		t.Fatalf("jobs=%#v", workflow.Phases[0].Jobs)
	}
}

func TestWorkflowFromDocRespectsExplicitDependsOn(t *testing.T) {
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
		t.Fatalf("explicit graph should not infer b depends_on: %#v", workflow.Phases[1].DependsOn)
	}
	if len(workflow.Phases[2].DependsOn) != 2 {
		t.Fatalf("verify depends_on=%#v", workflow.Phases[2].DependsOn)
	}
}

func TestNormalizeWorkflowRegisterForNativeWebappProjectDefaultsToK8sJob(t *testing.T) {
	req := server.WorkflowRegister{
		Project: "glimmung",
		Name:    "agent-run",
		Phases: []server.PhaseSpec{
			{Name: "prepare"},
			{Name: "test", Verify: true},
			{Name: "cleanup", Always: true},
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

func TestValidateWorkflowForNativeWebappProjectRejectsExplicitGHA(t *testing.T) {
	req := server.WorkflowRegister{
		Project: "glimmung",
		Name:    "agent-run",
		Phases: []server.PhaseSpec{
			{Name: "prepare", Kind: "gha_dispatch"},
			{Name: "test", Kind: "k8s_job", Verify: true},
			{Name: "cleanup", Kind: "k8s_job", Always: true},
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

func TestHostFromDocConvertsLeaseAndTimes(t *testing.T) {
	raw := []byte(`{
		"id": "runner-1",
		"name": "runner-1",
		"capabilities": {"gpu": "none"},
		"currentLeaseId": "lease-1",
		"lastHeartbeat": "2026-05-11T03:00:00Z",
		"lastUsedAt": "2026-05-11T02:00:00Z",
		"drained": true,
		"createdAt": "2026-05-10T03:00:00Z"
	}`)
	var doc hostDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode doc: %v", err)
	}

	host := hostFromDoc(doc)

	if host.CurrentLeaseID == nil || *host.CurrentLeaseID != "lease-1" {
		t.Fatalf("CurrentLeaseID=%v", host.CurrentLeaseID)
	}
	if host.LastHeartbeat == nil || host.LastUsedAt == nil {
		t.Fatalf("host times=%#v", host)
	}
	if !host.Drained {
		t.Fatal("Drained=false, want true")
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
			ID:      "legacy-counter",
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
	workflowRunID := int64(123)
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
				PhaseKind:        "gha_dispatch",
				WorkflowFilename: "agent.yml",
				WorkflowRunID:    &workflowRunID,
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
		t.Fatalf("legacy fallback ref=%#v", reports[1])
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
		t.Fatalf("legacy github fields should be nil: %#v", detail)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].ID != "comment-1" {
		t.Fatalf("comments=%#v", detail.Comments)
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
	pending := leaseDoc{State: "pending"}

	if cancelLeaseCandidateRank(claimed) >= cancelLeaseCandidateRank(released) {
		t.Fatal("claimed lease should rank ahead of released lease")
	}
	if cancelLeaseCandidateRank(pending) >= cancelLeaseCandidateRank(released) {
		t.Fatal("pending lease should rank ahead of released lease")
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
			ID:         "released-legacy",
			Project:    "ambience",
			State:      "released",
			ReleasedAt: "2026-05-11T04:00:00Z",
		},
		{
			ID:          "claimed-legacy",
			Project:     "ambience",
			State:       "claimed",
			RequestedAt: "2026-05-11T05:00:00Z",
		},
	}

	found := selectLeaseDocByPublicRef(docs, "ambience/lease")

	if found == nil || found.ID != "claimed-legacy" {
		t.Fatalf("selected=%#v, want claimed legacy lease", found)
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

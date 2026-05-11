package cosmos

import (
	"encoding/json"
	"testing"
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

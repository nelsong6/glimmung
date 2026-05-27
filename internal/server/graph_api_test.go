package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type fakeGraphStore struct {
	fakeReadStore
	issue       IssueDetail
	issues      []IssueRow
	runs        []RunReport
	touchpoints []TouchpointRow
	signals     []GraphSignal
	nativeLogs  NativeRunLogsResponse
}

func (s fakeGraphStore) ListIssues(context.Context, IssueListFilter) ([]IssueRow, error) {
	return s.issues, nil
}

func (s fakeGraphStore) GetIssueDetailByNumber(_ context.Context, project string, number int) (IssueDetail, error) {
	if s.issue.Project == project && s.issue.Number != nil && *s.issue.Number == number {
		return s.issue, nil
	}
	return IssueDetail{}, ErrNotFound
}

func (s fakeGraphStore) ArchiveIssueByNumber(context.Context, IssueArchive) (IssueDetail, error) {
	return IssueDetail{}, ErrUnsupported
}

func (s fakeGraphStore) CreateIssue(context.Context, IssueCreate) (IssueDetail, error) {
	return IssueDetail{}, ErrUnsupported
}

func (s fakeGraphStore) PatchIssueByNumber(context.Context, IssuePatch) (IssueDetail, error) {
	return IssueDetail{}, ErrUnsupported
}

func (s fakeGraphStore) AddIssueComment(context.Context, IssueCommentAdd) (IssueComment, error) {
	return IssueComment{}, ErrUnsupported
}

func (s fakeGraphStore) UpdateIssueComment(context.Context, IssueCommentUpdate) (IssueComment, error) {
	return IssueComment{}, ErrUnsupported
}

func (s fakeGraphStore) DeleteIssueComment(context.Context, IssueCommentDelete) (IssueDetail, error) {
	return IssueDetail{}, ErrUnsupported
}

func (s fakeGraphStore) ListProjectRuns(_ context.Context, project string, _ int) ([]RunReport, error) {
	out := make([]RunReport, 0, len(s.runs))
	for _, run := range s.runs {
		if run.Project == project {
			out = append(out, run)
		}
	}
	return out, nil
}

func (s fakeGraphStore) GetRunReportByNumber(context.Context, string, int, string) (RunReport, error) {
	return RunReport{}, ErrUnsupported
}

func (s fakeGraphStore) ListTouchpoints(_ context.Context, filter TouchpointListFilter) ([]TouchpointRow, error) {
	out := make([]TouchpointRow, 0, len(s.touchpoints))
	for _, row := range s.touchpoints {
		if filter.Project == "" || row.Project == filter.Project {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s fakeGraphStore) GetTouchpointForIssue(context.Context, string, int) (TouchpointDetail, error) {
	return TouchpointDetail{}, ErrUnsupported
}

func (s fakeGraphStore) EnsureTouchpoint(context.Context, TouchpointCreate) (TouchpointDetail, error) {
	return TouchpointDetail{}, ErrUnsupported
}

func (s fakeGraphStore) ListGraphSignals(context.Context, GraphSignalFilter) ([]GraphSignal, error) {
	return s.signals, nil
}

func (s fakeGraphStore) GetNativeRunStatusByID(context.Context, string, string) (NativeRunStatusResponse, error) {
	return NativeRunStatusResponse{}, ErrUnsupported
}

func (s fakeGraphStore) RecordNativeEventByID(context.Context, string, string, NativeRunEventRequest) (NativeRunEventResult, error) {
	return NativeRunEventResult{}, ErrUnsupported
}

func (s fakeGraphStore) ListNativeEventsByID(context.Context, string, string, *int, *string, *int) (NativeRunLogsResponse, error) {
	return s.nativeLogs, nil
}

func TestIssueGraphByNumberBuildsRunAttemptAndTouchpointNodes(t *testing.T) {
	issueNumber := 17
	runNumber := 1
	runDisplay := "1"
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	runRef := "glimmung#17/runs/1"
	touchpointRef := "nelsong6/glimmung#452"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "glimmung",
			Name:    "agent-run",
			Phases: []PhaseSpec{
				{
					Name:    "env-prep",
					Kind:    "k8s_job",
					Outputs: []string{"validation_url"},
					Jobs: []NativeJobSpec{{
						ID:   "prepare",
						Name: stringPtr("prepare env"),
						Steps: []NativeStepSpec{{
							Slug:  "checkout",
							Title: stringPtr("checkout"),
						}},
					}},
				},
				{Name: "agent-execute", DependsOn: []string{"env-prep"}},
			},
			PR: PrPrimitive{Enabled: true},
		}}},
		issue: IssueDetail{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &issueNumber,
			Title:   "Port graph",
			State:   "open",
			Labels:  []string{"backend"},
		},
		runs: []RunReport{{
			Project:           "glimmung",
			RunRef:            runRef,
			RunNumber:         &runNumber,
			RunDisplayNumber:  &runDisplay,
			Workflow:          "agent-run",
			IssueRef:          stringPtr("glimmung#17"),
			IssueNumber:       &issueNumber,
			State:             "in_progress",
			CurrentPhase:      stringPtr("agent-execute"),
			ValidationURL:     stringPtr("https://preview.example"),
			CumulativeCostUSD: 1.25,
			StartedAt:         now,
			UpdatedAt:         now,
			Attempts: []RunReportAttempt{{
				AttemptIndex:       0,
				Phase:              "env-prep",
				PhaseKind:          "k8s_job",
				WorkflowFilename:   "k8s_job:env-prep",
				DispatchedAt:       now,
				CompletedAt:        &now,
				Conclusion:         stringPtr("success"),
				VerificationStatus: stringPtr("pass"),
				EvidenceRefs:       []string{"blob://artifacts/glimmung/17/verification.json"},
				LogArchiveURL:      stringPtr("blob://artifacts/glimmung/17/native.log"),
				PhaseOutputs:       map[string]string{"validation_url": "https://preview.example"},
				JobCompletions: []RunAttemptJobCompletion{{
					JobID:              "prepare",
					CompletedAt:        &now,
					Conclusion:         "success",
					VerificationStatus: stringPtr("pass"),
				}},
			}},
		}},
		touchpoints: []TouchpointRow{{
			Ref:          touchpointRef,
			Project:      "glimmung",
			Repo:         "nelsong6/glimmung",
			PRNumber:     452,
			Title:        "graph port",
			State:        "ready",
			HTMLURL:      stringPtr("https://github.com/nelsong6/glimmung/pull/452"),
			LinkedRunRef: stringPtr(runRef),
			Evidence: []TouchpointEvidence{{
				Kind:         "screenshot",
				Ref:          "blob://artifacts/runs/glimmung/run-1/screenshots/default.png",
				Label:        "default",
				URL:          "/v1/artifacts/runs/glimmung/run-1/screenshots/default.png",
				ArtifactPath: "runs/glimmung/run-1/screenshots/default.png",
			}},
		}},
		signals: []GraphSignal{{
			ID:         "sig-1",
			TargetType: "run",
			TargetRepo: "glimmung",
			TargetID:   runRef,
			Source:     "glimmung_ui",
			State:      "pending",
			EnqueuedAt: now.Add(time.Minute),
			Payload:    map[string]any{"kind": "reject"},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var graph IssueGraph
	getJSON(t, handler, "/v1/issues/by-number/glimmung/17/graph", &graph)

	if graph.IssueRef != "glimmung#17" {
		t.Fatalf("issue_ref=%q", graph.IssueRef)
	}
	assertGraphNode(t, graph, "issue:glimmung#17", "issue")
	runNode := assertGraphNode(t, graph, "run:"+runRef, "run")
	if runNode.Metadata["workflow"].(string) != "agent-run" {
		t.Fatalf("run metadata=%#v", runNode.Metadata)
	}
	if _, ok := runNode.Metadata["workflow_graph"]; ok {
		t.Fatalf("run metadata should not carry retired workflow_graph topology fallback: %#v", runNode.Metadata)
	}
	assertGraphNode(t, graph, "attempt:"+runRef+":0", "attempt")
	attemptNode := assertGraphNode(t, graph, "attempt:"+runRef+":0", "attempt")
	if got, ok := attemptNode.Metadata["jobs_count"].(float64); !ok || got != 1 {
		t.Fatalf("attempt jobs_count=%#v", got)
	}
	assertGraphNode(t, graph, "pr:"+touchpointRef, "pr")
	assertGraphEdge(t, graph, "run:"+runRef, "pr:"+touchpointRef, "opened")
	assertGraphEdge(t, graph, "run:"+runRef, "signal:glimmung_ui:"+runRef+":"+now.Add(time.Minute).Format(time.RFC3339Nano), "feedback")
	if graph.Projection.IssueRef != "glimmung#17" {
		t.Fatalf("projection issue_ref=%q", graph.Projection.IssueRef)
	}
	if graph.Projection.CurrentRunRef == nil || *graph.Projection.CurrentRunRef != runRef {
		t.Fatalf("current_run_ref=%#v", graph.Projection.CurrentRunRef)
	}
	if graph.Projection.NextAction.Kind != "feedback_pending" {
		t.Fatalf("next action=%#v", graph.Projection.NextAction)
	}
	assertProjectionEdge(t, graph.Projection, "run:"+runRef, "phase:"+runRef+":env-prep", "contains")
	assertProjectionEdge(t, graph.Projection, "phase:"+runRef+":env-prep", "phase:"+runRef+":agent-execute", "depends_on")
	if len(graph.Projection.Runs) != 1 {
		t.Fatalf("projection runs=%#v", graph.Projection.Runs)
	}
	envPhase := assertProjectionPhase(t, graph.Projection.Runs[0], "env-prep")
	if envPhase.State != "succeeded" || len(envPhase.Jobs) != 1 || envPhase.Jobs[0].State != "succeeded" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	if envPhase.Jobs[0].Conclusion == nil || *envPhase.Jobs[0].Conclusion != "success" || envPhase.Jobs[0].CompletedAt == nil {
		t.Fatalf("env-prep job completion=%#v", envPhase.Jobs[0])
	}
	if len(envPhase.Jobs[0].Steps) != 1 || envPhase.Jobs[0].Steps[0].Slug != "checkout" {
		t.Fatalf("env-prep job steps=%#v", envPhase.Jobs[0].Steps)
	}
	executePhase := assertProjectionPhase(t, graph.Projection.Runs[0], "agent-execute")
	if executePhase.State != "dispatching" {
		t.Fatalf("agent-execute state=%q", executePhase.State)
	}
	assertProjectionEvidence(t, graph.Projection.Runs[0], "validation", "https://preview.example")
	assertProjectionEvidence(t, graph.Projection.Runs[0], "artifact", "blob://artifacts/glimmung/17/verification.json")
	assertProjectionEvidence(t, graph.Projection.Runs[0], "log", "blob://artifacts/glimmung/17/native.log")
	assertProjectionEvidence(t, graph.Projection.Runs[0], "screenshot", "blob://artifacts/runs/glimmung/run-1/screenshots/default.png")
	assertProjectionEvidence(t, graph.Projection.Runs[0], "pull_request", "https://github.com/nelsong6/glimmung/pull/452")
	if len(graph.Projection.Signals) != 1 || graph.Projection.Signals[0].Kind != "reject" {
		t.Fatalf("projection signals=%#v", graph.Projection.Signals)
	}
}

func TestRunCycleGraphProjectionUsesCanonicalStateAndNativeActivity(t *testing.T) {
	issueNumber := 17
	runNumber := 1
	cycleNumber := 1
	runCycle := 1
	runDisplay := "1.1"
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	runRef := "glimmung#17/runs/1.1"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "glimmung",
			Name:    "agent-run",
			Phases: []PhaseSpec{
				{
					Name: "env-prep",
					Kind: "k8s_job",
					Jobs: []NativeJobSpec{{
						ID:   "prepare",
						Name: stringPtr("prepare env"),
						Steps: []NativeStepSpec{{
							Slug:  "checkout",
							Title: stringPtr("checkout"),
						}},
					}},
				},
				{
					Name:      "agent-execute",
					Kind:      "k8s_job",
					DependsOn: []string{"env-prep"},
					RecyclePolicy: &RecyclePolicy{
						MaxAttempts: 3,
						On:          []string{"verify_fail", "verify_malformed"},
						LandsAt:     "self",
					},
					Jobs: []NativeJobSpec{{ID: "agent"}},
				},
			},
			PR: PrPrimitive{Enabled: true},
		}}},
		issue: IssueDetail{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &issueNumber,
			Title:   "Port graph",
			State:   "open",
		},
		runs: []RunReport{{
			ID:                  "run-1",
			Project:             "glimmung",
			RunRef:              runRef,
			RunNumber:           &runNumber,
			CycleNumber:         &cycleNumber,
			RunCycleNumber:      &runCycle,
			RunDisplayNumber:    &runDisplay,
			Workflow:            "agent-run",
			IssueRef:            stringPtr("glimmung#17"),
			IssueNumber:         &issueNumber,
			State:               "in_progress",
			CurrentPhase:        stringPtr("env-prep"),
			CumulativeCostUSD:   0,
			StartedAt:           now,
			UpdatedAt:           now,
			AttemptsCount:       1,
			ValidationURL:       nil,
			ScreenshotsMarkdown: nil,
			Attempts: []RunReportAttempt{{
				AttemptIndex:     0,
				Phase:            "env-prep",
				PhaseKind:        "k8s_job",
				WorkflowFilename: "k8s_job:env-prep",
				DispatchedAt:     now,
			}},
		}},
		nativeLogs: NativeRunLogsResponse{Events: []NativeRunLogEvent{{
			Project:      "glimmung",
			RunRef:       runRef,
			AttemptIndex: 0,
			Phase:        "env-prep",
			JobID:        "prepare",
			Seq:          1,
			Event:        "step_started",
			StepSlug:     "checkout",
			CreatedAt:    now.Format(time.RFC3339Nano),
		}}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/glimmung/issues/17/runs/1/cycles/1/graph", &projection)

	if len(projection.Runs) != 1 {
		t.Fatalf("projection runs=%#v", projection.Runs)
	}
	if got := projection.Runs[0].Topology.RecycleArrows; len(got) != 1 ||
		got[0].Source != "agent-execute" ||
		got[0].Target != "agent-execute" ||
		got[0].Kind != "phase_recycle" ||
		got[0].Trigger != "verify_fail / verify_malformed" ||
		got[0].MaxAttempts != 3 {
		t.Fatalf("projection topology recycle arrows=%#v", got)
	}
	if got := projection.Runs[0].Topology.Phases; len(got) != 2 ||
		got[0].Name != "env-prep" ||
		got[1].Name != "agent-execute" ||
		len(got[1].DependsOn) != 1 ||
		got[1].DependsOn[0] != "env-prep" {
		t.Fatalf("projection topology phases=%#v", got)
	}
	if got := projection.Runs[0].Topology.Terminal; got.Kind != "touchpoint" || !got.Enabled {
		t.Fatalf("projection topology terminal=%#v", got)
	}
	envPhase := assertProjectionPhase(t, projection.Runs[0], "env-prep")
	if envPhase.State != "active" || envPhase.Jobs[0].State != "active" || envPhase.Jobs[0].Steps[0].State != "active" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	executePhase := assertProjectionPhase(t, projection.Runs[0], "agent-execute")
	if executePhase.State != "not_started" || executePhase.Jobs[0].State != "not_started" {
		t.Fatalf("agent-execute projection=%#v", executePhase)
	}
}

func TestWorkflowTopologyForRunMarksRecycleOriginArrowActive(t *testing.T) {
	workflow := Workflow{
		Project: "ambience",
		Name:    "default",
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job"},
			{Name: "llm-work", Kind: "k8s_job", DependsOn: []string{"env-prep"}},
			{
				Name:      "evidence-gate",
				Kind:      "k8s_job",
				DependsOn: []string{"llm-work"},
				RecyclePolicy: &RecyclePolicy{
					MaxAttempts: 3,
					On:          []string{"verify_fail"},
					LandsAt:     "env-prep",
				},
			},
		},
		PR: PrPrimitive{
			Enabled: true,
			RecyclePolicy: &RecyclePolicy{
				MaxAttempts: 3,
				On:          []string{"changes_requested"},
				LandsAt:     "env-prep",
			},
		},
	}
	origin := "recycle_policy"

	topology := workflowTopologyForRun(workflow, RunReport{
		OriginKind:      &origin,
		EntrypointPhase: stringPtr("env-prep"),
		TriggerSource:   map[string]any{"failing_phase": "evidence-gate"},
	})

	if topology.DefaultEntry == nil || topology.DefaultEntry.Active {
		t.Fatalf("default entry active=%#v, want inactive for recycle", topology.DefaultEntry)
	}
	if len(topology.RecycleArrows) != 2 {
		t.Fatalf("recycle arrows=%#v", topology.RecycleArrows)
	}
	if !topology.RecycleArrows[0].Active || topology.RecycleArrows[1].Active {
		t.Fatalf("recycle arrows active=%#v, want only phase recycle active", topology.RecycleArrows)
	}
}

func TestWorkflowTopologyForRunMarksTouchpointRecycleOriginArrowActive(t *testing.T) {
	workflow := Workflow{
		Project: "ambience",
		Name:    "default",
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job"},
			{
				Name:      "evidence-gate",
				Kind:      "k8s_job",
				DependsOn: []string{"env-prep"},
				RecyclePolicy: &RecyclePolicy{
					MaxAttempts: 3,
					On:          []string{"verify_fail"},
					LandsAt:     "env-prep",
				},
			},
		},
		PR: PrPrimitive{
			Enabled: true,
			RecyclePolicy: &RecyclePolicy{
				MaxAttempts: 3,
				On:          []string{"changes_requested"},
				LandsAt:     "env-prep",
			},
		},
	}
	origin := "pr_feedback"

	topology := workflowTopologyForRun(workflow, RunReport{OriginKind: &origin})

	if topology.DefaultEntry == nil || topology.DefaultEntry.Active {
		t.Fatalf("default entry active=%#v, want inactive for PR feedback recycle", topology.DefaultEntry)
	}
	if len(topology.RecycleArrows) != 2 {
		t.Fatalf("recycle arrows=%#v", topology.RecycleArrows)
	}
	if topology.RecycleArrows[0].Active || !topology.RecycleArrows[1].Active {
		t.Fatalf("recycle arrows active=%#v, want only touchpoint recycle active", topology.RecycleArrows)
	}
}

func TestWorkflowTopologyForRunKeepsManualEntryActiveForDispatch(t *testing.T) {
	workflow := Workflow{
		Project: "ambience",
		Name:    "default",
		Phases:  []PhaseSpec{{Name: "env-prep", Kind: "k8s_job"}},
		PR:      PrPrimitive{Enabled: true},
	}
	origin := "dispatch"

	topology := workflowTopologyForRun(workflow, RunReport{OriginKind: &origin})

	if topology.DefaultEntry == nil || !topology.DefaultEntry.Active {
		t.Fatalf("default entry active=%#v, want active for dispatch", topology.DefaultEntry)
	}
}

func TestRunCycleGraphProjectionUsesDurableExecutions(t *testing.T) {
	issueNumber := 17
	runNumber := 1
	cycleNumber := 1
	runCycle := 1
	runDisplay := "1.1"
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	completed := now.Add(2 * time.Minute).Format(time.RFC3339Nano)
	runRef := "glimmung#17/runs/1.1"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "glimmung",
			Name:    "agent-run",
			Phases: []PhaseSpec{
				{Name: "env-prep", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "prepare"}}},
				{Name: "agent-execute", Kind: "k8s_job", DependsOn: []string{"env-prep"}, Jobs: []NativeJobSpec{{ID: "agent"}}},
			},
		}}},
		issue: IssueDetail{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &issueNumber,
			Title:   "Port graph",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-1",
			Project:          "glimmung",
			RunRef:           runRef,
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "agent-run",
			IssueRef:         stringPtr("glimmung#17"),
			IssueNumber:      &issueNumber,
			State:            "aborted",
			StartedAt:        now,
			UpdatedAt:        now,
			PhaseExecutions: []RunPhaseExecution{
				{
					Name:        "env-prep",
					Kind:        "k8s_job",
					State:       "failed",
					Reason:      stringPtr("dispatch_timeout"),
					CreatedAt:   now.Format(time.RFC3339Nano),
					CompletedAt: &completed,
					Jobs: []RunJobExecution{{
						ID:          "prepare",
						State:       "failed",
						Reason:      stringPtr("dispatch_timeout"),
						CreatedAt:   now.Format(time.RFC3339Nano),
						CompletedAt: &completed,
						Steps: []RunStepExecution{{
							Slug:      "job",
							State:     "not_started",
							CreatedAt: now.Format(time.RFC3339Nano),
						}},
					}},
				},
				{
					Name:        "agent-execute",
					Kind:        "k8s_job",
					State:       "skipped",
					CreatedAt:   now.Format(time.RFC3339Nano),
					CompletedAt: &completed,
					Jobs: []RunJobExecution{{
						ID:          "agent",
						State:       "skipped",
						CreatedAt:   now.Format(time.RFC3339Nano),
						CompletedAt: &completed,
						Steps: []RunStepExecution{{
							Slug:        "job",
							State:       "skipped",
							CreatedAt:   now.Format(time.RFC3339Nano),
							CompletedAt: &completed,
						}},
					}},
				},
			},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/glimmung/issues/17/runs/1/cycles/1/graph", &projection)

	envPhase := assertProjectionPhase(t, projection.Runs[0], "env-prep")
	if envPhase.State != "failed" || envPhase.Reason == nil || *envPhase.Reason != "dispatch_timeout" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	if envPhase.Jobs[0].State != "failed" || envPhase.Jobs[0].Reason == nil || *envPhase.Jobs[0].Reason != "dispatch_timeout" {
		t.Fatalf("env-prep job projection=%#v", envPhase.Jobs[0])
	}
	executePhase := assertProjectionPhase(t, projection.Runs[0], "agent-execute")
	if executePhase.State != "skipped" || executePhase.Jobs[0].State != "skipped" {
		t.Fatalf("agent-execute projection=%#v", executePhase)
	}
}

func TestRunCycleGraphProjectionShowsCarriedForwardEntrypointInputs(t *testing.T) {
	issueNumber := 171
	runNumber := 4
	cycleNumber := 6
	runCycle := 2
	runDisplay := "4.2"
	now := time.Date(2026, 5, 26, 6, 18, 0, 0, time.UTC)
	completed := now.Format(time.RFC3339Nano)
	completedAt := now
	runRef := "ambience#171/runs/4.2"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "ambience",
			Name:    "default",
			Phases: []PhaseSpec{
				{Name: "env-prep", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "env-prep", Name: stringPtr("Environment prep")}}},
				{Name: "llm-work", Kind: "k8s_job", DependsOn: []string{"env-prep"}, Jobs: []NativeJobSpec{{ID: "llm-implement"}}},
			},
		}}},
		issue: IssueDetail{
			Ref:     "ambience#171",
			Project: "ambience",
			Number:  &issueNumber,
			Title:   "Bog bubbles",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-4-2",
			Project:          "ambience",
			RunRef:           runRef,
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "default",
			IssueRef:         stringPtr("ambience#171"),
			IssueNumber:      &issueNumber,
			State:            "review_required",
			StartedAt:        now,
			UpdatedAt:        now,
			PhaseExecutions: []RunPhaseExecution{{
				Name:        "env-prep",
				Kind:        "k8s_job",
				State:       "skipped",
				CreatedAt:   completed,
				CompletedAt: &completed,
				Jobs: []RunJobExecution{{
					ID:          "env-prep",
					State:       "skipped",
					CreatedAt:   completed,
					CompletedAt: &completed,
					Steps: []RunStepExecution{{
						Slug:        "emit-env-outputs",
						State:       "skipped",
						CreatedAt:   completed,
						CompletedAt: &completed,
					}},
				}},
			}},
			Attempts: []RunReportAttempt{{
				AttemptIndex: 0,
				Phase:        "env-prep",
				PhaseKind:    "k8s_job",
				CarryForward: true,
				DispatchedAt: now,
				CompletedAt:  &completedAt,
				Conclusion:   stringPtr("success"),
				Decision:     stringPtr("advance"),
				PhaseOutputs: map[string]string{"validation_url": "https://slot.example"},
			}},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/ambience/issues/171/runs/4/cycles/2/graph", &projection)

	envPhase := assertProjectionPhase(t, projection.Runs[0], "env-prep")
	if envPhase.State != "succeeded" || envPhase.Reason == nil || *envPhase.Reason != "carried_forward" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	if envPhase.Jobs[0].State != "succeeded" || envPhase.Jobs[0].Reason == nil || *envPhase.Jobs[0].Reason != "carried_forward" {
		t.Fatalf("env-prep job projection=%#v", envPhase.Jobs[0])
	}
	if !envPhase.Attempts[0].CarryForward {
		t.Fatalf("carry_forward not projected: %#v", envPhase.Attempts[0])
	}
}

func TestRunCycleGraphProjectionKeepsPendingWorkflowJobsWithDurableExecutions(t *testing.T) {
	issueNumber := 17
	runNumber := 1
	cycleNumber := 1
	runCycle := 1
	runDisplay := "1.1"
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	runRef := "glimmung#17/runs/1.1"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "glimmung",
			Name:    "agent-run",
			Phases: []PhaseSpec{
				{
					Name: "env-prep",
					Kind: "k8s_job",
					Jobs: []NativeJobSpec{{
						ID: "prepare",
						Steps: []NativeStepSpec{{
							Slug:  "checkout",
							Title: stringPtr("Checkout"),
						}},
					}},
				},
				{
					Name:      "agent-execute",
					Kind:      "k8s_job",
					DependsOn: []string{"env-prep"},
					Jobs: []NativeJobSpec{{
						ID:   "agent",
						Name: stringPtr("Run agent"),
						Steps: []NativeStepSpec{{
							Slug:  "run-agent",
							Title: stringPtr("Run agent"),
						}},
					}},
				},
			},
		}}},
		issue: IssueDetail{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &issueNumber,
			Title:   "Port graph",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-1",
			Project:          "glimmung",
			RunRef:           runRef,
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "agent-run",
			IssueRef:         stringPtr("glimmung#17"),
			IssueNumber:      &issueNumber,
			State:            "in_progress",
			CurrentPhase:     stringPtr("env-prep"),
			StartedAt:        now,
			UpdatedAt:        now,
			PhaseExecutions: []RunPhaseExecution{{
				Name:      "env-prep",
				Kind:      "k8s_job",
				State:     "active",
				CreatedAt: now.Format(time.RFC3339Nano),
				Jobs: []RunJobExecution{{
					ID:        "prepare",
					State:     "active",
					CreatedAt: now.Format(time.RFC3339Nano),
					Steps: []RunStepExecution{{
						Slug:      "checkout",
						Title:     stringPtr("Checkout"),
						State:     "active",
						CreatedAt: now.Format(time.RFC3339Nano),
					}},
				}},
			}},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/glimmung/issues/17/runs/1/cycles/1/graph", &projection)

	envPhase := assertProjectionPhase(t, projection.Runs[0], "env-prep")
	if envPhase.State != "active" || envPhase.Jobs[0].Steps[0].State != "active" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	executePhase := assertProjectionPhase(t, projection.Runs[0], "agent-execute")
	if executePhase.State != "not_started" || len(executePhase.Jobs) != 1 || executePhase.Jobs[0].State != "not_started" {
		t.Fatalf("agent-execute projection=%#v", executePhase)
	}
	if len(executePhase.Jobs[0].Steps) != 1 ||
		executePhase.Jobs[0].Steps[0].Slug != "run-agent" ||
		executePhase.Jobs[0].Steps[0].State != "not_started" {
		t.Fatalf("agent-execute steps=%#v", executePhase.Jobs[0].Steps)
	}
}

func TestApplyNativeEventsResetsUnobservedFailedSteps(t *testing.T) {
	exitCode := 1
	stepFailed := "step_failed"
	exitNonzero := "exit_nonzero"
	jobFailed := "job_failed"
	run := RunProjectionRun{
		Phases: []RunProjectionPhase{{
			Name: "env-prep",
			Kind: "k8s_job",
			Attempts: []RunProjectionAttempt{{
				AttemptIndex: 0,
				Phase:        "env-prep",
				PhaseKind:    "k8s_job",
			}},
			Jobs: []RunProjectionJob{{
				ID:     "env-prep",
				State:  "failed",
				Reason: &stepFailed,
				Steps: []RunProjectionStep{
					{Slug: "check-validation-env", State: "failed", Reason: &exitNonzero, ExitCode: &exitCode},
					{Slug: "emit-env-outputs", State: "failed", Reason: &jobFailed},
				},
			}},
		}},
	}
	events := []NativeRunLogEvent{{
		AttemptIndex: 0,
		Phase:        "env-prep",
		JobID:        "env-prep",
		Seq:          173,
		Event:        "step_failed",
		StepSlug:     "check-validation-env",
		ExitCode:     &exitCode,
	}}

	applyNativeEventsToProjectionRun(&run, events)

	steps := run.Phases[0].Jobs[0].Steps
	if steps[0].State != "failed" || steps[0].Reason == nil || *steps[0].Reason != "exit_nonzero" {
		t.Fatalf("check-validation-env step=%#v", steps[0])
	}
	if steps[1].State != "not_started" || steps[1].Reason != nil || steps[1].ExitCode != nil {
		t.Fatalf("emit-env-outputs step=%#v", steps[1])
	}
}

func TestRunCycleGraphProjectionShowsLegacyAbortedDispatchTimeout(t *testing.T) {
	issueNumber := 17
	runNumber := 1
	cycleNumber := 1
	runCycle := 1
	runDisplay := "1.1"
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	runRef := "glimmung#17/runs/1.1"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "glimmung",
			Name:    "agent-run",
			Phases: []PhaseSpec{
				{Name: "env-prep", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "prepare"}}},
				{Name: "agent-execute", Kind: "k8s_job", DependsOn: []string{"env-prep"}, Jobs: []NativeJobSpec{{ID: "agent"}}},
			},
		}}},
		issue: IssueDetail{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &issueNumber,
			Title:   "Port graph",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-1",
			Project:          "glimmung",
			RunRef:           runRef,
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "agent-run",
			IssueRef:         stringPtr("glimmung#17"),
			IssueNumber:      &issueNumber,
			State:            "aborted",
			CurrentPhase:     stringPtr("env-prep"),
			AbortReason:      stringPtr("dispatch_timeout"),
			StartedAt:        now,
			UpdatedAt:        now,
			Attempts: []RunReportAttempt{{
				AttemptIndex:     0,
				Phase:            "env-prep",
				PhaseKind:        "k8s_job",
				WorkflowFilename: "k8s_job:env-prep",
				DispatchedAt:     now.Add(-11 * time.Minute),
			}},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/glimmung/issues/17/runs/1/cycles/1/graph", &projection)

	envPhase := assertProjectionPhase(t, projection.Runs[0], "env-prep")
	if envPhase.State != "failed" || envPhase.Reason == nil || *envPhase.Reason != "dispatch_timeout" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	if envPhase.Jobs[0].State != "failed" || envPhase.Jobs[0].Reason == nil || *envPhase.Jobs[0].Reason != "dispatch_timeout" {
		t.Fatalf("env-prep job projection=%#v", envPhase.Jobs[0])
	}
	executePhase := assertProjectionPhase(t, projection.Runs[0], "agent-execute")
	if executePhase.State != "skipped" || executePhase.Jobs[0].State != "skipped" {
		t.Fatalf("agent-execute projection=%#v", executePhase)
	}
}

func TestRunCycleGraphProjectionShowsForwardDispatchFailureWithoutFakeStepFailure(t *testing.T) {
	issueNumber := 171
	runNumber := 3
	cycleNumber := 4
	runCycle := 1
	runDisplay := "3.1"
	now := time.Date(2026, 5, 25, 7, 32, 14, 0, time.UTC)
	abortReason := `forward_dispatch_failed: phase "llm-work" input "claude_ca_namespace" refs phase "env-prep" which has no captured outputs on this run`
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "ambience",
			Name:    "default",
			Phases: []PhaseSpec{
				{Name: "env-prep", Kind: "k8s_job", Outputs: []string{"claude_ca_namespace"}, Jobs: []NativeJobSpec{{ID: "env-prep"}}},
				{
					Name:      "llm-work",
					Kind:      "k8s_job",
					DependsOn: []string{"env-prep"},
					Inputs:    map[string]string{"claude_ca_namespace": "${{ phases.env-prep.outputs.claude_ca_namespace }}"},
					Jobs: []NativeJobSpec{
						{ID: "llm-test-plan", Steps: []NativeStepSpec{{Slug: "clone"}, {Slug: "run-test-plan"}}},
						{ID: "llm-implement", Steps: []NativeStepSpec{{Slug: "clone"}, {Slug: "run-implementation"}}},
					},
				},
				{Name: "llm-verify", Kind: "k8s_job", DependsOn: []string{"llm-work"}, Jobs: []NativeJobSpec{{ID: "llm-verify"}}},
			},
		}}},
		issue: IssueDetail{
			Ref:     "ambience#171",
			Project: "ambience",
			Number:  &issueNumber,
			Title:   "Bog bubbles",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-3",
			Project:          "ambience",
			RunRef:           "ambience#171/runs/3.1",
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "default",
			IssueRef:         stringPtr("ambience#171"),
			IssueNumber:      &issueNumber,
			State:            "aborted",
			CurrentPhase:     stringPtr("llm-work"),
			AbortReason:      &abortReason,
			StartedAt:        now.Add(-time.Minute),
			UpdatedAt:        now,
			Attempts: []RunReportAttempt{{
				AttemptIndex:     0,
				Phase:            "env-prep",
				PhaseKind:        "k8s_job",
				WorkflowFilename: "k8s_job:env-prep",
				DispatchedAt:     now.Add(-time.Minute),
				CompletedAt:      &now,
				Conclusion:       stringPtr("success"),
			}},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/ambience/issues/171/runs/3/cycles/1/graph", &projection)

	if projection.Runs[0].AbortReason == nil || *projection.Runs[0].AbortReason != abortReason {
		t.Fatalf("run abort reason not projected: %#v", projection.Runs[0].AbortReason)
	}
	workPhase := assertProjectionPhase(t, projection.Runs[0], "llm-work")
	if workPhase.State != "failed" || workPhase.Reason == nil || *workPhase.Reason != "dispatch_failed" {
		t.Fatalf("llm-work projection=%#v", workPhase)
	}
	for _, job := range workPhase.Jobs {
		if job.State != "failed" || job.Reason == nil || *job.Reason != "dispatch_failed" {
			t.Fatalf("llm-work job projection=%#v", job)
		}
		for _, step := range job.Steps {
			if step.State != "not_started" || step.Reason != nil || step.ExitCode != nil {
				t.Fatalf("dispatch failure should not synthesize failed step: %#v", step)
			}
		}
	}
}

func TestSystemGraphUsesProjectFilter(t *testing.T) {
	number := 17
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	store := fakeGraphStore{
		issues: []IssueRow{{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &number,
			Title:   "Port graph",
			State:   "open",
		}},
		runs: []RunReport{{
			Project:     "glimmung",
			RunRef:      "glimmung#17/runs/1",
			Workflow:    "agent-run",
			IssueRef:    stringPtr("glimmung#17"),
			IssueNumber: &number,
			State:       "in_progress",
			StartedAt:   now,
			UpdatedAt:   now,
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var graph IssueGraph
	getJSON(t, handler, "/v1/graph?project=glimmung", &graph)

	assertGraphNode(t, graph, "issue:glimmung#17", "issue")
	assertGraphNode(t, graph, "run:glimmung#17/runs/1", "run")
	assertGraphEdge(t, graph, "issue:glimmung#17", "run:glimmung#17/runs/1", "spawned")
}

func assertGraphNode(t *testing.T, graph IssueGraph, id, kind string) GraphNode {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.ID == id {
			if node.Kind != kind {
				t.Fatalf("node %s kind=%s, want %s", id, node.Kind, kind)
			}
			return node
		}
	}
	encoded, _ := json.MarshalIndent(graph.Nodes, "", "  ")
	t.Fatalf("missing node %s in %s", id, encoded)
	return GraphNode{}
}

func assertGraphEdge(t *testing.T, graph IssueGraph, source, target, kind string) {
	t.Helper()
	for _, edge := range graph.Edges {
		if edge.Source == source && edge.Target == target && edge.Kind == kind {
			return
		}
	}
	encoded, _ := json.MarshalIndent(graph.Edges, "", "  ")
	t.Fatalf("missing edge %s --%s--> %s in %s", source, kind, target, encoded)
}

func assertProjectionPhase(t *testing.T, run RunProjectionRun, name string) RunProjectionPhase {
	t.Helper()
	for _, phase := range run.Phases {
		if phase.Name == name {
			return phase
		}
	}
	encoded, _ := json.MarshalIndent(run.Phases, "", "  ")
	t.Fatalf("missing projection phase %s in %s", name, encoded)
	return RunProjectionPhase{}
}

func assertProjectionEvidence(t *testing.T, run RunProjectionRun, kind, ref string) {
	t.Helper()
	for _, evidence := range run.Evidence {
		if evidence.Kind == kind && evidence.Ref == ref {
			return
		}
	}
	encoded, _ := json.MarshalIndent(run.Evidence, "", "  ")
	t.Fatalf("missing projection evidence %s:%s in %s", kind, ref, encoded)
}

func assertProjectionEdge(t *testing.T, projection RunGraphProjection, source, target, kind string) {
	t.Helper()
	for _, edge := range projection.Edges {
		if edge.Source == source && edge.Target == target && edge.Kind == kind {
			return
		}
	}
	encoded, _ := json.MarshalIndent(projection.Edges, "", "  ")
	t.Fatalf("missing projection edge %s --%s--> %s in %s", source, kind, target, encoded)
}

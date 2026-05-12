package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/publicids"
)

type GraphRuntimeStore interface {
	ReadStore
	IssueStore
	RunStore
	TouchpointStore
}

type GraphSignalStore interface {
	ListGraphSignals(ctx context.Context, filter GraphSignalFilter) ([]GraphSignal, error)
}

type GraphSignalFilter struct {
	State string
}

type GraphSignal struct {
	ID                string
	TargetType        string
	TargetRepo        string
	TargetID          string
	Source            string
	Payload           map[string]any
	State             string
	EnqueuedAt        time.Time
	ProcessedDecision *string
	FailureReason     *string
}

type GraphNode struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Label     string         `json:"label"`
	State     *string        `json:"state"`
	Timestamp *time.Time     `json:"timestamp"`
	Metadata  map[string]any `json:"metadata"`
}

type GraphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
}

type IssueGraph struct {
	IssueRef string      `json:"issue_ref"`
	Nodes    []GraphNode `json:"nodes"`
	Edges    []GraphEdge `json:"edges"`
}

func issueGraphByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		graphStore, ok := store.(GraphRuntimeStore)
		if !ok || graphStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "graph store not configured")
			return
		}
		number, err := strconv.Atoi(r.PathValue("issue_number"))
		if err != nil || number < 1 {
			writeProblem(w, http.StatusBadRequest, "issue_number must be a positive integer")
			return
		}
		graph, err := buildIssueGraphByNumber(r.Context(), graphStore, optionalGraphSignalStore(store), r.PathValue("project"), number)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "issue not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "build issue graph failed")
			return
		}
		writeJSON(w, http.StatusOK, graph)
	}
}

func systemGraph(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		graphStore, ok := store.(GraphRuntimeStore)
		if !ok || graphStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "graph store not configured")
			return
		}
		graph, err := buildSystemGraph(r.Context(), graphStore, optionalGraphSignalStore(store), r.URL.Query().Get("project"))
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "build system graph failed")
			return
		}
		writeJSON(w, http.StatusOK, graph)
	}
}

func optionalGraphSignalStore(store ReadStore) GraphSignalStore {
	if sigStore, ok := store.(GraphSignalStore); ok {
		return sigStore
	}
	return nil
}

func buildIssueGraphByNumber(ctx context.Context, store GraphRuntimeStore, signalStore GraphSignalStore, project string, number int) (IssueGraph, error) {
	issue, err := store.GetIssueDetailByNumber(ctx, project, number)
	if err != nil {
		return IssueGraph{}, err
	}
	publicIssueRef := publicids.IssueRef(issue.Project, issue.Number)
	issueNodeID := "issue:" + publicIssueRef

	graph := IssueGraph{
		IssueRef: publicIssueRef,
		Nodes: []GraphNode{{
			ID:        issueNodeID,
			Kind:      "issue",
			Label:     issueGraphIssueLabel(issue),
			State:     stringPointerOrNil(issue.State),
			Timestamp: nil,
			Metadata: map[string]any{
				"issue_ref": publicIssueRef,
				"project":   issue.Project,
				"repo":      issue.Repo,
				"number":    issue.Number,
				"html_url":  issue.HTMLURL,
				"labels":    sliceOrEmpty(issue.Labels),
			},
		}},
		Edges: []GraphEdge{},
	}

	runs, err := store.ListProjectRuns(ctx, issue.Project, 500)
	if err != nil {
		return IssueGraph{}, err
	}
	runs = issueGraphRuns(runs, issue.Project, number, publicIssueRef)
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].StartedAt.Before(runs[j].StartedAt)
	})
	runRefs := map[string]bool{}
	runNodeByID := map[string]string{}
	for _, run := range runs {
		runRefs[run.RunRef] = true
		if run.ID != "" {
			runNodeByID[run.ID] = "run:" + run.RunRef
		}
	}

	workflows, err := store.ListWorkflows(ctx)
	if err != nil {
		return IssueGraph{}, err
	}
	workflowsByKey := map[string]Workflow{}
	for _, wf := range workflows {
		workflowsByKey[wf.Project+"/"+wf.Name] = wf
	}

	touchpoints, err := store.ListTouchpoints(ctx, TouchpointListFilter{Project: issue.Project})
	if err != nil {
		return IssueGraph{}, err
	}
	touchpoints = issueGraphTouchpoints(touchpoints, publicIssueRef, number, runRefs)
	prByRunRef := map[string]string{}
	prByIssue := []string{}
	for _, tp := range touchpoints {
		nodeID := "pr:" + tp.Ref
		graph.Nodes = append(graph.Nodes, graphNodeFromTouchpoint(tp))
		if tp.LinkedRunRef != nil && *tp.LinkedRunRef != "" {
			prByRunRef[*tp.LinkedRunRef] = nodeID
		} else {
			prByIssue = append(prByIssue, nodeID)
		}
	}

	for _, run := range runs {
		graph.Nodes = append(graph.Nodes, graphNodeFromRunReport(run, workflowsByKey[run.Project+"/"+run.Workflow]))
		graph.Edges = append(graph.Edges, GraphEdge{Source: issueNodeID, Target: "run:" + run.RunRef, Kind: "spawned"})
		if run.ParentRunRef != nil && runRefs[*run.ParentRunRef] {
			graph.Edges = append(graph.Edges, GraphEdge{Source: "run:" + *run.ParentRunRef, Target: "run:" + run.RunRef, Kind: "resumed_from"})
		}
		previousAttemptNode := ""
		for _, attempt := range run.Attempts {
			attemptNodeID := fmt.Sprintf("attempt:%s:%d", run.RunRef, attempt.AttemptIndex)
			graph.Nodes = append(graph.Nodes, graphNodeFromRunAttempt(run, attempt))
			source := "run:" + run.RunRef
			edgeKind := "attempted"
			if previousAttemptNode != "" {
				source = previousAttemptNode
				edgeKind = "retried"
			}
			graph.Edges = append(graph.Edges, GraphEdge{Source: source, Target: attemptNodeID, Kind: edgeKind})
			previousAttemptNode = attemptNodeID
		}
		if prNodeID := prByRunRef[run.RunRef]; prNodeID != "" {
			graph.Edges = append(graph.Edges, GraphEdge{Source: "run:" + run.RunRef, Target: prNodeID, Kind: "opened"})
		}
	}
	for _, prNodeID := range prByIssue {
		graph.Edges = append(graph.Edges, GraphEdge{Source: issueNodeID, Target: prNodeID, Kind: "opened"})
	}
	if signalStore != nil {
		signals, err := signalStore.ListGraphSignals(ctx, GraphSignalFilter{})
		if err != nil {
			return IssueGraph{}, err
		}
		appendIssueGraphSignals(&graph, signals, issue, issueNodeID, runRefs, runNodeByID, touchpoints)
	}
	return graph, nil
}

func buildSystemGraph(ctx context.Context, store GraphRuntimeStore, signalStore GraphSignalStore, project string) (IssueGraph, error) {
	issues, err := store.ListIssues(ctx, IssueListFilter{Project: project, State: "open"})
	if err != nil {
		return IssueGraph{}, err
	}
	graph := IssueGraph{IssueRef: "system", Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	issueNodeByRef := map[string]string{}
	issueProjectByRef := map[string]string{}
	for _, issue := range issues {
		if issue.Number == nil {
			continue
		}
		nodeID := "issue:" + issue.Ref
		issueNodeByRef[issue.Ref] = nodeID
		issueProjectByRef[issue.Ref] = issue.Project
		graph.Nodes = append(graph.Nodes, GraphNode{
			ID:        nodeID,
			Kind:      "issue",
			Label:     issue.Title,
			State:     stringPointerOrNil(issue.State),
			Timestamp: nil,
			Metadata: map[string]any{
				"issue_ref": issue.Ref,
				"project":   issue.Project,
				"repo":      issue.Repo,
				"number":    issue.Number,
				"html_url":  issue.HTMLURL,
				"labels":    sliceOrEmpty(issue.Labels),
			},
		})
	}

	workflows, err := store.ListWorkflows(ctx)
	if err != nil {
		return IssueGraph{}, err
	}
	workflowsByKey := map[string]Workflow{}
	for _, wf := range workflows {
		workflowsByKey[wf.Project+"/"+wf.Name] = wf
	}

	projects := sortedProjectsFromIssues(issues, project)
	runRefs := map[string]bool{}
	runNodeByRef := map[string]string{}
	runNodeByID := map[string]string{}
	for _, p := range projects {
		runs, err := store.ListProjectRuns(ctx, p, 500)
		if err != nil {
			return IssueGraph{}, err
		}
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartedAt.Before(runs[j].StartedAt) })
		for _, run := range runs {
			if run.State != "in_progress" || run.IssueRef == nil {
				continue
			}
			issueNodeID := issueNodeByRef[*run.IssueRef]
			if issueNodeID == "" {
				continue
			}
			runRefs[run.RunRef] = true
			runNodeByRef[run.RunRef] = "run:" + run.RunRef
			if run.ID != "" {
				runNodeByID[run.ID] = "run:" + run.RunRef
			}
			graph.Nodes = append(graph.Nodes, graphNodeFromRunReport(run, workflowsByKey[run.Project+"/"+run.Workflow]))
			graph.Edges = append(graph.Edges, GraphEdge{Source: issueNodeID, Target: "run:" + run.RunRef, Kind: "spawned"})
			previousAttemptNode := ""
			for _, attempt := range run.Attempts {
				attemptNodeID := fmt.Sprintf("attempt:%s:%d", run.RunRef, attempt.AttemptIndex)
				graph.Nodes = append(graph.Nodes, graphNodeFromRunAttempt(run, attempt))
				source := "run:" + run.RunRef
				edgeKind := "attempted"
				if previousAttemptNode != "" {
					source = previousAttemptNode
					edgeKind = "retried"
				}
				graph.Edges = append(graph.Edges, GraphEdge{Source: source, Target: attemptNodeID, Kind: edgeKind})
				previousAttemptNode = attemptNodeID
			}
		}
	}

	for _, p := range projects {
		touchpoints, err := store.ListTouchpoints(ctx, TouchpointListFilter{Project: p})
		if err != nil {
			return IssueGraph{}, err
		}
		for _, tp := range touchpoints {
			if tp.State != "ready" && tp.State != "needs_review" {
				continue
			}
			nodeID := "pr:" + tp.Ref
			source := ""
			if tp.LinkedRunRef != nil {
				source = runNodeByRef[*tp.LinkedRunRef]
			}
			if source == "" && tp.LinkedIssueRef != nil {
				source = issueNodeByRef[*tp.LinkedIssueRef]
			}
			if source == "" {
				continue
			}
			graph.Nodes = append(graph.Nodes, graphNodeFromTouchpoint(tp))
			graph.Edges = append(graph.Edges, GraphEdge{Source: source, Target: nodeID, Kind: "opened"})
		}
	}

	if signalStore != nil {
		signals, err := signalStore.ListGraphSignals(ctx, GraphSignalFilter{State: "pending"})
		if err != nil {
			return IssueGraph{}, err
		}
		appendSystemGraphSignals(&graph, signals, issueNodeByRef, runNodeByRef, runNodeByID, issueProjectByRef, project)
	}

	return graph, nil
}

func issueGraphIssueLabel(issue IssueDetail) string {
	if issue.Number == nil {
		return issue.Title
	}
	return fmt.Sprintf("#%d %s", *issue.Number, issue.Title)
}

func issueGraphRuns(runs []RunReport, project string, number int, issueRef string) []RunReport {
	out := make([]RunReport, 0, len(runs))
	for _, run := range runs {
		if run.Project != project {
			continue
		}
		if run.IssueNumber != nil && *run.IssueNumber == number {
			out = append(out, run)
			continue
		}
		if run.IssueRef != nil && *run.IssueRef == issueRef {
			out = append(out, run)
		}
	}
	return out
}

func issueGraphTouchpoints(rows []TouchpointRow, issueRef string, issueNumber int, runRefs map[string]bool) []TouchpointRow {
	out := make([]TouchpointRow, 0, len(rows))
	for _, row := range rows {
		if row.LinkedIssueRef != nil && *row.LinkedIssueRef == issueRef {
			out = append(out, row)
			continue
		}
		if row.IssueNumber != nil && *row.IssueNumber == issueNumber {
			out = append(out, row)
			continue
		}
		if row.LinkedRunRef != nil && runRefs[*row.LinkedRunRef] {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

func graphNodeFromRunReport(run RunReport, workflow Workflow) GraphNode {
	metadata := map[string]any{
		"run_ref":              run.RunRef,
		"run_number":           run.RunNumber,
		"run_display_number":   run.RunDisplayNumber,
		"parent_run_ref":       run.ParentRunRef,
		"root_run_ref":         run.RootRunRef,
		"origin_kind":          run.OriginKind,
		"is_cycle":             run.IsCycle,
		"cycle_number":         run.CycleNumber,
		"project":              run.Project,
		"workflow":             run.Workflow,
		"issue_ref":            run.IssueRef,
		"issue_repo":           run.IssueRepo,
		"validation_url":       run.ValidationURL,
		"screenshots_markdown": run.ScreenshotsMarkdown,
		"abort_reason":         run.AbortReason,
		"cumulative_cost_usd":  run.CumulativeCostUSD,
		"cloned_from_run_ref":  run.ParentRunRef,
		"entrypoint_phase":     run.EntrypointPhase,
		"workflow_graph":       workflowGraphMetadata(workflow),
		"run_graph":            runGraphMetadata(run),
	}
	return GraphNode{
		ID:        "run:" + run.RunRef,
		Kind:      "run",
		Label:     runGraphLabel(run),
		State:     stringPointerOrNil(run.State),
		Timestamp: &run.StartedAt,
		Metadata:  metadata,
	}
}

func graphNodeFromRunAttempt(run RunReport, attempt RunReportAttempt) GraphNode {
	timestamp := attempt.DispatchedAt
	metadata := attemptGraphMetadata(run, attempt)
	return GraphNode{
		ID:        fmt.Sprintf("attempt:%s:%d", run.RunRef, attempt.AttemptIndex),
		Kind:      "attempt",
		Label:     fmt.Sprintf("%s #%d", firstNonEmpty(attempt.Phase, "attempt"), attempt.AttemptIndex),
		State:     stringPointerOrNil(attemptGraphState(attempt)),
		Timestamp: &timestamp,
		Metadata:  metadata,
	}
}

func graphNodeFromTouchpoint(tp TouchpointRow) GraphNode {
	label := fmt.Sprintf("PR #%d", tp.PRNumber)
	if tp.PRNumber <= 0 {
		label = tp.Ref
	}
	return GraphNode{
		ID:        "pr:" + tp.Ref,
		Kind:      "pr",
		Label:     label,
		State:     stringPointerOrNil(tp.State),
		Timestamp: nil,
		Metadata: map[string]any{
			"report_ref":       tp.Ref,
			"project":          tp.Project,
			"repo":             tp.Repo,
			"number":           tp.PRNumber,
			"title":            tp.Title,
			"html_url":         tp.HTMLURL,
			"linked_issue_ref": tp.LinkedIssueRef,
			"linked_run_ref":   tp.LinkedRunRef,
			"run_state":        tp.RunState,
			"validation_url":   tp.ValidationURL,
		},
	}
}

func runGraphLabel(run RunReport) string {
	if run.RunDisplayNumber != nil && *run.RunDisplayNumber != "" {
		return "Run " + *run.RunDisplayNumber
	}
	if run.RunNumber != nil {
		return "Run " + strconv.Itoa(*run.RunNumber)
	}
	return run.Workflow
}

func attemptGraphState(attempt RunReportAttempt) string {
	if attempt.SkippedFromRunRef != nil {
		return "skipped"
	}
	if attempt.VerificationStatus != nil && *attempt.VerificationStatus != "" {
		return *attempt.VerificationStatus
	}
	if attempt.CompletedAt != nil {
		return "completed"
	}
	return "pending"
}

func attemptGraphMetadata(run RunReport, attempt RunReportAttempt) map[string]any {
	var verification any
	if attempt.VerificationStatus != nil {
		verification = map[string]any{
			"status":        *attempt.VerificationStatus,
			"evidence_refs": sliceOrEmpty(attempt.EvidenceRefs),
		}
	}
	return map[string]any{
		"attempt_index":        attempt.AttemptIndex,
		"phase":                attempt.Phase,
		"phase_kind":           attempt.PhaseKind,
		"workflow_filename":    attempt.WorkflowFilename,
		"workflow_run_id":      attempt.WorkflowRunID,
		"completed_at":         attempt.CompletedAt,
		"decision":             attempt.Decision,
		"verification":         verification,
		"cost_usd":             attempt.CostUSD,
		"conclusion":           attempt.Conclusion,
		"phase_outputs":        attempt.PhaseOutputs,
		"log_archive_url":      attempt.LogArchiveURL,
		"jobs":                 []map[string]any{},
		"jobs_count":           0,
		"steps_count":          0,
		"run_ref":              run.RunRef,
		"run_number":           run.RunNumber,
		"skipped_from_run_ref": attempt.SkippedFromRunRef,
	}
}

func workflowGraphMetadata(workflow Workflow) map[string]any {
	if workflow.Name == "" {
		return map[string]any{
			"phases":         []string{},
			"default_entry":  nil,
			"recycle_arrows": []map[string]any{},
			"terminal":       map[string]any{"kind": "report", "enabled": false},
		}
	}
	phaseNames := make([]string, 0, len(workflow.Phases))
	arrows := make([]map[string]any, 0)
	for _, phase := range workflow.Phases {
		if phase.Name == "" {
			continue
		}
		phaseNames = append(phaseNames, phase.Name)
		if phase.RecyclePolicy != nil {
			target := phase.RecyclePolicy.LandsAt
			if target == "" || target == "self" {
				target = phase.Name
			}
			arrows = append(arrows, map[string]any{
				"source":       phase.Name,
				"target":       target,
				"trigger":      strings.Join(phase.RecyclePolicy.On, " / "),
				"max_attempts": phase.RecyclePolicy.MaxAttempts,
				"active":       false,
				"kind":         "phase_recycle",
			})
		}
	}
	if workflow.PR.RecyclePolicy != nil {
		arrows = append(arrows, map[string]any{
			"source":       "report",
			"target":       workflow.PR.RecyclePolicy.LandsAt,
			"trigger":      strings.Join(workflow.PR.RecyclePolicy.On, " / "),
			"max_attempts": workflow.PR.RecyclePolicy.MaxAttempts,
			"active":       false,
			"kind":         "report_recycle",
		})
	}
	var defaultEntry any
	if len(phaseNames) > 0 {
		defaultEntry = map[string]any{"target": phaseNames[0], "active": true, "kind": "default"}
	}
	return map[string]any{
		"phases":         phaseNames,
		"default_entry":  defaultEntry,
		"recycle_arrows": arrows,
		"terminal":       map[string]any{"kind": "report", "enabled": workflow.PR.Enabled},
	}
}

func runGraphMetadata(run RunReport) map[string]any {
	cycles := make([]map[string]any, 0, len(run.Attempts))
	for _, attempt := range run.Attempts {
		state := attemptGraphState(attempt)
		cycles = append(cycles, map[string]any{
			"cycle_index":   attempt.AttemptIndex,
			"attempt_index": attempt.AttemptIndex,
			"state":         state,
			"started_at":    attempt.DispatchedAt,
			"completed_at":  attempt.CompletedAt,
			"stages": []map[string]any{{
				"stage_id": attempt.Phase,
				"name":     attempt.Phase,
				"kind":     firstNonEmpty(attempt.PhaseKind, "gha_dispatch"),
				"state":    state,
				"jobs": []map[string]any{{
					"job_id":       firstNonEmpty(attempt.WorkflowFilename, attempt.Phase, "phase"),
					"name":         firstNonEmpty(attempt.WorkflowFilename, attempt.Phase, "phase"),
					"state":        workflowRunStepState(attempt),
					"started_at":   attempt.DispatchedAt,
					"completed_at": attempt.CompletedAt,
					"steps": []map[string]any{{
						"step_id":      "workflow-run",
						"slug":         "workflow-run",
						"title":        "Workflow run",
						"state":        workflowRunStepState(attempt),
						"started_at":   attempt.DispatchedAt,
						"completed_at": attempt.CompletedAt,
						"exit_code":    nil,
						"message":      attempt.Conclusion,
					}},
				}},
			}},
		})
	}
	return map[string]any{
		"run_ref":    run.RunRef,
		"run_number": run.RunNumber,
		"lineage": map[string]any{
			"cloned_from_run_ref": run.ParentRunRef,
			"entrypoint_phase":    run.EntrypointPhase,
		},
		"cycles": cycles,
	}
}

func workflowRunStepState(attempt RunReportAttempt) string {
	if attempt.CompletedAt != nil {
		return "completed"
	}
	return "in_progress"
}

func appendIssueGraphSignals(graph *IssueGraph, signals []GraphSignal, issue IssueDetail, issueNodeID string, runRefs map[string]bool, runNodeByID map[string]string, touchpoints []TouchpointRow) {
	prNodeByRef := map[string]string{}
	prNumberByNode := map[string]string{}
	for _, tp := range touchpoints {
		prNodeByRef[tp.Ref] = "pr:" + tp.Ref
		prNumberByNode[strconv.Itoa(tp.PRNumber)] = "pr:" + tp.Ref
	}
	runNodeByRef := map[string]string{}
	for ref := range runRefs {
		runNodeByRef[ref] = "run:" + ref
	}
	for _, sig := range signals {
		targetNode, targetRef := issueSignalTarget(sig, issue, issueNodeID, runNodeByRef, runNodeByID, prNodeByRef, prNumberByNode)
		if targetNode == "" {
			continue
		}
		node := graphNodeFromSignal(sig, targetRef)
		graph.Nodes = append(graph.Nodes, node)
		graph.Edges = append(graph.Edges, GraphEdge{Source: targetNode, Target: node.ID, Kind: "feedback"})
		for _, run := range graph.Nodes {
			if run.Kind == "run" && run.Timestamp != nil && run.Timestamp.After(sig.EnqueuedAt) {
				graph.Edges = append(graph.Edges, GraphEdge{Source: node.ID, Target: run.ID, Kind: "re_dispatched"})
				break
			}
		}
	}
}

func appendSystemGraphSignals(graph *IssueGraph, signals []GraphSignal, issueNodeByRef, runNodeByRef, runNodeByID, issueProjectByRef map[string]string, project string) {
	for _, sig := range signals {
		targetNode := ""
		targetRef := sig.TargetID
		switch sig.TargetType {
		case "issue":
			targetRef = sig.TargetID
			if !strings.Contains(targetRef, "#") && sig.TargetRepo != "" {
				targetRef = publicids.IssueRef(sig.TargetRepo, nil)
			}
			targetNode = issueNodeByRef[targetRef]
		case "run":
			targetNode = runNodeByRef[sig.TargetID]
			if targetNode == "" {
				targetNode = runNodeByID[sig.TargetID]
			}
			targetRef = sig.TargetID
		}
		if targetNode == "" {
			continue
		}
		if project != "" && issueProjectByRef[targetRef] != "" && issueProjectByRef[targetRef] != project {
			continue
		}
		node := graphNodeFromSignal(sig, targetRef)
		graph.Nodes = append(graph.Nodes, node)
		graph.Edges = append(graph.Edges, GraphEdge{Source: targetNode, Target: node.ID, Kind: "feedback"})
	}
}

func issueSignalTarget(sig GraphSignal, issue IssueDetail, issueNodeID string, runNodeByRef, runNodeByID, prNodeByRef, prNumberByNode map[string]string) (string, string) {
	switch sig.TargetType {
	case "issue":
		if sig.TargetID == issue.Ref || (issue.Number != nil && sig.TargetID == strconv.Itoa(*issue.Number)) {
			return issueNodeID, issue.Ref
		}
	case "run":
		if node := runNodeByRef[sig.TargetID]; node != "" {
			return node, sig.TargetID
		}
		if node := runNodeByID[sig.TargetID]; node != "" {
			return node, sig.TargetID
		}
	case "pr":
		if node := prNodeByRef[sig.TargetID]; node != "" {
			return node, sig.TargetID
		}
		if node := prNumberByNode[sig.TargetID]; node != "" {
			return node, sig.TargetID
		}
	}
	return "", ""
}

func graphNodeFromSignal(sig GraphSignal, targetRef string) GraphNode {
	source := firstNonEmpty(sig.Source, "signal")
	label := source
	if kind, ok := sig.Payload["kind"].(string); ok && kind != "" {
		label = kind
	}
	id := fmt.Sprintf("signal:%s:%s:%s", source, targetRef, sig.EnqueuedAt.Format(time.RFC3339Nano))
	return GraphNode{
		ID:        id,
		Kind:      "signal",
		Label:     label,
		State:     stringPointerOrNil(sig.State),
		Timestamp: &sig.EnqueuedAt,
		Metadata: map[string]any{
			"signal_ref":     strings.TrimPrefix(id, "signal:"),
			"source":         sig.Source,
			"target_type":    sig.TargetType,
			"target_repo":    sig.TargetRepo,
			"target_ref":     targetRef,
			"decision":       sig.ProcessedDecision,
			"payload":        mapOrEmpty(sig.Payload),
			"failure_reason": sig.FailureReason,
		},
	}
}

func sortedProjectsFromIssues(issues []IssueRow, project string) []string {
	if project != "" {
		return []string{project}
	}
	seen := map[string]bool{}
	for _, issue := range issues {
		if issue.Project != "" {
			seen[issue.Project] = true
		}
	}
	projects := make([]string, 0, len(seen))
	for p := range seen {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	return projects
}

func stringPointerOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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
	IssueRef   string             `json:"issue_ref"`
	Nodes      []GraphNode        `json:"nodes"`
	Edges      []GraphEdge        `json:"edges"`
	Projection RunGraphProjection `json:"projection"`
}

type RunGraphProjection struct {
	IssueRef      string                    `json:"issue_ref"`
	Runs          []RunProjectionRun        `json:"runs"`
	Edges         []RunProjectionEdge       `json:"edges"`
	CurrentRunRef *string                   `json:"current_run_ref,omitempty"`
	DefaultFocus  *RunProjectionFocus       `json:"default_focus,omitempty"`
	NextAction    RunProjectionAction       `json:"next_action"`
	Touchpoints   []RunProjectionTouchpoint `json:"touchpoints"`
	Signals       []RunProjectionSignal     `json:"signals"`
}

type RunProjectionEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
}

type RunProjectionFocus struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

type RunProjectionAction struct {
	Kind      string  `json:"kind"`
	Label     string  `json:"label"`
	TargetRef *string `json:"target_ref,omitempty"`
	Detail    *string `json:"detail,omitempty"`
}

type RunProjectionRun struct {
	RunRef            string                  `json:"run_ref"`
	RunNumber         *int                    `json:"run_number,omitempty"`
	RunDisplayNumber  *string                 `json:"run_display_number,omitempty"`
	Workflow          string                  `json:"workflow"`
	WorkflowSchemaRef string                  `json:"workflow_schema_ref,omitempty"`
	State             string                  `json:"state"`
	CurrentPhase      *string                 `json:"current_phase,omitempty"`
	OriginKind        *string                 `json:"origin_kind,omitempty"`
	IsCycle           bool                    `json:"is_cycle"`
	CycleNumber       *int                    `json:"cycle_number,omitempty"`
	RunCycleNumber    *int                    `json:"run_cycle_number,omitempty"`
	ValidationURL     *string                 `json:"validation_url,omitempty"`
	AbortReason       *string                 `json:"abort_reason,omitempty"`
	CostUSD           float64                 `json:"cost_usd"`
	AttemptsCount     int                     `json:"attempts_count"`
	StartedAt         string                  `json:"started_at"`
	UpdatedAt         string                  `json:"updated_at"`
	CompletedAt       *string                 `json:"completed_at,omitempty"`
	Topology          RunProjectionTopology   `json:"topology"`
	Phases            []RunProjectionPhase    `json:"phases"`
	Evidence          []RunProjectionEvidence `json:"evidence"`
}

type RunProjectionTopology struct {
	Phases        []RunProjectionTopologyPhase `json:"phases"`
	DefaultEntry  *RunProjectionDefaultEntry   `json:"default_entry"`
	RecycleArrows []RunProjectionRecycle       `json:"recycle_arrows"`
	Terminal      RunProjectionTerminal        `json:"terminal"`
}

type RunProjectionTopologyPhase struct {
	Name                     string                     `json:"name"`
	Kind                     string                     `json:"kind"`
	Verify                   bool                       `json:"verify"`
	Always                   bool                       `json:"always"`
	EvidenceVerificationGate bool                       `json:"evidence_verification_gate"`
	DependsOn                []string                   `json:"depends_on"`
	Jobs                     []RunProjectionTopologyJob `json:"jobs"`
}

type RunProjectionTopologyJob struct {
	ID    string  `json:"id"`
	Name  *string `json:"name,omitempty"`
	Image string  `json:"image,omitempty"`
}

type RunProjectionDefaultEntry struct {
	Target string `json:"target"`
	Active bool   `json:"active"`
	Kind   string `json:"kind"`
}

type RunProjectionTerminal struct {
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
}

type RunProjectionRecycle struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Trigger     string `json:"trigger"`
	MaxAttempts int    `json:"max_attempts"`
	Active      bool   `json:"active"`
	Kind        string `json:"kind"`
}

type RunProjectionPhase struct {
	Name      string                 `json:"name"`
	Kind      string                 `json:"kind"`
	State     string                 `json:"state"`
	Reason    *string                `json:"reason,omitempty"`
	Verify    bool                   `json:"verify"`
	Always    bool                   `json:"always"`
	DependsOn []string               `json:"depends_on"`
	Jobs      []RunProjectionJob     `json:"jobs"`
	Attempts  []RunProjectionAttempt `json:"attempts"`
}

type RunProjectionJob struct {
	ID          string              `json:"id"`
	Name        *string             `json:"name,omitempty"`
	State       string              `json:"state"`
	Reason      *string             `json:"reason,omitempty"`
	K8sJobName  *string             `json:"k8s_job_name,omitempty"`
	Conclusion  *string             `json:"conclusion,omitempty"`
	CompletedAt *string             `json:"completed_at,omitempty"`
	Steps       []RunProjectionStep `json:"steps"`
}

type RunProjectionStep struct {
	Slug     string  `json:"slug"`
	Title    *string `json:"title,omitempty"`
	State    string  `json:"state"`
	Reason   *string `json:"reason,omitempty"`
	ExitCode *int    `json:"exit_code,omitempty"`
}

type RunProjectionAttempt struct {
	AttemptIndex       int                       `json:"attempt_index"`
	Phase              string                    `json:"phase"`
	PhaseKind          string                    `json:"phase_kind"`
	State              string                    `json:"state"`
	Conclusion         *string                   `json:"conclusion,omitempty"`
	VerificationStatus *string                   `json:"verification_status,omitempty"`
	Decision           *string                   `json:"decision,omitempty"`
	DispatchedAt       string                    `json:"dispatched_at"`
	CompletedAt        *string                   `json:"completed_at,omitempty"`
	CostUSD            *float64                  `json:"cost_usd,omitempty"`
	LogArchiveURL      *string                   `json:"log_archive_url,omitempty"`
	EvidenceRefs       []string                  `json:"evidence_refs"`
	PhaseOutputs       map[string]string         `json:"phase_outputs"`
	JobCompletions     []RunAttemptJobCompletion `json:"job_completions"`
}

type RunProjectionEvidence struct {
	Kind  string  `json:"kind"`
	Ref   string  `json:"ref"`
	Label string  `json:"label"`
	URL   *string `json:"url,omitempty"`
}

type RunProjectionTouchpoint struct {
	Ref           string  `json:"ref"`
	Repo          string  `json:"repo"`
	PRNumber      int     `json:"pr_number"`
	Title         string  `json:"title"`
	State         string  `json:"state"`
	HTMLURL       *string `json:"html_url,omitempty"`
	LinkedRunRef  *string `json:"linked_run_ref,omitempty"`
	ValidationURL *string `json:"validation_url,omitempty"`
}

type RunProjectionSignal struct {
	ID                string  `json:"id"`
	TargetType        string  `json:"target_type"`
	TargetRepo        string  `json:"target_repo"`
	TargetID          string  `json:"target_id"`
	Source            string  `json:"source"`
	State             string  `json:"state"`
	Kind              string  `json:"kind,omitempty"`
	Feedback          string  `json:"feedback,omitempty"`
	ProcessedDecision *string `json:"processed_decision,omitempty"`
	FailureReason     *string `json:"failure_reason,omitempty"`
}

var markdownEvidenceURL = regexp.MustCompile(`!?\[[^\]]*\]\(([^)]+)\)`)

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
			writeInternalError(w, r, err, "build issue graph failed")
			return
		}
		writeJSON(w, http.StatusOK, graph)
	}
}

func runCycleGraphProjectionByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		graphStore, ok := store.(GraphRuntimeStore)
		if !ok || graphStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "graph store not configured")
			return
		}
		issueNumber, err := strconv.Atoi(r.PathValue("issue_number"))
		if err != nil || issueNumber < 1 {
			writeProblem(w, http.StatusBadRequest, "issue_number must be a positive integer")
			return
		}
		issue, err := graphStore.GetIssueDetailByNumber(r.Context(), r.PathValue("project"), issueNumber)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "issue not found")
			return
		case err != nil:
			writeInternalError(w, r, err, "read issue failed")
			return
		}
		if issue.Number == nil {
			writeProblem(w, http.StatusNotFound, "issue not found")
			return
		}
		runs, err := graphStore.ListProjectRuns(r.Context(), issue.Project, 500)
		if err != nil {
			writeInternalError(w, r, err, "list runs failed")
			return
		}
		runs = issueGraphRuns(runs, issue.Project, *issue.Number, publicids.IssueRef(issue.Project, issue.Number))
		selected, ok := selectRunCycleForProjection(runs, r.PathValue("run_number"), r.PathValue("cycle_number"))
		if !ok {
			writeProblem(w, http.StatusNotFound, "run cycle not found")
			return
		}
		workflows, err := graphStore.ListWorkflows(r.Context())
		if err != nil {
			writeInternalError(w, r, err, "list workflows failed")
			return
		}
		workflowsByKey := map[string]Workflow{}
		for _, wf := range workflows {
			workflowsByKey[wf.Project+"/"+wf.Name] = wf
		}
		addWorkflowSchemasForRuns(r.Context(), graphStore, []RunReport{selected}, workflowsByKey)
		touchpoints, err := graphStore.ListTouchpoints(r.Context(), TouchpointListFilter{Project: issue.Project})
		if err != nil {
			writeInternalError(w, r, err, "list touchpoints failed")
			return
		}
		touchpoints = issueGraphTouchpoints(touchpoints, publicids.IssueRef(issue.Project, issue.Number), *issue.Number, map[string]bool{selected.RunRef: true})
		var signals []GraphSignal
		if signalStore := optionalGraphSignalStore(store); signalStore != nil {
			signals, err = signalStore.ListGraphSignals(r.Context(), GraphSignalFilter{})
			if err != nil {
				writeInternalError(w, r, err, "list graph signals failed")
				return
			}
		}
		projection := buildRunGraphProjection(publicids.IssueRef(issue.Project, issue.Number), []RunReport{selected}, workflowsByKey, touchpoints, signals)
		if nativeStore, ok := store.(NativeRunStore); ok && nativeStore != nil && len(projection.Runs) == 1 && selected.ID != "" {
			logs, err := nativeStore.ListNativeEventsByID(r.Context(), selected.Project, selected.ID, nil, nil, nil)
			if err != nil && !errors.Is(err, ErrNotFound) {
				writeInternalError(w, r, err, "list native events failed")
				return
			}
			applyNativeEventsToProjectionRun(&projection.Runs[0], logs.Events)
		}
		writeJSON(w, http.StatusOK, projection)
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
			writeInternalError(w, r, err, "build system graph failed")
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
	addWorkflowSchemasForRuns(ctx, store, runs, workflowsByKey)

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
		graph.Nodes = append(graph.Nodes, graphNodeFromRunReport(run, workflowForRunReport(run, workflowsByKey)))
		graph.Edges = append(graph.Edges, GraphEdge{Source: issueNodeID, Target: "run:" + run.RunRef, Kind: "spawned"})
		if run.ParentRunRef != nil && runRefs[*run.ParentRunRef] {
			graph.Edges = append(graph.Edges, GraphEdge{Source: "run:" + *run.ParentRunRef, Target: "run:" + run.RunRef, Kind: "cycled_from"})
		}
		previousAttemptNode := ""
		workflow := workflowForRunReport(run, workflowsByKey)
		for _, attempt := range run.Attempts {
			attemptNodeID := fmt.Sprintf("attempt:%s:%d", run.RunRef, attempt.AttemptIndex)
			graph.Nodes = append(graph.Nodes, graphNodeFromRunAttempt(run, attempt, workflow))
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
	var signals []GraphSignal
	if signalStore != nil {
		signals, err = signalStore.ListGraphSignals(ctx, GraphSignalFilter{})
		if err != nil {
			return IssueGraph{}, err
		}
		appendIssueGraphSignals(&graph, signals, issue, issueNodeID, runRefs, runNodeByID, touchpoints)
	}
	graph.Projection = buildRunGraphProjection(publicIssueRef, runs, workflowsByKey, touchpoints, signals)
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
		addWorkflowSchemasForRuns(ctx, store, runs, workflowsByKey)
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartedAt.Before(runs[j].StartedAt) })
		for _, run := range runs {
			if !runStateIsActive(run.State) || run.IssueRef == nil {
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
			graph.Nodes = append(graph.Nodes, graphNodeFromRunReport(run, workflowForRunReport(run, workflowsByKey)))
			graph.Edges = append(graph.Edges, GraphEdge{Source: issueNodeID, Target: "run:" + run.RunRef, Kind: "spawned"})
			previousAttemptNode := ""
			workflow := workflowForRunReport(run, workflowsByKey)
			for _, attempt := range run.Attempts {
				attemptNodeID := fmt.Sprintf("attempt:%s:%d", run.RunRef, attempt.AttemptIndex)
				graph.Nodes = append(graph.Nodes, graphNodeFromRunAttempt(run, attempt, workflow))
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
		"run_cycle_number":     run.RunCycleNumber,
		"project":              run.Project,
		"workflow":             run.Workflow,
		"workflow_schema_ref":  run.WorkflowSchemaRef,
		"issue_ref":            run.IssueRef,
		"issue_repo":           run.IssueRepo,
		"validation_url":       run.ValidationURL,
		"screenshots_markdown": run.ScreenshotsMarkdown,
		"abort_reason":         run.AbortReason,
		"cumulative_cost_usd":  run.CumulativeCostUSD,
		"entrypoint_phase":     run.EntrypointPhase,
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

func graphNodeFromRunAttempt(run RunReport, attempt RunReportAttempt, workflow Workflow) GraphNode {
	timestamp := attempt.DispatchedAt
	metadata := attemptGraphMetadata(run, attempt, workflow)
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
			"touchpoint_ref":   tp.Ref,
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
	if attempt.VerificationStatus != nil && *attempt.VerificationStatus != "" {
		return *attempt.VerificationStatus
	}
	if attempt.CompletedAt != nil {
		return "completed"
	}
	return "pending"
}

func attemptGraphMetadata(run RunReport, attempt RunReportAttempt, workflow Workflow) map[string]any {
	var verification any
	if attempt.VerificationStatus != nil {
		verification = map[string]any{
			"status":        *attempt.VerificationStatus,
			"evidence_refs": sliceOrEmpty(attempt.EvidenceRefs),
		}
	}
	jobs := attemptGraphJobs(attempt, workflow)
	return map[string]any{
		"attempt_index":     attempt.AttemptIndex,
		"phase":             attempt.Phase,
		"phase_kind":        attempt.PhaseKind,
		"workflow_filename": attempt.WorkflowFilename,
		"completed_at":      attempt.CompletedAt,
		"decision":          attempt.Decision,
		"verification":      verification,
		"cost_usd":          attempt.CostUSD,
		"conclusion":        attempt.Conclusion,
		"phase_outputs":     attempt.PhaseOutputs,
		"log_archive_url":   attempt.LogArchiveURL,
		"jobs":              jobs,
		"jobs_count":        len(jobs),
		"steps_count":       countAttemptGraphSteps(jobs),
		"run_ref":           run.RunRef,
		"run_number":        run.RunNumber,
	}
}

func attemptGraphJobs(attempt RunReportAttempt, workflow Workflow) []map[string]any {
	completions := attemptJobCompletionsByID(attempt)
	if phase := phaseSpecByName(workflow.Phases, attempt.Phase); phase != nil && len(phase.Jobs) > 0 {
		jobs := make([]map[string]any, 0, len(phase.Jobs))
		for _, job := range phase.Jobs {
			jobID := firstNonEmpty(job.ID, "job")
			state, conclusion, completedAt := projectionJobCompletionAttrs(completions[jobID], workflowRunStepState(attempt), attempt.CompletedAt != nil)
			steps := make([]map[string]any, 0, len(job.Steps))
			for _, step := range job.Steps {
				slug := firstNonEmpty(step.Slug, "step")
				steps = append(steps, map[string]any{
					"step_id":      slug,
					"slug":         slug,
					"title":        firstNonEmpty(stringValueOrEmpty(step.Title), slug),
					"state":        state,
					"started_at":   attempt.DispatchedAt,
					"completed_at": completedAt,
					"exit_code":    nil,
					"message":      conclusion,
				})
			}
			if len(steps) == 0 {
				steps = append(steps, map[string]any{
					"step_id":      "job",
					"slug":         "job",
					"title":        firstNonEmpty(stringValueOrEmpty(job.Name), jobID),
					"state":        state,
					"started_at":   attempt.DispatchedAt,
					"completed_at": completedAt,
					"exit_code":    nil,
					"message":      conclusion,
				})
			}
			jobs = append(jobs, map[string]any{
				"job_id":       jobID,
				"name":         firstNonEmpty(stringValueOrEmpty(job.Name), jobID),
				"state":        state,
				"started_at":   attempt.DispatchedAt,
				"completed_at": completedAt,
				"conclusion":   conclusion,
				"steps":        steps,
			})
		}
		return jobs
	}
	if len(completions) > 0 {
		ids := make([]string, 0, len(completions))
		for id := range completions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		jobs := make([]map[string]any, 0, len(ids))
		for _, jobID := range ids {
			state, conclusion, completedAt := projectionJobCompletionAttrs(completions[jobID], workflowRunStepState(attempt), attempt.CompletedAt != nil)
			jobs = append(jobs, map[string]any{
				"job_id":       jobID,
				"name":         jobID,
				"state":        state,
				"started_at":   attempt.DispatchedAt,
				"completed_at": completedAt,
				"conclusion":   conclusion,
				"steps": []map[string]any{{
					"step_id":      "job",
					"slug":         "job",
					"title":        "Job",
					"state":        state,
					"started_at":   attempt.DispatchedAt,
					"completed_at": completedAt,
					"exit_code":    nil,
					"message":      conclusion,
				}},
			})
		}
		return jobs
	}
	jobID := firstNonEmpty(attempt.WorkflowFilename, attempt.Phase, "phase")
	stepState := workflowRunStepState(attempt)
	return []map[string]any{{
		"job_id":       jobID,
		"name":         jobID,
		"state":        stepState,
		"started_at":   attempt.DispatchedAt,
		"completed_at": attempt.CompletedAt,
		"steps": []map[string]any{{
			"step_id":      "workflow-run",
			"slug":         "workflow-run",
			"title":        "Workflow run",
			"state":        stepState,
			"started_at":   attempt.DispatchedAt,
			"completed_at": attempt.CompletedAt,
			"exit_code":    nil,
			"message":      attempt.Conclusion,
		}},
	}}
}

func attemptJobCompletionsByID(attempt RunReportAttempt) map[string]RunAttemptJobCompletion {
	out := make(map[string]RunAttemptJobCompletion, len(attempt.JobCompletions))
	for _, completion := range attempt.JobCompletions {
		if completion.JobID == "" {
			continue
		}
		out[completion.JobID] = completion
	}
	return out
}

func countAttemptGraphSteps(jobs []map[string]any) int {
	total := 0
	for _, job := range jobs {
		if steps, ok := job["steps"].([]map[string]any); ok {
			total += len(steps)
		}
	}
	return total
}

func runGraphMetadata(run RunReport) map[string]any {
	cycles := make([]map[string]any, 0, len(run.Attempts))
	for _, attempt := range run.Attempts {
		state := attemptGraphState(attempt)
		jobState := workflowRunStepState(attempt)
		cycles = append(cycles, map[string]any{
			"cycle_index":   attempt.AttemptIndex,
			"attempt_index": attempt.AttemptIndex,
			"state":         state,
			"started_at":    attempt.DispatchedAt,
			"completed_at":  attempt.CompletedAt,
			"stages": []map[string]any{{
				"stage_id": attempt.Phase,
				"name":     attempt.Phase,
				"kind":     firstNonEmpty(attempt.PhaseKind, workflowKindNativeK8sJob),
				"state":    state,
				"jobs": []map[string]any{{
					"job_id":       firstNonEmpty(attempt.WorkflowFilename, attempt.Phase, "phase"),
					"name":         firstNonEmpty(attempt.WorkflowFilename, attempt.Phase, "phase"),
					"state":        jobState,
					"started_at":   attempt.DispatchedAt,
					"completed_at": attempt.CompletedAt,
					"steps": []map[string]any{{
						"step_id":      "workflow-run",
						"slug":         "workflow-run",
						"title":        "Workflow run",
						"state":        jobState,
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
			"parent_run_ref":   run.ParentRunRef,
			"entrypoint_phase": run.EntrypointPhase,
		},
		"cycles": cycles,
	}
}

func buildRunGraphProjection(issueRef string, runs []RunReport, workflowsByKey map[string]Workflow, touchpoints []TouchpointRow, signals []GraphSignal) RunGraphProjection {
	projection := RunGraphProjection{
		IssueRef:    issueRef,
		Runs:        make([]RunProjectionRun, 0, len(runs)),
		Touchpoints: projectionTouchpoints(touchpoints),
		Signals:     projectionSignals(issueRef, runs, touchpoints, signals),
	}
	for _, run := range runs {
		workflow := workflowForRunReport(run, workflowsByKey)
		projection.Runs = append(projection.Runs, runProjectionFromReport(run, workflow, touchpoints))
	}
	projection.Edges = projectionEdges(projection.Runs, projection.Touchpoints, projection.Signals)
	projection.CurrentRunRef = projectionCurrentRunRef(projection.Runs)
	projection.DefaultFocus = projectionDefaultFocus(projection.Runs, projection.Touchpoints)
	projection.NextAction = projectionNextAction(projection.Runs, projection.Touchpoints, projection.Signals)
	return projection
}

type workflowSchemaLookupStore interface {
	GetWorkflowBySchemaRef(ctx context.Context, project, schemaRef string) (*Workflow, error)
}

func addWorkflowSchemasForRuns(ctx context.Context, store ReadStore, runs []RunReport, workflowsByKey map[string]Workflow) {
	lookup, ok := store.(workflowSchemaLookupStore)
	if !ok || lookup == nil {
		return
	}
	for _, run := range runs {
		if strings.TrimSpace(run.WorkflowSchemaRef) == "" {
			continue
		}
		key := workflowSchemaMapKey(run.Project, run.WorkflowSchemaRef)
		if _, exists := workflowsByKey[key]; exists {
			continue
		}
		wf, err := lookup.GetWorkflowBySchemaRef(ctx, run.Project, run.WorkflowSchemaRef)
		if err != nil || wf == nil {
			continue
		}
		workflowsByKey[key] = *wf
	}
}

func workflowForRunReport(run RunReport, workflowsByKey map[string]Workflow) Workflow {
	if strings.TrimSpace(run.WorkflowSchemaRef) != "" {
		if wf, ok := workflowsByKey[workflowSchemaMapKey(run.Project, run.WorkflowSchemaRef)]; ok {
			return wf
		}
	}
	return workflowsByKey[run.Project+"/"+run.Workflow]
}

func workflowSchemaMapKey(project, schemaRef string) string {
	return project + "/schema/" + schemaRef
}

func selectRunCycleForProjection(runs []RunReport, runSegment, cycleSegment string) (RunReport, bool) {
	runSegment = strings.TrimSpace(runSegment)
	cycleSegment = strings.TrimSpace(cycleSegment)
	display := runSegment
	if cycleSegment != "" && !strings.Contains(runSegment, ".") {
		display = runSegment + "." + cycleSegment
	}
	for _, run := range runs {
		if run.RunDisplayNumber != nil && strings.TrimSpace(*run.RunDisplayNumber) == display {
			return run, true
		}
		if run.RunNumber != nil && run.RunCycleNumber != nil &&
			strconv.Itoa(*run.RunNumber) == runSegment &&
			strconv.Itoa(*run.RunCycleNumber) == cycleSegment {
			return run, true
		}
	}
	if cycleSegment == "" {
		for _, run := range runs {
			if run.RunDisplayNumber != nil && strings.TrimSpace(*run.RunDisplayNumber) == runSegment {
				return run, true
			}
			if run.CycleNumber != nil && strconv.Itoa(*run.CycleNumber) == runSegment {
				return run, true
			}
		}
	}
	for _, run := range runs {
		if run.CycleNumber != nil && strconv.Itoa(*run.CycleNumber) == runSegment {
			if cycleSegment == "" || run.RunCycleNumber == nil || strconv.Itoa(*run.RunCycleNumber) == cycleSegment {
				return run, true
			}
		}
	}
	return RunReport{}, false
}

func runProjectionFromReport(run RunReport, workflow Workflow, touchpoints []TouchpointRow) RunProjectionRun {
	attemptsCount := run.AttemptsCount
	if attemptsCount == 0 {
		attemptsCount = len(run.Attempts)
	}
	return RunProjectionRun{
		RunRef:            run.RunRef,
		RunNumber:         run.RunNumber,
		RunDisplayNumber:  run.RunDisplayNumber,
		Workflow:          run.Workflow,
		WorkflowSchemaRef: run.WorkflowSchemaRef,
		State:             firstNonEmpty(run.State, "unknown"),
		CurrentPhase:      run.CurrentPhase,
		OriginKind:        run.OriginKind,
		IsCycle:           run.IsCycle,
		CycleNumber:       run.CycleNumber,
		RunCycleNumber:    run.RunCycleNumber,
		ValidationURL:     run.ValidationURL,
		AbortReason:       run.AbortReason,
		CostUSD:           run.CumulativeCostUSD,
		AttemptsCount:     attemptsCount,
		StartedAt:         run.StartedAt.Format(time.RFC3339Nano),
		UpdatedAt:         run.UpdatedAt.Format(time.RFC3339Nano),
		CompletedAt:       timeStringPtr(run.CompletedAt),
		Topology:          workflowTopologyFromWorkflow(workflow),
		Phases:            runProjectionPhases(run, workflow),
		Evidence:          runProjectionEvidence(run, touchpoints),
	}
}

func workflowTopologyFromWorkflow(workflow Workflow) RunProjectionTopology {
	topology := RunProjectionTopology{
		Phases:        []RunProjectionTopologyPhase{},
		RecycleArrows: []RunProjectionRecycle{},
		Terminal:      RunProjectionTerminal{Kind: "touchpoint", Enabled: false},
	}
	if workflow.Name == "" {
		return topology
	}
	for _, phase := range workflow.Phases {
		if phase.Name == "" {
			continue
		}
		topology.Phases = append(topology.Phases, RunProjectionTopologyPhase{
			Name:                     phase.Name,
			Kind:                     workflowPhaseKind(phase.Kind),
			Verify:                   phase.Verify,
			Always:                   phase.Always,
			EvidenceVerificationGate: phase.EvidenceVerificationGate,
			DependsOn:                sliceOrEmpty(phase.DependsOn),
			Jobs:                     runProjectionTopologyJobs(phase),
		})
		if phase.RecyclePolicy != nil {
			target := phase.RecyclePolicy.LandsAt
			if target == "" || target == "self" {
				target = phase.Name
			}
			topology.RecycleArrows = append(topology.RecycleArrows, RunProjectionRecycle{
				Source:      phase.Name,
				Target:      target,
				Trigger:     strings.Join(phase.RecyclePolicy.On, " / "),
				MaxAttempts: phase.RecyclePolicy.MaxAttempts,
				Active:      false,
				Kind:        "phase_recycle",
			})
		}
	}
	if len(topology.Phases) > 0 {
		topology.DefaultEntry = &RunProjectionDefaultEntry{Target: topology.Phases[0].Name, Active: true, Kind: "default"}
	}
	if workflow.PR.RecyclePolicy != nil {
		topology.RecycleArrows = append(topology.RecycleArrows, RunProjectionRecycle{
			Source:      "touchpoint",
			Target:      workflow.PR.RecyclePolicy.LandsAt,
			Trigger:     strings.Join(workflow.PR.RecyclePolicy.On, " / "),
			MaxAttempts: workflow.PR.RecyclePolicy.MaxAttempts,
			Active:      false,
			Kind:        "touchpoint_recycle",
		})
	}
	topology.Terminal.Enabled = workflow.PR.Enabled
	return topology
}

func runProjectionTopologyJobs(phase PhaseSpec) []RunProjectionTopologyJob {
	jobs := make([]RunProjectionTopologyJob, 0, len(phase.Jobs))
	for _, job := range phase.Jobs {
		jobs = append(jobs, RunProjectionTopologyJob{
			ID:    firstNonEmpty(job.ID, phase.Name),
			Name:  job.Name,
			Image: job.Image,
		})
	}
	return jobs
}

func runProjectionPhases(run RunReport, workflow Workflow) []RunProjectionPhase {
	if len(run.PhaseExecutions) > 0 {
		return runProjectionPhasesFromExecutions(run, workflow)
	}
	if len(workflow.Phases) > 0 {
		phases := make([]RunProjectionPhase, 0, len(workflow.Phases))
		terminalFailureSeen := false
		for _, phase := range workflow.Phases {
			attempts := attemptsForProjectionPhase(run.Attempts, phase.Name)
			state := projectionPhaseState(run, phase.Name, attempts, terminalFailureSeen, phaseSkippedByEntrypoint(workflow.Phases, phase.Name, run.EntrypointPhase))
			reason := projectionPhaseReason(state, attempts, run.AbortReason)
			phases = append(phases, RunProjectionPhase{
				Name:      phase.Name,
				Kind:      workflowPhaseKind(phase.Kind),
				State:     state,
				Reason:    reason,
				Verify:    phase.Verify,
				Always:    phase.Always,
				DependsOn: sliceOrEmpty(phase.DependsOn),
				Jobs:      runProjectionJobs(phase, state, reason, attempts),
				Attempts:  runProjectionAttempts(attempts),
			})
			if state == "failed" {
				terminalFailureSeen = true
			}
		}
		return phases
	}

	seen := map[string]bool{}
	phases := make([]RunProjectionPhase, 0)
	terminalFailureSeen := false
	for _, attempt := range run.Attempts {
		name := firstNonEmpty(attempt.Phase, "phase")
		if seen[name] {
			continue
		}
		seen[name] = true
		attempts := attemptsForProjectionPhase(run.Attempts, name)
		state := projectionPhaseState(run, name, attempts, terminalFailureSeen, false)
		reason := projectionPhaseReason(state, attempts, run.AbortReason)
		phase := PhaseSpec{
			Name:             name,
			Kind:             firstNonEmpty(attempt.PhaseKind, workflowKindNativeK8sJob),
			WorkflowFilename: attempt.WorkflowFilename,
		}
		phases = append(phases, RunProjectionPhase{
			Name:     name,
			Kind:     phase.Kind,
			State:    state,
			Reason:   reason,
			Jobs:     runProjectionJobs(phase, state, reason, attempts),
			Attempts: runProjectionAttempts(attempts),
		})
		if state == "failed" {
			terminalFailureSeen = true
		}
	}
	return phases
}

func runProjectionPhasesFromExecutions(run RunReport, workflow Workflow) []RunProjectionPhase {
	phaseByName := map[string]PhaseSpec{}
	for _, phase := range workflow.Phases {
		phaseByName[phase.Name] = phase
	}
	executionByName := map[string]RunPhaseExecution{}
	executionOrder := make([]string, 0, len(run.PhaseExecutions))
	for _, execution := range run.PhaseExecutions {
		name := firstNonEmpty(execution.Name, "phase")
		if _, ok := executionByName[name]; !ok {
			executionOrder = append(executionOrder, name)
		}
		executionByName[name] = execution
	}
	if len(workflow.Phases) > 0 {
		phases := make([]RunProjectionPhase, 0, len(workflow.Phases)+len(run.PhaseExecutions))
		emitted := map[string]bool{}
		terminalFailureSeen := false
		for _, spec := range workflow.Phases {
			if spec.Name == "" {
				continue
			}
			attempts := attemptsForProjectionPhase(run.Attempts, spec.Name)
			if execution, ok := executionByName[spec.Name]; ok {
				phase := projectionPhaseFromExecution(execution, spec, attempts, run.AbortReason)
				phases = append(phases, phase)
				emitted[spec.Name] = true
				if phase.State == "failed" {
					terminalFailureSeen = true
				}
				continue
			}
			state := projectionPhaseState(run, spec.Name, attempts, terminalFailureSeen, phaseSkippedByEntrypoint(workflow.Phases, spec.Name, run.EntrypointPhase))
			reason := projectionPhaseReason(state, attempts, run.AbortReason)
			phases = append(phases, RunProjectionPhase{
				Name:      spec.Name,
				Kind:      workflowPhaseKind(spec.Kind),
				State:     state,
				Reason:    reason,
				Verify:    spec.Verify,
				Always:    spec.Always,
				DependsOn: sliceOrEmpty(spec.DependsOn),
				Jobs:      runProjectionJobs(spec, state, reason, attempts),
				Attempts:  runProjectionAttempts(attempts),
			})
			if state == "failed" {
				terminalFailureSeen = true
			}
		}
		for _, name := range executionOrder {
			if emitted[name] {
				continue
			}
			execution := executionByName[name]
			spec := phaseByName[name]
			phases = append(phases, projectionPhaseFromExecution(execution, spec, attemptsForProjectionPhase(run.Attempts, name), run.AbortReason))
		}
		return phases
	}

	phases := make([]RunProjectionPhase, 0, len(run.PhaseExecutions))
	for _, execution := range run.PhaseExecutions {
		spec := phaseByName[execution.Name]
		attempts := attemptsForProjectionPhase(run.Attempts, execution.Name)
		phases = append(phases, projectionPhaseFromExecution(execution, spec, attempts, run.AbortReason))
	}
	return phases
}

func projectionPhaseFromExecution(execution RunPhaseExecution, spec PhaseSpec, attempts []RunReportAttempt, abortReason *string) RunProjectionPhase {
	name := firstNonEmpty(execution.Name, spec.Name, "phase")
	kind := workflowPhaseKind(firstNonEmpty(execution.Kind, spec.Kind))
	state := projectionExecutionState(execution.State)
	reason := projectionExecutionReason(state, execution.Reason, attempts, abortReason)
	return RunProjectionPhase{
		Name:      name,
		Kind:      kind,
		State:     state,
		Reason:    reason,
		Verify:    spec.Verify,
		Always:    spec.Always,
		DependsOn: sliceOrEmpty(spec.DependsOn),
		Jobs:      runProjectionJobsForExecution(execution, spec, state, reason, attempts),
		Attempts:  runProjectionAttempts(attempts),
	}
}

func projectionExecutionReason(state string, executionReason *string, attempts []RunReportAttempt, abortReason *string) *string {
	if state != "failed" {
		return executionReason
	}
	if len(attempts) == 0 && abortReason != nil && strings.TrimSpace(*abortReason) != "" {
		reason := projectionFailureReason(*abortReason)
		if reason == "dispatch_failed" {
			return stringPointerOrNil(reason)
		}
	}
	return executionReason
}

func runProjectionJobsForExecution(
	execution RunPhaseExecution,
	spec PhaseSpec,
	phaseState string,
	phaseReason *string,
	attempts []RunReportAttempt,
) []RunProjectionJob {
	executionJobs := runProjectionJobsFromExecutions(execution.Jobs, latestJobCompletionsByJob(attempts))
	executionJobs = applyUndispatchedPhaseReason(executionJobs, phaseReason, len(attempts) == 0)
	if len(spec.Jobs) == 0 {
		return executionJobs
	}
	plannedJobs := runProjectionJobs(spec, phaseState, phaseReason, nil)
	if len(executionJobs) == 0 {
		return plannedJobs
	}
	executedByID := make(map[string]RunProjectionJob, len(executionJobs))
	for _, job := range executionJobs {
		executedByID[job.ID] = job
	}
	plannedByID := make(map[string]RunProjectionJob, len(plannedJobs))
	for _, job := range plannedJobs {
		plannedByID[job.ID] = job
	}
	out := make([]RunProjectionJob, 0, len(plannedJobs)+len(executionJobs))
	emitted := map[string]bool{}
	for _, jobSpec := range spec.Jobs {
		jobID := firstNonEmpty(jobSpec.ID, "job")
		if job, ok := executedByID[jobID]; ok {
			out = append(out, job)
			emitted[jobID] = true
			continue
		}
		if job, ok := plannedByID[jobID]; ok {
			out = append(out, job)
			emitted[jobID] = true
		}
	}
	for _, job := range executionJobs {
		if emitted[job.ID] {
			continue
		}
		out = append(out, job)
	}
	return out
}

func runProjectionJobsFromExecutions(executions []RunJobExecution, completions map[string]RunAttemptJobCompletion) []RunProjectionJob {
	jobs := make([]RunProjectionJob, 0, len(executions))
	for _, execution := range executions {
		completion := completions[execution.ID]
		conclusion := stringPointerOrNil(completion.Conclusion)
		completedAt := execution.CompletedAt
		if completedAt == nil {
			completedAt = timeStringPtr(completion.CompletedAt)
		}
		steps := make([]RunProjectionStep, 0, len(execution.Steps))
		for _, step := range execution.Steps {
			steps = append(steps, RunProjectionStep{
				Slug:     firstNonEmpty(step.Slug, "step"),
				Title:    step.Title,
				State:    projectionExecutionState(step.State),
				Reason:   step.Reason,
				ExitCode: step.ExitCode,
			})
		}
		jobs = append(jobs, RunProjectionJob{
			ID:          firstNonEmpty(execution.ID, "job"),
			Name:        execution.Name,
			State:       projectionExecutionState(execution.State),
			Reason:      execution.Reason,
			K8sJobName:  execution.K8sJobName,
			Conclusion:  conclusion,
			CompletedAt: completedAt,
			Steps:       steps,
		})
	}
	return jobs
}

func applyUndispatchedPhaseReason(jobs []RunProjectionJob, phaseReason *string, undispatched bool) []RunProjectionJob {
	if !undispatched || phaseReason == nil || *phaseReason == "" {
		return jobs
	}
	out := make([]RunProjectionJob, len(jobs))
	copy(out, jobs)
	for i := range out {
		if out[i].State != "failed" {
			continue
		}
		if out[i].Reason == nil || *out[i].Reason == "" || *out[i].Reason == "job_failed" {
			out[i].Reason = phaseReason
		}
	}
	return out
}

func projectionExecutionState(state string) string {
	switch state {
	case "not_started", "skipped", "dispatching", "active", "succeeded", "failed":
		return state
	default:
		return "not_started"
	}
}

func attemptsForProjectionPhase(attempts []RunReportAttempt, phaseName string) []RunReportAttempt {
	out := make([]RunReportAttempt, 0)
	for _, attempt := range attempts {
		if firstNonEmpty(attempt.Phase, "phase") == phaseName {
			out = append(out, attempt)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AttemptIndex < out[j].AttemptIndex })
	return out
}

func projectionPhaseState(run RunReport, phaseName string, attempts []RunReportAttempt, terminalFailureSeen bool, skippedByEntrypoint bool) string {
	if len(attempts) == 0 {
		if skippedByEntrypoint || (terminalFailureSeen && projectionRunTerminal(run)) {
			return "skipped"
		}
		if run.CurrentPhase != nil && *run.CurrentPhase == phaseName {
			if projectionRunFailedTerminal(run) {
				return "failed"
			}
			return "dispatching"
		}
		return "not_started"
	}
	latest := attempts[len(attempts)-1]
	if latest.CompletedAt == nil {
		if projectionRunFailedTerminal(run) {
			return "failed"
		}
		return "dispatching"
	}
	if latest.VerificationStatus != nil {
		switch *latest.VerificationStatus {
		case "pass":
			return "succeeded"
		case "fail", "error":
			return "failed"
		}
	}
	if latest.Conclusion != nil {
		switch *latest.Conclusion {
		case "success":
			return "succeeded"
		case "cancelled", "failure", "timed_out":
			return "failed"
		}
	}
	return "succeeded"
}

func projectionRunTerminal(run RunReport) bool {
	return run.State != "" && run.State != "in_progress"
}

func projectionRunFailedTerminal(run RunReport) bool {
	switch run.State {
	case "aborted", "recycled", "failed":
		return true
	default:
		return false
	}
}

func phaseSkippedByEntrypoint(phases []PhaseSpec, phaseName string, entrypoint *string) bool {
	if entrypoint == nil || *entrypoint == "" || *entrypoint == phaseName {
		return false
	}
	for _, phase := range phases {
		if phase.Name == phaseName {
			return true
		}
		if phase.Name == *entrypoint {
			return false
		}
	}
	return false
}

func projectionPhaseReason(state string, attempts []RunReportAttempt, abortReason *string) *string {
	if state != "failed" {
		return nil
	}
	if abortReason != nil && strings.TrimSpace(*abortReason) != "" {
		return stringPointerOrNil(projectionFailureReason(*abortReason))
	}
	if len(attempts) == 0 {
		return stringPointerOrNil("job_failed")
	}
	latest := attempts[len(attempts)-1]
	if latest.VerificationStatus != nil {
		switch *latest.VerificationStatus {
		case "fail":
			return stringPointerOrNil("verification_failed")
		case "error":
			return stringPointerOrNil("verification_error")
		}
	}
	if latest.Conclusion != nil {
		switch *latest.Conclusion {
		case "cancelled":
			return stringPointerOrNil("cancelled")
		case "timed_out":
			return stringPointerOrNil("timeout")
		case "failure":
			return stringPointerOrNil("job_failed")
		}
	}
	return stringPointerOrNil("job_failed")
}

func projectionFailureReason(reason string) string {
	normalized := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case strings.Contains(normalized, "dispatch_timeout"):
		return "dispatch_timeout"
	case strings.Contains(normalized, "forward_dispatch_failed"),
		strings.Contains(normalized, "retry_dispatch_failed"),
		strings.Contains(normalized, "teardown_dispatch_failed"),
		strings.Contains(normalized, "cleanup_dispatch_failed"):
		return "dispatch_failed"
	case strings.Contains(normalized, "timeout"), strings.Contains(normalized, "timed_out"):
		return "timeout"
	case strings.Contains(normalized, "cancel"):
		return "cancelled"
	case strings.Contains(normalized, "native_dispatch_failed"):
		return "dispatch_failed"
	case strings.Contains(normalized, "admission_failed"):
		return "admission_failed"
	case strings.Contains(normalized, "verification"):
		return "verification_failed"
	case normalized != "":
		return normalized
	default:
		return "job_failed"
	}
}

func runProjectionJobs(phase PhaseSpec, phaseState string, phaseReason *string, attempts []RunReportAttempt) []RunProjectionJob {
	jobCompletions := latestJobCompletionsByJob(attempts)
	if len(phase.Jobs) == 0 {
		jobID := firstNonEmpty(phase.WorkflowFilename, phase.Name, "phase")
		if len(jobCompletions) == 1 {
			for id := range jobCompletions {
				jobID = id
			}
		}
		state, conclusion, completedAt := projectionJobCompletionAttrs(jobCompletions[jobID], phaseState, len(attempts) > 0)
		reason := projectionJobReason(state, jobCompletions[jobID], phaseReason)
		return []RunProjectionJob{{
			ID:          jobID,
			Name:        stringPointerOrNil(jobID),
			State:       state,
			Reason:      reason,
			Conclusion:  conclusion,
			CompletedAt: completedAt,
			Steps: []RunProjectionStep{{
				Slug:  "workflow-run",
				Title: stringPointerOrNil("Workflow run"),
				State: projectionStepStateForJob(state, reason, jobCompletions[jobID]),
			}},
		}}
	}
	jobs := make([]RunProjectionJob, 0, len(phase.Jobs))
	for _, job := range phase.Jobs {
		jobID := firstNonEmpty(job.ID, "job")
		state, conclusion, completedAt := projectionJobCompletionAttrs(jobCompletions[jobID], phaseState, len(attempts) > 0)
		reason := projectionJobReason(state, jobCompletions[jobID], phaseReason)
		stepState := projectionStepStateForJob(state, reason, jobCompletions[jobID])
		steps := make([]RunProjectionStep, 0, len(job.Steps))
		for _, step := range job.Steps {
			slug := firstNonEmpty(step.Slug, "step")
			steps = append(steps, RunProjectionStep{
				Slug:  slug,
				Title: step.Title,
				State: stepState,
			})
		}
		if len(steps) == 0 {
			steps = append(steps, RunProjectionStep{
				Slug:  "job",
				Title: job.Name,
				State: stepState,
			})
		}
		jobs = append(jobs, RunProjectionJob{
			ID:          jobID,
			Name:        job.Name,
			State:       state,
			Reason:      reason,
			Conclusion:  conclusion,
			CompletedAt: completedAt,
			Steps:       steps,
		})
	}
	return jobs
}

func latestJobCompletionsByJob(attempts []RunReportAttempt) map[string]RunAttemptJobCompletion {
	if len(attempts) == 0 {
		return map[string]RunAttemptJobCompletion{}
	}
	latest := attempts[len(attempts)-1]
	out := make(map[string]RunAttemptJobCompletion, len(latest.JobCompletions))
	for _, completion := range latest.JobCompletions {
		if completion.JobID == "" {
			continue
		}
		out[completion.JobID] = completion
	}
	return out
}

func projectionJobCompletionAttrs(completion RunAttemptJobCompletion, phaseState string, attempted bool) (string, *string, *string) {
	if completion.JobID == "" {
		if !attempted && (phaseState == "dispatching" || phaseState == "active") {
			return "not_started", nil, nil
		}
		return projectionJobState(phaseState), nil, nil
	}
	conclusion := stringPointerOrNil(completion.Conclusion)
	completedAt := timeStringPtr(completion.CompletedAt)
	if completion.VerificationStatus != nil {
		switch *completion.VerificationStatus {
		case "pass":
			return "succeeded", conclusion, completedAt
		case "fail", "error":
			return "failed", conclusion, completedAt
		}
	}
	switch completion.Conclusion {
	case "success":
		return "succeeded", conclusion, completedAt
	case "cancelled", "failure", "timed_out":
		return "failed", conclusion, completedAt
	case "skipped":
		return "skipped", conclusion, completedAt
	default:
		return "failed", conclusion, completedAt
	}
}

func projectionJobState(phaseState string) string {
	switch phaseState {
	case "succeeded":
		return "succeeded"
	case "failed":
		return "failed"
	case "active":
		return "active"
	case "dispatching":
		return "dispatching"
	case "skipped":
		return "skipped"
	case "not_started":
		return "not_started"
	default:
		return "failed"
	}
}

func projectionStepState(jobState string) string {
	switch jobState {
	case "succeeded", "failed", "skipped":
		return jobState
	default:
		return "not_started"
	}
}

func projectionStepStateForJob(jobState string, jobReason *string, completion RunAttemptJobCompletion) string {
	if completion.JobID == "" && jobState == "failed" && jobReason != nil && (*jobReason == "dispatch_failed" || *jobReason == "dispatch_timeout") {
		return "not_started"
	}
	return projectionStepState(jobState)
}

func projectionJobReason(state string, completion RunAttemptJobCompletion, fallback *string) *string {
	if state != "failed" {
		return nil
	}
	if completion.JobID == "" {
		if fallback != nil && *fallback != "" {
			return fallback
		}
		return stringPointerOrNil("job_failed")
	}
	if completion.VerificationStatus != nil {
		switch *completion.VerificationStatus {
		case "fail":
			return stringPointerOrNil("verification_failed")
		case "error":
			return stringPointerOrNil("verification_error")
		}
	}
	switch completion.Conclusion {
	case "cancelled":
		return stringPointerOrNil("cancelled")
	case "timed_out":
		return stringPointerOrNil("timeout")
	case "failure":
		return stringPointerOrNil("step_failed")
	default:
		return stringPointerOrNil("job_failed")
	}
}

func runProjectionAttempts(attempts []RunReportAttempt) []RunProjectionAttempt {
	out := make([]RunProjectionAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		out = append(out, RunProjectionAttempt{
			AttemptIndex:       attempt.AttemptIndex,
			Phase:              attempt.Phase,
			PhaseKind:          firstNonEmpty(attempt.PhaseKind, workflowKindNativeK8sJob),
			State:              projectionAttemptState(attempt),
			Conclusion:         attempt.Conclusion,
			VerificationStatus: attempt.VerificationStatus,
			Decision:           attempt.Decision,
			DispatchedAt:       attempt.DispatchedAt.Format(time.RFC3339Nano),
			CompletedAt:        timeStringPtr(attempt.CompletedAt),
			CostUSD:            attempt.CostUSD,
			LogArchiveURL:      attempt.LogArchiveURL,
			EvidenceRefs:       sliceOrEmpty(attempt.EvidenceRefs),
			PhaseOutputs:       mapStringOrEmpty(attempt.PhaseOutputs),
			JobCompletions:     sliceOrEmpty(attempt.JobCompletions),
		})
	}
	return out
}

func projectionAttemptState(attempt RunReportAttempt) string {
	if attempt.CompletedAt == nil {
		return "dispatching"
	}
	if attempt.VerificationStatus != nil {
		switch *attempt.VerificationStatus {
		case "pass":
			return "succeeded"
		case "fail", "error":
			return "failed"
		}
	}
	if attempt.Conclusion != nil {
		switch *attempt.Conclusion {
		case "success":
			return "succeeded"
		case "cancelled", "failure", "timed_out":
			return "failed"
		}
	}
	return "succeeded"
}

func runProjectionEvidence(run RunReport, touchpoints []TouchpointRow) []RunProjectionEvidence {
	evidence := make([]RunProjectionEvidence, 0)
	seen := map[string]bool{}
	add := func(kind, ref, label string, url *string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[kind+"\x00"+ref] {
			return
		}
		seen[kind+"\x00"+ref] = true
		evidence = append(evidence, RunProjectionEvidence{Kind: kind, Ref: ref, Label: label, URL: url})
	}
	if run.ValidationURL != nil && *run.ValidationURL != "" {
		add("validation", *run.ValidationURL, "validation", run.ValidationURL)
	}
	if run.ScreenshotsMarkdown != nil {
		for i, url := range markdownEvidenceURLs(*run.ScreenshotsMarkdown) {
			u := url
			add("screenshot", url, fmt.Sprintf("screenshot %d", i+1), &u)
		}
		if len(markdownEvidenceURLs(*run.ScreenshotsMarkdown)) == 0 && strings.TrimSpace(*run.ScreenshotsMarkdown) != "" {
			add("screenshots", "screenshots_markdown", "screenshots", nil)
		}
	}
	for _, attempt := range run.Attempts {
		for _, ref := range attempt.EvidenceRefs {
			add("artifact", ref, evidenceLabel(ref), evidenceURL(ref))
		}
		if attempt.LogArchiveURL != nil && *attempt.LogArchiveURL != "" {
			add("log", *attempt.LogArchiveURL, "native events", evidenceURL(*attempt.LogArchiveURL))
		}
	}
	for _, tp := range touchpoints {
		if tp.LinkedRunRef != nil && *tp.LinkedRunRef != run.RunRef {
			continue
		}
		if tp.HTMLURL != nil && *tp.HTMLURL != "" {
			add("pull_request", *tp.HTMLURL, fmt.Sprintf("PR #%d", tp.PRNumber), tp.HTMLURL)
		}
		if tp.ValidationURL != nil && *tp.ValidationURL != "" {
			add("validation", *tp.ValidationURL, "touchpoint validation", tp.ValidationURL)
		}
	}
	return evidence
}

func applyNativeEventsToProjectionRun(run *RunProjectionRun, events []NativeRunLogEvent) {
	if run == nil || len(events) == 0 {
		return
	}
	for phaseIndex := range run.Phases {
		phase := &run.Phases[phaseIndex]
		if len(phase.Attempts) == 0 {
			continue
		}
		latestAttempt := phase.Attempts[len(phase.Attempts)-1].AttemptIndex
		phaseActive := false
		phaseFailed := phase.State == "failed"
		for jobIndex := range phase.Jobs {
			job := &phase.Jobs[jobIndex]
			jobEvents := nativeEventsForProjectionJob(events, latestAttempt, phase.Name, job.ID)
			if len(jobEvents) == 0 {
				continue
			}
			observedStepSlug := map[string]bool{}
			for _, event := range jobEvents {
				if event.StepSlug != "" {
					observedStepSlug[event.StepSlug] = true
				}
			}
			if job.State == "dispatching" || job.State == "not_started" {
				job.State = "active"
			}
			stepBySlug := map[string]int{}
			for stepIndex := range job.Steps {
				stepBySlug[job.Steps[stepIndex].Slug] = stepIndex
			}
			for _, event := range jobEvents {
				if event.StepSlug == "" || event.Event == "log" {
					continue
				}
				stepIndex, ok := stepBySlug[event.StepSlug]
				if !ok {
					job.Steps = append(job.Steps, RunProjectionStep{Slug: event.StepSlug, Title: stringPointerOrNil(event.StepSlug), State: "not_started"})
					stepIndex = len(job.Steps) - 1
					stepBySlug[event.StepSlug] = stepIndex
				}
				step := &job.Steps[stepIndex]
				switch event.Event {
				case "step_started":
					if step.State != "succeeded" && step.State != "failed" && step.State != "skipped" {
						step.State = "active"
					}
				case "step_completed":
					step.State = "succeeded"
					step.ExitCode = event.ExitCode
				case "step_skipped":
					step.State = "skipped"
				case "step_failed":
					step.State = "failed"
					step.ExitCode = event.ExitCode
					step.Reason = stringPointerOrNil("exit_nonzero")
					job.State = "failed"
					job.Reason = stringPointerOrNil("step_failed")
					phaseFailed = true
				}
			}
			if job.State == "active" {
				phaseActive = true
			}
			resetUnobservedFailedSteps(job, observedStepSlug)
		}
		switch {
		case phaseFailed:
			phase.State = "failed"
			if phase.Reason == nil {
				phase.Reason = stringPointerOrNil("job_failed")
			}
		case phase.State == "dispatching" && phaseActive:
			phase.State = "active"
		}
	}
}

func resetUnobservedFailedSteps(job *RunProjectionJob, observedStepSlug map[string]bool) {
	if job == nil || len(observedStepSlug) == 0 {
		return
	}
	for stepIndex := range job.Steps {
		step := &job.Steps[stepIndex]
		if observedStepSlug[step.Slug] {
			continue
		}
		if step.State != "failed" {
			continue
		}
		if step.Reason != nil && *step.Reason != "job_failed" {
			continue
		}
		step.State = "not_started"
		step.Reason = nil
		step.ExitCode = nil
	}
}

func nativeEventsForProjectionJob(events []NativeRunLogEvent, attemptIndex int, phase, jobID string) []NativeRunLogEvent {
	out := make([]NativeRunLogEvent, 0)
	for _, event := range events {
		if event.AttemptIndex != attemptIndex {
			continue
		}
		if event.Phase != "" && event.Phase != phase {
			continue
		}
		if event.JobID != jobID {
			continue
		}
		out = append(out, event)
	}
	return out
}

func markdownEvidenceURLs(markdown string) []string {
	matches := markdownEvidenceURL.FindAllStringSubmatch(markdown, -1)
	urls := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 && strings.TrimSpace(match[1]) != "" {
			urls = append(urls, strings.TrimSpace(match[1]))
		}
	}
	return urls
}

func evidenceLabel(ref string) string {
	clean := strings.TrimRight(strings.Split(strings.Split(ref, "?")[0], "#")[0], "/")
	if clean == "" {
		return ref
	}
	parts := strings.Split(clean, "/")
	return firstNonEmpty(parts[len(parts)-1], ref)
}

func evidenceURL(ref string) *string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "/v1/artifacts/") {
		return &ref
	}
	if strings.HasPrefix(ref, "blob://artifacts/") {
		u := "/v1/artifacts/" + strings.TrimPrefix(ref, "blob://artifacts/")
		return &u
	}
	return nil
}

func projectionTouchpoints(touchpoints []TouchpointRow) []RunProjectionTouchpoint {
	out := make([]RunProjectionTouchpoint, 0, len(touchpoints))
	for _, tp := range touchpoints {
		out = append(out, RunProjectionTouchpoint{
			Ref:           tp.Ref,
			Repo:          tp.Repo,
			PRNumber:      tp.PRNumber,
			Title:         tp.Title,
			State:         firstNonEmpty(tp.State, "unknown"),
			HTMLURL:       tp.HTMLURL,
			LinkedRunRef:  tp.LinkedRunRef,
			ValidationURL: tp.ValidationURL,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

func projectionSignals(issueRef string, runs []RunReport, touchpoints []TouchpointRow, signals []GraphSignal) []RunProjectionSignal {
	out := make([]RunProjectionSignal, 0)
	for _, signal := range signals {
		if !signalMatchesProjection(issueRef, runs, touchpoints, signal) {
			continue
		}
		out = append(out, RunProjectionSignal{
			ID:                signal.ID,
			TargetType:        signal.TargetType,
			TargetRepo:        signal.TargetRepo,
			TargetID:          signal.TargetID,
			Source:            signal.Source,
			State:             firstNonEmpty(signal.State, "pending"),
			Kind:              firstNonEmpty(stringValue(signal.Payload["kind"]), stringValue(signal.Payload["state"])),
			Feedback:          firstNonEmpty(stringValue(signal.Payload["feedback"]), stringValue(signal.Payload["body"])),
			ProcessedDecision: signal.ProcessedDecision,
			FailureReason:     signal.FailureReason,
		})
	}
	return out
}

func projectionEdges(runs []RunProjectionRun, touchpoints []RunProjectionTouchpoint, signals []RunProjectionSignal) []RunProjectionEdge {
	edges := make([]RunProjectionEdge, 0)
	add := func(source, target, kind string) {
		if source == "" || target == "" {
			return
		}
		edges = append(edges, RunProjectionEdge{Source: source, Target: target, Kind: kind})
	}
	runRefs := map[string]bool{}
	for _, run := range runs {
		runRef := "run:" + run.RunRef
		runRefs[run.RunRef] = true
		for _, phase := range run.Phases {
			phaseRef := fmt.Sprintf("phase:%s:%s", run.RunRef, phase.Name)
			add(runRef, phaseRef, "contains")
			for _, dep := range phase.DependsOn {
				add(fmt.Sprintf("phase:%s:%s", run.RunRef, dep), phaseRef, "depends_on")
			}
			for _, job := range phase.Jobs {
				jobRef := fmt.Sprintf("job:%s:%s:%s", run.RunRef, phase.Name, job.ID)
				add(phaseRef, jobRef, "contains")
				for _, step := range job.Steps {
					add(jobRef, fmt.Sprintf("step:%s:%s:%s:%s", run.RunRef, phase.Name, job.ID, step.Slug), "contains")
				}
			}
		}
	}
	for _, tp := range touchpoints {
		if tp.LinkedRunRef != nil && runRefs[*tp.LinkedRunRef] {
			add("run:"+*tp.LinkedRunRef, "touchpoint:"+tp.Ref, "opened")
		}
	}
	for _, signal := range signals {
		signalRef := "signal:" + signal.ID
		switch signal.TargetType {
		case "run":
			for _, run := range runs {
				if signal.TargetID == run.RunRef {
					add("run:"+run.RunRef, signalRef, "feedback")
					break
				}
			}
		case "pr":
			for _, tp := range touchpoints {
				if signal.TargetID == tp.Ref || signal.TargetID == strconv.Itoa(tp.PRNumber) || signal.TargetRepo+"#"+signal.TargetID == tp.Ref {
					add("touchpoint:"+tp.Ref, signalRef, "feedback")
					break
				}
			}
		}
	}
	return edges
}

func signalMatchesProjection(issueRef string, runs []RunReport, touchpoints []TouchpointRow, signal GraphSignal) bool {
	switch signal.TargetType {
	case "issue":
		if signal.TargetID == issueRef {
			return true
		}
		if strings.HasPrefix(issueRef, signal.TargetRepo+"#") && strings.TrimPrefix(issueRef, signal.TargetRepo+"#") == signal.TargetID {
			return true
		}
	case "run":
		for _, run := range runs {
			if signal.TargetID == run.RunRef || signal.TargetID == run.ID {
				return true
			}
		}
	case "pr":
		for _, tp := range touchpoints {
			prNumber := strconv.Itoa(tp.PRNumber)
			if signal.TargetID == tp.Ref || signal.TargetID == prNumber {
				return true
			}
			if signal.TargetRepo != "" && signal.TargetRepo+"#"+signal.TargetID == tp.Ref {
				return true
			}
		}
	}
	return false
}

func projectionCurrentRunRef(runs []RunProjectionRun) *string {
	for i := len(runs) - 1; i >= 0; i-- {
		if runStateIsActive(runs[i].State) {
			return &runs[i].RunRef
		}
	}
	if len(runs) == 0 {
		return nil
	}
	return &runs[len(runs)-1].RunRef
}

func projectionDefaultFocus(runs []RunProjectionRun, touchpoints []RunProjectionTouchpoint) *RunProjectionFocus {
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		if !runStateIsActive(run.State) {
			continue
		}
		for _, phase := range run.Phases {
			if phase.State == "active" {
				return &RunProjectionFocus{Kind: "phase", Ref: run.RunRef + "#" + phase.Name}
			}
		}
		return &RunProjectionFocus{Kind: "run", Ref: run.RunRef}
	}
	for i := len(touchpoints) - 1; i >= 0; i-- {
		if touchpointNeedsDecision(touchpoints[i]) {
			return &RunProjectionFocus{Kind: "touchpoint", Ref: touchpoints[i].Ref}
		}
	}
	if len(runs) > 0 {
		return &RunProjectionFocus{Kind: "run", Ref: runs[len(runs)-1].RunRef}
	}
	return nil
}

func projectionNextAction(runs []RunProjectionRun, touchpoints []RunProjectionTouchpoint, signals []RunProjectionSignal) RunProjectionAction {
	for _, signal := range signals {
		if signal.State == "pending" || signal.State == "processing" {
			detail := signal.Feedback
			return RunProjectionAction{Kind: "feedback_pending", Label: "feedback pending", TargetRef: &signal.ID, Detail: stringPointerOrNil(detail)}
		}
	}
	for i := len(runs) - 1; i >= 0; i-- {
		if runStateIsActive(runs[i].State) {
			return RunProjectionAction{Kind: "watch_run", Label: "watch run", TargetRef: &runs[i].RunRef}
		}
	}
	for i := len(touchpoints) - 1; i >= 0; i-- {
		if touchpointNeedsDecision(touchpoints[i]) {
			return RunProjectionAction{Kind: "review_touchpoint", Label: "review touchpoint", TargetRef: &touchpoints[i].Ref}
		}
	}
	if len(runs) > 0 {
		last := runs[len(runs)-1]
		if last.State == "aborted" || last.State == "failed" {
			return RunProjectionAction{Kind: "inspect_failure", Label: "inspect failure", TargetRef: &last.RunRef}
		}
	}
	return RunProjectionAction{Kind: "none", Label: "no action"}
}

func runStateIsActive(state string) bool {
	return state == "in_progress" || state == "pending" || state == "queued"
}

func touchpointNeedsDecision(tp RunProjectionTouchpoint) bool {
	switch tp.State {
	case "ready", "needs_review", "open", "review_required":
		return true
	default:
		return false
	}
}

func workflowRunStepState(attempt RunReportAttempt) string {
	if attempt.CompletedAt != nil {
		if attempt.VerificationStatus != nil {
			switch *attempt.VerificationStatus {
			case "pass":
				return "succeeded"
			case "fail", "error":
				return "failed"
			}
		}
		if attempt.Conclusion != nil {
			switch *attempt.Conclusion {
			case "success":
				return "succeeded"
			case "cancelled", "failure", "timed_out":
				return "failed"
			}
		}
		return "succeeded"
	}
	return "active"
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

func stringValueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func timeStringPtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.Format(time.RFC3339Nano)
	return &formatted
}

func mapStringOrEmpty(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return values
}

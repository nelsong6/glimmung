/**
 * Issue detail view (#42, #81) — issue meta + tabbed content.
 *
 * Tabs: summary / runs / touchpoint.
 *   - summary: title link, body, edit form.
 *   - run: workflow's DAG painted with active run state. Phases
 *     as nodes, PR primitive as the trailing node. Cool-toned
 *     definition view when no run is in flight; nodes color in by
 *     state when one is. Click a node to drill into the latest
 *     attempt that exercised it.
 *   - runs: list/timeline of every run on this issue. Click a row
 *     to load that run in the run tab.
 *   - touchpoint: issue-level decision surface and evidence summary.
 *
 * Conceptual move per #81: "list of steps that ran" -> "graph that
 * runs". Phases render left-to-right; sibling jobs stack inside a phase.
 *
 * Routed canonically via `/projects/<project>/issues/<number>`.
 */
import { Fragment, useEffect, useMemo, useRef, useState } from "react";
import { Link, useLocation, useNavigate, useOutletContext, useParams } from "react-router-dom";
import { authedFetch, currentConfig } from "./auth";
import { lokiExploreUrl } from "./grafanaLinks";
import { PhaseGraph, type PhaseGraphPhase } from "./PhaseGraph";
import { issueRunSelectionPath } from "./routes";
import {
  runTopologyToPhaseGraphModel,
  type RunProjectionTopologySource,
  workflowToPhaseGraphModel,
} from "./workflowGraphModel";
import { resolveProjectWorkflow } from "./workflowLookup";

type IssueDetail = {
  ref: string;
  project: string;
  repo: string | null;
  number: number | null;
  title: string;
  body: string;
  state: string;
  labels: string[];
  html_url: string | null;
  metadata: Record<string, unknown>;
  comments: IssueComment[];
  last_run_ref: string | null;
  last_run_number: number | null;
  last_run_state: string | null;
  issue_lock_held: boolean;
  preserve_test_env: boolean;
};

type IssueComment = {
  id: string;
  author: string;
  body: string;
  created_at: string;
  updated_at: string;
};

export type IssueDetailTarget = { project: string; issue_number: number };

export type GraphNode = {
  id: string;
  kind: "issue" | "run" | "attempt" | "pr" | "signal";
  label: string;
  state: string | null;
  timestamp: string | null;
  metadata: Record<string, unknown>;
};

export type IssueGraph = {
  issue_ref: string;
  nodes: GraphNode[];
  edges: Array<{
    source: string;
    target: string;
    kind:
      | "spawned"
      | "attempted"
      | "retried"
      | "opened"
      | "feedback"
      | "re_dispatched"
      | "cycled_from";
  }>;
  projection?: RunGraphProjection;
};

type RunGraphProjection = {
  issue_ref: string;
  runs: RunProjectionRun[];
  edges: Array<{ source: string; target: string; kind: string }>;
  current_run_ref?: string | null;
  default_focus?: { kind: string; ref: string } | null;
  next_action: {
    kind: string;
    label: string;
    target_ref?: string | null;
    detail?: string | null;
  };
  touchpoints: RunProjectionTouchpoint[];
  signals: RunProjectionSignal[];
};

type RunProjectionRun = {
  run_ref: string;
  run_number?: number | null;
  run_display_number?: string | null;
  cycle_number?: number | null;
  run_cycle_number?: number | null;
  workflow_schema_ref?: string | null;
  queue_state?: string | null;
  admission_error?: string | null;
  slot_lease_ref?: string | null;
  workflow: string;
  state: string;
  current_phase?: string | null;
  validation_url?: string | null;
  abort_reason?: string | null;
  cost_usd: number;
  attempts_count: number;
  started_at: string;
  updated_at: string;
  completed_at?: string | null;
  topology: RunProjectionTopology;
  phases: RunProjectionPhase[];
  evidence: RunProjectionEvidence[];
};

type RunProjectionTopology = RunProjectionTopologySource & {
  default_entry: { target: string; active: boolean; kind: string } | null;
};

type RunProjectionPhase = {
  name: string;
  kind: string;
  state: string;
  reason?: string | null;
  verify: boolean;
  run_on: string;
  purpose: string;
  depends_on: string[];
  jobs: Array<{
    id: string;
    name?: string | null;
    state: string;
    reason?: string | null;
    k8s_job_name?: string | null;
    conclusion?: string | null;
    completed_at?: string | null;
    cost_usd?: number | null;
    steps: Array<{ slug: string; title?: string | null; state: string; reason?: string | null; exit_code?: number | null }>;
  }>;
  attempts: Array<{
    attempt_index: number;
    state: string;
    conclusion?: string | null;
    verification_status?: string | null;
    decision?: string | null;
    log_archive_url?: string | null;
    evidence_refs: string[];
    job_completions?: Array<{
      job_id: string;
      completed_at?: string | null;
      conclusion: string;
      verification_status?: string | null;
      verification_reasons?: string[];
      cost_usd?: number;
      phase_outputs?: Record<string, string>;
    }>;
  }>;
  inner_jobs?: RunProjectionInnerJob[];
};

// RunProjectionInnerJob mirrors server.InnerJobRef — the child k8s Job
// a phase script spawned in a slot namespace. See
// docs/inner-job-observation.md.
type RunProjectionInnerJob = {
  parent_job_id: string;
  parent_step_slug?: string | null;
  namespace: string;
  job_name: string;
  intent: string;
  label?: string;
  selector?: string;
  registered_at: string;
  state?: string; // active | succeeded | failed | unknown
  reason?: string;
  completed_at?: string | null;
  log_archive_url?: string;
};

type RunProjectionEvidence = {
  kind: string;
  ref: string;
  label: string;
  url?: string | null;
  content_type?: string | null;
  size_bytes?: number;
  duration_ms?: number;
  artifact_path?: string | null;
};

type RunProjectionTouchpoint = {
  ref: string;
  repo: string;
  pr_number: number;
  title: string;
  state: string;
  html_url?: string | null;
  linked_run_ref?: string | null;
  validation_url?: string | null;
  evidence: Array<{
    kind: string;
    ref: string;
    label: string;
    url?: string | null;
    artifact_path?: string | null;
    content_type?: string | null;
    size_bytes?: number;
    duration_ms?: number;
  }>;
};

type RunProjectionSignal = {
  id: string;
  target_type: string;
  target_repo: string;
  target_id: string;
  source: string;
  state: string;
  kind?: string;
  feedback?: string;
  processed_decision?: string | null;
  failure_reason?: string | null;
};

type NativeRunEvent = {
  project: string;
  run_ref: string;
  attempt_index: number;
  phase: string;
  job_id: string;
  seq: number;
  event: string;
  step_slug: string;
  message: string;
  exit_code: number | null;
  metadata: Record<string, unknown>;
  created_at: string;
};

type NativeRunEventsResponse = {
  project: string;
  run_ref: string;
  attempt_index: number | null;
  job_id: string | null;
  events: NativeRunEvent[];
  archive_url: string | null;
};

type NativeLogViewMode = "transcript" | "raw";

type AgentTranscriptEntry = {
  id: string;
  kind: "assistant" | "tool_call" | "tool_result" | "result" | "reasoning" | "raw";
  seq: number;
  createdAt: string;
  title: string;
  text?: string;
  toolName?: string;
  toolUseId?: string;
  input?: unknown;
  raw?: unknown;
  costUsd?: number | null;
};

type Workflow = {
  id: string;
  project: string;
  name: string;
  phases: WorkflowPhase[];
  pr: { recycle_policy: WorkflowRecyclePolicy | null };
  workflow_filename: string | null;
  workflow_ref: string | null;
  default_requirements: Record<string, unknown>;
};

type WorkflowPhase = {
  name: string;
  kind: string;
  workflow_filename: string;
  workflow_ref: string;
  verify: boolean;
  run_on?: string;
  purpose?: string;
  evidence_verification_gate?: boolean;
  depends_on?: string[];
  recycle_policy: WorkflowRecyclePolicy | null;
  jobs?: WorkflowJob[];
};

type WorkflowJob = {
  id: string;
  name?: string | null;
  image?: string;
  primitive?: string;
};

type WorkflowRecyclePolicy = {
  max_attempts: number;
  on: string[];
  lands_at: string;
};


type NativeAttemptJob = {
  job_id: string;
  name?: string | null;
  state?: string | null;
  cost_usd?: number | null;
  steps: NativeAttemptStep[];
};

type NativeAttemptStep = {
  slug: string;
  title?: string | null;
  state?: string | null;
  message?: string | null;
  exit_code?: number | null;
};

export type DispatchState =
  | { kind: "idle" }
  | { kind: "dispatching" }
  | { kind: "result"; state: string }
  | { kind: "error"; message: string };

export type AbortState =
  | { kind: "idle" }
  | { kind: "armed" }       // first click on `abort` — show `abort?` / `keep`
  | { kind: "aborting" }
  | { kind: "error"; message: string };

type AuthContext = {
  signedIn: boolean;
  isAdmin: boolean;
  snap?: {
    projects: Array<{
      name: string;
      github_repo: string;
    }>;
    workflows: Workflow[];
  } | null;
};

type Tab = "summary" | "runs" | "workflow" | "touchpoint";

const TAB_SLUGS: Record<Tab, string> = {
  summary: "summary",
  runs: "runs",
  workflow: "workflow",
  touchpoint: "touchpoint",
};

const SLUG_TO_TAB: Record<string, Tab> = {
  summary: "summary",
  runs: "runs",
  workflow: "workflow",
  touchpoint: "touchpoint",
};

const POLL_INTERVAL_MS = 3000;
const RUN_VIEWER_IDLE_DISPATCH: DispatchState = { kind: "idle" };
const RUN_VIEWER_IDLE_ABORT: AbortState = { kind: "idle" };

// Pull a human-readable cause out of the raw error string built in
// dispatchRun: `/v1/runs/dispatch -> <status>: <body>`. API errors
// arrive as `{"detail":"..."}` JSON; surface that detail if present so
// users see "403: email not allowed" instead of opaque JSON.
function formatDispatchError(message: string): string {
  const m = message.match(/-> (\d+): ([\s\S]*)$/);
  if (!m) return message;
  const status = m[1];
  const body = m[2].trim();
  try {
    const parsed = JSON.parse(body) as { detail?: unknown };
    if (typeof parsed.detail === "string" && parsed.detail) {
      return `${status}: ${parsed.detail}`;
    }
  } catch {
    // body isn't JSON — fall through and show as-is
  }
  return `${status}: ${body}`;
}

type IssueDetailRouteParams = {
  project?: string;
  issueNumber?: string;
  runId?: string;
  cycleId?: string;
  phaseId?: string;
  jobId?: string;
  stepId?: string;
  workflowRunId?: string;
};

export function IssueDetailView() {
  const navigate = useNavigate();
  const location = useLocation();
  const params = useParams<IssueDetailRouteParams>();
  const { signedIn, isAdmin, snap } = useOutletContext<AuthContext>();

  const issueNumber = params.issueNumber ? Number.parseInt(params.issueNumber, 10) : null;
  const target: IssueDetailTarget | null = params.project && issueNumber !== null && Number.isFinite(issueNumber)
    ? {
        project: params.project ?? "",
        issue_number: issueNumber,
      }
    : null;

  const baseUrl =
    target
      ? `/projects/${encodeURIComponent(target.project)}/issues/${target.issue_number}`
      : "/issues";

  // Tab is URL-driven so each tab is deep-linkable. Bare issue URLs are
  // normalized to `/summary` so the breadcrumb leaf and address bar stay aligned.
  const lastSeg = location.pathname.split("/").filter(Boolean).pop() ?? "";
  // params.runId / workflowRunId hold user-facing run numbers (e.g. "3"),
  // not internal IDs. They force their owning tab independent of graph load.
  const tab: Tab = params.workflowRunId ? "workflow" : params.runId ? "runs" : (SLUG_TO_TAB[lastSeg] ?? "summary");
  const setTab = (t: Tab) => navigate(`${baseUrl}/${TAB_SLUGS[t]}`);

  const [detail, setDetail] = useState<IssueDetail | null>(null);
  const [graph, setGraph] = useState<IssueGraph | null>(null);
  const [runProjection, setRunProjection] = useState<RunGraphProjection | null>(null);
  const [projectWorkflows, setProjectWorkflows] = useState<Workflow[]>([]);

  // Resolve the URL run-number slug to the internal graph node ID so RunViewer
  // can look it up. Null while graph is loading; tab is still "runs" via params.runId.
  const selectedRunId = (() => {
    if (!params.runId || !graph) return null;
    const node = graph.nodes
      .filter((n) => n.kind === "run")
      .find((n) => issueRunSlug(graph, n) === params.runId);
    return node ? runIdFromNode(node) : null;
  })();
  const selectedWorkflowRunId = (() => {
    if (!params.workflowRunId || !graph) return null;
    const node = graph.nodes
      .filter((n) => n.kind === "run")
      .find((n) => issueRunSlug(graph, n) === params.workflowRunId);
    return node ? runIdFromNode(node) : null;
  })();

  // Navigate to a run using its user-facing issue-scoped number (issueRunSlug),
  // never the internal backing ID.
  const selectRun = (runId: string | null) => {
    if (!runId) { navigate(`${baseUrl}/runs`); return; }
    const node = graph?.nodes.find((n) => n.kind === "run" && runIdFromNode(n) === runId);
    const slug = node && graph ? issueRunSlug(graph, node) : runId;
    navigate(`${baseUrl}/runs/${slug}`);
  };
  const selectWorkflowRun = (runId: string | null) => {
    if (!runId) { navigate(`${baseUrl}/workflow`); return; }
    const node = graph?.nodes.find((n) => n.kind === "run" && runIdFromNode(n) === runId);
    const slug = node && graph ? issueRunSlug(graph, node) : runId;
    navigate(`${baseUrl}/workflow/${slug}`);
  };
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [refreshTick, setRefreshTick] = useState(0);
  const [dispatchState, setDispatchState] = useState<DispatchState>({ kind: "idle" });
  const [abortState, setAbortState] = useState<AbortState>({ kind: "idle" });
  const issueWorkflowCandidates = useMemo(
    () => mergeWorkflows(snap?.workflows ?? [], projectWorkflows),
    [projectWorkflows, snap?.workflows],
  );
  const currentWorkflowDefinition = useMemo(
    () => detail ? singleProjectWorkflow(issueWorkflowCandidates, detail.project) : null,
    [detail, issueWorkflowCandidates],
  );
  const selectedWorkflowRun = useMemo(
    () => graph && selectedWorkflowRunId
      ? graph.nodes.find((n) => n.kind === "run" && runIdFromNode(n) === selectedWorkflowRunId) ?? null
      : null,
    [graph, selectedWorkflowRunId],
  );
  const selectedWorkflowRunWorkflow = useMemo(
    () => detail && selectedWorkflowRun
      ? resolveRunWorkflow(issueWorkflowCandidates, detail.project, selectedWorkflowRun)
      : null,
    [detail, issueWorkflowCandidates, selectedWorkflowRun],
  );
  const detailUrl =
    target
      ? `/v1/issues/by-number/${encodeURIComponent(target.project)}/${target.issue_number}`
      : null;
  const graphUrl =
    target
      ? `/v1/issues/by-number/${encodeURIComponent(target.project)}/${target.issue_number}/graph`
      : null;
  const runGraphUrl =
    target && params.runId && params.cycleId
      ? `/v1/projects/${encodeURIComponent(target.project)}` +
        `/issues/${target.issue_number}` +
        `/runs/${encodeURIComponent(params.runId)}` +
        `/cycles/${encodeURIComponent(params.cycleId)}/graph`
      : null;
  const heading =
    target
      ? `#${target.issue_number}`
      : "";
  const selectTab = (t: Tab) => {
    setTab(t);
  };

  const dispatchRun = async () => {
    if (!detail) return;
    setDispatchState({ kind: "dispatching" });
    try {
      const r = await authedFetch("/v1/runs/dispatch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          issue_number: detail.number,
          project: detail.project,
          workflow: stringOrNull(detail.metadata?.workflow) ?? undefined,
        }),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`/v1/runs/dispatch -> ${r.status}: ${text}`);
      }
      const result = await r.json() as { state?: string };
      setDispatchState({ kind: "result", state: result.state ?? "dispatched" });
      setRefreshTick((t) => t + 1);
      setTab("runs");
    } catch (e) {
      setDispatchState({ kind: "error", message: String(e) });
    }
  };

  const abortRun = async (runNumber: string) => {
    if (!detail || detail.number === null) return;
    setAbortState({ kind: "aborting" });
    try {
      const runPath = encodeURIComponent(runNumber);
      const r = await authedFetch(
        `/v1/projects/${encodeURIComponent(detail.project)}/issues/${detail.number}/runs/${runPath}/abort?reason=aborted_via_ui`,
        { method: "POST" },
      );
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`/v1/projects/${detail.project}/issues/${detail.number}/runs/${runNumber}/abort -> ${r.status}: ${text}`);
      }
      setAbortState({ kind: "idle" });
      setRefreshTick((t) => t + 1);
    } catch (e) {
      setAbortState({ kind: "error", message: String(e) });
    }
  };

  useEffect(() => {
    setAbortState({ kind: "idle" });
  }, [detail?.ref]);

  useEffect(() => {
    // On a runs/:runNumber or workflow/:runNumber URL the last segment is the run number, not a tab slug —
    // skip slug normalization so we don't strip it from the URL.
    if (params.runId || params.workflowRunId) return;
    const canonicalSlug = TAB_SLUGS[tab];
    if (lastSeg !== canonicalSlug) {
      navigate(`${baseUrl}/${canonicalSlug}`, { replace: true });
      return;
    }
  }, [baseUrl, lastSeg, navigate, params.runId, params.workflowRunId, tab]);

  useEffect(() => {
    if (!graph?.projection) return;
    if (params.workflowRunId) {
      const run = projectionRunByLegacySlug(graph.projection, params.workflowRunId);
      if (run) {
        navigate(projectionRunCyclePath(baseUrl, run), { replace: true });
      }
      return;
    }
    if (params.runId && !params.cycleId) {
      const run = latestProjectionCycleForRun(graph.projection, params.runId);
      if (run) {
        navigate(projectionRunCyclePath(baseUrl, run), { replace: true });
      }
    }
  }, [baseUrl, graph, navigate, params.cycleId, params.runId, params.workflowRunId]);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      if (!detailUrl) return;
      setError(null);
      try {
        const requests: Promise<Response>[] = [fetch(detailUrl)];
        if (graphUrl) requests.push(fetch(graphUrl));
        if (runGraphUrl) requests.push(fetch(runGraphUrl));
        const responses = await Promise.all(requests);
        const d = responses[0];
        if (!d.ok) throw new Error(`${detailUrl} -> ${d.status}`);
        if (cancelled) return;
        setDetail((await d.json()) as IssueDetail);
        if (graphUrl && responses[1]) {
          const g = responses[1];
          if (!g.ok) throw new Error(`${graphUrl} -> ${g.status}`);
          setGraph((await g.json()) as IssueGraph);
        } else {
          setGraph(null);
        }
        if (runGraphUrl && responses[2]) {
          const rg = responses[2];
          if (!rg.ok) throw new Error(`${runGraphUrl} -> ${rg.status}`);
          setRunProjection((await rg.json()) as RunGraphProjection);
        } else {
          setRunProjection(null);
        }
      } catch (e) {
        if (!cancelled) setError(String(e));
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, [detailUrl, graphUrl, runGraphUrl, refreshTick]);

  useEffect(() => {
    const project = detail?.project;
    if (!project) {
      setProjectWorkflows([]);
      return;
    }
    let cancelled = false;
    const load = async () => {
      try {
        const r = await fetch(`/v1/workflows?project=${encodeURIComponent(project)}`);
        if (!r.ok) throw new Error(`/v1/workflows?project=${project} -> ${r.status}`);
        const workflows = (await r.json()) as Workflow[];
        if (!cancelled) setProjectWorkflows(workflows);
      } catch {
        if (!cancelled) setProjectWorkflows([]);
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, [detail?.project]);

  // While the run tab is open and a run is actually in flight, poll
  // detail+graph so DAG nodes fill in as conclusions / verification /
  // decisions land server-side.
  const isInFlight = !!(detail && (detail.issue_lock_held || runStateIsActive(detail.last_run_state ?? "")));

  useEffect(() => {
    if (tab !== "runs") return;
    if (!isInFlight) return;
    const id = setInterval(() => setRefreshTick((t) => t + 1), POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [tab, isInFlight]);

  if (!target) {
    return <div className="empty">Issue route is missing a project issue number.</div>;
  }

  return (
    <>
      {error && <div className="empty error">{error}</div>}
      {detail === null && !error ? (
        <div className="empty">Loading…</div>
      ) : detail ? (
        <>
          <IssueHeader detail={detail} heading={heading} />

          <div className="dashboard-nav" aria-label="issue sections">
            <TabButton current={tab} value="summary" onSelect={selectTab}>
              summary
            </TabButton>
            <TabButton current={tab} value="runs" onSelect={selectTab}>
              runs
              {isInFlight && <span className="tab-dot" aria-label="active" />}
            </TabButton>
            <TabButton current={tab} value="workflow" onSelect={selectTab}>
              workflow
            </TabButton>
            <TabButton current={tab} value="touchpoint" onSelect={selectTab}>
              touchpoint
            </TabButton>
          </div>

          <div className="tab-panel">
            {tab === "summary" && (
              <DescriptionTab
                detail={detail}
                signedIn={signedIn}
                editing={editing}
                onEdit={() => setEditing(true)}
                onCancelEdit={() => setEditing(false)}
                onSaved={() => {
                  setEditing(false);
                  setRefreshTick((t) => t + 1);
                }}
                onCommentChanged={() => setRefreshTick((t) => t + 1)}
              />
            )}
            {tab === "runs" && (
              <RunsPane
                graph={graph}
                graphAvailable={!!graphUrl}
                project={detail.project}
                repo={detail.repo}
                detail={detail}
                currentWorkflow={currentWorkflowDefinition}
                signedIn={signedIn}
                isAdmin={isAdmin}
                dispatchState={dispatchState}
                abortState={abortState}
                onArmAbort={() => setAbortState({ kind: "armed" })}
                onCancelAbort={() => setAbortState({ kind: "idle" })}
                onConfirmAbort={(runNumber) => void abortRun(runNumber)}
                selectedRunId={selectedRunId}
                onSelectRun={selectRun}
                selectedRunProjection={runProjection?.runs[0] ?? null}
                selectedRunRequested={Boolean(params.runId)}
                selectedPhaseId={params.phaseId ?? null}
                selectedJobId={params.jobId ?? null}
                selectedStepId={params.stepId ?? null}
                executionLoading={Boolean(runGraphUrl) && runProjection === null && !error}
                onSelectProjectionRun={(run) => navigate(projectionRunCyclePath(baseUrl, run))}
                onSelectProjectionNode={(run, selection) => navigate(projectionSelectionPath(baseUrl, run, selection))}
                onViewRunWorkflow={selectWorkflowRun}
                onDispatch={() => void dispatchRun()}
                onOpenTouchpoint={() => setTab("touchpoint")}
              />
            )}
            {tab === "workflow" && (
              <WorkflowPane
                graph={graph}
                graphAvailable={!!graphUrl}
                project={detail.project}
                repo={detail.repo}
                currentWorkflow={currentWorkflowDefinition}
                selectedRun={selectedWorkflowRun}
                selectedRunWorkflow={selectedWorkflowRunWorkflow}
                selectedRunRequested={Boolean(params.workflowRunId)}
                onBackToDefinition={() => selectWorkflowRun(null)}
              />
            )}
            {tab === "touchpoint" && (
              <TouchpointTab
                graph={graph}
                graphAvailable={!!graphUrl}
                repo={detail.repo}
                signedIn={signedIn}
                isAdmin={isAdmin}
                onSubmitted={() => setRefreshTick((t) => t + 1)}
              />
            )}
          </div>
        </>
      ) : null}
    </>
  );
}

function IssueHeader({ detail, heading }: { detail: IssueDetail; heading: string }) {
  return (
    <section className="project-hero issue-hero">
      <div className="project-hero-main">
        <div className="project-kicker mono">issue</div>
        <div className="issue-title-row">
          <h2>{detail.title}</h2>
          {(detail.labels.length > 0 || detail.issue_lock_held || detail.preserve_test_env) && (
            <div className="issue-title-pills" aria-label="issue labels">
              {detail.labels.map((label) => (
                <span className="pill info" key={label}>{label}</span>
              ))}
              {detail.issue_lock_held && <span className="pill busy">in flight</span>}
              {detail.preserve_test_env && <span className="pill info" title="test env stays alive through touchpoint review">preserve env</span>}
            </div>
          )}
        </div>
        <div className="project-repo mono">{heading}</div>
      </div>
    </section>
  );
}

function TabButton({
  current,
  value,
  onSelect,
  children,
}: {
  current: Tab;
  value: Tab;
  onSelect: (t: Tab) => void;
  children: React.ReactNode;
}) {
  const selected = current === value;
  return (
    <button
      type="button"
      aria-pressed={selected}
      className={`dashboard-nav-link${selected ? " selected" : ""}`}
      onClick={() => onSelect(value)}
    >
      {children}
    </button>
  );
}

function DescriptionTab({
  detail,
  signedIn,
  editing,
  onEdit,
  onCancelEdit,
  onSaved,
  onCommentChanged,
}: {
  detail: IssueDetail;
  signedIn: boolean;
  editing: boolean;
  onEdit: () => void;
  onCancelEdit: () => void;
  onSaved: () => void;
  onCommentChanged: () => void;
}) {
  if (editing && signedIn) {
    return <IssueEditForm detail={detail} onCancel={onCancelEdit} onSaved={onSaved} />;
  }
  return (
    <>
      {signedIn && (
        <div style={{ display: "flex", justifyContent: "flex-end", marginBottom: "0.5rem" }}>
          <button type="button" className="link" onClick={onEdit}>
            edit
          </button>
        </div>
      )}
      {detail.body.trim() ? (
        <pre
          style={{
            whiteSpace: "pre-wrap",
            fontFamily: "inherit",
            background: "#0a0a0c",
            padding: "0.75rem 1rem",
            border: "1px solid #2a2a2e",
            borderRadius: "4px",
            margin: 0,
          }}
        >
          {detail.body}
        </pre>
      ) : (
        <div className="empty dim">(no description)</div>
      )}
      <IssueComments detail={detail} signedIn={signedIn} onChanged={onCommentChanged} />
    </>
  );
}

function IssueComments({
  detail,
  signedIn,
  onChanged,
}: {
  detail: IssueDetail;
  signedIn: boolean;
  onChanged: () => void;
}) {
  const [body, setBody] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editingBody, setEditingBody] = useState("");
  const [deletingId, setDeletingId] = useState<string | null>(null);

  const issueNumber = detail.number;
  const commentsUrl = issueNumber !== null
    ? `/v1/issues/by-number/${encodeURIComponent(detail.project)}/${issueNumber}/comments`
    : null;

  const postComment = async (e: React.FormEvent) => {
    e.preventDefault();
    const text = body.trim();
    if (!text) return;
    setBusy(true);
    setError(null);
    try {
      if (!commentsUrl) throw new Error("Issue number required for comments");
      const r = await authedFetch(commentsUrl, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ body: text }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setBody("");
      onChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const saveEdit = async (commentId: string) => {
    const text = editingBody.trim();
    if (!text) return;
    setBusy(true);
    setError(null);
    try {
      if (!commentsUrl) throw new Error("Issue number required for comments");
      const r = await authedFetch(`${commentsUrl}/${encodeURIComponent(commentId)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ body: text }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setEditingId(null);
      setEditingBody("");
      onChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const deleteComment = async (commentId: string) => {
    setBusy(true);
    setError(null);
    try {
      if (!commentsUrl) throw new Error("Issue number required for comments");
      const r = await authedFetch(`${commentsUrl}/${encodeURIComponent(commentId)}`, {
        method: "DELETE",
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setDeletingId(null);
      onChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="issue-comments">
      <h2>comments</h2>
      {detail.comments.length === 0 ? (
        <div className="empty dim">No comments yet.</div>
      ) : (
        <div className="comment-thread">
          {detail.comments.map((comment) => {
            const isEditing = editingId === comment.id;
            const isDeleting = deletingId === comment.id;
            return (
              <article className="comment-row" key={comment.id}>
                <div className="comment-meta">
                  <span className="mono">{comment.author}</span>
                  <span className="dim">{formatTimestamp(comment.updated_at)}</span>
                </div>
                {isEditing ? (
                  <div className="comment-edit">
                    <textarea
                      value={editingBody}
                      onChange={(e) => setEditingBody(e.target.value)}
                      rows={4}
                      disabled={busy}
                    />
                    <div className="comment-actions">
                      <button
                        type="button"
                        className="link"
                        onClick={() => void saveEdit(comment.id)}
                        disabled={busy || !editingBody.trim()}
                      >
                        save
                      </button>
                      <span className="sep">/</span>
                      <button
                        type="button"
                        className="link"
                        onClick={() => {
                          setEditingId(null);
                          setEditingBody("");
                        }}
                        disabled={busy}
                      >
                        cancel
                      </button>
                    </div>
                  </div>
                ) : (
                  <pre className="comment-body">{comment.body}</pre>
                )}
                {signedIn && !isEditing && (
                  <div className="comment-actions">
                    <button
                      type="button"
                      className="link"
                      onClick={() => {
                        setEditingId(comment.id);
                        setEditingBody(comment.body);
                        setDeletingId(null);
                      }}
                      disabled={busy}
                    >
                      edit
                    </button>
                    <span className="sep">/</span>
                    {isDeleting ? (
                      <>
                        <button
                          type="button"
                          className="link danger-text"
                          onClick={() => void deleteComment(comment.id)}
                          disabled={busy}
                        >
                          delete?
                        </button>
                        <span className="sep">/</span>
                        <button
                          type="button"
                          className="link"
                          onClick={() => setDeletingId(null)}
                          disabled={busy}
                        >
                          keep
                        </button>
                      </>
                    ) : (
                      <button
                        type="button"
                        className="link danger-text"
                        onClick={() => {
                          setDeletingId(comment.id);
                          setEditingId(null);
                        }}
                        disabled={busy}
                      >
                        delete
                      </button>
                    )}
                  </div>
                )}
              </article>
            );
          })}
        </div>
      )}
      {signedIn && (
        <form className="admin-form comment-form" onSubmit={postComment}>
          <label>
            <span>comment</span>
            <textarea
              value={body}
              onChange={(e) => setBody(e.target.value)}
              rows={4}
              disabled={busy}
            />
          </label>
          {error && <div className="error">{error}</div>}
          <button type="submit" disabled={busy || !body.trim()}>
            {busy ? "posting…" : "comment"}
          </button>
        </form>
      )}
    </section>
  );
}

export function RunViewer({
  graph,
  graphAvailable,
  signedIn,
  isAdmin = false,
  project,
  repo,
  workflow = null,
  inFlight,
  dispatchState,
  onRedispatch,
  abortState,
  onArmAbort,
  onCancelAbort,
  onConfirmAbort,
  selectedRunId,
  onBackToRuns,
  actionsVisible = true,
}: {
  graph: IssueGraph | null;
  graphAvailable: boolean;
  signedIn: boolean;
  isAdmin?: boolean;
  project: string;
  repo: string | null;
  workflow?: Workflow | null;
  inFlight: boolean;
  dispatchState: DispatchState;
  onRedispatch: () => void;
  abortState: AbortState;
  onArmAbort: () => void;
  onCancelAbort: () => void;
  onConfirmAbort: (runNumber: string) => void;
  selectedRunId: string | null;
  onBackToRuns: () => void;
  actionsVisible?: boolean;
}) {
  // Pick the run we're painting. Caller-selected wins; fall back to
  // active, then most recent. `null` only when there are no runs at
  // all on this issue.
  const focused = useMemo(() => {
    if (!graph) return null;
    if (selectedRunId) {
      const node = graph.nodes.find(
        (n) => n.kind === "run" && runIdFromNode(n) === selectedRunId,
      );
      if (node) return node;
    }
    return findActiveRun(graph) ?? findLastCompletedRun(graph);
  }, [graph, selectedRunId]);

  // Drill-in panel: which DAG node the user clicked. Reset when the
  // focused run changes so we don't carry a stale selection.
  const [drillNodeId, setDrillNodeId] = useState<string | null>(null);
  useEffect(() => {
    setDrillNodeId(null);
  }, [focused?.id]);

  if (!graphAvailable) {
    return (
      <div className="empty">
        Run details aren't available for native issues yet.
      </div>
    );
  }
  if (!graph) {
    return <div className="empty">Loading run state…</div>;
  }

  const dispatching = dispatchState.kind === "dispatching";
  const dispatchDisabled = inFlight || dispatching || !signedIn || !isAdmin;
  // Abort button shows only when an actual run record exists in flight.
  // Lock-only state (issue_lock_held but no run yet) doesn't have a run
  // id to target; that case stays as "Run lock held — waiting…".
  // Always targets the currently in-flight run, even when a different
  // historical run is selected for viewing in the run tab.
  const activeRun = findActiveRun(graph);
  const abortableRunNumber = activeRun ? runRouteSlugFromNode(activeRun) : null;
  const aborting = abortState.kind === "aborting";
  const armed = abortState.kind === "armed";
  const dispatchLabel = dispatching
    ? "dispatching…"
    : inFlight
    ? "in flight"
    : !signedIn
    ? "sign in"
    : !isAdmin
    ? "admin only"
    : "re-dispatch";
  const dispatchTitle = !signedIn
    ? undefined
    : !isAdmin && !dispatching && !inFlight
    ? "Re-dispatching is restricted to admins. Ask an admin to promote your account at auth.romaine.life/admin."
    : undefined;
  const actions = actionsVisible ? (
    <div
      className="run-actions"
      style={{ display: "flex", alignItems: "center", gap: "0.75rem", flexWrap: "wrap" }}
    >
      <button
        type="button"
        className="link"
        onClick={onRedispatch}
        disabled={dispatchDisabled}
        title={dispatchTitle}
      >
        {dispatchLabel}
      </button>
      {dispatchState.kind === "error" && (
        <span className="dispatch-error" role="alert">
          <span className="pill drain">error</span>
          <span className="dispatch-error-message">{formatDispatchError(dispatchState.message)}</span>
        </span>
      )}
      {signedIn && abortableRunNumber !== null && (
        <span style={{ marginLeft: "1rem" }}>
          {armed || aborting ? (
            <span className="confirm">
              <button
                type="button"
                className="link danger-text"
                onClick={() => onConfirmAbort(abortableRunNumber)}
                disabled={aborting}
              >
                {aborting ? "aborting…" : "abort?"}
              </button>
              <span className="sep">/</span>
              <button
                type="button"
                className="link"
                onClick={onCancelAbort}
                disabled={aborting}
              >
                keep
              </button>
            </span>
          ) : (
            <button
              type="button"
              className="link danger-text"
              onClick={onArmAbort}
            >
              abort
            </button>
          )}
        </span>
      )}
      {abortState.kind === "error" && (
        <span
          className="pill drain"
          style={{ marginLeft: "0.5rem" }}
          title={abortState.message}
        >
          abort error
        </span>
      )}
      {selectedRunId && focused && (
        <span className="dim mono">
          showing {runDisplayName(focused)}{" "}
          <button type="button" className="link" onClick={onBackToRuns}>
            back to runs
          </button>
        </span>
      )}
    </div>
  ) : null;

  if (!focused) {
    if (inFlight) {
      return (
        <>
          {actions}
          <DefinitionDag workflow={null} project={project} />
          <div className="empty">
            Run lock held — waiting for the run record to land.
          </div>
        </>
      );
    }
    return (
      <>
        {actions}
        <DefinitionDag workflow={null} project={project} />
        <div className="empty">No runs yet — re-dispatch above to start one.</div>
      </>
    );
  }

  const isActive = focused.state === "in_progress";

  return (
    <>
      {actions}
      {actionsVisible && (
        <button type="button" className="link" onClick={onBackToRuns}>
          ← runs
        </button>
      )}
      {!isActive && !selectedRunId && (
        <div className="run-status-banner">
          No run in flight. Showing the last completed run.
        </div>
      )}
      <PipelineDag
        run={focused}
        graph={graph}
        workflow={workflow}
        selectedNodeId={drillNodeId}
        onSelectNode={setDrillNodeId}
      />
      <DrillIn
        nodeId={drillNodeId}
        run={focused}
        graph={graph}
        project={project}
        repo={repo}
        onClose={() => setDrillNodeId(null)}
      />
      {drillNodeId === null && (
        <RunMetaSummary run={focused} graph={graph} repo={repo} live={isActive} />
      )}
    </>
  );
}

// Cool-toned definition view of the workflow's DAG when no run has
// landed yet.
function DefinitionDag({
  workflow,
  project,
}: {
  workflow: Workflow | null;
  project: string;
}) {
  const location = useLocation();
  const graphModel = workflow ? workflowToPhaseGraphModel(workflow) : null;
  return (
    <div className="dag-wrap">
      {graphModel && (
        <PhaseGraph
          phases={graphModel.phases}
          dagClassName="dag-definition"
          ariaLabel="workflow definition"
          entryArrows={graphModel.entryArrows}
          recycleArrows={graphModel.recycleArrows}
        />
      )}
      {!workflow && (
        <div className="dim mono" style={{ marginTop: "0.5rem" }}>
          Workflow definition unavailable in the current snapshot.
        </div>
      )}
      {workflow && (
        <div style={{ marginTop: "0.5rem" }}>
          <Link
            className="link"
            to={`/projects/${encodeURIComponent(project)}/workflows/${encodeURIComponent(workflow.name)}`}
            state={{ returnTo: location.pathname, returnLabel: "issue" }}
          >
            view workflow definition
          </Link>
        </div>
      )}
    </div>
  );
}

// Pipeline graph painted with the focused run's state. Workflow topology
// supplies phase columns and declared jobs; attempts paint runtime state.

function PipelineDag({
  run,
  graph,
  workflow,
  selectedNodeId,
  onSelectNode,
}: {
  run: GraphNode;
  graph: IssueGraph;
  workflow: Workflow | null;
  selectedNodeId: string | null;
  onSelectNode: (id: string | null) => void;
}) {
  const phaseRollups = useMemo(() => phaseNodesForRun(graph, run), [graph, run]);
  const rollupByName = useMemo(() => {
    const m = new Map<string, PhaseRollup>();
    for (const p of phaseRollups) m.set(p.phaseName, p);
    return m;
  }, [phaseRollups]);
  const meta = run.metadata;
  // Workflow topology comes from the same adapter as the definition page.
  // Run attempts only paint state onto those phase slots.
  const graphModel = useMemo(() => {
    if (workflow) return workflowToPhaseGraphModel(workflow);
    return null;
  }, [workflow]);
  const unavailableWorkflowName = stringOrNull(meta.workflow);
  if (!graphModel) {
    return (
      <div className="dag-wrap">
        <div className="empty">
          Workflow definition unavailable
          {unavailableWorkflowName ? (
            <>
              {" "}
              for <span className="mono">{unavailableWorkflowName}</span>
            </>
          ) : null}
          .
        </div>
      </div>
    );
  }
  const activeEntry = stringOrNull(meta.entrypoint_phase)
    ?? graphModel.phases[0]?.name
    ?? null;
  // Render-phase callback paints the phase's current job/attempt state.
  // The visible phase container ref is wired on PhaseGraph itself so
  // entry/recycle arrows target the phase surface, not this child job.
  const renderPhase = (graphPhase: PhaseGraphPhase) => {
    const rollup = rollupByName.get(graphPhase.name) ?? {
      phaseName: graphPhase.name,
      attempts: [],
      latest: run,
      status: { cls: "", text: "not started" },
      jobLabel: graphPhase.name,
    };
    const nativeJobs = nativeAttemptJobs(rollup.latest.metadata.jobs);
    const plannedJobs = graphPhase.jobs && graphPhase.jobs.length > 0
      ? graphPhase.jobs
      : [{ id: graphPhase.name, name: graphPhase.name }];
    const nativeByID = new Map(nativeJobs.map((job) => [job.job_id, job]));
    const plannedIDs = new Set(plannedJobs.map((job) => job.id));
    const jobNodes = [
      ...plannedJobs.map((job) => {
        const native = nativeByID.get(job.id);
        return {
          id: job.id,
          label: native?.name || job.name || job.id,
          state: native?.state || rollup.status.text,
          cls: native ? nativeStatePill(native.state || "") : rollup.status.cls,
        };
      }),
      ...nativeJobs
        .filter((job) => !plannedIDs.has(job.job_id))
        .map((job) => ({
          id: job.job_id,
          label: job.name || job.job_id,
          state: job.state || rollup.status.text,
          cls: nativeStatePill(job.state || ""),
        })),
    ];
    if (jobNodes.length > 1) {
      return (
        <>
          {jobNodes.map((job) => (
            <DagPhaseNode
              key={job.id}
              phase={{
                ...rollup,
                jobLabel: job.label,
                status: {
                  cls: job.cls,
                  text: job.state,
                },
              }}
              selected={selectedNodeId === `phase:${graphPhase.name}`}
              onSelect={() =>
                onSelectNode(
                  selectedNodeId === `phase:${graphPhase.name}` ? null : `phase:${graphPhase.name}`,
                )
              }
            />
          ))}
        </>
      );
    }
    return (
      <DagPhaseNode
        phase={rollup}
        selected={selectedNodeId === `phase:${graphPhase.name}`}
        onSelect={() =>
          onSelectNode(
            selectedNodeId === `phase:${graphPhase.name}` ? null : `phase:${graphPhase.name}`,
          )
        }
      />
    );
  };

  return (
    <div className="dag-wrap">
      <PhaseGraph
        phases={graphModel.phases}
        renderPhase={renderPhase}
        ariaLabel="pipeline"
        entryPhaseName={activeEntry}
        entryArrows={graphModel.entryArrows}
        recycleArrows={graphModel.recycleArrows}
      />
    </div>
  );
}

type PhaseRollup = {
  phaseName: string;
  attempts: GraphNode[];
  latest: GraphNode;
  status: { cls: string; text: string };
  jobLabel: string;
};

function phaseNodesForRun(graph: IssueGraph, run: GraphNode): PhaseRollup[] {
  const attempts = attemptsForRun(graph, run.id);
  const byPhase = new Map<string, GraphNode[]>();
  for (const a of attempts) {
    const phase = stringOrNull(a.metadata.phase) ?? "phase";
    const arr = byPhase.get(phase) ?? [];
    arr.push(a);
    byPhase.set(phase, arr);
  }
  const out: PhaseRollup[] = [];
  for (const [phaseName, arr] of byPhase) {
    const latest = arr[arr.length - 1];
    const jobs = nativeAttemptJobs(latest.metadata.jobs);
    const jobLabel = jobs.length > 1
      ? `${jobs.length} jobs`
      : jobs[0]?.name || jobs[0]?.job_id || phaseName;
    out.push({ phaseName, attempts: arr, latest, status: phaseStatus(latest), jobLabel });
  }
  if (out.length === 0) {
    // Run exists but no attempts dispatched yet (rare — pre-record_started
    // window). Still render a placeholder so the DAG isn't empty.
    out.push({
      phaseName: stringOrNull(run.metadata.workflow) ?? "phase",
      attempts: [],
      latest: run,
      status: { cls: "pending", text: "dispatching" },
      jobLabel: "dispatching",
    });
  }
  return out;
}

function phaseStatus(attempt: GraphNode): { cls: string; text: string } {
  const meta = attempt.metadata;
  const completed = stringOrNull(meta.completed_at);
  const conclusion = stringOrNull(meta.conclusion);
  const verification = isRecord(meta.verification) ? meta.verification : null;
  const verStatus = verification ? stringOrNull(verification.status) : null;
  const nativeJobs = nativeAttemptJobs(meta.jobs);
  const nativeRunning = nativeJobs.some((j) => j.state === "active" || j.steps.some((s) => s.state === "active"));
  const nativeFailed = nativeJobs.some((j) => j.state === "failed" || j.steps.some((s) => s.state === "failed"));
  const nativeSucceeded = nativeJobs.length > 0 && nativeJobs.every((j) => j.state === "succeeded" || j.state === "skipped");
  if (!completed) {
    if (nativeRunning) {
      return { cls: "busy", text: "running" };
    }
    if (verStatus === "pass") return { cls: "free", text: "pass" };
    if (verStatus === "fail") return { cls: "drain", text: "fail" };
    if (verStatus === "error") return { cls: "drain", text: "error" };
    if (nativeFailed) return { cls: "drain", text: "failed" };
    if (nativeSucceeded) return { cls: "free", text: "pass" };
    return { cls: "pending", text: "dispatching" };
  }
  // Verification status is the authoritative verdict on a verify phase
  // and must beat the k8s conclusion. The job conclusion can be "success"
  // (exit 0, the runner Pod ran to completion and emitted an artifact)
  // while the artifact's verification.status is "fail" — that's what
  // verify_fail looks like at this layer. The previous ordering combined
  // these in one `||` and rendered "pass" for any conclusion-success
  // attempt, hiding verify_fail aborts in the latest-run strip.
  if (verStatus === "pass") return { cls: "free", text: "pass" };
  if (verStatus === "fail") return { cls: "drain", text: "fail" };
  if (verStatus === "error") return { cls: "drain", text: "error" };
  if (conclusion === "success") return { cls: "free", text: "pass" };
  if (conclusion === "cancelled") return { cls: "drain", text: "cancelled" };
  if (conclusion) return { cls: "drain", text: conclusion };
  return { cls: "", text: "completed" };
}

function DagPhaseNode({
  phase,
  selected,
  onSelect,
}: {
  phase: PhaseRollup;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      className={`dag-node dag-node-phase${selected ? " selected" : ""}`}
      onClick={onSelect}
      aria-pressed={selected}
    >
      <div className="dag-job-head">
        <span className="dag-job-title">{phase.jobLabel}</span>
        <span className="dag-job-kicker">job</span>
      </div>
      <div className="dag-node-state">
        <span className={`pill ${phase.status.cls}`}>{phase.status.text}</span>
      </div>
      {phase.attempts.length > 1 && (
        <div className="dag-node-meta dim mono">×{phase.attempts.length}</div>
      )}
    </button>
  );
}

function DrillIn({
  nodeId,
  run,
  graph,
  project,
  repo,
  onClose,
}: {
  nodeId: string | null;
  run: GraphNode;
  graph: IssueGraph;
  project: string;
  repo: string | null;
  onClose: () => void;
}) {
  if (nodeId === null) return null;
  const meta = run.metadata;
  if (nodeId === "pr") {
    const touchpointId = stringOrNull(meta.touchpoint_ref);
    const touchpointState = stringOrNull(meta.touchpoint_state);
    const touchpointTitle = stringOrNull(meta.touchpoint_title);
    const touchpointUrl = stringOrNull(meta.touchpoint_url);
    const primitiveState = stringOrNull(meta.pr_primitive_state);
    const primitiveError = stringOrNull(meta.pr_primitive_error);
    const prNumber = numberOrNull(meta.pr_number);
    const prBranch = stringOrNull(meta.pr_branch);
    return (
      <div className="run-panel">
        <div className="run-panel-header">
          <div>
            <strong>touchpoint</strong>
            <span
              className={`pill ${
                primitiveState === "failed" ? "drain" : touchpointId || prNumber ? "free" : ""
              }`}
              style={{ marginLeft: "0.5rem" }}
            >
              {primitiveState === "failed" ? "failed" : touchpointState ?? (prNumber ? "opened" : "pending")}
            </span>
          </div>
          <button type="button" className="link" onClick={onClose}>
            close
          </button>
        </div>
        <div className="run-panel-meta">
          {touchpointId && (
            <div>
              <span className="key">touchpoint</span>{" "}
              <span className="mono" title={touchpointId}>{touchpointId.slice(0, 8)}…</span>
            </div>
          )}
          {touchpointTitle && (
            <div>
              <span className="key">title</span> <span>{touchpointTitle}</span>
            </div>
          )}
          {prNumber !== null && repo ? (
            <div>
              <span className="key">PR</span>{" "}
              <a className="mono" href={touchpointUrl || `https://github.com/${repo}/pull/${prNumber}`} target="_blank" rel="noreferrer">
                #{prNumber}
              </a>
            </div>
          ) : (
            <div className="dim mono">No touchpoint evidence opened yet for this run.</div>
          )}
          {prBranch && (
            <div>
              <span className="key">branch</span> <span className="mono">{prBranch}</span>
            </div>
          )}
          {primitiveError && (
            <div>
              <span className="key">error</span> <span className="mono">{primitiveError}</span>
            </div>
          )}
        </div>
      </div>
    );
  }
  if (nodeId.startsWith("phase:")) {
    const phaseName = nodeId.slice("phase:".length);
    const phases = phaseNodesForRun(graph, run);
    const rollup = phases.find((p) => p.phaseName === phaseName);
    if (!rollup) return null;
    return (
      <div className="run-panel">
        <div className="run-panel-header">
          <div>
            <strong>{rollup.phaseName}</strong>
            <span className={`pill ${rollup.status.cls}`} style={{ marginLeft: "0.5rem" }}>
              {rollup.status.text}
            </span>
            <span className="dim mono" style={{ marginLeft: "0.5rem" }}>
              {rollup.attempts.length} attempt{rollup.attempts.length === 1 ? "" : "s"}
            </span>
          </div>
          <button type="button" className="link" onClick={onClose}>
            close
          </button>
        </div>
        {rollup.attempts.length === 0 ? (
          <div className="empty dim">No attempts dispatched yet.</div>
        ) : (
          <div className="attempt-list">
            {rollup.attempts.map((a) => (
              <AttemptCard key={a.id} attempt={a} project={project} />
            ))}
          </div>
        )}
      </div>
    );
  }
  return null;
}

function RunMetaSummary({
  run,
  graph,
  repo,
  live,
}: {
  run: GraphNode;
  graph: IssueGraph;
  repo: string | null;
  live: boolean;
}) {
  // Lightweight tail summary so the user has context without having to
  // drill in. When a node is selected, this gets replaced by the
  // drill-in panel.
  const attempts = attemptsForRun(graph, run.id);
  const meta = run.metadata;
  const cumulativeCost = numberOrNull(meta.cumulative_cost_usd);
  const workflow = stringOrNull(meta.workflow);
  const abortReason = stringOrNull(meta.abort_reason);
  const prNumber = numberOrNull(meta.pr_number);
  const parentRunRef = stringOrNull(meta.parent_run_ref);
  const entrypointPhase = stringOrNull(meta.entrypoint_phase);
  return (
    <div className="run-panel-meta" style={{ marginTop: "0.5rem" }}>
      <div>
        <span className="key">state</span>{" "}
        <span className={`pill ${runStatePill(run.state ?? "")}`}>{run.state ?? "—"}</span>
        {live && <span className="live-dot" aria-label="live" style={{ marginLeft: "0.5rem" }} />}
      </div>
      {workflow && (
        <div>
          <span className="key">workflow</span> <span className="mono">{workflow}</span>
        </div>
      )}
      {parentRunRef && (
        <div>
          <span className="key">previous cycle</span>{" "}
          <span className="mono" title={parentRunRef}>
            {parentRunRef}
          </span>
          {entrypointPhase && (
            <>
              {" "}
              <span className="dim mono">at {entrypointPhase}</span>
            </>
          )}
        </div>
      )}
      <div>
        <span className="key">attempts</span> <span className="mono">{attempts.length}</span>
      </div>
      {cumulativeCost !== null && (
        <div>
          <span className="key">cost</span> <span className="mono">${cumulativeCost.toFixed(4)}</span>
        </div>
      )}
      {prNumber !== null && repo && (
        <div>
          <span className="key">PR</span>{" "}
          <a className="mono" href={`https://github.com/${repo}/pull/${prNumber}`} target="_blank" rel="noreferrer">
            #{prNumber}
          </a>
        </div>
      )}
      {abortReason && (
        <div>
          <span className="key">abort</span> <span className="mono">{abortReason}</span>
        </div>
      )}
    </div>
  );
}

// Threshold past which a `dispatching` attempt is visually flagged as
// stuck. Post-#79, the workflow's `started` callback fires from the
// route job's first step, which lands within ~10–15s of dispatch under
// normal conditions. 30s gives the runner some slack but flags real
// dispatch failures (the orphan-webhook bug surfaced as exactly this).
const STUCK_DISPATCHING_MS = 30_000;

function RunsPane({
  graph,
  graphAvailable,
  project,
  repo,
  detail,
  currentWorkflow,
  signedIn,
  isAdmin,
  dispatchState,
  abortState,
  onArmAbort,
  onCancelAbort,
  onConfirmAbort,
  selectedRunId,
  onSelectRun,
  selectedRunProjection,
  selectedRunRequested,
  selectedPhaseId,
  selectedJobId,
  selectedStepId,
  executionLoading,
  onSelectProjectionRun,
  onSelectProjectionNode,
  onViewRunWorkflow,
  onDispatch,
  onOpenTouchpoint,
}: {
  graph: IssueGraph | null;
  graphAvailable: boolean;
  project: string;
  repo: string | null;
  detail: IssueDetail;
  currentWorkflow: Workflow | null;
  signedIn: boolean;
  isAdmin: boolean;
  dispatchState: DispatchState;
  abortState: AbortState;
  onArmAbort: () => void;
  onCancelAbort: () => void;
  onConfirmAbort: (runNumber: string) => void;
  selectedRunId: string | null;
  onSelectRun: (runId: string | null) => void;
  selectedRunProjection: RunProjectionRun | null;
  selectedRunRequested: boolean;
  selectedPhaseId: string | null;
  selectedJobId: string | null;
  selectedStepId: string | null;
  executionLoading: boolean;
  onSelectProjectionRun: (run: RunProjectionRun) => void;
  onSelectProjectionNode: (run: RunProjectionRun, selection: ProjectionSelection) => void;
  onViewRunWorkflow: (runId: string) => void;
  onDispatch: () => void;
  onOpenTouchpoint: () => void;
}) {
  const dispatching = dispatchState.kind === "dispatching";
  // Disable for non-admins so a 403 is impossible by clicking. The server
  // is still authoritative — disabling is purely a UX layer.
  const dispatchDisabled =
    detail.issue_lock_held || dispatching || !signedIn || !isAdmin;
  const buttonLabel = dispatching
    ? "dispatching…"
    : detail.issue_lock_held
    ? "in flight"
    : !signedIn
    ? "sign in"
    : !isAdmin
    ? "admin only"
    : "new run";
  const buttonTitle = !signedIn
    ? undefined
    : !isAdmin && !dispatching && !detail.issue_lock_held
    ? "Dispatching runs is restricted to admins. Ask an admin to promote your account at auth.romaine.life/admin."
    : undefined;
  const activeRunNumber = projectionActiveRun(graph?.projection)?.run_display_number
    ?? (() => {
      const node = graph ? findActiveRun(graph) : null;
      return node ? runRouteSlugFromNode(node) : null;
    })();
  const aborting = abortState.kind === "aborting";
  const armed = abortState.kind === "armed";
  const cancelVisible = signedIn && isAdmin && activeRunNumber !== null;
  const newRunButton = (
    <div
      className="run-actions"
      style={{ display: "flex", alignItems: "center", gap: "0.75rem", flexWrap: "wrap", marginBottom: "1rem" }}
    >
      <button
        type="button"
        className="link"
        onClick={onDispatch}
        disabled={dispatchDisabled}
        title={buttonTitle}
      >
        {buttonLabel}
      </button>
      {dispatchState.kind === "result" && (
        <span className={`pill ${dispatchResultPill(dispatchState.state)}`}>
          {dispatchResultLabel(dispatchState.state)}
        </span>
      )}
      {dispatchState.kind === "error" && (
        <span className="dispatch-error" role="alert">
          <span className="pill drain">error</span>
          <span className="dispatch-error-message">{formatDispatchError(dispatchState.message)}</span>
        </span>
      )}
      {cancelVisible && (
        armed || aborting ? (
          <span className="confirm">
            <button
              type="button"
              className="link danger-text"
              onClick={() => onConfirmAbort(activeRunNumber)}
              disabled={aborting}
            >
              {aborting ? "cancelling…" : "cancel?"}
            </button>
            <span className="sep">/</span>
            <button
              type="button"
              className="link"
              onClick={onCancelAbort}
              disabled={aborting}
            >
              keep
            </button>
          </span>
        ) : (
          <button
            type="button"
            className="link danger-text"
            onClick={onArmAbort}
          >
            cancel run
          </button>
        )
      )}
      {abortState.kind === "error" && (
        <span
          className="pill drain"
          title={abortState.message}
        >
          cancel error
        </span>
      )}
    </div>
  );

  if (!graphAvailable) {
    return (
      <div className="empty">
        Run history isn't available for native issues yet.
      </div>
    );
  }
  if (!graph) {
    return <div className="empty">Loading run history…</div>;
  }
  const projectionRuns = (graph.projection?.runs ?? [])
    .slice()
    .sort((a, b) => (b.started_at ?? "").localeCompare(a.started_at ?? ""));
  const runs = graph.nodes
    .filter((n) => n.kind === "run")
    .slice()
    .sort((a, b) => (b.timestamp ?? "").localeCompare(a.timestamp ?? ""));
  if (runs.length === 0) {
    return (
      <>
        {newRunButton}
        <div className="empty">No runs yet.</div>
      </>
    );
  }
  if (selectedRunRequested) {
    if (executionLoading) {
      return <div className="empty">Loading run graph…</div>;
    }
    if (!selectedRunProjection) {
      return (
        <>
          <button type="button" className="link" onClick={() => onSelectRun(null)}>
            ← runs
          </button>
          <div className="empty">Run graph was not found.</div>
        </>
      );
    }
    return (
      <RunExecutionView
        run={selectedRunProjection}
        project={project}
        issueNumber={detail.number}
        repo={repo}
        selectedPhaseId={selectedPhaseId}
        selectedJobId={selectedJobId}
        selectedStepId={selectedStepId}
        onBackToRuns={() => onSelectRun(null)}
        onSelectNode={(selection) => onSelectProjectionNode(selectedRunProjection, selection)}
      />
    );
  }
  if (selectedRunId) {
    const run = graph.nodes.find((n) => n.kind === "run" && runIdFromNode(n) === selectedRunId) ?? null;
    return (
      <RunDetailView
        graph={graph}
        run={run}
        project={project}
        repo={repo}
        currentWorkflow={currentWorkflow}
        onBackToRuns={() => onSelectRun(null)}
        onViewRunWorkflow={() => onViewRunWorkflow(selectedRunId)}
        onOpenTouchpoint={onOpenTouchpoint}
      />
    );
  }
  return (
    <>
      {newRunButton}
      <h2>Run history</h2>
      <table>
        <thead>
          <tr>
            <th>Run</th>
            <th>Cycle</th>
            <th>Run cycle</th>
            <th>State</th>
            <th>Started</th>
            <th title="The prior cycle that directly produced this cycle, when any.">Previous</th>
            <th>Cost</th>
            <th>Touchpoint</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {(projectionRuns.length > 0 ? projectionRuns : []).map((r, index) => {
            const path = projectionRunCyclePath("", r);
            const cost = r.cost_usd;
            return (
              <tr key={r.run_ref}>
                <td className="mono">{projectionRunHistoryCountDisplay(r, index, projectionRuns.length)}</td>
                <td className="mono">
                  <button
                    type="button"
                    className="link mono"
                    title={r.run_ref}
                    onClick={() => onSelectProjectionRun(r)}
                  >
                    {projectionCycleDisplay(r)}
                  </button>
                </td>
                <td className="mono">{r.run_cycle_number ?? "—"}</td>
                <td>
                  <button
                    type="button"
                    className="link"
                    onClick={() => onSelectProjectionRun(r)}
                    title={`View ${path}`}
                  >
                    <span className={`pill ${runStatePill(r.state ?? "")}`}>{r.state ?? "—"}</span>
                  </button>
                </td>
                <td className="mono dim">{r.started_at ? formatTime(r.started_at) : "—"}</td>
                <td className="mono"><span className="dim">—</span></td>
                <td className="mono">{Number.isFinite(cost) ? `$${cost.toFixed(4)}` : "—"}</td>
                <td className="mono dim">—</td>
                <td>
                  <button
                    type="button"
                    className="link"
                    onClick={() => onSelectProjectionRun(r)}
                  >
                    view
                  </button>
                </td>
              </tr>
            );
          })}
          {projectionRuns.length === 0 && runs.map((r, index) => {
            const id = runIdFromNode(r);
            const slug = issueRunSlug(graph, r);
            const meta = r.metadata;
            const cost = numberOrNull(meta.cumulative_cost_usd);
            const prNumber = numberOrNull(meta.pr_number);
            const lineage = computeCycleLineage(graph, id);
            return (
              <tr key={r.id}>
                <td className="mono">{graphRunHistoryCountDisplay(r, index, runs.length)}</td>
                <td className="mono">
                  <button
                    type="button"
                    className="link mono"
                    title={id}
                    onClick={() => onSelectRun(id)}
                  >
                    {runSlugValueDisplay(slug)}
                  </button>
                </td>
                <td className="mono">{runCycleDisplay(r)}</td>
                <td>
                  <button
                    type="button"
                    className="link"
                    onClick={() => onSelectRun(id)}
                    title={`View ${runSlugDisplay(slug)}`}
                  >
                    <span className={`pill ${runStatePill(r.state ?? "")}`}>{r.state ?? "—"}</span>
                  </button>
                </td>
                <td className="mono dim">{r.timestamp ? formatTime(r.timestamp) : "—"}</td>
                <td className="mono">
                  {lineage.kicker ? (
                    <RunRefLink graph={graph} runId={lineage.kicker} onSelectRun={onSelectRun} />
                  ) : (
                    <span className="dim">—</span>
                  )}
                </td>
                <td className="mono">{cost !== null ? `$${cost.toFixed(4)}` : "—"}</td>
                <td className="mono dim">{prNumber !== null ? `#${prNumber}` : "—"}</td>
                <td>
                  <button
                    type="button"
                    className="link"
                    onClick={() => onSelectRun(id)}
                  >
                    view
                  </button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </>
  );
}

type ProjectionSelection = {
  phase?: string | null;
  job?: string | null;
  step?: string | null;
};

function RunExecutionView({
  run,
  project,
  issueNumber,
  repo,
  selectedPhaseId,
  selectedJobId,
  selectedStepId,
  onBackToRuns,
  onSelectNode,
}: {
  run: RunProjectionRun;
  project: string;
  issueNumber: number | null;
  repo: string | null;
  selectedPhaseId: string | null;
  selectedJobId: string | null;
  selectedStepId: string | null;
  onBackToRuns: () => void;
  onSelectNode: (selection: ProjectionSelection) => void;
}) {
  const inspectorRef = useRef<HTMLDivElement | null>(null);
  const selectedPhase = selectedPhaseId
    ? projectionPhaseForSelection(run, selectedPhaseId)
    : null;
  const selectedJob = selectedPhase && selectedJobId
    ? selectedPhase.jobs.find((job) => job.id === selectedJobId) ?? null
    : null;
  const selectedStep = selectedJob && selectedStepId
    ? selectedJob.steps.find((step) => step.slug === selectedStepId) ?? null
    : null;
  const selectedKey = selectedJob
    ? `job:${selectedPhaseId}:${selectedJob.id}`
    : selectedPhase
      ? `phase:${selectedPhase.name}`
      : null;
  const selectedRouteKey = [selectedPhaseId, selectedJobId, selectedStepId].filter(Boolean).join(":");

  useEffect(() => {
    if (!selectedPhaseId) return;
    inspectorRef.current?.scrollIntoView({ block: "start", behavior: "smooth" });
  }, [selectedPhaseId, selectedRouteKey]);

  return (
    <>
      <div className="run-section-header">
        <button type="button" className="link" onClick={onBackToRuns}>
          ← runs
        </button>
        <h2>{projectionRunLabel(run)} execution</h2>
        <span className={`pill ${runStatePill(run.state)}`}>{run.state}</span>
      </div>
      <ProjectionPipelineDag
        run={run}
        selectedKey={selectedKey}
        selectedPhaseName={selectedPhase?.name ?? null}
        onSelectNode={onSelectNode}
      />
      <div ref={inspectorRef}>
        {selectedPhase ? (
          <ProjectionInspector
            run={run}
            phase={selectedPhase}
            job={selectedJob}
            step={selectedStep}
            project={project}
            issueNumber={issueNumber}
            repo={repo}
            onClose={() => onSelectNode({})}
            onSelectStep={(jobId, stepSlug) => onSelectNode({ phase: selectedPhase.name, job: jobId, step: stepSlug })}
          />
        ) : (
          <ProjectionRunMetaSummary run={run} repo={repo} />
        )}
      </div>
    </>
  );
}

function ProjectionPipelineDag({
  run,
  selectedKey,
  selectedPhaseName,
  onSelectNode,
}: {
  run: RunProjectionRun;
  selectedKey: string | null;
  selectedPhaseName: string | null;
  onSelectNode: (selection: ProjectionSelection) => void;
}) {
  const executionPhaseByName = useMemo(() => {
    const phasesByName = new Map<string, RunProjectionPhase>();
    for (const phase of run.phases) phasesByName.set(phase.name, phase);
    return phasesByName;
  }, [run.phases]);
  const graphModel = useMemo(() => runTopologyToPhaseGraphModel(run.topology), [run.topology]);
  const renderPhase = (graphPhase: PhaseGraphPhase) => {
    const phase = executionPhaseByName.get(graphPhase.name);
    const jobs = phase && phase.jobs.length > 0
      ? phase.jobs.map((job) => ({
          id: job.id,
          name: job.name ?? job.id,
          state: job.state,
          reason: job.reason,
          selection: { phase: phase.name, job: job.id },
        }))
      : (graphPhase.jobs && graphPhase.jobs.length > 0
          ? graphPhase.jobs
          : [{ id: graphPhase.name, name: graphPhase.name }]
        ).map((job) => ({
          id: job.id,
          name: job.name ?? job.id,
          state: phase?.state ?? "not_started",
          reason: phase?.reason ?? null,
          selection: { phase: graphPhase.name, job: job.id },
        }));
    return (
      <>
        {jobs.map((job) => {
          const key = `job:${graphPhase.name}:${job.id}`;
          return (
            <button
              type="button"
              className={`dag-node dag-node-phase${selectedKey === key ? " selected" : ""}`}
              key={job.id}
              onClick={() => onSelectNode(job.selection)}
              aria-pressed={selectedKey === key}
            >
              <div className="dag-job-head">
                <span className="dag-job-title">{job.name || job.id}</span>
                <span className="dag-job-kicker">job</span>
              </div>
              <div className="dag-node-state">
                <span className={`pill ${graphStatePill(job.state)}`}>{formatGraphState(job.state)}</span>
              </div>
              {job.reason && <div className="dag-node-meta dim mono">{job.reason}</div>}
            </button>
          );
        })}
      </>
    );
  };
  return (
    <div className="dag-wrap">
      <PhaseGraph
        phases={graphModel.phases}
        renderPhase={renderPhase}
        ariaLabel="run execution"
        selectedPhaseName={selectedPhaseName}
        onSelectPhase={(phase) => onSelectNode({ phase: phase.name })}
        entryPhaseName={run.current_phase ?? null}
        entryArrows={graphModel.entryArrows}
        recycleArrows={graphModel.recycleArrows}
      />
    </div>
  );
}

function isDispatchFailureReason(reason?: string | null): boolean {
  return reason === "dispatch_failed" || reason === "dispatch_timeout";
}

function projectionDispatchFailureDetail(
  run: RunProjectionRun,
  phase: RunProjectionPhase,
  job: RunProjectionPhase["jobs"][number] | null,
): string | null {
  if (!isDispatchFailureReason(job?.reason ?? phase.reason)) {
    return null;
  }
  return run.abort_reason
    ?? "The job was not dispatched, so no step-level logs were created.";
}

function projectionPhaseForSelection(run: RunProjectionRun, phaseName: string): RunProjectionPhase | null {
  const projected = run.phases.find((phase) => phase.name === phaseName);
  if (projected) return projected;
  const topologyPhase = run.topology.phases.find((phase) => phase.name === phaseName);
  if (!topologyPhase) return null;
  const jobs = topologyPhase.jobs && topologyPhase.jobs.length > 0
    ? topologyPhase.jobs
    : [{ id: topologyPhase.name, name: topologyPhase.name }];
  return {
    name: topologyPhase.name,
    kind: topologyPhase.kind,
    state: "not_started",
    verify: topologyPhase.verify ?? false,
    run_on: topologyPhase.run_on ?? "success",
    purpose: topologyPhase.purpose ?? "work",
    depends_on: topologyPhase.depends_on ?? [],
    jobs: jobs.map((job) => ({
      id: job.id,
      name: job.name ?? job.id,
      state: "not_started",
      steps: [{
        slug: "job",
        title: job.name ?? job.id,
        state: "not_started",
      }],
    })),
    attempts: [],
  };
}

function ProjectionInspector({
  run,
  phase,
  job,
  step,
  project,
  issueNumber,
  repo,
  onClose,
  onSelectStep,
}: {
  run: RunProjectionRun;
  phase: RunProjectionPhase;
  job: RunProjectionPhase["jobs"][number] | null;
  step: RunProjectionPhase["jobs"][number]["steps"][number] | null;
  project: string;
  issueNumber: number | null;
  repo: string | null;
  onClose: () => void;
  onSelectStep: (jobId: string, stepSlug: string) => void;
}) {
  const latestAttempt = phase.attempts[phase.attempts.length - 1] ?? null;
  const selectedJob = job ?? phase.jobs[0] ?? null;
  const nativeJob = selectedJob ? projectionJobToNativeJob(selectedJob) : null;
  const runNumber = run.run_display_number ?? (run.run_number ? `${run.run_number}.${run.run_cycle_number ?? 1}` : null);
  const dispatchFailureDetail = projectionDispatchFailureDetail(run, phase, selectedJob);
  const selectedJobCost = selectedJob?.cost_usd ?? null;
  return (
    <div className="run-panel">
      <div className="run-panel-header">
        <div>
          <strong>{selectedJob ? selectedJob.name || selectedJob.id : phase.name}</strong>
          <span className={`pill ${graphStatePill(selectedJob?.state ?? phase.state)}`} style={{ marginLeft: "0.5rem" }}>
            {formatGraphState(selectedJob?.state ?? phase.state)}
          </span>
          {selectedJob?.reason && <span className="dim mono" style={{ marginLeft: "0.5rem" }}>{selectedJob.reason}</span>}
        </div>
        <button type="button" className="link" onClick={onClose}>
          close
        </button>
      </div>
      <div className="run-panel-meta">
        <div>
          <span className="key">phase</span> <span className="mono">{phase.name}</span>
        </div>
        {selectedJob && (
          <div>
            <span className="key">job</span> <span className="mono">{selectedJob.id}</span>
          </div>
        )}
        {selectedJobCost !== null && (
          <div>
            <span className="key">job cost</span> <span className="mono">{formatUsd4(selectedJobCost)}</span>
          </div>
        )}
        {selectedJob?.k8s_job_name && (
          <div>
            <span className="key">k8s</span> <span className="mono">{selectedJob.k8s_job_name}</span>{" "}
            {(() => {
              // Render a Grafana Explore deep-link to the pod's Loki
              // stream so the data is one click away. We intentionally
              // do not bound `to` for active jobs — Grafana follows
              // "now" so a live stuck step keeps streaming. For
              // completed jobs we still leave a generous window (the
              // job projection does not carry started_at, so we anchor
              // on the run's started_at — phases are short relative to
              // the 24h default fallback).
              const lokiUrl = lokiExploreUrl(currentConfig(), selectedJob.k8s_job_name, {
                from: run.started_at,
                to: selectedJob.completed_at ?? undefined,
              });
              return lokiUrl ? (
                <a
                  href={lokiUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="link"
                  style={{ marginLeft: "0.5rem" }}
                  title="open this pod's logs in Grafana Explore (Loki)"
                >
                  logs ↗
                </a>
              ) : null;
            })()}
          </div>
        )}
        {step && (
          <div>
            <span className="key">step</span> <span className="mono">{step.slug}</span>
          </div>
        )}
        {selectedJob && phase.inner_jobs && phase.inner_jobs.length > 0 && (
          <InnerJobsRow innerJobs={phase.inner_jobs.filter((ij) => ij.parent_job_id === selectedJob.id)} run={run} />
        )}
        {latestAttempt && (
          <div>
            <span className="key">attempt</span> <span className="mono">{latestAttempt.attempt_index}</span>
          </div>
        )}
        {repo && (
          <div>
            <span className="key">repo</span> <span className="mono">{repo}</span>
          </div>
        )}
      </div>
      {dispatchFailureDetail && (
        <div className="run-failure-detail" role="alert">
          <span className="pill drain">dispatch failed</span>
          <span className="mono">{dispatchFailureDetail}</span>
        </div>
      )}
      {selectedJob && nativeJob ? (
        latestAttempt && issueNumber !== null && runNumber ? (
          <NativeJobInspector
            project={project}
            runId={run.run_ref}
            issueNumber={issueNumber}
            runNumber={runNumber}
            attemptIndex={latestAttempt.attempt_index}
            jobs={[nativeJob]}
            archiveUrl={latestAttempt.log_archive_url ?? null}
            live={selectedJob.state === "active" || selectedJob.state === "dispatching"}
            selectedJobId={selectedJob.id}
            selectedStepSlug={step?.slug ?? null}
            onSelectStep={onSelectStep}
          />
        ) : (
          <PlannedNativeJobInspector
            job={nativeJob}
            selectedStepSlug={step?.slug ?? null}
            onSelectStep={onSelectStep}
          />
        )
      ) : (
        <div className="native-log-panel dim mono">No job logs are available for this selection.</div>
      )}
    </div>
  );
}

// InnerJobsRow renders the child k8s Jobs a phase script spawned in a
// slot namespace (the ambience verification-agent pattern). Each row
// shows the child's namespace + job_name + intent + state, and a
// Grafana Explore deep-link scoped to the child's namespace so logs
// are one click away even though the child runs outside glimmung-runs.
function InnerJobsRow({
  innerJobs,
  run,
}: {
  innerJobs: RunProjectionInnerJob[];
  run: RunProjectionRun;
}) {
  if (!innerJobs.length) return null;
  return (
    <div style={{ width: "100%" }}>
      <div>
        <span className="key">inner jobs</span>
      </div>
      <ul style={{ listStyle: "none", margin: "0.25rem 0 0", padding: "0 0 0 0.5rem" }}>
        {innerJobs.map((ij) => {
          // Prefer the durable URL the reconciler stamped on
          // termination (it covers the child's full life window with
          // the canonical reconciler-time bounds). Fall back to the
          // client-built Loki link while the child is still active or
          // when the reconciler hasn't observed it yet.
          const lokiUrl =
            ij.log_archive_url ??
            lokiExploreUrl(
              currentConfig(),
              ij.job_name,
              { from: ij.registered_at, to: ij.completed_at ?? undefined },
              ij.namespace,
            );
          const state = (ij.state ?? "unknown").trim() || "unknown";
          const pill = state === "succeeded" ? "free" : state === "failed" ? "drain" : "busy";
          return (
            <li key={`${ij.namespace}/${ij.job_name}`} className="run-panel-meta" style={{ padding: "0.15rem 0" }}>
              <div>
                <span className={`pill ${pill}`}>{state}</span>{" "}
                <span className="mono">{ij.namespace}/{ij.job_name}</span>
              </div>
              <div>
                <span className="key">intent</span> <span className="mono">{ij.intent || "unknown"}</span>
                {ij.label && (
                  <>
                    {" "}
                    <span className="key">label</span> <span className="mono">{ij.label}</span>
                  </>
                )}
                {ij.reason && (
                  <>
                    {" "}
                    <span className="key">reason</span> <span className="mono">{ij.reason}</span>
                  </>
                )}
                {lokiUrl && (
                  <a
                    href={lokiUrl}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="link"
                    style={{ marginLeft: "0.5rem" }}
                    title={`open ${ij.namespace}/${ij.job_name} logs in Grafana Explore`}
                  >
                    logs ↗
                  </a>
                )}
              </div>
            </li>
          );
        })}
      </ul>
      {/* run.started_at is referenced only so the prop is typed; the
          inner job's registered_at is the authoritative start. */}
      <span style={{ display: "none" }}>{run.started_at}</span>
    </div>
  );
}

function ProjectionRunMetaSummary({ run, repo }: { run: RunProjectionRun; repo: string | null }) {
  const counts = countProjectionPhases(run);
  return (
    <div className="run-panel-meta" style={{ marginTop: "0.5rem" }}>
      <div>
        <span className="key">state</span>{" "}
        <span className={`pill ${runStatePill(run.state)}`}>{run.state}</span>
      </div>
      <div>
        <span className="key">workflow</span> <span className="mono">{run.workflow}</span>
      </div>
      <div>
        <span className="key">phases</span>{" "}
        <span className="mono">
          {counts.succeeded} done / {counts.active} active / {counts.failed} failed / {counts.skipped} skipped
        </span>
      </div>
      <div>
        <span className="key">cost</span> <span className="mono">${run.cost_usd.toFixed(4)}</span>
      </div>
      {run.abort_reason && (
        <div>
          <span className="key">abort</span> <span className="mono">{run.abort_reason}</span>
        </div>
      )}
      {repo && (
        <div>
          <span className="key">repo</span> <span className="mono">{repo}</span>
        </div>
      )}
    </div>
  );
}

function projectionJobToNativeJob(job: RunProjectionPhase["jobs"][number]): NativeAttemptJob {
  return {
    job_id: job.id,
    name: job.name,
    state: job.state,
    cost_usd: job.cost_usd ?? null,
    steps: job.steps.map((step) => ({
      slug: step.slug,
      title: step.title,
      state: step.state,
      exit_code: step.exit_code ?? null,
    })),
  };
}

function WorkflowPane({
  graph,
  graphAvailable,
  project,
  repo,
  currentWorkflow,
  selectedRun,
  selectedRunWorkflow,
  selectedRunRequested,
  onBackToDefinition,
}: {
  graph: IssueGraph | null;
  graphAvailable: boolean;
  project: string;
  repo: string | null;
  currentWorkflow: Workflow | null;
  selectedRun: GraphNode | null;
  selectedRunWorkflow: Workflow | null;
  selectedRunRequested: boolean;
  onBackToDefinition: () => void;
}) {
  if (!graphAvailable) {
    return (
      <div className="empty">
        Workflow state isn't available for native issues yet.
      </div>
    );
  }
  if (selectedRunRequested && !graph) {
    return <div className="empty">Loading run workflow…</div>;
  }
  if (selectedRunRequested && !selectedRun) {
    return (
      <>
        <button type="button" className="link" onClick={onBackToDefinition}>
          back to workflow definition
        </button>
        <div className="empty">Run workflow was not found.</div>
      </>
    );
  }
  if (selectedRun && graph) {
    return (
      <>
        <div className="run-section-header">
          <h2>{runDisplayName(selectedRun)} workflow</h2>
          <button type="button" className="link" onClick={onBackToDefinition}>
            back to workflow definition
          </button>
        </div>
        <RunViewer
          graph={graph}
          graphAvailable={graphAvailable}
          signedIn={false}
          project={project}
          repo={repo}
          workflow={selectedRunWorkflow}
          inFlight={selectedRun.state === "in_progress"}
          dispatchState={RUN_VIEWER_IDLE_DISPATCH}
          onRedispatch={() => undefined}
          abortState={RUN_VIEWER_IDLE_ABORT}
          onArmAbort={() => undefined}
          onCancelAbort={() => undefined}
          onConfirmAbort={() => undefined}
          selectedRunId={runIdFromNode(selectedRun)}
          onBackToRuns={onBackToDefinition}
          actionsVisible={false}
        />
      </>
    );
  }
  return (
    <section>
      <div className="run-section-header">
        <h2>Workflow definition</h2>
      </div>
      <DefinitionDag workflow={currentWorkflow} project={project} />
    </section>
  );
}

function RunDetailView({
  graph,
  run,
  project,
  repo,
  currentWorkflow,
  onBackToRuns,
  onViewRunWorkflow,
  onOpenTouchpoint,
}: {
  graph: IssueGraph;
  run: GraphNode | null;
  project: string;
  repo: string | null;
  currentWorkflow: Workflow | null;
  onBackToRuns: () => void;
  onViewRunWorkflow: () => void;
  onOpenTouchpoint: () => void;
}) {
  if (!run) {
    return (
      <>
        <button type="button" className="link" onClick={onBackToRuns}>
          ← runs
        </button>
        <div className="empty">Run detail was not found.</div>
      </>
    );
  }
  const attempts = attemptsForRun(graph, run.id);
  const meta = run.metadata;
  const workflow = stringOrNull(meta.workflow);
  const entrypointPhase = stringOrNull(meta.entrypoint_phase);
  const abortReason = stringOrNull(meta.abort_reason);
  const cumulativeCost = numberOrNull(meta.cumulative_cost_usd);
  const prNumber = numberOrNull(meta.pr_number);
  const lineage = computeCycleLineage(graph, runIdFromNode(run));
  return (
    <>
      <div className="run-section-header">
        <button type="button" className="link" onClick={onBackToRuns}>
          ← runs
        </button>
        <button type="button" className="link" onClick={onViewRunWorkflow}>
          view run workflow
        </button>
      </div>
      <section>
        <div className="run-section-header">
          <h2>{runDisplayName(run)} detail</h2>
          <span className={`pill ${runStatePill(run.state ?? "")}`}>{run.state ?? "—"}</span>
        </div>
        <div className="project-info">
          <div className="row">
            <span className="key">project</span>
            <span className="val mono">{project}</span>
          </div>
          <div className="row">
            <span className="key">workflow</span>
            <span className="val mono">{workflow ?? "—"}</span>
          </div>
          <div className="row">
            <span className="key">current definition</span>
            <span className="val mono">{currentWorkflow?.name ?? "unavailable"}</span>
          </div>
          <div className="row">
            <span className="key">cycle</span>
            <span className="val mono">{issueRunSlug(graph, run)}</span>
          </div>
          <div className="row">
            <span className="key">run</span>
            <span className="val mono">{runNumberDisplay(run)}</span>
          </div>
          <div className="row">
            <span className="key">run cycle</span>
            <span className="val mono">{runCycleDisplay(run)}</span>
          </div>
          <div className="row">
            <span className="key">started</span>
            <span className="val mono">{run.timestamp ? formatTime(run.timestamp) : "—"}</span>
          </div>
          <div className="row">
            <span className="key">cycle depth</span>
            <span className="val mono">{lineage.depth}</span>
          </div>
          <div className="row">
            <span className="key">previous</span>
            <span className="val mono">
              {lineage.kicker ? (
                <RunRefLink graph={graph} runId={lineage.kicker} onSelectRun={() => undefined} />
              ) : "—"}
            </span>
          </div>
          <div className="row">
            <span className="key">entrypoint</span>
            <span className="val mono">{entrypointPhase ?? "default"}</span>
          </div>
          <div className="row">
            <span className="key">attempts</span>
            <span className="val mono">{attempts.length}</span>
          </div>
          <div className="row">
            <span className="key">cost</span>
            <span className="val mono">{cumulativeCost !== null ? `$${cumulativeCost.toFixed(4)}` : "—"}</span>
          </div>
          <div className="row">
            <span className="key">touchpoint</span>
            <span className="val">
              {prNumber !== null && repo ? (
                <a className="mono" href={`https://github.com/${repo}/pull/${prNumber}`} target="_blank" rel="noreferrer">
                  #{prNumber}
                </a>
              ) : (
                <button type="button" className="link" onClick={onOpenTouchpoint}>
                  view touchpoint
                </button>
              )}
            </span>
          </div>
          {abortReason && (
            <div className="row">
              <span className="key">abort</span>
              <span className="val mono">{abortReason}</span>
            </div>
          )}
        </div>
      </section>
      <h2>Attempts</h2>
      {attempts.length > 0 ? (
        <div className="attempt-list">
          {attempts.map((attempt) => (
            <AttemptCard key={attempt.id} attempt={attempt} project={project} />
          ))}
        </div>
      ) : (
        <div className="empty">No attempts recorded for this run.</div>
      )}
    </>
  );
}

function TouchpointTab({
  graph,
  graphAvailable,
  repo,
  signedIn,
  isAdmin,
  onSubmitted,
}: {
  graph: IssueGraph | null;
  graphAvailable: boolean;
  repo: string | null;
  signedIn: boolean;
  isAdmin: boolean;
  onSubmitted: () => void;
}) {
  const [feedback, setFeedback] = useState("");
  const [reject, setReject] = useState<
    | { kind: "idle" }
    | { kind: "submitting" }
    | { kind: "submitted"; signalId: string }
    | { kind: "error"; message: string }
  >({ kind: "idle" });
  const [approve, setApprove] = useState<
    | { kind: "idle" }
    | { kind: "submitting" }
    | { kind: "submitted"; signalId: string }
    | { kind: "error"; message: string }
  >({ kind: "idle" });

  if (!graphAvailable) {
    return (
      <div className="empty">
        Touchpoint evidence isn't available for native issues yet.
      </div>
    );
  }
  if (!graph) {
    return <div className="empty">Loading touchpoint…</div>;
  }

  const projection = graph.projection;
  const projectionTouchpoints = projection?.touchpoints ?? [];
  const projectedRun = latestProjectionRun(projection);
  const projectedTouchpoint = projectionTouchpoints.find((tp) => touchpointNeedsDecision(tp))
    ?? projectionTouchpoints[projectionTouchpoints.length - 1]
    ?? null;
  const pendingSignal = projection?.signals.find((signal) => (
    signal.state === "pending" || signal.state === "processing"
  )) ?? null;
  const prNodes = graph.nodes.filter((n) => n.kind === "pr");
  const latestRun = findActiveRun(graph) ?? findLastCompletedRun(graph);
  const latestMeta = latestRun?.metadata ?? {};
  const latestPr = prNodes[prNodes.length - 1] ?? null;
  const latestPrMeta = latestPr?.metadata ?? {};
  const prNumber =
    projectedTouchpoint?.pr_number
    ?? numberOrNull(latestMeta.pr_number)
    ?? numberOrNull(latestPrMeta.number)
    ?? prNumberFromNode(latestPr);
  const touchpointTitle = projectedTouchpoint?.title ?? stringOrNull(latestMeta.touchpoint_title) ?? stringOrNull(latestPrMeta.title);
  const touchpointState = projectedTouchpoint?.state ?? stringOrNull(latestMeta.touchpoint_state) ?? latestPr?.state;
  const touchpointUrl = projectedTouchpoint?.html_url ?? stringOrNull(latestMeta.touchpoint_url) ?? stringOrNull(latestPrMeta.html_url);
  const evidenceRepo = projectedTouchpoint?.repo ?? repo ?? stringOrNull(latestPrMeta.repo);
  const validationUrl = projectedRun?.validation_url ?? projectedTouchpoint?.validation_url ?? stringOrNull(latestMeta.validation_url);
  const projectionEvidence = projectedRun?.evidence ?? [];
  const structuredVideos = projectionEvidence.filter(isInlineVideoEvidence);
  const structuredScreenshots = projectionEvidence.filter(isInlineScreenshotEvidence);
  const listedEvidence = projectionEvidence.filter((item) => (
    !isInlineVideoEvidence(item)
    && !isInlineScreenshotEvidence(item)
    && !(structuredVideos.length > 0 && isRawVideoArtifact(item))
    && !(structuredScreenshots.length > 0 && isRawScreenshotArtifact(item))
  ));
  const hasCurrentEvidence = prNumber !== null || Boolean(validationUrl) || projectionEvidence.length > 0;
  const canReject = signedIn && isAdmin && !pendingSignal && prNumber !== null && Boolean(evidenceRepo) && reject.kind !== "submitting" && approve.kind !== "submitting";
  const canApprove = signedIn && isAdmin && !pendingSignal && prNumber !== null && Boolean(evidenceRepo) && approve.kind !== "submitting" && reject.kind !== "submitting";

  const submitApprove = async () => {
    if (prNumber === null || !evidenceRepo) return;
    setApprove({ kind: "submitting" });
    try {
      const r = await authedFetch("/v1/signals", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          target_type: "pr",
          target_repo: evidenceRepo,
          target_ref: String(prNumber),
          source: "glimmung_ui",
          payload: { kind: "approve" },
        }),
      });
      if (!r.ok) throw new Error(`/v1/signals -> ${r.status}: ${await r.text()}`);
      const sig = await r.json() as { ref?: string };
      setApprove({ kind: "submitted", signalId: sig.ref ?? "signal" });
      onSubmitted();
      window.setTimeout(onSubmitted, 3000);
    } catch (e) {
      setApprove({ kind: "error", message: e instanceof Error ? e.message : String(e) });
    }
  };

  const submitReject = async () => {
    if (!feedback.trim() || prNumber === null || !evidenceRepo) return;
    setReject({ kind: "submitting" });
    try {
      const r = await authedFetch("/v1/signals", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          target_type: "pr",
          target_repo: evidenceRepo,
          target_ref: String(prNumber),
          source: "glimmung_ui",
          payload: { kind: "reject", feedback: feedback.trim() },
        }),
      });
      if (!r.ok) throw new Error(`/v1/signals -> ${r.status}: ${await r.text()}`);
      const sig = await r.json() as { ref?: string };
      setReject({ kind: "submitted", signalId: sig.ref ?? "signal" });
      setFeedback("");
      onSubmitted();
      window.setTimeout(onSubmitted, 3000);
    } catch (e) {
      setReject({ kind: "error", message: e instanceof Error ? e.message : String(e) });
    }
  };

  return (
    <>
      <div className="project-info">
        <div className="row">
          <span className="key">state</span>
          <span className="val">
            <span className={`pill ${touchpointState === "open" || touchpointState === "ready" ? "busy" : touchpointState ? "free" : ""}`}>
              {touchpointState ?? "pending evidence"}
            </span>
          </span>
        </div>
        <div className="row">
          <span className="key">feedback</span>
          <span className="val">
            {pendingSignal ? (
              <span className={`pill ${pendingSignal.state === "processing" ? "busy" : "info"}`}>
                {pendingSignal.state}
              </span>
            ) : (
              <span className="dim">clear</span>
            )}
          </span>
        </div>
        <div className="row">
          <span className="key">surface</span>
          <span className="val">live issue decision</span>
        </div>
        <div className="row">
          <span className="key">PR</span>
          <span className="val">
            {prNumber !== null && evidenceRepo ? (
              <a className="mono" href={touchpointUrl || `https://github.com/${evidenceRepo}/pull/${prNumber}`} target="_blank" rel="noreferrer">
                #{prNumber}{touchpointTitle ? ` — ${touchpointTitle}` : ""}
              </a>
            ) : (
              <span className="dim">No PR evidence yet.</span>
            )}
          </span>
        </div>
        <div className="row">
          <span className="key">validation</span>
          <span className="val">
            {validationUrl ? (
              <a className="mono" href={validationUrl} target="_blank" rel="noreferrer">
                {validationUrl}
              </a>
            ) : (
              <span className="dim">No validation URL recorded.</span>
            )}
          </span>
        </div>
      </div>

      {(projectionEvidence.length > 0 || !hasCurrentEvidence) && (
        <>
          <h2>Evidence</h2>
          {structuredVideos.length > 0 && <StructuredVideoEvidence items={structuredVideos} />}
          {structuredScreenshots.length > 0 && <StructuredScreenshotEvidence items={structuredScreenshots} />}
          {listedEvidence.length > 0 && (
            <div className="project-info touchpoint-evidence-list">
              {listedEvidence.map((item) => (
                <div className="row" key={`${item.kind}:${item.ref}`}>
                  <span className="key">{item.kind}</span>
                  <span className="val">
                    {item.url ? (
                      <a className="mono" href={item.url} target="_blank" rel="noreferrer">
                        {item.label}
                      </a>
                    ) : (
                      <span className="mono">{item.label}</span>
                    )}
                  </span>
                </div>
              ))}
            </div>
          )}
          {projectionEvidence.length === 0 && (
            <div className="empty">No current evidence has landed yet.</div>
          )}
        </>
      )}

      <h2>Approve</h2>
      <p className="dim">merges the PR idempotently, releases the test slot, closes the issue.</p>
      <div className="review-actions">
        <button
          type="button"
          className="link"
          onClick={() => void submitApprove()}
          disabled={!canApprove}
          title={!signedIn ? "sign in" : !isAdmin ? "admin only" : undefined}
        >
          {approve.kind === "submitting" ? "queueing..." : !signedIn ? "sign in" : !isAdmin ? "admin only" : "approve"}
        </button>
        {pendingSignal && <span className="dim mono">decision already queued</span>}
        {approve.kind === "submitted" && (
          <span className="pill free">queued {approve.signalId.slice(0, 8)}</span>
        )}
        {approve.kind === "error" && (
          <span className="pill drain" title={approve.message}>error</span>
        )}
      </div>

      <h2>Request changes</h2>
      <textarea
        value={feedback}
        onChange={(e) => setFeedback(e.target.value)}
        placeholder="what needs to change?"
        rows={5}
        className="feedback-box"
        disabled={!canReject}
      />
      <div className="review-actions">
        <button
          type="button"
          className="link"
          onClick={() => void submitReject()}
          disabled={!canReject || !feedback.trim()}
          title={!signedIn ? "sign in" : !isAdmin ? "admin only" : undefined}
        >
          {reject.kind === "submitting" ? "queueing..." : !signedIn ? "sign in" : !isAdmin ? "admin only" : "request changes"}
        </button>
        {pendingSignal && <span className="dim mono">feedback already queued</span>}
        {reject.kind === "submitted" && (
          <span className="pill free">queued {reject.signalId.slice(0, 8)}</span>
        )}
        {reject.kind === "error" && (
          <span className="pill drain" title={reject.message}>error</span>
        )}
      </div>
    </>
  );
}

function latestProjectionRun(projection: RunGraphProjection | undefined | null): RunProjectionRun | null {
  if (!projection || projection.runs.length === 0) return null;
  if (projection.current_run_ref) {
    return projection.runs.find((run) => run.run_ref === projection.current_run_ref) ?? projection.runs[projection.runs.length - 1];
  }
  return projection.runs[projection.runs.length - 1];
}

function projectionActiveRun(projection: RunGraphProjection | undefined | null): RunProjectionRun | null {
  return projection?.runs.find((run) => runStateIsActive(run.state)) ?? null;
}

function projectionRunByLegacySlug(projection: RunGraphProjection, slug: string): RunProjectionRun | null {
  return projection.runs.find((run) => {
    const cycle = run.cycle_number !== null && run.cycle_number !== undefined ? String(run.cycle_number) : null;
    return cycle === slug || run.run_display_number === slug || projectionRunNumberSegment(run) === slug;
  }) ?? null;
}

function latestProjectionCycleForRun(projection: RunGraphProjection, runSegment: string): RunProjectionRun | null {
  const matches = projection.runs
    .filter((run) => projectionRunNumberSegment(run) === runSegment || run.run_display_number === runSegment)
    .sort((a, b) => (b.run_cycle_number ?? 0) - (a.run_cycle_number ?? 0));
  return matches[0] ?? projectionRunByLegacySlug(projection, runSegment);
}

function projectionRunNumberSegment(run: RunProjectionRun): string {
  if (run.run_number !== null && run.run_number !== undefined) return String(run.run_number);
  if (run.cycle_number !== null && run.cycle_number !== undefined) return String(run.cycle_number);
  return encodeURIComponent(run.run_ref);
}

function projectionCycleSegment(run: RunProjectionRun): string {
  if (run.run_cycle_number !== null && run.run_cycle_number !== undefined) return String(run.run_cycle_number);
  if (run.run_display_number) {
    const parts = run.run_display_number.split(".");
    if (parts[1]) return parts[1];
  }
  return "1";
}

function projectionRunCyclePath(baseUrl: string, run: RunProjectionRun): string {
  return issueRunSelectionPath(baseUrl, {
    runId: projectionRunNumberSegment(run),
    cycleId: projectionCycleSegment(run),
  });
}

function projectionSelectionPath(baseUrl: string, run: RunProjectionRun, selection: ProjectionSelection): string {
  return issueRunSelectionPath(baseUrl, {
    runId: projectionRunNumberSegment(run),
    cycleId: projectionCycleSegment(run),
    phaseId: selection.phase,
    jobId: selection.job,
    stepId: selection.step,
  });
}

function projectionRunLabel(run: RunProjectionRun): string {
  if (run.run_display_number) return `cycle ${run.run_display_number}`;
  if (run.cycle_number !== null && run.cycle_number !== undefined) return `cycle ${run.cycle_number}`;
  if (run.run_number !== null && run.run_number !== undefined) return `run ${run.run_number}`;
  return run.run_ref;
}

function projectionCycleDisplay(run: RunProjectionRun): string {
  if (run.run_number !== null && run.run_number !== undefined) return String(run.run_number);
  if (run.run_display_number) return run.run_display_number.split(".")[0] || run.run_display_number;
  if (run.cycle_number !== null && run.cycle_number !== undefined) return String(run.cycle_number);
  return run.run_ref;
}

function projectionRunHistoryCountDisplay(run: RunProjectionRun, index: number, total: number): string {
  if (run.cycle_number !== null && run.cycle_number !== undefined) return String(run.cycle_number);
  return String(Math.max(total - index, 1));
}

function countProjectionPhases(run: RunProjectionRun) {
  return run.phases.reduce(
    (acc, phase) => {
      if (phase.state === "active" || phase.state === "dispatching") acc.active += 1;
      else if (phase.state === "succeeded") acc.succeeded += 1;
      else if (phase.state === "skipped") acc.skipped += 1;
      else if (phase.state === "failed") acc.failed += 1;
      else acc.pending += 1;
      return acc;
    },
    { active: 0, succeeded: 0, skipped: 0, failed: 0, pending: 0 },
  );
}

function dispatchResultLabel(state: string): string {
  switch (state) {
    case "dispatched":
      return "dispatched";
    case "no_capacity":
      return "no capacity";
    case "dispatch_failed":
      return "dispatch failed";
    case "queued":
      return "queued";
    default:
      return state.replaceAll("_", " ");
  }
}

function dispatchResultPill(state: string): string {
  if (state === "dispatched") return "free";
  if (state === "no_capacity" || state === "queued") return "pending";
  if (state === "dispatch_failed") return "drain";
  return "info";
}

function graphStatePill(state: string): string {
  if (state === "succeeded") return "free";
  if (state === "active") return "busy";
  if (state === "failed") return "drain";
  if (state === "dispatching") return "pending";
  return "";
}

function formatGraphState(state: string): string {
  return state.replace(/_/g, " ");
}

function touchpointNeedsDecision(tp: RunProjectionTouchpoint): boolean {
  return ["ready", "needs_review", "open", "review_required"].includes(tp.state);
}

function prNumberFromNode(node: GraphNode | null): number | null {
  if (!node) return null;
  const match = node.id.match(/#(\d+)$/);
  return match ? parseInt(match[1], 10) : null;
}

function AttemptCard({
  attempt,
  project,
}: {
  attempt: GraphNode;
  project: string;
}) {
  const meta = attempt.metadata;
  const phase = stringOrNull(meta.phase) ?? "attempt";
  const dispatchedAt = attempt.timestamp;
  const completedAt = stringOrNull(meta.completed_at);
  const conclusion = stringOrNull(meta.conclusion);
  const decision = stringOrNull(meta.decision);
  const workflowFilename = stringOrNull(meta.workflow_filename);
  const verification = isRecord(meta.verification) ? meta.verification : null;
  const verificationStatus = verification ? stringOrNull(verification.status) : null;
  // Cost prefers the phase-reported top-level cost_usd (#69 — non-verify
  // LLM phases set this directly without a verification.json) and falls
  // back to verification.cost_usd for verify phases that emit the artifact.
  const attemptCost = numberOrNull(meta.cost_usd);
  const verificationCost = verification ? numberOrNull(verification.cost_usd) : null;
  const displayedCost = attemptCost ?? verificationCost;
  const verificationReasons = verification && Array.isArray(verification.reasons)
    ? verification.reasons.filter((r): r is string => typeof r === "string")
    : [];
  const phaseKind = stringOrNull(meta.phase_kind);
  const attemptIndex = numberOrNull(meta.attempt_index);
  const logArchiveUrl = stringOrNull(meta.log_archive_url);
  const nativeJobs = nativeAttemptJobs(meta.jobs);

  const nativeRunning = nativeJobs.some((j) => j.state === "active" || j.steps.some((s) => s.state === "active"));
  const nativeFailed = nativeJobs.some((j) => j.state === "failed" || j.steps.some((s) => s.state === "failed"));
  const nativeSucceeded = nativeJobs.length > 0 && nativeJobs.every((j) => j.state === "succeeded" || j.state === "skipped");

  // Pre-start progression:
  //   no completed_at + no active native step -> dispatching
  //   active native step                      -> running
  //   completed_at or terminal native job     -> terminal
  // The native runner can leave completed_at unset if a callback failed;
  // do not keep showing those terminal jobs as active after the run aborts.
  const nativeTerminal = nativeFailed || nativeSucceeded;
  const running = !completedAt && !nativeTerminal && nativeRunning;
  const dispatching = !completedAt && !nativeTerminal && !running;
  const elapsedMs = dispatchedAt && running ? now() - parseTs(dispatchedAt) : null;
  const stuckDispatching =
    dispatching && elapsedMs !== null && elapsedMs > STUCK_DISPATCHING_MS;
  const elapsedLabel = dispatchedAt
    ? running
      ? `${formatDuration(elapsedMs ?? 0)} elapsed`
      : completedAt
        ? `ran ${formatDuration(parseTs(completedAt) - parseTs(dispatchedAt))}`
        : null
    : null;

  const statusPill = (() => {
    if (dispatching) return { cls: "pending", text: "dispatching" };
    if (running) return { cls: "busy", text: "running" };
    if (verificationStatus === "pass") return { cls: "free", text: "pass" };
    if (verificationStatus === "fail") return { cls: "drain", text: "fail" };
    if (verificationStatus === "error") return { cls: "drain", text: "error" };
    if (nativeFailed) return { cls: "drain", text: "failed" };
    if (nativeSucceeded) return { cls: "free", text: "success" };
    if (conclusion === "success") return { cls: "free", text: "success" };
    if (conclusion === "cancelled") return { cls: "drain", text: "cancelled" };
    if (conclusion) return { cls: "drain", text: conclusion };
    return { cls: "", text: "completed" };
  })();

  const runIdFromAttempt = attempt.id.startsWith("attempt:")
    ? attempt.id.split(":")[1] ?? ""
    : "";
  const branchName = runIdFromAttempt ? `glimmung/${runIdFromAttempt}` : null;

  return (
    <div className={`attempt-card${running ? " running" : ""}${stuckDispatching ? " stuck" : ""}`}>
      <div className="attempt-card-head">
        <strong>{attempt.label}</strong>
        <span className={`pill ${statusPill.cls}`}>{statusPill.text}</span>
        <span className="dim mono">{phase}</span>
        {elapsedLabel && <span className="dim mono">{elapsedLabel}</span>}
        {stuckDispatching && (
          <span className="pill drain" title="No native job activity recorded after dispatch.">
            stuck
          </span>
        )}
      </div>
      <div className="attempt-card-body">
        {dispatchedAt && (
          <div>
            <span className="key">dispatched</span>{" "}
            <span className="mono">{formatTime(dispatchedAt)}</span>
          </div>
        )}
        {completedAt && (
          <div>
            <span className="key">completed</span>{" "}
            <span className="mono">{formatTime(completedAt)}</span>
          </div>
        )}
        {workflowFilename && (
          <div>
            <span className="key">workflow</span>{" "}
            <span className="mono">{workflowFilename}</span>
          </div>
        )}
        {branchName && (
          <div>
            <span className="key">branch</span>{" "}
            <span className="mono">{branchName}</span>
          </div>
        )}
        {displayedCost !== null && (
          <div>
            <span className="key">cost</span>{" "}
            <span className="mono">${displayedCost.toFixed(4)}</span>
          </div>
        )}
        {decision && (
          <div>
            <span className="key">decision</span>{" "}
            <span className="mono">{decision}</span>
          </div>
        )}
      </div>
      {verificationReasons.length > 0 && (
        <ul className="attempt-card-reasons">
          {verificationReasons.map((r, i) => (
            <li key={i} className="mono dim">
              {r}
            </li>
          ))}
        </ul>
      )}
      {phaseKind === "k8s_job" && runIdFromAttempt && attemptIndex !== null && (
        <NativeJobInspector
          project={project}
          runId={runIdFromAttempt}
          attemptIndex={attemptIndex}
          jobs={nativeJobs}
          archiveUrl={logArchiveUrl}
          live={running && nativeRunning}
        />
      )}
    </div>
  );
}

function NativeJobInspector({
  project,
  runId,
  issueNumber = null,
  runNumber = null,
  attemptIndex,
  jobs,
  archiveUrl,
  live,
  selectedJobId = null,
  selectedStepSlug = null,
  onSelectStep,
}: {
  project: string;
  runId: string;
  issueNumber?: number | null;
  runNumber?: string | null;
  attemptIndex: number;
  jobs: NativeAttemptJob[];
  archiveUrl: string | null;
  live: boolean;
  selectedJobId?: string | null;
  selectedStepSlug?: string | null;
  onSelectStep?: (jobId: string, stepSlug: string) => void;
}) {
  const [logs, setLogs] = useState<NativeRunEventsResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const scopedJobs = useMemo(
    () => selectedJobId ? jobs.filter((job) => job.job_id === selectedJobId) : jobs,
    [jobs, selectedJobId],
  );
  const stepRefs = useMemo(() => nativeStepRefs(scopedJobs), [scopedJobs]);
  const defaultSelection = useMemo(
    () => selectedStepSlug
      ? stepRefs.find((step) => step.step.slug === selectedStepSlug)?.key ?? preferredNativeStepKey(stepRefs)
      : preferredNativeStepKey(stepRefs),
    [selectedStepSlug, stepRefs],
  );
  const [selectedKey, setSelectedKey] = useState<string | null>(defaultSelection);
  const [viewMode, setViewMode] = useState<NativeLogViewMode>("transcript");
  const selected = stepRefs.find((step) => step.key === selectedKey) ?? stepRefs[0] ?? null;

  useEffect(() => {
    let cancelled = false;
    setLogs(null);
    setError(null);
    setSelectedKey(defaultSelection);
    const base = runNumber && issueNumber !== null
      ? nativeRunApiBaseForNumber(project, issueNumber, runNumber)
      : nativeRunApiBase(project, runId);
    if (!base) {
      setError("events unavailable for malformed run ref");
      return () => {
        cancelled = true;
      };
    }
    const jobParam = selectedJobId ? `&job_id=${encodeURIComponent(selectedJobId)}` : "";
    const url = `${base}/events?attempt_index=${attemptIndex}&limit=200${jobParam}`;
    const load = () => {
      fetch(url)
      .then(async (res) => {
        if (!res.ok) throw new Error(`events ${res.status}`);
        const body = await res.json() as NativeRunEventsResponse;
        if (!cancelled) setLogs(body);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err));
      });
    };
    load();
    const timer = live ? window.setInterval(load, 3000) : null;
    return () => {
      cancelled = true;
      if (timer !== null) window.clearInterval(timer);
    };
  }, [project, runId, issueNumber, runNumber, attemptIndex, defaultSelection, live, selectedJobId]);

  useEffect(() => {
    setViewMode("transcript");
  }, [selectedKey]);

  if (error) {
    return (
      <div className="native-log-panel">
        <span className="pill drain">log error</span>{" "}
        <span className="mono dim">{error}</span>
      </div>
    );
  }
  if (!logs) {
    return <div className="native-log-panel dim mono">loading native events…</div>;
  }
  const events = logs.events;
  const selectedEvents = selected
    ? events.filter((event) => (
        event.job_id === selected.job.job_id
        && (event.step_slug === selected.step.slug || (event.event === "log" && !event.step_slug))
      ))
    : events;
  const transcriptEntries = selected && nativeSelectionIsLlm(selected.job, selected.step)
    ? agentTranscriptEntries(selectedEvents)
    : [];
  const transcriptAvailable = Boolean(selected && nativeSelectionIsLlm(selected.job, selected.step) && transcriptEntries.length > 0);
  const activeViewMode: NativeLogViewMode = transcriptAvailable ? viewMode : "raw";
  return (
    <div className="native-inspector">
      <div className="native-inspector-head">
        <div>
          <span className="key">native job inspector</span>
          <span className="mono dim">
            {events.length} event{events.length === 1 ? "" : "s"}
            {live ? " · live" : ""}
          </span>
        </div>
        <div className="native-inspector-actions">
          {transcriptAvailable && (
            <div className="native-view-toggle" role="group" aria-label="native log view">
              <button
                type="button"
                aria-pressed={activeViewMode === "transcript"}
                onClick={() => setViewMode("transcript")}
              >
                transcript
              </button>
              <button
                type="button"
                aria-pressed={activeViewMode === "raw"}
                onClick={() => setViewMode("raw")}
              >
                raw
              </button>
            </div>
          )}
          {(logs.archive_url || archiveUrl) && (
            <a
              className="mono"
              href={`/v1/artifacts/${artifactPathFromUrl(logs.archive_url || archiveUrl || "")}`}
              target="_blank"
              rel="noreferrer"
            >
              archive
            </a>
          )}
        </div>
      </div>
      <div className="step-log-layout native-step-log-layout">
        <aside className="step-list" aria-label="native job steps">
          {stepRefs.length === 0 ? (
            <div className="native-step-empty mono dim">no native steps declared</div>
          ) : (
            stepRefs.map(({ key, job, step }, index) => (
              <Fragment key={key}>
                {(index === 0 || stepRefs[index - 1]?.job.job_id !== job.job_id) && (
                  <div className="native-job-label">
                    <span className="mono">{job.name || job.job_id}</span>
                    <span className={`pill ${nativeStatePill(job.state ?? "")}`}>
                      {job.state || "not run"}
                    </span>
                    {job.cost_usd !== null && job.cost_usd !== undefined && (
                      <span className="mono dim">{formatUsd4(job.cost_usd)}</span>
                    )}
                  </div>
                )}
                <button
                  type="button"
                  className={`step-row ${nativeStepRowClass(step.state ?? "")}${key === selected?.key ? " selected" : ""}`}
                  onClick={() => {
                    setSelectedKey(key);
                    onSelectStep?.(job.job_id, step.slug);
                  }}
                >
                  <span>{nativeStepGlyph(step.state ?? "")}</span>
                  <strong>
                    {step.title || step.slug}
                    {nativeSelectionIsLlm(job, step) && <span className="native-step-llm">llm</span>}
                  </strong>
                  <small>
                    {step.exit_code !== null && step.exit_code !== undefined
                      ? `exit ${step.exit_code}`
                      : step.state ? formatGraphState(step.state) : "not run"}
                  </small>
                </button>
              </Fragment>
            ))
          )}
        </aside>
        {activeViewMode === "transcript" ? (
          <AgentTranscriptView entries={transcriptEntries} />
        ) : (
          <pre className="step-terminal native-step-terminal">
            {selected
              ? nativeTerminalText(selected.job, selected.step, selectedEvents)
              : nativeTerminalText(null, null, events)}
          </pre>
        )}
      </div>
    </div>
  );
}

function AgentTranscriptView({ entries }: { entries: AgentTranscriptEntry[] }) {
  return (
    <div className="agent-transcript" aria-label="agent transcript">
      {entries.map((entry) => {
        if (entry.kind === "assistant") {
          return (
            <article key={entry.id} className="agent-transcript-entry assistant">
              <div className="agent-transcript-entry-head">
                <strong>{entry.title}</strong>
                <span className="mono dim">#{entry.seq}</span>
              </div>
              <div className="agent-transcript-text">{entry.text}</div>
            </article>
          );
        }
        if (entry.kind === "result") {
          return (
            <article key={entry.id} className="agent-transcript-entry result">
              <div className="agent-transcript-entry-head">
                <strong>{entry.title}</strong>
                <span className="mono dim">#{entry.seq}</span>
              </div>
              <div className="agent-transcript-result">
                {entry.costUsd !== null && entry.costUsd !== undefined && (
                  <span className="mono">{formatUsd4(entry.costUsd)}</span>
                )}
                {entry.text && <span>{entry.text}</span>}
              </div>
            </article>
          );
        }
        if (entry.kind === "reasoning") {
          return (
            <article key={entry.id} className="agent-transcript-entry reasoning">
              <div className="agent-transcript-entry-head">
                <strong>{entry.title}</strong>
                <span className="mono dim">#{entry.seq}</span>
              </div>
              <div className="agent-transcript-muted">{entry.text}</div>
            </article>
          );
        }
        const body = entry.kind === "tool_call"
          ? formatAgentJson(entry.input ?? entry.raw)
          : entry.text ?? formatAgentJson(entry.raw);
        return (
          <details key={entry.id} className={`agent-transcript-entry ${entry.kind}`}>
            <summary>
              <span>{entry.title}</span>
              {entry.toolName && <strong>{entry.toolName}</strong>}
              <span className="mono dim">#{entry.seq}</span>
            </summary>
            <pre>{body}</pre>
          </details>
        );
      })}
    </div>
  );
}

function PlannedNativeJobInspector({
  job,
  selectedStepSlug = null,
  onSelectStep,
}: {
  job: NativeAttemptJob;
  selectedStepSlug?: string | null;
  onSelectStep?: (jobId: string, stepSlug: string) => void;
}) {
  const stepRefs = useMemo(() => nativeStepRefs([job]), [job]);
  const defaultSelection = useMemo(
    () => selectedStepSlug
      ? stepRefs.find((step) => step.step.slug === selectedStepSlug)?.key ?? preferredNativeStepKey(stepRefs)
      : preferredNativeStepKey(stepRefs),
    [selectedStepSlug, stepRefs],
  );
  const [selectedKey, setSelectedKey] = useState<string | null>(defaultSelection);
  const selected = stepRefs.find((step) => step.key === selectedKey) ?? stepRefs[0] ?? null;

  useEffect(() => {
    setSelectedKey(defaultSelection);
  }, [defaultSelection]);

  return (
    <div className="native-inspector">
      <div className="native-inspector-head">
        <div>
          <span className="key">native job inspector</span>
          <span className="mono dim">planned</span>
        </div>
      </div>
      <div className="step-log-layout native-step-log-layout">
        <aside className="step-list" aria-label="native job steps">
          {stepRefs.length === 0 ? (
            <div className="native-step-empty mono dim">no native steps declared</div>
          ) : (
            stepRefs.map(({ key, job: refJob, step }, index) => (
              <Fragment key={key}>
                {(index === 0 || stepRefs[index - 1]?.job.job_id !== refJob.job_id) && (
                  <div className="native-job-label">
                    <span className="mono">{refJob.name || refJob.job_id}</span>
                    <span className={`pill ${nativeStatePill(refJob.state ?? "")}`}>
                      {refJob.state || "not run"}
                    </span>
                    {refJob.cost_usd !== null && refJob.cost_usd !== undefined && (
                      <span className="mono dim">{formatUsd4(refJob.cost_usd)}</span>
                    )}
                  </div>
                )}
                <button
                  type="button"
                  className={`step-row ${nativeStepRowClass(step.state ?? "")}${key === selected?.key ? " selected" : ""}`}
                  onClick={() => {
                    setSelectedKey(key);
                    onSelectStep?.(refJob.job_id, step.slug);
                  }}
                >
                  <span>{nativeStepGlyph(step.state ?? "")}</span>
                  <strong>
                    {step.title || step.slug}
                    {nativeSelectionIsLlm(refJob, step) && <span className="native-step-llm">llm</span>}
                  </strong>
                  <small>
                    {step.exit_code !== null && step.exit_code !== undefined
                      ? `exit ${step.exit_code}`
                      : step.state ? formatGraphState(step.state) : "not run"}
                  </small>
                </button>
              </Fragment>
            ))
          )}
        </aside>
        <pre className="step-terminal native-step-terminal">
          {selected
            ? nativeTerminalText(selected.job, selected.step, [])
            : nativeTerminalText(null, null, [])}
        </pre>
      </div>
    </div>
  );
}

function nativeRunApiBase(project: string, runRef: string): string | null {
  const parsed = runRef.match(/^[^#]+#(\d+)\/runs\/(.+)$/);
  if (parsed) {
    return `/v1/projects/${encodeURIComponent(project)}` +
      `/issues/${encodeURIComponent(parsed[1])}` +
      `/runs/${encodeURIComponent(parsed[2])}/native`;
  }
  return null;
}

function nativeRunApiBaseForNumber(project: string, issueNumber: number, runNumber: string): string {
  return `/v1/projects/${encodeURIComponent(project)}` +
    `/issues/${encodeURIComponent(issueNumber)}` +
    `/runs/${encodeURIComponent(runNumber)}/native`;
}

function nativeStepRefs(jobs: NativeAttemptJob[]): Array<{
  key: string;
  job: NativeAttemptJob;
  step: NativeAttemptStep;
}> {
  return jobs.flatMap((job) => (
    job.steps.length > 0
      ? job.steps.map((step) => ({
          key: `${job.job_id}:${step.slug}`,
          job,
          step,
        }))
      : [{
          key: `${job.job_id}:job`,
          job,
          step: {
            slug: "job",
            title: job.name || job.job_id,
            state: job.state,
          },
        }]
  ));
}

function preferredNativeStepKey(
  refs: Array<{ key: string; step: NativeAttemptStep }>,
): string | null {
  return (
    refs.find((ref) => ref.step.state === "active")?.key
    ?? refs.find((ref) => ref.step.state === "failed")?.key
    ?? refs.find((ref) => ref.step.state === "not_started")?.key
    ?? refs[0]?.key
    ?? null
  );
}

function nativeStepRowClass(state: string): string {
  if (state === "succeeded") return "done";
  if (state === "skipped") return "skipped";
  if (state === "active") return "active";
  if (state === "failed") return "failed";
  return "pending";
}

function nativeStepGlyph(state: string): string {
  if (state === "succeeded") return "✓";
  if (state === "active") return "▶";
  if (state === "failed") return "!";
  if (state === "skipped") return "↷";
  return "·";
}

function nativeTerminalText(
  job: NativeAttemptJob | null,
  step: NativeAttemptStep | null,
  events: NativeRunEvent[],
): string {
  const heading = job && step
    ? [`# ${job.name || job.job_id}`, `$ step ${step.slug}`]
    : ["# native events"];
  if (job && step && nativeSelectionIsLlm(job, step)) {
    heading.push("# llm step");
  }
  const stepMessage = step?.message ? [`# ${step.message}`] : [];
  const lines = events.length > 0
    ? events.map(nativeEventLine)
    : ["No hot native events recorded for this selection."];
  return [...heading, ...stepMessage, "", ...lines].join("\n");
}

function nativeEventLine(event: NativeRunEvent): string {
  const prefix = [
    `[${event.seq}]`,
    event.step_slug || event.job_id,
    event.event,
  ].join(" ");
  const suffix = event.exit_code !== null ? ` exit ${event.exit_code}` : "";
  if (!event.message) return `${prefix}${suffix}`;
  if (event.event === "log") return event.message;
  return `${prefix}: ${event.message}${suffix}`;
}

function agentTranscriptEntries(events: NativeRunEvent[]): AgentTranscriptEntry[] {
  const entries: AgentTranscriptEntry[] = [];
  const toolNamesById = new Map<string, string>();
  events.forEach((event) => {
    if (event.event !== "log") return;
    const payloads = parseAgentLogPayloads(event.message);
    if (payloads.length === 0) {
      entries.push({
        id: `raw-${event.seq}`,
        kind: "raw",
        seq: event.seq,
        createdAt: event.created_at,
        title: logStreamTitle(event),
        text: event.message,
      });
      return;
    }
    const before = entries.length;
    payloads.forEach((payload, index) => {
      appendAgentPayloadEntries(entries, event, payload, index, toolNamesById);
    });
    if (entries.length === before) {
      entries.push({
        id: `raw-${event.seq}`,
        kind: "raw",
        seq: event.seq,
        createdAt: event.created_at,
        title: "json event",
        raw: payloads.length === 1 ? payloads[0] : payloads,
      });
    }
  });
  return entries;
}

function appendAgentPayloadEntries(
  entries: AgentTranscriptEntry[],
  event: NativeRunEvent,
  payload: unknown,
  payloadIndex: number,
  toolNamesById: Map<string, string>,
) {
  const obj = recordValue(payload);
  if (!obj) {
    entries.push({
      id: `raw-${event.seq}-${payloadIndex}`,
      kind: "raw",
      seq: event.seq,
      createdAt: event.created_at,
      title: "json value",
      raw: payload,
    });
    return;
  }

  const type = stringValue(obj.type);
  if (type === "assistant") {
    const message = recordValue(obj.message);
    const content = arrayValue(message?.content);
    content.forEach((block, blockIndex) => {
      const blockObj = recordValue(block);
      if (!blockObj) return;
      const blockType = stringValue(blockObj.type);
      if (blockType === "text") {
        entries.push({
          id: `assistant-${event.seq}-${payloadIndex}-${blockIndex}`,
          kind: "assistant",
          seq: event.seq,
          createdAt: event.created_at,
          title: "assistant",
          text: stringValue(blockObj.text) ?? "",
        });
        return;
      }
      if (blockType === "tool_use") {
        const id = stringValue(blockObj.id);
        const name = stringValue(blockObj.name) ?? "tool";
        if (id) toolNamesById.set(id, name);
        entries.push({
          id: `tool-call-${event.seq}-${payloadIndex}-${blockIndex}`,
          kind: "tool_call",
          seq: event.seq,
          createdAt: event.created_at,
          title: "tool call",
          toolName: name,
          toolUseId: id ?? undefined,
          input: blockObj.input,
        });
        return;
      }
      if (blockType === "thinking") {
        entries.push({
          id: `reasoning-${event.seq}-${payloadIndex}-${blockIndex}`,
          kind: "reasoning",
          seq: event.seq,
          createdAt: event.created_at,
          title: "reasoning",
          text: "Thinking/signature content hidden in transcript view. Open raw to inspect the original event.",
        });
        return;
      }
      entries.push({
        id: `assistant-raw-${event.seq}-${payloadIndex}-${blockIndex}`,
        kind: "raw",
        seq: event.seq,
        createdAt: event.created_at,
        title: blockType ? `assistant ${blockType}` : "assistant block",
        raw: blockObj,
      });
    });
    return;
  }

  if (type === "user") {
    const message = recordValue(obj.message);
    const content = arrayValue(message?.content);
    content.forEach((block, blockIndex) => {
      const blockObj = recordValue(block);
      if (!blockObj) return;
      const blockType = stringValue(blockObj.type);
      if (blockType === "tool_result") {
        const toolUseId = stringValue(blockObj.tool_use_id);
        const toolName = toolUseId ? toolNamesById.get(toolUseId) : undefined;
        entries.push({
          id: `tool-result-${event.seq}-${payloadIndex}-${blockIndex}`,
          kind: "tool_result",
          seq: event.seq,
          createdAt: event.created_at,
          title: "tool result",
          toolName,
          toolUseId: toolUseId ?? undefined,
          text: toolResultText(blockObj.content),
        });
        return;
      }
      if (blockType === "text") {
        entries.push({
          id: `user-text-${event.seq}-${payloadIndex}-${blockIndex}`,
          kind: "raw",
          seq: event.seq,
          createdAt: event.created_at,
          title: "user text",
          text: stringValue(blockObj.text) ?? "",
        });
        return;
      }
      entries.push({
        id: `user-raw-${event.seq}-${payloadIndex}-${blockIndex}`,
        kind: "raw",
        seq: event.seq,
        createdAt: event.created_at,
        title: blockType ? `user ${blockType}` : "user block",
        raw: blockObj,
      });
    });
    return;
  }

  if (type === "result") {
    entries.push({
      id: `result-${event.seq}-${payloadIndex}`,
      kind: "result",
      seq: event.seq,
      createdAt: event.created_at,
      title: "result",
      costUsd: numberValue(obj.total_cost_usd) ?? numberValue(obj.cost_usd),
      text: resultSummaryText(obj),
      raw: obj,
    });
    return;
  }

  if (type === "system") {
    entries.push({
      id: `system-${event.seq}-${payloadIndex}`,
      kind: "raw",
      seq: event.seq,
      createdAt: event.created_at,
      title: stringValue(obj.subtype) ? `system ${stringValue(obj.subtype)}` : "system event",
      raw: obj,
    });
    return;
  }

  entries.push({
    id: `json-${event.seq}-${payloadIndex}`,
    kind: "raw",
    seq: event.seq,
    createdAt: event.created_at,
    title: type ? `${type} event` : "json event",
    raw: obj,
  });
}

function parseAgentLogPayloads(message: string): unknown[] {
  const trimmed = message.trim();
  if (!trimmed) return [];
  try {
    return [JSON.parse(trimmed) as unknown];
  } catch {
    const chunks = balancedJsonChunks(trimmed);
    return chunks.flatMap((chunk) => {
      try {
        return [JSON.parse(chunk) as unknown];
      } catch {
        return [];
      }
    });
  }
}

function balancedJsonChunks(input: string): string[] {
  const chunks: string[] = [];
  const stack: string[] = [];
  let start = -1;
  let inString = false;
  let escaped = false;
  for (let index = 0; index < input.length; index += 1) {
    const ch = input[index];
    if (start === -1) {
      if (ch === "{" || ch === "[") {
        start = index;
        stack.push(ch);
      }
      continue;
    }
    if (inString) {
      if (escaped) {
        escaped = false;
      } else if (ch === "\\") {
        escaped = true;
      } else if (ch === "\"") {
        inString = false;
      }
      continue;
    }
    if (ch === "\"") {
      inString = true;
      continue;
    }
    if (ch === "{" || ch === "[") {
      stack.push(ch);
      continue;
    }
    if (ch === "}" || ch === "]") {
      const opener = stack.pop();
      if (!opener || (opener === "{" && ch !== "}") || (opener === "[" && ch !== "]")) {
        start = -1;
        stack.length = 0;
        continue;
      }
      if (stack.length === 0) {
        chunks.push(input.slice(start, index + 1));
        start = -1;
      }
    }
  }
  return chunks;
}

function toolResultText(value: unknown): string {
  const text = contentText(value);
  const parsed = parseJsonString(text);
  const parsedObj = recordValue(parsed);
  if (parsedObj) {
    const sections = ["stdout", "stderr", "output", "content"]
      .map((key) => {
        const section = stringValue(parsedObj[key]);
        return section ? `# ${key}\n${section}` : "";
      })
      .filter(Boolean);
    if (sections.length > 0) return sections.join("\n\n");
  }
  return text;
}

function contentText(value: unknown): string {
  if (typeof value === "string") return value;
  if (Array.isArray(value)) {
    return value.map((item) => {
      const obj = recordValue(item);
      if (!obj) return String(item);
      if (stringValue(obj.type) === "text") return stringValue(obj.text) ?? "";
      return formatAgentJson(obj);
    }).filter(Boolean).join("\n\n");
  }
  return formatAgentJson(value);
}

function resultSummaryText(obj: Record<string, unknown>): string {
  const subtype = stringValue(obj.subtype);
  const durationMs = numberValue(obj.duration_ms);
  const pieces = [
    subtype,
    durationMs !== null ? `${Math.round(durationMs / 1000)}s` : null,
  ].filter(Boolean);
  return pieces.join(" · ");
}

function parseJsonString(text: string): unknown {
  const trimmed = text.trim();
  if (!trimmed) return null;
  try {
    return JSON.parse(trimmed) as unknown;
  } catch {
    return null;
  }
}

function formatAgentJson(value: unknown): string {
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value, null, 2) ?? String(value);
  } catch {
    return String(value);
  }
}

function logStreamTitle(event: NativeRunEvent): string {
  const stream = stringValue(event.metadata?.stream);
  return stream ? `${stream} log` : "log";
}

function recordValue(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null;
}

function arrayValue(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" ? value : null;
}

function numberValue(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function StructuredScreenshotEvidence({ items }: { items: RunProjectionEvidence[] }) {
  return (
    <div className="evidence-gallery">
      {items.map((item) => (
        <a key={`${item.kind}:${item.ref}`} className="evidence-shot" href={item.url ?? item.ref} target="_blank" rel="noreferrer">
          <img src={item.url ?? item.ref} alt={item.label} loading="lazy" />
          <span>{item.label}</span>
        </a>
      ))}
    </div>
  );
}

function StructuredVideoEvidence({ items }: { items: RunProjectionEvidence[] }) {
  return (
    <div className="evidence-video-gallery">
      {items.map((item) => (
        <div key={`${item.kind}:${item.ref}`} className="evidence-video">
          <video src={item.url ?? item.ref} controls preload="metadata" />
          <div className="evidence-video-caption">
            <a className="mono" href={item.url ?? item.ref} target="_blank" rel="noreferrer">
              {item.label}
            </a>
            {item.duration_ms ? (
              <span className="dim mono">{Math.round(item.duration_ms / 1000)}s</span>
            ) : null}
          </div>
        </div>
      ))}
    </div>
  );
}

function isInlineVideoEvidence(item: RunProjectionEvidence): boolean {
  return item.kind === "video" && Boolean(item.url);
}

function isInlineScreenshotEvidence(item: RunProjectionEvidence): boolean {
  return item.kind === "screenshot" && Boolean(item.url);
}

function isRawVideoArtifact(item: RunProjectionEvidence): boolean {
  if (item.kind !== "artifact") return false;
  return /\.(webm|mp4|mov|m4v)$/i.test(item.ref.split(/[?#]/)[0] ?? "");
}

function isRawScreenshotArtifact(item: RunProjectionEvidence): boolean {
  if (item.kind !== "artifact") return false;
  return /\.(png|jpe?g|webp|gif)$/i.test(item.ref.split(/[?#]/)[0] ?? "");
}

function nativeSelectionIsLlm(job: NativeAttemptJob | null, step: NativeAttemptStep): boolean {
  const marker = [
    job?.job_id ?? "",
    job?.name ?? "",
    step.slug,
    step.title ?? "",
  ].join(" ").toLowerCase();
  return marker.includes("llm") || marker.includes("run-agent") || marker.includes("claude");
}

function findActiveRun(graph: IssueGraph): GraphNode | null {
  return graph.nodes.find((n) => n.kind === "run" && runStateIsActive(n.state ?? "")) ?? null;
}

function findLastCompletedRun(graph: IssueGraph): GraphNode | null {
  const completed = graph.nodes
    .filter((n) => n.kind === "run" && !runStateIsActive(n.state ?? ""))
    .sort((a, b) => (b.timestamp ?? "").localeCompare(a.timestamp ?? ""));
  return completed[0] ?? null;
}

function attemptsForRun(graph: IssueGraph, runNodeId: string): GraphNode[] {
  // run node id is `run:<run_ref>`; attempt ids are `attempt:<run_ref>:<index>`.
  const runId = runNodeId.startsWith("run:") ? runNodeId.slice(4) : runNodeId;
  const prefix = `attempt:${runId}:`;
  return graph.nodes
    .filter((n) => n.kind === "attempt" && n.id.startsWith(prefix))
    .sort((a, b) => {
      const ai = parseInt(a.id.split(":").pop() ?? "0", 10);
      const bi = parseInt(b.id.split(":").pop() ?? "0", 10);
      return ai - bi;
    });
}

// Run nodes are keyed `run:<run_ref>` in the graph endpoint.
function runIdFromNode(n: GraphNode): string {
  return n.id.startsWith("run:") ? n.id.slice(4) : n.id;
}

function issueRunSlug(graph: IssueGraph, run: GraphNode): string {
  const explicit = numberOrNull(run.metadata.cycle_number);
  if (explicit !== null) return String(explicit);
  const display = stringOrNull(run.metadata.run_display_number);
  if (display) return display;
  const issueRuns = graph.nodes
    .filter((node) => node.kind === "run")
    .slice()
    .sort((a, b) => (a.timestamp ?? "").localeCompare(b.timestamp ?? ""));
  const ordinal = issueRuns.findIndex((node) => node.id === run.id) + 1;
  return String(Math.max(ordinal, 1));
}

function runSlugDisplay(slug: string): string {
  return /^\d+(\.\d+)?$/.test(slug) ? `cycle ${slug}` : `${slug.slice(0, 8)}…`;
}

function runSlugValueDisplay(slug: string): string {
  return /^\d+(\.\d+)?$/.test(slug) ? slug.split(".")[0] : `${slug.slice(0, 8)}…`;
}

function runRouteSlugFromNode(run: GraphNode): string | null {
  const display = stringOrNull(run.metadata.run_display_number);
  if (display) return display;
  const cycle = numberOrNull(run.metadata.cycle_number);
  if (cycle !== null) return String(cycle);
  const logicalRun = numberOrNull(run.metadata.run_number);
  if (logicalRun !== null) return String(logicalRun);
  return null;
}

function runNumberDisplay(run: GraphNode): string {
  const runNumber = numberOrNull(run.metadata.run_number);
  return runNumber !== null ? String(runNumber) : "—";
}

function runCycleDisplay(run: GraphNode): string {
  const runCycle = numberOrNull(run.metadata.run_cycle_number);
  if (runCycle !== null) return String(runCycle);
  const display = stringOrNull(run.metadata.run_display_number);
  return display ?? "—";
}

function graphRunHistoryCountDisplay(run: GraphNode, index: number, total: number): string {
  const cycleNumber = numberOrNull(run.metadata.cycle_number);
  if (cycleNumber !== null) return String(cycleNumber);
  return String(Math.max(total - index, 1));
}

type CycleLineage = {
  depth: number;
  kicker: string | null;
  origin: string | null;
};

// Walk the parent_run_ref chain to compute automatic cycle depth + origin.
// The kicker is the immediate parent (depth >= 1). Origin is the chain root.
// Cycles are guarded against malformed graphs.
function computeCycleLineage(graph: IssueGraph, runId: string): CycleLineage {
  const byId = new Map<string, GraphNode>();
  for (const node of graph.nodes) {
    if (node.kind === "run") byId.set(runIdFromNode(node), node);
  }
  let cursor: string | null = runId;
  let depth = 0;
  let kicker: string | null = null;
  let origin: string | null = null;
  const visited = new Set<string>();
  while (cursor) {
    if (visited.has(cursor)) break;
    visited.add(cursor);
    const node = byId.get(cursor);
    if (!node) break;
    const parent = stringOrNull(node.metadata.parent_run_ref)
      ?? stringOrNull(node.metadata.parent_run_ref);
    if (!parent) {
      origin = depth === 0 ? null : cursor;
      break;
    }
    if (depth === 0) kicker = parent;
    depth += 1;
    cursor = parent;
  }
  return { depth, kicker, origin };
}

function RunRefLink({
  graph,
  runId,
  onSelectRun,
}: {
  graph: IssueGraph;
  runId: string;
  onSelectRun: (runId: string) => void;
}) {
  const node = graph.nodes.find((n) => n.kind === "run" && runIdFromNode(n) === runId);
  const label = node ? runSlugDisplay(issueRunSlug(graph, node)) : `${runId.slice(0, 8)}…`;
  return (
    <button
      type="button"
      className="link mono"
      title={runId}
      onClick={() => onSelectRun(runId)}
    >
      {label}
    </button>
  );
}

function runDisplayName(run: GraphNode): string {
  return runSlugDisplay(issueRunSlug({ issue_ref: "", nodes: [run], edges: [] }, run));
}

function mergeWorkflows(primary: Workflow[], fallback: Workflow[]): Workflow[] {
  const seen = new Set<string>();
  const merged: Workflow[] = [];
  for (const workflow of [...primary, ...fallback]) {
    const key = `${workflow.project}/${workflow.name}`;
    if (seen.has(key)) continue;
    seen.add(key);
    merged.push(workflow);
  }
  return merged;
}

function singleProjectWorkflow(workflows: Workflow[], project: string): Workflow | null {
  const projectWorkflows = workflows.filter((workflow) => workflow.project === project);
  return projectWorkflows.length === 1 ? projectWorkflows[0] : null;
}

function resolveRunWorkflow(workflows: Workflow[], project: string, run: GraphNode | null): Workflow | null {
  if (!run) return null;
  return resolveProjectWorkflow(workflows, project, [stringOrNull(run.metadata.workflow)]);
}

function isRecord(x: unknown): x is Record<string, unknown> {
  return typeof x === "object" && x !== null && !Array.isArray(x);
}

function nativeAttemptJobs(x: unknown): NativeAttemptJob[] {
  if (!Array.isArray(x)) return [];
  return x.flatMap((raw): NativeAttemptJob[] => {
    if (!isRecord(raw)) return [];
    const jobId = stringOrNull(raw.job_id) ?? stringOrNull(raw.id);
    if (!jobId) return [];
    const steps = Array.isArray(raw.steps)
      ? raw.steps.flatMap((s): NativeAttemptStep[] => {
          if (!isRecord(s)) return [];
          const slug = stringOrNull(s.slug);
          if (!slug) return [];
          return [{
            slug,
            title: stringOrNull(s.title),
            state: stringOrNull(s.state),
            message: stringOrNull(s.message),
            exit_code: numberOrNull(s.exit_code),
          }];
        })
      : [];
    return [{
      job_id: jobId,
      name: stringOrNull(raw.name),
      state: stringOrNull(raw.state),
      cost_usd: numberOrNull(raw.cost_usd),
      steps,
    }];
  });
}

function nativeStatePill(state: string): string {
  if (state === "succeeded") return "free";
  if (state === "active") return "busy";
  if (state === "failed") return "drain";
  if (state === "dispatching") return "pending";
  return "";
}

function numberOrNull(x: unknown): number | null {
  return typeof x === "number" && Number.isFinite(x) ? x : null;
}

function formatUsd4(value: number): string {
  return `$${value.toFixed(4)}`;
}

function stringOrNull(x: unknown): string | null {
  return typeof x === "string" && x.length > 0 ? x : null;
}

function artifactPathFromUrl(url: string): string {
  const prefix = "blob://artifacts/";
  if (url.startsWith(prefix)) return url.slice(prefix.length);
  return url.replace(/^\/v1\/artifacts\//, "");
}

function parseTs(s: string): number {
  const n = Date.parse(s);
  return Number.isFinite(n) ? n : 0;
}

function now(): number {
  return Date.now();
}

function formatTime(s: string): string {
  const n = parseTs(s);
  if (!n) return s;
  return new Date(n).toLocaleString();
}

function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "—";
  const sec = Math.floor(ms / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  const remSec = sec % 60;
  if (min < 60) return `${min}m ${remSec}s`;
  const hr = Math.floor(min / 60);
  const remMin = min % 60;
  return `${hr}h ${remMin}m`;
}

function IssueEditForm({
  detail,
  onCancel,
  onSaved,
}: {
  detail: IssueDetail;
  onCancel: () => void;
  onSaved: () => void;
}) {
  const [title, setTitle] = useState(detail.title);
  const [body, setBody] = useState(detail.body);
  const [labels, setLabels] = useState(detail.labels.join(", "));
  const [state, setState] = useState(detail.state);
  const [preserveTestEnv, setPreserveTestEnv] = useState(detail.preserve_test_env);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const labelList = labels
        .split(",")
        .map((s) => s.trim())
        .filter((s) => s.length > 0);
      if (detail.number === null) {
        setError("Issue number required for edits");
        return;
      }
      const url = `/v1/issues/by-number/${encodeURIComponent(detail.project)}/${detail.number}`;
      const r = await authedFetch(url, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          title,
          body,
          labels: labelList,
          state,
          preserve_test_env: preserveTestEnv,
        }),
      });
      if (!r.ok) {
        setError(`${r.status}: ${await r.text()}`);
        return;
      }
      onSaved();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="admin-form" style={{ marginTop: "0.5rem" }}>
      <label>
        <span>Title</span>
        <input value={title} onChange={(e) => setTitle(e.target.value)} required />
      </label>
      <label>
        <span>Body</span>
        <textarea
          value={body}
          onChange={(e) => setBody(e.target.value)}
          rows={10}
          style={{ fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace", fontSize: "0.85rem" }}
        />
      </label>
      <label>
        <span>Labels (comma-separated)</span>
        <input value={labels} onChange={(e) => setLabels(e.target.value)} className="mono" />
      </label>
      <label>
        <span>State</span>
        <select value={state} onChange={(e) => setState(e.target.value)}>
          <option value="open">open</option>
          <option value="closed">closed</option>
        </select>
      </label>
      <label style={{ flexDirection: "row", alignItems: "center", gap: "0.5rem" }}>
        <input
          type="checkbox"
          checked={preserveTestEnv}
          onChange={(e) => setPreserveTestEnv(e.target.checked)}
        />
        <span>preserve test env through touchpoint review</span>
      </label>
      {error && <div className="error">{error}</div>}
      <div style={{ display: "flex", gap: "0.5rem" }}>
        <button type="submit" disabled={busy}>
          {busy ? "Saving…" : "Save"}
        </button>
        <button type="button" className="link" onClick={onCancel} disabled={busy}>
          cancel
        </button>
      </div>
    </form>
  );
}

function runStatePill(state: string): string {
  if (state === "passed") return "free";
  if (state === "in_progress" || state === "queued" || state === "pending") return "busy";
  if (state === "review_required") return "info";
  if (state === "aborted") return "drain";
  return "";
}

function runStateIsActive(state: string): boolean {
  return state === "in_progress" || state === "queued" || state === "pending";
}

function formatTimestamp(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

/**
 * Issue detail view (#42) — issue meta + tabbed content (description /
 * in progress / lineage).
 *
 * The persistent header carries identity + last-run state. Below it,
 * tabs split the content:
 *   - description: title link, body, edit form
 *   - in progress: active-run focus with attempt cards; polls while
 *     a run is in flight, falls back to last-completed-run summary
 *     (or "no runs yet") otherwise
 *   - lineage: full graph from `/v1/issues/{repo}/{n}/graph` rendered
 *     as SVG, one column per Run, attempts stacked, PR at the footer,
 *     Signals attached as side-events
 *
 * Routed via `/issues/<owner>/<repo>/<n>` (GH-anchored) or
 * `/issues/<project>/<issueId>` (native). Target is derived from the
 * URL params so deep-link reloads land directly here.
 */
import { useEffect, useMemo, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { authedFetch } from "./auth";

type IssueDetail = {
  id: string;
  project: string;
  repo: string | null;
  number: number | null;
  title: string;
  body: string;
  state: string;
  labels: string[];
  html_url: string | null;
  last_run_id: string | null;
  last_run_state: string | null;
  issue_lock_held: boolean;
};

export type IssueDetailTarget =
  | { kind: "gh"; repo: string; issue_number: number }
  | { kind: "native"; project: string; issue_id: string };

type GraphNode = {
  id: string;
  kind: "issue" | "run" | "attempt" | "pr" | "signal";
  label: string;
  state: string | null;
  timestamp: string | null;
  metadata: Record<string, unknown>;
};

type GraphEdge = {
  source: string;
  target: string;
  kind: "spawned" | "attempted" | "retried" | "opened" | "feedback" | "re_dispatched";
};

type IssueGraph = {
  issue_id: string;
  nodes: GraphNode[];
  edges: GraphEdge[];
};

type DispatchState =
  | { kind: "idle" }
  | { kind: "dispatching" }
  | { kind: "error"; message: string };

type Tab = "description" | "in_progress" | "lineage";

const TAB_SLUGS: Record<Tab, string> = {
  description: "description",
  in_progress: "in-progress",
  lineage: "lineage",
};

const SLUG_TO_TAB: Record<string, Tab> = {
  description: "description",
  "in-progress": "in_progress",
  lineage: "lineage",
};

const COL_WIDTH = 200;
const ROW_HEIGHT = 56;
const SIGNAL_OFFSET_X = 220;
const TOP_PADDING = 40;
const LEFT_PADDING = 40;
const POLL_INTERVAL_MS = 3000;

type Layout = {
  positions: Map<string, { x: number; y: number; w: number; h: number }>;
  width: number;
  height: number;
};

type IssueDetailRouteParams = {
  owner?: string;
  repo?: string;
  n?: string;
  project?: string;
  issueId?: string;
};

export function IssueDetailView() {
  const navigate = useNavigate();
  const location = useLocation();
  const params = useParams<IssueDetailRouteParams>();

  // Two route shapes land here. GH-anchored has 3 segments after /issues
  // (`:owner/:repo/:n`); native has 2 (`:project/:issueId`). React-router
  // only fills params for the matched route, so `params.n` being set
  // disambiguates.
  const target: IssueDetailTarget = params.n
    ? {
        kind: "gh",
        repo: `${params.owner ?? ""}/${params.repo ?? ""}`,
        issue_number: parseInt(params.n, 10),
      }
    : {
        kind: "native",
        project: params.project ?? "",
        issue_id: params.issueId ?? "",
      };

  const baseUrl =
    target.kind === "gh"
      ? `/issues/${target.repo}/${target.issue_number}`
      : `/issues/${encodeURIComponent(target.project)}/${encodeURIComponent(target.issue_id)}`;

  // Tab is URL-driven so each tab is deep-linkable. Bare `/issues/...`
  // (no tab segment) falls back to description.
  const lastSeg = location.pathname.split("/").filter(Boolean).pop() ?? "";
  const tab: Tab = SLUG_TO_TAB[lastSeg] ?? "description";
  const setTab = (t: Tab) => navigate(`${baseUrl}/${TAB_SLUGS[t]}`);

  const [detail, setDetail] = useState<IssueDetail | null>(null);
  const [graph, setGraph] = useState<IssueGraph | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<GraphNode | null>(null);
  const [editing, setEditing] = useState(false);
  const [refreshTick, setRefreshTick] = useState(0);
  const [dispatchState, setDispatchState] = useState<DispatchState>({ kind: "idle" });

  const detailUrl =
    target.kind === "gh"
      ? `/v1/issues/${target.repo}/${target.issue_number}`
      : `/v1/issues/by-id/${encodeURIComponent(target.project)}/${encodeURIComponent(target.issue_id)}`;
  const graphUrl =
    target.kind === "gh"
      ? `/v1/issues/${target.repo}/${target.issue_number}/graph`
      : null;
  const heading =
    target.kind === "gh"
      ? `${target.repo}#${target.issue_number}`
      : `${target.project} (native)`;
  const repoForLinks = target.kind === "gh" ? target.repo : null;

  const onBack = () => navigate("/issues");

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      setError(null);
      try {
        const requests: Promise<Response>[] = [authedFetch(detailUrl)];
        if (graphUrl) requests.push(authedFetch(graphUrl));
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
      } catch (e) {
        if (!cancelled) setError(String(e));
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, [detailUrl, graphUrl, refreshTick]);

  // While the in-progress tab is open and a run is actually in flight,
  // poll the same endpoints so attempt cards fill in as conclusions /
  // verification verdicts / decisions land server-side.
  const isInFlight = !!(detail && (detail.issue_lock_held || detail.last_run_state === "in_progress"));

  const onRedispatch = async () => {
    if (!detail) return;
    setDispatchState({ kind: "dispatching" });
    try {
      const r = await authedFetch("/v1/runs/dispatch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ issue_id: detail.id, project: detail.project }),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`/v1/runs/dispatch -> ${r.status}: ${text}`);
      }
      setDispatchState({ kind: "idle" });
      setRefreshTick((t) => t + 1);
    } catch (e) {
      setDispatchState({ kind: "error", message: String(e) });
    }
  };
  useEffect(() => {
    if (tab !== "in_progress") return;
    if (!isInFlight) return;
    const id = setInterval(() => setRefreshTick((t) => t + 1), POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [tab, isInFlight]);

  const layout = useMemo<Layout | null>(() => {
    if (!graph) return null;
    return computeLayout(graph);
  }, [graph]);

  return (
    <>
      <h2>
        <button type="button" className="link" onClick={onBack} style={{ marginRight: "1rem" }}>
          ← back
        </button>
        {heading}
      </h2>
      {error && <div className="empty error">{error}</div>}
      {detail === null && !error ? (
        <div className="empty">Loading…</div>
      ) : detail ? (
        <>
          <IssueHeader detail={detail} />

          <div className="tabs" role="tablist">
            <TabButton current={tab} value="description" onSelect={setTab}>
              description
            </TabButton>
            <TabButton current={tab} value="in_progress" onSelect={setTab}>
              in progress
              {isInFlight && <span className="tab-dot" aria-label="active" />}
            </TabButton>
            <TabButton current={tab} value="lineage" onSelect={setTab}>
              lineage
            </TabButton>
          </div>

          <div className="tab-panel">
            {tab === "description" && (
              <DescriptionTab
                detail={detail}
                editing={editing}
                onEdit={() => setEditing(true)}
                onCancelEdit={() => setEditing(false)}
                onSaved={() => {
                  setEditing(false);
                  setRefreshTick((t) => t + 1);
                }}
              />
            )}
            {tab === "in_progress" && (
              <InProgressTab
                graph={graph}
                graphAvailable={!!graphUrl}
                repo={repoForLinks}
                inFlight={isInFlight}
                dispatchState={dispatchState}
                onRedispatch={() => void onRedispatch()}
              />
            )}
            {tab === "lineage" && (
              <LineageTab
                graph={graph}
                graphAvailable={!!graphUrl}
                layout={layout}
                selected={selected}
                onSelect={setSelected}
              />
            )}
          </div>
        </>
      ) : null}
    </>
  );
}

function IssueHeader({ detail }: { detail: IssueDetail }) {
  return (
    <div className="project-info">
      <div className="row">
        <span className="key">title</span>
        <span className="val">
          {detail.html_url ? (
            <a href={detail.html_url} target="_blank" rel="noreferrer">
              {detail.title}
            </a>
          ) : (
            detail.title
          )}
        </span>
      </div>
      <div className="row">
        <span className="key">project</span>
        <span className="val mono">{detail.project}</span>
      </div>
      <div className="row">
        <span className="key">state</span>
        <span className="val mono">{detail.state}</span>
      </div>
      <div className="row">
        <span className="key">labels</span>
        <span className="val mono dim">
          {detail.labels.length === 0 ? "—" : detail.labels.join(", ")}
        </span>
      </div>
      <div className="row">
        <span className="key">last run</span>
        <span className="val">
          {detail.last_run_state ? (
            <span className={`pill ${runStatePill(detail.last_run_state)}`}>
              {detail.last_run_state}
            </span>
          ) : (
            "—"
          )}
          {detail.issue_lock_held && (
            <span className="pill busy" style={{ marginLeft: "0.5rem" }}>
              in flight
            </span>
          )}
        </span>
      </div>
    </div>
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
      role="tab"
      aria-selected={selected}
      className={`tab${selected ? " selected" : ""}`}
      onClick={() => onSelect(value)}
    >
      {children}
    </button>
  );
}

function DescriptionTab({
  detail,
  editing,
  onEdit,
  onCancelEdit,
  onSaved,
}: {
  detail: IssueDetail;
  editing: boolean;
  onEdit: () => void;
  onCancelEdit: () => void;
  onSaved: () => void;
}) {
  if (editing) {
    return <IssueEditForm detail={detail} onCancel={onCancelEdit} onSaved={onSaved} />;
  }
  return (
    <>
      <div style={{ display: "flex", justifyContent: "flex-end", marginBottom: "0.5rem" }}>
        <button type="button" className="link" onClick={onEdit}>
          edit
        </button>
      </div>
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
    </>
  );
}

function InProgressTab({
  graph,
  graphAvailable,
  repo,
  inFlight,
  dispatchState,
  onRedispatch,
}: {
  graph: IssueGraph | null;
  graphAvailable: boolean;
  repo: string | null;
  inFlight: boolean;
  dispatchState: DispatchState;
  onRedispatch: () => void;
}) {
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

  const activeRun = findActiveRun(graph);
  const lastCompleted = findLastCompletedRun(graph);

  const dispatching = dispatchState.kind === "dispatching";
  const dispatchDisabled = inFlight || dispatching;
  const actions = (
    <div className="run-actions">
      <button
        type="button"
        className="link"
        onClick={onRedispatch}
        disabled={dispatchDisabled}
      >
        {dispatching
          ? "dispatching…"
          : inFlight
          ? "in flight"
          : "re-dispatch"}
      </button>
      {dispatchState.kind === "error" && (
        <span
          className="pill drain"
          style={{ marginLeft: "0.5rem" }}
          title={dispatchState.message}
        >
          error
        </span>
      )}
    </div>
  );

  if (activeRun) {
    return (
      <>
        {actions}
        <RunPanel run={activeRun} graph={graph} repo={repo} live />
      </>
    );
  }
  if (inFlight) {
    return (
      <>
        {actions}
        <div className="empty">
          Run lock held — waiting for the run record to land.
        </div>
      </>
    );
  }
  if (lastCompleted) {
    return (
      <>
        {actions}
        <div className="run-status-banner">
          No run in flight. Showing the last completed run.
        </div>
        <RunPanel run={lastCompleted} graph={graph} repo={repo} live={false} />
      </>
    );
  }
  return (
    <>
      {actions}
      <div className="empty">
        No runs yet — re-dispatch above to start one.
      </div>
    </>
  );
}

function RunPanel({
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
  const attempts = attemptsForRun(graph, run.id);
  const meta = run.metadata;
  const cumulativeCost = numberOrNull(meta.cumulative_cost_usd);
  const workflow = stringOrNull(meta.workflow);
  const triggerSource = stringOrNull(meta.trigger_source);
  const abortReason = stringOrNull(meta.abort_reason);
  const prNumber = numberOrNull(meta.pr_number);
  const prBranch = stringOrNull(meta.pr_branch);
  const stateLabel = run.state ?? "unknown";

  return (
    <div className="run-panel">
      <div className="run-panel-header">
        <div>
          <span className={`pill ${runStatePill(stateLabel)}`}>{stateLabel}</span>
          <span className="mono dim" style={{ marginLeft: "0.5rem" }}>
            {run.label}
          </span>
          {live && <span className="live-dot" aria-label="live" />}
        </div>
        {run.timestamp && (
          <span className="dim mono">started {formatTime(run.timestamp)}</span>
        )}
      </div>
      <div className="run-panel-meta">
        {workflow && (
          <div>
            <span className="key">workflow</span>{" "}
            <span className="mono">{workflow}</span>
          </div>
        )}
        {triggerSource && (
          <div>
            <span className="key">trigger</span>{" "}
            <span className="mono">{triggerSource}</span>
          </div>
        )}
        <div>
          <span className="key">attempts</span>{" "}
          <span className="mono">{attempts.length}</span>
        </div>
        {cumulativeCost !== null && (
          <div>
            <span className="key">cost</span>{" "}
            <span className="mono">${cumulativeCost.toFixed(4)}</span>
          </div>
        )}
        {prNumber !== null && repo && (
          <div>
            <span className="key">PR</span>{" "}
            <a
              className="mono"
              href={`https://github.com/${repo}/pull/${prNumber}`}
              target="_blank"
              rel="noreferrer"
            >
              #{prNumber}
            </a>
            {prBranch && <span className="dim mono"> ({prBranch})</span>}
          </div>
        )}
        {abortReason && (
          <div>
            <span className="key">abort</span>{" "}
            <span className="mono">{abortReason}</span>
          </div>
        )}
      </div>

      {attempts.length === 0 ? (
        <div className="empty dim">No attempts dispatched yet.</div>
      ) : (
        <div className="attempt-list">
          {attempts.map((a) => (
            <AttemptCard key={a.id} attempt={a} repo={repo} />
          ))}
        </div>
      )}
    </div>
  );
}

function AttemptCard({ attempt, repo }: { attempt: GraphNode; repo: string | null }) {
  const meta = attempt.metadata;
  const phase = stringOrNull(meta.phase) ?? "attempt";
  const dispatchedAt = attempt.timestamp;
  const completedAt = stringOrNull(meta.completed_at);
  const conclusion = stringOrNull(meta.conclusion);
  const decision = stringOrNull(meta.decision);
  const workflowRunId = meta.workflow_run_id != null ? String(meta.workflow_run_id) : null;
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

  const running = !completedAt;
  const elapsedLabel = dispatchedAt
    ? running
      ? `${formatDuration(now() - parseTs(dispatchedAt))} elapsed`
      : completedAt
        ? `ran ${formatDuration(parseTs(completedAt) - parseTs(dispatchedAt))}`
        : null
    : null;

  const statusPill = (() => {
    if (running) return { cls: "busy", text: "running" };
    if (verificationStatus === "pass") return { cls: "free", text: "pass" };
    if (verificationStatus === "fail") return { cls: "drain", text: "fail" };
    if (verificationStatus === "error") return { cls: "drain", text: "error" };
    if (conclusion === "success") return { cls: "free", text: "success" };
    if (conclusion === "cancelled") return { cls: "drain", text: "cancelled" };
    if (conclusion) return { cls: "drain", text: conclusion };
    return { cls: "", text: "completed" };
  })();

  return (
    <div className={`attempt-card${running ? " running" : ""}`}>
      <div className="attempt-card-head">
        <strong>{attempt.label}</strong>
        <span className={`pill ${statusPill.cls}`}>{statusPill.text}</span>
        <span className="dim mono">{phase}</span>
        {elapsedLabel && <span className="dim mono">{elapsedLabel}</span>}
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
        {workflowRunId && (
          <div>
            <span className="key">gh run</span>{" "}
            {repo ? (
              <a
                className="mono"
                href={`https://github.com/${repo}/actions/runs/${workflowRunId}`}
                target="_blank"
                rel="noreferrer"
              >
                {workflowRunId}
              </a>
            ) : (
              <span className="mono">{workflowRunId}</span>
            )}
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
    </div>
  );
}

function LineageTab({
  graph,
  graphAvailable,
  layout,
  selected,
  onSelect,
}: {
  graph: IssueGraph | null;
  graphAvailable: boolean;
  layout: Layout | null;
  selected: GraphNode | null;
  onSelect: (n: GraphNode | null) => void;
}) {
  if (!graphAvailable) {
    return (
      <div className="empty">
        Lineage isn't available for native issues yet.
      </div>
    );
  }
  if (!graph || !layout) {
    return <div className="empty">Loading graph…</div>;
  }
  if (graph.nodes.length === 1) {
    return <div className="empty">No runs yet — dispatch from the Issues page.</div>;
  }
  return (
    <>
      <GraphCanvas graph={graph} layout={layout} selected={selected} onSelect={onSelect} />
      {selected && <NodeDetailPanel node={selected} onClose={() => onSelect(null)} />}
    </>
  );
}

function findActiveRun(graph: IssueGraph): GraphNode | null {
  return graph.nodes.find((n) => n.kind === "run" && n.state === "in_progress") ?? null;
}

function findLastCompletedRun(graph: IssueGraph): GraphNode | null {
  const completed = graph.nodes
    .filter((n) => n.kind === "run" && n.state !== "in_progress")
    .sort((a, b) => (b.timestamp ?? "").localeCompare(a.timestamp ?? ""));
  return completed[0] ?? null;
}

function attemptsForRun(graph: IssueGraph, runNodeId: string): GraphNode[] {
  // run node id is `run:<run_id>`; attempt ids are `attempt:<run_id>:<index>`.
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

function isRecord(x: unknown): x is Record<string, unknown> {
  return typeof x === "object" && x !== null && !Array.isArray(x);
}

function numberOrNull(x: unknown): number | null {
  return typeof x === "number" && Number.isFinite(x) ? x : null;
}

function stringOrNull(x: unknown): string | null {
  return typeof x === "string" && x.length > 0 ? x : null;
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
      const url = `/v1/issues/by-id/${encodeURIComponent(detail.project)}/${encodeURIComponent(detail.id)}`;
      const r = await authedFetch(url, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          title,
          body,
          labels: labelList,
          state,
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

function GraphCanvas({
  graph,
  layout,
  selected,
  onSelect,
}: {
  graph: IssueGraph;
  layout: Layout;
  selected: GraphNode | null;
  onSelect: (n: GraphNode | null) => void;
}) {
  return (
    <svg
      width={layout.width}
      height={layout.height}
      style={{
        background: "#0a0a0c",
        border: "1px solid #2a2a2e",
        borderRadius: "4px",
        display: "block",
      }}
    >
      <defs>
        <marker
          id="arrow"
          viewBox="0 0 10 10"
          refX="10"
          refY="5"
          markerWidth="6"
          markerHeight="6"
          orient="auto-start-reverse"
        >
          <path d="M 0 0 L 10 5 L 0 10 z" fill="#555" />
        </marker>
        <marker
          id="arrow-feedback"
          viewBox="0 0 10 10"
          refX="10"
          refY="5"
          markerWidth="6"
          markerHeight="6"
          orient="auto-start-reverse"
        >
          <path d="M 0 0 L 10 5 L 0 10 z" fill="#fb923c" />
        </marker>
      </defs>

      {graph.edges.map((e, i) => (
        <EdgeLine key={i} edge={e} layout={layout} />
      ))}

      {graph.nodes.map((n) => (
        <NodeBox
          key={n.id}
          node={n}
          layout={layout}
          isSelected={selected?.id === n.id}
          onClick={() => onSelect(n)}
        />
      ))}
    </svg>
  );
}

function EdgeLine({ edge, layout }: { edge: GraphEdge; layout: Layout }) {
  const a = layout.positions.get(edge.source);
  const b = layout.positions.get(edge.target);
  if (!a || !b) return null;

  const isFeedback = edge.kind === "feedback" || edge.kind === "re_dispatched";
  const stroke = isFeedback ? "#fb923c" : "#555";
  const dash = edge.kind === "re_dispatched" ? "4,4" : undefined;
  const marker = isFeedback ? "url(#arrow-feedback)" : "url(#arrow)";

  const x1 = a.x + a.w / 2;
  const y1 = a.y + a.h;
  const x2 = b.x + b.w / 2;
  const y2 = b.y;

  const sameColumn = Math.abs(x1 - x2) < 1;
  if (sameColumn) {
    return (
      <line
        x1={x1} y1={y1} x2={x2} y2={y2}
        stroke={stroke}
        strokeWidth={1.5}
        strokeDasharray={dash}
        markerEnd={marker}
      />
    );
  }
  const mx = (x1 + x2) / 2;
  const my = Math.max(y1, y2) + 12;
  const d = `M ${x1} ${y1} Q ${mx} ${my} ${x2} ${y2}`;
  return (
    <path
      d={d}
      fill="none"
      stroke={stroke}
      strokeWidth={1.5}
      strokeDasharray={dash}
      markerEnd={marker}
    />
  );
}

function NodeBox({
  node,
  layout,
  isSelected,
  onClick,
}: {
  node: GraphNode;
  layout: Layout;
  isSelected: boolean;
  onClick: () => void;
}) {
  const pos = layout.positions.get(node.id);
  if (!pos) return null;

  const fill = nodeFill(node);
  const stroke = isSelected ? "#60a5fa" : "#2a2a2e";

  return (
    <g style={{ cursor: "pointer" }} onClick={onClick}>
      <rect
        x={pos.x}
        y={pos.y}
        width={pos.w}
        height={pos.h}
        rx={4}
        fill={fill.bg}
        stroke={stroke}
        strokeWidth={isSelected ? 2 : 1}
      />
      <text
        x={pos.x + 10}
        y={pos.y + 18}
        fill={fill.fg}
        fontSize="11"
        fontFamily="ui-monospace, SFMono-Regular, Menlo, monospace"
      >
        {truncate(node.label, Math.floor((pos.w - 20) / 6.5))}
      </text>
      {node.state && (
        <text
          x={pos.x + 10}
          y={pos.y + 36}
          fill={fill.dim}
          fontSize="10"
          fontFamily="ui-monospace, SFMono-Regular, Menlo, monospace"
        >
          {node.state}
        </text>
      )}
    </g>
  );
}

function NodeDetailPanel({ node, onClose }: { node: GraphNode; onClose: () => void }) {
  return (
    <div
      style={{
        marginTop: "1rem",
        padding: "0.75rem 1rem",
        border: "1px solid #2a2a2e",
        borderRadius: "4px",
        background: "#0a0a0c",
      }}
    >
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <strong>
          {node.kind} — {node.label}
        </strong>
        <button type="button" className="link" onClick={onClose}>
          close
        </button>
      </div>
      {node.state && <div className="dim mono" style={{ marginTop: 4 }}>state: {node.state}</div>}
      {node.timestamp && <div className="dim mono" style={{ marginTop: 2 }}>at: {node.timestamp}</div>}
      <pre
        className="mono"
        style={{
          marginTop: "0.5rem",
          fontSize: "0.8rem",
          background: "#050507",
          padding: "0.5rem",
          borderRadius: "3px",
          overflowX: "auto",
        }}
      >
        {JSON.stringify(node.metadata, null, 2)}
      </pre>
    </div>
  );
}

function computeLayout(graph: IssueGraph): Layout {
  const positions = new Map<string, { x: number; y: number; w: number; h: number }>();

  const issueNode = graph.nodes.find((n) => n.kind === "issue");
  if (issueNode) {
    positions.set(issueNode.id, {
      x: LEFT_PADDING,
      y: TOP_PADDING,
      w: COL_WIDTH * 2,
      h: 44,
    });
  }

  const runs = graph.nodes
    .filter((n) => n.kind === "run")
    .sort((a, b) => (a.timestamp ?? "").localeCompare(b.timestamp ?? ""));

  const runIdToCol = new Map<string, number>();
  runs.forEach((r, i) => runIdToCol.set(r.id, i));

  const attemptByRun = new Map<string, GraphNode[]>();
  for (const n of graph.nodes) {
    if (n.kind !== "attempt") continue;
    const parts = n.id.split(":");
    const runId = `run:${parts[1]}`;
    const arr = attemptByRun.get(runId) ?? [];
    arr.push(n);
    attemptByRun.set(runId, arr);
  }
  for (const arr of attemptByRun.values()) {
    arr.sort((a, b) => {
      const ai = parseInt(a.id.split(":").pop() ?? "0", 10);
      const bi = parseInt(b.id.split(":").pop() ?? "0", 10);
      return ai - bi;
    });
  }

  const prByRun = new Map<string, GraphNode>();
  for (const e of graph.edges) {
    if (e.kind !== "opened") continue;
    const target = graph.nodes.find((n) => n.id === e.target);
    if (target?.kind === "pr") prByRun.set(e.source, target);
  }

  const columnTopY = TOP_PADDING + 44 + 40;
  let maxColumnBottomY = columnTopY;
  for (const run of runs) {
    const col = runIdToCol.get(run.id) ?? 0;
    const colX = LEFT_PADDING + col * (COL_WIDTH + 24);
    let y = columnTopY;

    positions.set(run.id, {
      x: colX,
      y,
      w: COL_WIDTH,
      h: 44,
    });
    y += 44 + 12;

    for (const a of attemptByRun.get(run.id) ?? []) {
      positions.set(a.id, { x: colX, y, w: COL_WIDTH, h: 44 });
      y += 44 + 8;
    }

    const pr = prByRun.get(run.id);
    if (pr && !positions.has(pr.id)) {
      positions.set(pr.id, { x: colX, y, w: COL_WIDTH, h: 44 });
      y += 44 + 8;
    }

    maxColumnBottomY = Math.max(maxColumnBottomY, y);
  }

  for (const n of graph.nodes) {
    if (n.kind !== "signal") continue;
    const fbEdge = graph.edges.find((e) => e.kind === "feedback" && e.target === n.id);
    let baseX = LEFT_PADDING;
    let baseY = maxColumnBottomY;
    if (fbEdge) {
      const sourcePos = positions.get(fbEdge.source);
      if (sourcePos) {
        baseX = sourcePos.x;
        baseY = sourcePos.y;
      }
    }
    positions.set(n.id, {
      x: baseX + SIGNAL_OFFSET_X,
      y: baseY,
      w: COL_WIDTH - 40,
      h: ROW_HEIGHT - 12,
    });
  }

  let width = LEFT_PADDING * 2 + Math.max(1, runs.length) * (COL_WIDTH + 24) + 80;
  let height = maxColumnBottomY + TOP_PADDING;
  for (const p of positions.values()) {
    width = Math.max(width, p.x + p.w + LEFT_PADDING);
    height = Math.max(height, p.y + p.h + TOP_PADDING);
  }

  return { positions, width, height };
}

function nodeFill(n: GraphNode): { bg: string; fg: string; dim: string } {
  if (n.kind === "issue") return { bg: "#1a2030", fg: "#e8e8e8", dim: "#888" };
  if (n.kind === "pr") return { bg: "#1a1f1a", fg: "#a8d8a8", dim: "#6a8a6a" };
  if (n.kind === "signal") return { bg: "#2a1d10", fg: "#fb923c", dim: "#a87040" };

  const state = (n.state ?? "").toLowerCase();
  if (state === "passed" || state === "pass") {
    return { bg: "#14321e", fg: "#4ade80", dim: "#7aae8a" };
  }
  if (state === "in_progress") {
    return { bg: "#3a2a10", fg: "#fb923c", dim: "#a8754a" };
  }
  if (state === "aborted" || state === "fail" || state === "error") {
    return { bg: "#321414", fg: "#f87171", dim: "#a85050" };
  }
  return { bg: "#15151a", fg: "#c0c0c0", dim: "#666" };
}

function runStatePill(state: string): string {
  if (state === "passed") return "free";
  if (state === "in_progress") return "busy";
  if (state === "aborted") return "drain";
  return "";
}

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, Math.max(1, max - 1)) + "…";
}

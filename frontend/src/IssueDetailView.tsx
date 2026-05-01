/**
 * Issue detail view (#42) — issue meta + per-issue lineage graph.
 *
 * The graph is the answer to "what did the agent actually do on this
 * issue, where did it stall, what feedback drove which retry." Sourced
 * from `/v1/issues/{repo}/{n}/graph`; rendered as SVG with one column
 * per Run (left-to-right by created_at), PhaseAttempts stacked vertically
 * inside each Run column, PR at the column footer, and Signals attached
 * as side-events to their targets.
 */
import { useEffect, useMemo, useState } from "react";
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

const COL_WIDTH = 200;
const ROW_HEIGHT = 56;
const SIGNAL_OFFSET_X = 220;  // signals render to the right of their target's column
const TOP_PADDING = 40;
const LEFT_PADDING = 40;

type Layout = {
  positions: Map<string, { x: number; y: number; w: number; h: number }>;
  width: number;
  height: number;
};

export function IssueDetailView({
  target,
  onBack,
}: {
  target: IssueDetailTarget;
  onBack: () => void;
}) {
  const [detail, setDetail] = useState<IssueDetail | null>(null);
  const [graph, setGraph] = useState<IssueGraph | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<GraphNode | null>(null);
  const [editing, setEditing] = useState(false);
  const [refreshTick, setRefreshTick] = useState(0);

  // GH-anchored issues have a graph endpoint keyed off (repo, number);
  // native issues don't yet, so the lineage view is suppressed for them.
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
        {detail && (
          <button
            type="button"
            className="link"
            onClick={() => setEditing((e) => !e)}
            style={{ marginLeft: "1rem", fontSize: "0.85rem" }}
          >
            {editing ? "cancel edit" : "edit"}
          </button>
        )}
      </h2>
      {error && <div className="empty error">{error}</div>}
      {detail === null && !error ? (
        <div className="empty">Loading…</div>
      ) : detail ? (
        <>
          {editing ? (
            <IssueEditForm
              detail={detail}
              onCancel={() => setEditing(false)}
              onSaved={() => {
                setEditing(false);
                setRefreshTick((t) => t + 1);
              }}
            />
          ) : (
            <>
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

              {detail.body.trim() && (
                <>
                  <h2>Body</h2>
                  <pre style={{
                    whiteSpace: "pre-wrap",
                    fontFamily: "inherit",
                    background: "#0a0a0c",
                    padding: "0.75rem 1rem",
                    border: "1px solid #2a2a2e",
                    borderRadius: "4px",
                    margin: 0,
                  }}>
                    {detail.body}
                  </pre>
                </>
              )}
            </>
          )}

          {graphUrl && (
            <>
              <h2>Lineage graph</h2>
              {graph && layout ? (
                graph.nodes.length === 1 ? (
                  <div className="empty">No runs yet — dispatch from the Issues page.</div>
                ) : (
                  <GraphCanvas
                    graph={graph}
                    layout={layout}
                    selected={selected}
                    onSelect={setSelected}
                  />
                )
              ) : (
                <div className="empty">Loading graph…</div>
              )}

              {selected && (
                <NodeDetailPanel node={selected} onClose={() => setSelected(null)} />
              )}
            </>
          )}
        </>
      ) : null}
    </>
  );
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

  // Anchor at edge mid-points.
  const x1 = a.x + a.w / 2;
  const y1 = a.y + a.h;
  const x2 = b.x + b.w / 2;
  const y2 = b.y;

  // Curved path for cross-column edges (re_dispatched, feedback to a
  // different column); straight for same-column.
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
  // Quadratic curve through a midpoint between the two anchor points.
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

  // Issue (root) at top.
  const issueNode = graph.nodes.find((n) => n.kind === "issue");
  if (issueNode) {
    positions.set(issueNode.id, {
      x: LEFT_PADDING,
      y: TOP_PADDING,
      w: COL_WIDTH * 2,
      h: 44,
    });
  }

  // Order Runs by timestamp; assign each a column.
  const runs = graph.nodes
    .filter((n) => n.kind === "run")
    .sort((a, b) => (a.timestamp ?? "").localeCompare(b.timestamp ?? ""));

  // Map run id → column index. Also map attempt nodes to their run.
  const runIdToCol = new Map<string, number>();
  runs.forEach((r, i) => runIdToCol.set(r.id, i));

  const attemptByRun = new Map<string, GraphNode[]>();
  for (const n of graph.nodes) {
    if (n.kind !== "attempt") continue;
    // attempt id format: "attempt:<run_id>:<index>"
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
  // Map run → its PR via the "opened" edge.
  for (const e of graph.edges) {
    if (e.kind !== "opened") continue;
    const target = graph.nodes.find((n) => n.id === e.target);
    if (target?.kind === "pr") prByRun.set(e.source, target);
  }

  // Lay out columns.
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

  // Signals: right of their target's column at the target's row.
  for (const n of graph.nodes) {
    if (n.kind !== "signal") continue;
    // Find an incoming "feedback" edge to position by source.
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

  // Compute canvas size.
  let width = LEFT_PADDING * 2 + Math.max(1, runs.length) * (COL_WIDTH + 24) + 80;
  let height = maxColumnBottomY + TOP_PADDING;
  // Expand for any signal positions that overflow.
  for (const p of positions.values()) {
    width = Math.max(width, p.x + p.w + LEFT_PADDING);
    height = Math.max(height, p.y + p.h + TOP_PADDING);
  }

  return { positions, width, height };
}

function nodeFill(n: GraphNode): { bg: string; fg: string; dim: string } {
  // Status-driven coloring; defaults by kind.
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

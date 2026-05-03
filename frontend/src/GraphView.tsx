import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";

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
  kind: "spawned" | "attempted" | "retried" | "opened" | "feedback" | "re_dispatched" | "resumed_from";
};

type SystemGraph = {
  issue_id: string;
  nodes: GraphNode[];
  edges: GraphEdge[];
};

const STALE_RUN_MS = 30 * 60 * 1000;
const STALE_SIGNAL_MS = 60 * 1000;

export function GraphView({
  projectFilter,
}: {
  projectFilter: string | null;
}) {
  const navigate = useNavigate();
  const [graph, setGraph] = useState<SystemGraph | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const refresh = async () => {
    setLoading(true);
    setError(null);
    try {
      const url = projectFilter
        ? `/v1/graph?project=${encodeURIComponent(projectFilter)}`
        : "/v1/graph";
      const r = await fetch(url);
      if (!r.ok) throw new Error(`${url} -> ${r.status}`);
      setGraph((await r.json()) as SystemGraph);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setGraph(null);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void refresh();
    const id = window.setInterval(() => void refresh(), 5000);
    return () => window.clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectFilter]);

  const rows = useMemo(() => {
    if (!graph) return [];
    const issueNodes = graph.nodes.filter((n) => n.kind === "issue");
    return issueNodes.map((issue) => {
      const seen = new Set<string>([issue.id]);
      const queue = [issue.id];
      const nodes: GraphNode[] = [];
      while (queue.length > 0) {
        const source = queue.shift() ?? "";
        for (const edge of graph.edges.filter((e) => e.source === source)) {
          if (seen.has(edge.target)) continue;
          seen.add(edge.target);
          const node = graph.nodes.find((n) => n.id === edge.target);
          if (!node) continue;
          nodes.push(node);
          queue.push(node.id);
        }
      }
      return { issue, nodes };
    });
  }, [graph]);

  const openIssue = (issue: GraphNode) => {
    const repo = issue.metadata.repo;
    const number = issue.metadata.number;
    if (typeof repo === "string" && typeof number === "number") {
      navigate(`/issues/${repo}/${number}`);
      return;
    }
    const project = String(issue.metadata.project ?? "");
    const issueId = String(issue.metadata.issue_id ?? "");
    navigate(`/issues/${encodeURIComponent(project)}/${encodeURIComponent(issueId)}`);
  };

  const openNode = (issue: GraphNode, node: GraphNode) => {
    if (node.kind === "issue") {
      openIssue(node);
      return;
    }
    if (node.kind !== "pr") {
      openIssue(issue);
      return;
    }
    const repo = node.metadata.repo;
    const number = node.metadata.number;
    if (typeof repo === "string" && typeof number === "number") {
      navigate(`/prs/${repo}/${number}`);
    }
  };

  return (
    <>
      <h2>
        graph{rows ? ` (${rows.length})` : ""}
        {projectFilter && (
          <span className="filter-hint"> — filtered to {projectFilter}</span>
        )}
        <button
          type="button"
          className="inline-action"
          onClick={() => void refresh()}
          disabled={loading}
        >
          {loading ? "refreshing…" : "refresh"}
        </button>
      </h2>
      {error && <div className="empty error">{error}</div>}
      {!graph && !error ? (
        <div className="empty">{loading ? "Loading…" : ""}</div>
      ) : rows.length === 0 ? (
        <div className="empty">No open issues in the graph.</div>
      ) : (
        <div className="system-graph">
          {rows.map(({ issue, nodes }) => (
            <div className="graph-row" key={issue.id}>
              <button
                type="button"
                className="graph-node issue-node"
                onClick={() => openIssue(issue)}
              >
                <span className="graph-label">{issue.label}</span>
                <span className="graph-meta mono">{String(issue.metadata.project ?? "")}</span>
              </button>
              <div className="graph-flow">
                {nodes.length === 0 ? (
                  <span className="dim">no in-flight nodes</span>
                ) : (
                  nodes.map((node) => (
                    <button
                      type="button"
                      key={node.id}
                      className={`graph-node ${node.kind}-node ${nodeClass(node)}`}
                      onClick={() => openNode(issue, node)}
                      title={isStale(node) ? staleTitle(node) : undefined}
                    >
                      <span className="graph-kind mono">{node.kind}</span>
                      <span className="graph-label">{node.label}</span>
                      {node.state && <span className="graph-state mono">{node.state}</span>}
                      {isStale(node) && <span className="graph-state mono stale-label">stale</span>}
                    </button>
                  ))
                )}
              </div>
            </div>
          ))}
        </div>
      )}
    </>
  );
}

function nodeClass(node: GraphNode): string {
  if (isStale(node)) return "drain stale";
  if (node.kind === "signal") return "info";
  if (node.state === "in_progress" || node.state === "pending") return "busy";
  if (node.state === "aborted" || node.state === "failure") return "drain";
  if (node.state === "open" || node.state === "success" || node.state === "passed") return "free";
  return "info";
}

function isStale(node: GraphNode): boolean {
  if (!node.timestamp) return false;
  const age = Date.now() - new Date(node.timestamp).getTime();
  if (!Number.isFinite(age)) return false;
  if (node.kind === "run" && node.state === "in_progress") return age > STALE_RUN_MS;
  if (node.kind === "signal" && node.state === "pending") return age > STALE_SIGNAL_MS;
  return false;
}

function staleTitle(node: GraphNode): string {
  if (node.kind === "run") return "run has been in progress for more than 30 minutes";
  if (node.kind === "signal") return "signal has been pending for more than 1 minute";
  return "stale";
}

import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { authedFetch } from "./auth";

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

export function GraphView({
  signedIn,
  projectFilter,
}: {
  signedIn: boolean;
  projectFilter: string | null;
}) {
  const navigate = useNavigate();
  const [graph, setGraph] = useState<SystemGraph | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const refresh = async () => {
    if (!signedIn) {
      setGraph(null);
      setError("sign in to view graph");
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const url = projectFilter
        ? `/v1/graph?project=${encodeURIComponent(projectFilter)}`
        : "/v1/graph";
      const r = await authedFetch(url);
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
  }, [signedIn, projectFilter]);

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

  const openNode = (node: GraphNode) => {
    if (node.kind === "issue") {
      const repo = node.metadata.repo;
      const number = node.metadata.number;
      if (typeof repo === "string" && typeof number === "number") {
        navigate(`/issues/${repo}/${number}`);
        return;
      }
      const project = String(node.metadata.project ?? "");
      const issueId = String(node.metadata.issue_id ?? "");
      navigate(`/issues/${encodeURIComponent(project)}/${encodeURIComponent(issueId)}`);
      return;
    }
    if (node.kind === "pr") {
      const repo = node.metadata.repo;
      const number = node.metadata.number;
      if (typeof repo === "string" && typeof number === "number") {
        navigate(`/prs/${repo}/${number}`);
      }
    }
  };

  if (!signedIn) {
    return <div className="empty">Sign in to view graph.</div>;
  }

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
                onClick={() => openNode(issue)}
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
                      onClick={() => openNode(node)}
                      disabled={node.kind !== "issue" && node.kind !== "pr"}
                    >
                      <span className="graph-kind mono">{node.kind}</span>
                      <span className="graph-label">{node.label}</span>
                      {node.state && <span className="graph-state mono">{node.state}</span>}
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
  if (node.kind === "signal") return "info";
  if (node.state === "in_progress" || node.state === "pending") return "busy";
  if (node.state === "aborted" || node.state === "failure") return "drain";
  if (node.state === "open" || node.state === "success" || node.state === "passed") return "free";
  return "info";
}

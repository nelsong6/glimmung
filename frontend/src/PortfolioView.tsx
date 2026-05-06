import { useEffect, useMemo, useState } from "react";
import { authedFetch } from "./auth";

type PortfolioReviewState = "unreviewed" | "needs_review" | "approved" | "needs_work";

type PortfolioElement = {
  id: string;
  project: string;
  route: string;
  element_id: string;
  title: string;
  screenshot_url: string | null;
  preview_url: string | null;
  status: PortfolioReviewState;
  notes: string;
  last_touched_run_id: string | null;
  metadata: Record<string, unknown>;
  created_at: string;
  updated_at: string;
};

type DispatchResult = {
  state: string;
  lease_id: string | null;
  run_id: string | null;
  host: string | null;
  workflow: string | null;
  issue_lock_holder_id: string | null;
  detail: string | null;
};

type ActionStatus =
  | { kind: "idle" }
  | { kind: "saving"; key: string }
  | { kind: "dispatching"; key: string }
  | { kind: "result"; key: string; result: DispatchResult }
  | { kind: "error"; key: string; message: string };

const STATUS_OPTIONS: Array<{ value: PortfolioReviewState | "all"; label: string }> = [
  { value: "needs_review", label: "needs review" },
  { value: "needs_work", label: "needs work" },
  { value: "unreviewed", label: "unreviewed" },
  { value: "approved", label: "approved" },
  { value: "all", label: "all" },
];

export function PortfolioView({
  signedIn,
  projectFilter,
}: {
  signedIn: boolean;
  projectFilter: string | null;
}) {
  const [rows, setRows] = useState<PortfolioElement[] | null>(null);
  const [statusFilter, setStatusFilter] = useState<PortfolioReviewState | "all">("needs_review");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [action, setAction] = useState<ActionStatus>({ kind: "idle" });

  const refresh = async () => {
    setLoading(true);
    setError(null);
    try {
      const params = new URLSearchParams();
      if (projectFilter) params.set("project", projectFilter);
      if (statusFilter !== "all") params.set("status", statusFilter);
      const url = params.size > 0 ? `/v1/portfolio/elements?${params.toString()}` : "/v1/portfolio/elements";
      const r = await fetch(url);
      if (!r.ok) throw new Error(`${url} -> ${r.status}`);
      setRows((await r.json()) as PortfolioElement[]);
    } catch (e) {
      setError(String(e));
      setRows(null);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectFilter, statusFilter]);

  const projectCounts = useMemo(() => {
    const counts = new Map<string, number>();
    for (const row of rows ?? []) counts.set(row.project, (counts.get(row.project) ?? 0) + 1);
    return counts;
  }, [rows]);

  const patchStatus = async (row: PortfolioElement, status: PortfolioReviewState) => {
    setAction({ kind: "saving", key: row.id });
    try {
      const r = await authedFetch(`/v1/portfolio/elements/${encodeURIComponent(row.project)}/${encodeURIComponent(row.id)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ status }),
      });
      if (!r.ok) throw new Error(`/v1/portfolio/elements/${row.project}/${row.id} -> ${r.status}: ${await r.text()}`);
      setAction({ kind: "idle" });
      void refresh();
    } catch (e) {
      setAction({ kind: "error", key: row.id, message: String(e) });
    }
  };

  const dispatchProject = async (project: string) => {
    const key = `dispatch:${project}`;
    setAction({ kind: "dispatching", key });
    try {
      const r = await authedFetch("/v1/portfolio/elements/dispatch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          project,
          status: statusFilter === "all" ? "needs_review" : statusFilter,
        }),
      });
      if (!r.ok) throw new Error(`/v1/portfolio/elements/dispatch -> ${r.status}: ${await r.text()}`);
      setAction({ kind: "result", key, result: (await r.json()) as DispatchResult });
    } catch (e) {
      setAction({ kind: "error", key, message: String(e) });
    }
  };

  return (
    <>
      <h2>
        Portfolio review{rows ? ` (${rows.length})` : ""}
        {projectFilter && <span className="filter-hint"> — filtered to {projectFilter}</span>}
        <select
          className="inline-select"
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value as PortfolioReviewState | "all")}
        >
          {STATUS_OPTIONS.map((option) => (
            <option key={option.value} value={option.value}>{option.label}</option>
          ))}
        </select>
        <button type="button" className="inline-action" onClick={() => void refresh()} disabled={loading}>
          {loading ? "refreshing..." : "refresh"}
        </button>
      </h2>
      {error && <div className="empty error">{error}</div>}
      {rows === null && !error ? (
        <div className="empty">{loading ? "Loading..." : ""}</div>
      ) : rows && rows.length === 0 ? (
        <div className="empty">No portfolio rows match this filter.</div>
      ) : rows ? (
        <>
          <div className="portfolio-review-actions">
            {Array.from(projectCounts.entries()).map(([project, count]) => {
              const key = `dispatch:${project}`;
              const current = action.kind !== "idle" && action.key === key ? action : null;
              return (
                <button
                  key={project}
                  type="button"
                  className="inline-action"
                  onClick={() => void dispatchProject(project)}
                  disabled={!signedIn || current?.kind === "dispatching"}
                  title={!signedIn ? "Sign in to dispatch review work" : undefined}
                >
                  {current?.kind === "dispatching"
                    ? "dispatching..."
                    : `dispatch ${count} for ${project}`}
                </button>
              );
            })}
          </div>
          <table>
            <thead>
              <tr>
                {!projectFilter && <th>Project</th>}
                <th>Element</th>
                <th>Route</th>
                <th>Status</th>
                <th>Evidence</th>
                <th>Notes</th>
                <th>Action</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => {
                const current = action.kind !== "idle" && action.key === row.id ? action : null;
                return (
                  <tr key={row.id}>
                    {!projectFilter && <td>{row.project}</td>}
                    <td>
                      <strong>{row.title || row.element_id}</strong>
                      <div className="mono dim">{row.element_id}</div>
                    </td>
                    <td className="mono dim">{row.route}</td>
                    <td><span className={`pill ${statusClass(row.status)}`}>{row.status}</span></td>
                    <td className="mono dim">
                      {row.preview_url ? <a href={row.preview_url}>preview</a> : null}
                      {row.preview_url && row.screenshot_url ? " / " : null}
                      {row.screenshot_url ? <a href={row.screenshot_url}>screenshot</a> : null}
                      {!row.preview_url && !row.screenshot_url ? "-" : null}
                    </td>
                    <td>{row.notes || <span className="dim">-</span>}</td>
                    <td>
                      <button
                        type="button"
                        className="link"
                        onClick={() => void patchStatus(row, "approved")}
                        disabled={!signedIn || current?.kind === "saving" || row.status === "approved"}
                      >
                        approve
                      </button>
                      <span className="dim"> / </span>
                      <button
                        type="button"
                        className="link"
                        onClick={() => void patchStatus(row, "needs_work")}
                        disabled={!signedIn || current?.kind === "saving" || row.status === "needs_work"}
                      >
                        needs work
                      </button>
                      {current?.kind === "error" && (
                        <span className="pill drain" style={{ marginLeft: "0.5rem" }} title={current.message}>error</span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          {action.kind === "result" && (
            <div className="empty">
              Dispatch {action.result.state}
              {action.result.run_id ? `: ${action.result.run_id}` : ""}
            </div>
          )}
        </>
      ) : null}
    </>
  );
}

function statusClass(status: PortfolioReviewState): string {
  if (status === "approved") return "free";
  if (status === "needs_review") return "busy";
  if (status === "needs_work") return "drain";
  return "info";
}

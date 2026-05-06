/**
 * Touchpoints view across registered repos. Each row links to the detail view,
 * where the reject-with-feedback action lives.
 *
 * Sourced from `/v1/touchpoints`. Rows include both agent-opened GitHub PR
 * touchpoints and manually mirrored PRs with no run linkage.
 *
 * Row click navigates to `/touchpoints/<owner>/<repo>/<n>`.
 */
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";

type ReportRow = {
  id: string;
  project: string;
  repo: string;
  pr_number: number;
  pr_branch: string | null;
  title: string;
  state: string;
  merged: boolean;
  html_url: string | null;
  linked_issue_id: string | null;
  linked_run_id: string | null;
  issue_number: number | null;
  run_id: string | null;
  run_state: string | null;
  run_attempts: number;
  run_cumulative_cost_usd: number;
  pr_lock_held: boolean;
};

export function ReportsView({
  projectFilter,
}: {
  projectFilter: string | null;
}) {
  const navigate = useNavigate();
  const [rows, setRows] = useState<ReportRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const refresh = async () => {
    setLoading(true);
    setError(null);
    try {
      const r = await fetch("/v1/touchpoints");
      if (!r.ok) throw new Error(`/v1/touchpoints -> ${r.status}`);
      setRows((await r.json()) as ReportRow[]);
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
  }, []);

  const visibleRows = rows
    ? projectFilter
      ? rows.filter((r) => r.project === projectFilter)
      : rows
    : null;

  return (
    <>
      <h2>
        Touchpoints{visibleRows ? ` (${visibleRows.length})` : ""}
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
      {visibleRows === null && !error ? (
        <div className="empty">{loading ? "Loading…" : ""}</div>
      ) : visibleRows && visibleRows.length === 0 ? (
        <div className="empty">
          {projectFilter
            ? `No touchpoints for ${projectFilter}.`
            : "No touchpoints yet."}
        </div>
      ) : visibleRows ? (
        <table>
          <thead>
            <tr>
              <th>Project</th>
              <th>GitHub PR</th>
              <th>Title</th>
              <th>Issue</th>
              <th>Status</th>
              <th>Run state</th>
              <th>Attempts</th>
              <th>Cost</th>
              <th>Triage</th>
            </tr>
          </thead>
          <tbody>
            {visibleRows.map((row) => (
              <tr
                key={row.id}
                className={row.pr_lock_held ? "eligible" : ""}
                onClick={() => navigate(`/touchpoints/${row.repo}/${row.pr_number}`)}
                style={{ cursor: "pointer" }}
              >
                <td>{row.project}</td>
                <td className="mono">
                  {row.repo}#{row.pr_number}
                </td>
                <td>{row.title || <span className="dim">—</span>}</td>
                <td className="mono dim">
                  {row.issue_number !== null ? `#${row.issue_number}` : "—"}
                </td>
                <td>
                  <span className={`pill ${prStatePill(row)}`}>
                    {row.merged ? "merged" : row.state}
                  </span>
                </td>
                <td>
                  {row.run_state ? (
                    <span className={`pill ${runStatePill(row.run_state)}`}>
                      {row.run_state}
                    </span>
                  ) : (
                    <span className="dim">manual</span>
                  )}
                </td>
                <td className="mono dim">{row.run_attempts || "—"}</td>
                <td className="mono dim">
                  {row.run_cumulative_cost_usd
                    ? `$${row.run_cumulative_cost_usd.toFixed(2)}`
                    : "—"}
                </td>
                <td>
                  {row.pr_lock_held ? (
                    <span className="pill busy">in flight</span>
                  ) : (
                    <span className="dim">—</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </>
  );
}

function runStatePill(state: string): string {
  if (state === "passed") return "free";
  if (state === "in_progress") return "busy";
  if (state === "review_required") return "info";
  if (state === "aborted") return "drain";
  return "dim";
}

function prStatePill(row: ReportRow): string {
  if (row.merged) return "free";
  if (row.state === "ready" || row.state === "needs_review") return "busy";
  if (row.state === "closed") return "dim";
  return "dim";
}

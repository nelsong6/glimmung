/**
 * PRs view (#19) — agent-opened PRs across registered repos. Each row
 * links to the PR's detail view, where the reject-with-feedback action
 * lives.
 *
 * Sourced from `/v1/prs` (queries the runs container for any Run with
 * `pr_number` set). Refresh on mount + manual refresh button.
 */
import { useEffect, useState } from "react";
import { authedFetch } from "./auth";
import { PrDetailView } from "./PrDetailView";

type PrRow = {
  project: string;
  repo: string;
  pr_number: number;
  pr_branch: string | null;
  issue_number: number;
  run_id: string;
  run_state: string;
  run_attempts: number;
  run_cumulative_cost_usd: number;
  pr_lock_held: boolean;
};

type Selected = { repo: string; pr_number: number } | null;

export function PrsView({
  signedIn,
  projectFilter,
}: {
  signedIn: boolean;
  projectFilter: string | null;
}) {
  const [rows, setRows] = useState<PrRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [selected, setSelected] = useState<Selected>(null);

  const refresh = async () => {
    if (!signedIn) {
      setRows(null);
      setError("sign in to view PRs");
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const r = await authedFetch("/v1/prs");
      if (!r.ok) throw new Error(`/v1/prs -> ${r.status}`);
      setRows((await r.json()) as PrRow[]);
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
  }, [signedIn]);

  if (!signedIn) {
    return <div className="empty">Sign in to view PRs.</div>;
  }

  if (selected) {
    return (
      <PrDetailView
        repo={selected.repo}
        prNumber={selected.pr_number}
        onBack={() => {
          setSelected(null);
          void refresh();
        }}
      />
    );
  }

  const visibleRows = rows
    ? projectFilter
      ? rows.filter((r) => r.project === projectFilter)
      : rows
    : null;

  return (
    <>
      <h2>
        Agent-opened PRs{visibleRows ? ` (${visibleRows.length})` : ""}
        {projectFilter && (
          <span className="filter-hint"> — filtered to {projectFilter}</span>
        )}
        <button
          type="button"
          className="link"
          onClick={() => void refresh()}
          disabled={loading}
          style={{ marginLeft: "1rem" }}
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
            ? `No agent-opened PRs for ${projectFilter}.`
            : "No agent-opened PRs yet."}
        </div>
      ) : visibleRows ? (
        <table>
          <thead>
            <tr>
              <th>Project</th>
              <th>PR</th>
              <th>Issue</th>
              <th>Run state</th>
              <th>Attempts</th>
              <th>Cost</th>
              <th>Triage</th>
            </tr>
          </thead>
          <tbody>
            {visibleRows.map((row) => (
              <tr
                key={`${row.repo}#${row.pr_number}`}
                className={row.pr_lock_held ? "eligible" : ""}
                onClick={() =>
                  setSelected({ repo: row.repo, pr_number: row.pr_number })
                }
                style={{ cursor: "pointer" }}
              >
                <td>{row.project}</td>
                <td className="mono">
                  {row.repo}#{row.pr_number}
                </td>
                <td className="mono dim">#{row.issue_number}</td>
                <td>
                  <span className={`pill ${runStatePill(row.run_state)}`}>
                    {row.run_state}
                  </span>
                </td>
                <td className="mono dim">{row.run_attempts}</td>
                <td className="mono dim">${row.run_cumulative_cost_usd.toFixed(2)}</td>
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
  if (state === "aborted") return "drain";
  return "dim";
}

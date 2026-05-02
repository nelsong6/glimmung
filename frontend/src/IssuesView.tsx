/**
 * Issues view — lists open issues across all registered repos and lets the
 * authenticated user dispatch an agent run from glimmung directly (no GH
 * label round-trip required). Sources data live from `/v1/issues` per
 * mount; dispatch goes through `/v1/runs/dispatch`.
 *
 * Per #20: labels are *informational only* — surfaced as a row badge but
 * not used to gate dispatch. The dispatch button on each row is the
 * primitive trigger.
 */
import { useEffect, useState } from "react";
import { authedFetch } from "./auth";
import { IssueDetailView } from "./IssueDetailView";

type IssueRow = {
  id: string;
  project: string;
  repo: string | null;
  number: number | null;
  title: string;
  state: string;
  labels: string[];
  html_url: string | null;
  last_run_id: string | null;
  last_run_state: string | null;
  last_run_abort_reason: string | null;
  issue_lock_held: boolean;
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

type DispatchStatus =
  | { kind: "idle" }
  | { kind: "dispatching"; key: string }
  | { kind: "result"; key: string; result: DispatchResult }
  | { kind: "error"; key: string; message: string };

type Selected =
  | { kind: "gh"; repo: string; issue_number: number }
  | { kind: "native"; project: string; issue_id: string }
  | null;

export function IssuesView({
  signedIn,
  projectFilter,
}: {
  signedIn: boolean;
  projectFilter: string | null;
}) {
  const [rows, setRows] = useState<IssueRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [dispatchStatus, setDispatchStatus] = useState<DispatchStatus>({ kind: "idle" });
  const [selected, setSelected] = useState<Selected>(null);

  const refresh = async () => {
    if (!signedIn) {
      setRows(null);
      setError("sign in to view issues");
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const r = await authedFetch("/v1/issues");
      if (!r.ok) throw new Error(`/v1/issues -> ${r.status}`);
      setRows((await r.json()) as IssueRow[]);
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

  const dispatch = async (row: IssueRow) => {
    const key = rowKey(row);
    setDispatchStatus({ kind: "dispatching", key });
    try {
      const r = await authedFetch("/v1/runs/dispatch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ issue_id: row.id, project: row.project }),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`/v1/runs/dispatch -> ${r.status}: ${text}`);
      }
      const result = (await r.json()) as DispatchResult;
      setDispatchStatus({ kind: "result", key, result });
      // Refresh the row's last-run state — fire-and-forget.
      void refresh();
    } catch (e) {
      setDispatchStatus({ kind: "error", key, message: String(e) });
    }
  };

  if (!signedIn) {
    return <div className="empty">Sign in to view issues.</div>;
  }

  if (selected) {
    return (
      <IssueDetailView
        target={selected}
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
        Open issues{visibleRows ? ` (${visibleRows.length})` : ""}
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
            ? `No open issues for ${projectFilter}.`
            : "No open issues across registered repos."}
        </div>
      ) : visibleRows ? (
        <table>
          <thead>
            <tr>
              <th>Project</th>
              <th>Issue</th>
              <th>Title</th>
              <th>Labels</th>
              <th>Last run</th>
              <th>Action</th>
            </tr>
          </thead>
          <tbody>
            {visibleRows.map((row) => {
              const key = rowKey(row);
              const status = dispatchStatus.kind !== "idle" && dispatchStatus.key === key
                ? dispatchStatus
                : null;
              return (
                <tr key={key}>
                  <td>{row.project}</td>
                  <td className="mono">
                    {row.html_url && row.number !== null ? (
                      <a href={row.html_url} target="_blank" rel="noreferrer">#{row.number}</a>
                    ) : (
                      <span className="dim" title="glimmung-native, no GitHub counterpart">native</span>
                    )}
                  </td>
                  <td>
                    <button
                      type="button"
                      className="link"
                      onClick={() => setSelected(
                        row.repo && row.number !== null
                          ? { kind: "gh", repo: row.repo, issue_number: row.number }
                          : { kind: "native", project: row.project, issue_id: row.id }
                      )}
                      style={{ textAlign: "left" }}
                    >
                      {row.title}
                    </button>
                  </td>
                  <td className="mono dim">
                    {row.labels.length === 0 ? "—" : row.labels.join(", ")}
                  </td>
                  <td className="mono dim">
                    {renderLastRun(row)}
                  </td>
                  <td>
                    <button
                      type="button"
                      className="link"
                      onClick={() => void dispatch(row)}
                      disabled={
                        row.issue_lock_held
                        || (status?.kind === "dispatching")
                      }
                    >
                      {row.issue_lock_held
                        ? "in flight"
                        : status?.kind === "dispatching"
                        ? "dispatching…"
                        : "dispatch"}
                    </button>
                    {status?.kind === "result" && (
                      <span className={`pill ${pillClass(status.result.state)}`} style={{ marginLeft: "0.5rem" }}>
                        {status.result.state}
                      </span>
                    )}
                    {status?.kind === "error" && (
                      <span className="pill drain" style={{ marginLeft: "0.5rem" }} title={status.message}>
                        error
                      </span>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      ) : null}
    </>
  );
}

function rowKey(row: IssueRow): string {
  return row.repo && row.number !== null
    ? `${row.repo}#${row.number}`
    : `glimmung/${row.id}`;
}

function renderLastRun(row: IssueRow): string {
  if (!row.last_run_id) return "—";
  if (row.issue_lock_held) return `${row.last_run_state ?? "?"} (in flight)`;
  return `${row.last_run_state ?? "?"}`;
}

function pillClass(state: string): string {
  if (state === "dispatched") return "free";
  if (state === "pending") return "busy";
  if (state === "already_running") return "busy";
  return "drain";
}

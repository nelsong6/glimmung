/**
 * Issues view — lists open issues across all registered repos and lets the
 * authenticated user dispatch an agent run from glimmung directly (no GH
 * label round-trip required). Sources data live from `/v1/issues` per
 * mount; dispatch goes through `/v1/runs/dispatch`.
 *
 * Per #20: labels are *informational only* — surfaced as a row badge but
 * not used to gate dispatch. The dispatch button on each row is the
 * primitive trigger.
 *
 * Clicking an issue title navigates to the project-scoped Glimmung issue
 * route: `/projects/<project>/issues/<number>/summary`.
 */
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { authedFetch } from "./auth";

type IssueRow = {
  id: string;
  project: string;
  workflow: string | null;
  repo: string | null;
  number: number | null;
  title: string;
  state: string;
  labels: string[];
  html_url: string | null;
  last_run_id: string | null;
  last_run_number: number | null;
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

type IssueActionStatus =
  | { kind: "idle" }
  | { kind: "discarding"; key: string }
  | { kind: "error"; key: string; message: string };

export function IssuesView({
  signedIn,
  projectFilter,
  workflowFilter = null,
  headingLabel = "Open issues",
  maxRows = null,
  showProjectColumn = true,
  needsAttentionOnly = false,
}: {
  signedIn: boolean;
  projectFilter: string | null;
  workflowFilter?: string | null;
  headingLabel?: string;
  maxRows?: number | null;
  showProjectColumn?: boolean;
  needsAttentionOnly?: boolean;
}) {
  const navigate = useNavigate();
  const [rows, setRows] = useState<IssueRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [dispatchStatus, setDispatchStatus] = useState<DispatchStatus>({ kind: "idle" });
  const [issueActionStatus, setIssueActionStatus] = useState<IssueActionStatus>({ kind: "idle" });

  const refresh = async () => {
    setLoading(true);
    setError(null);
    try {
      const params = new URLSearchParams();
      if (projectFilter) params.set("project", projectFilter);
      if (workflowFilter) params.set("workflow", workflowFilter);
      if (needsAttentionOnly) params.set("needs_attention", "true");
      const url = params.size > 0 ? `/v1/issues?${params.toString()}` : "/v1/issues";
      const r = await fetch(url);
      if (!r.ok) throw new Error(`${url} -> ${r.status}`);
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
  }, [signedIn, projectFilter, workflowFilter, needsAttentionOnly]);

  const dispatch = async (row: IssueRow) => {
    const key = rowKey(row);
    setDispatchStatus({ kind: "dispatching", key });
    try {
      const r = await authedFetch("/v1/runs/dispatch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          issue_id: row.id,
          project: row.project,
          workflow: workflowFilter ?? row.workflow ?? undefined,
        }),
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

  const openDetail = (row: IssueRow) => {
    if (row.number === null) return;
    navigate(
      `/projects/${encodeURIComponent(row.project)}/issues/${row.number}/summary`
    );
  };

  const discard = async (row: IssueRow) => {
    const key = rowKey(row);
    const reason = window.prompt("Discard reason", "");
    if (reason === null) return;
    setIssueActionStatus({ kind: "discarding", key });
    try {
      const r = await authedFetch(
        `/v1/issues/by-id/${encodeURIComponent(row.project)}/${encodeURIComponent(row.id)}/discard`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ reason }),
        },
      );
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`/v1/issues/by-id/${row.project}/${row.id}/discard -> ${r.status}: ${text}`);
      }
      setIssueActionStatus({ kind: "idle" });
      void refresh();
    } catch (e) {
      setIssueActionStatus({ kind: "error", key, message: String(e) });
    }
  };

  const visibleRows = rows;
  const displayRows = maxRows !== null && visibleRows
    ? visibleRows.slice(0, maxRows)
    : visibleRows;

  return (
    <>
      <h2>
        {headingLabel}{visibleRows ? ` (${visibleRows.length})` : ""}
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
          {needsAttentionOnly
            ? projectFilter
              ? `No issues need attention for ${projectFilter}.`
              : "No issues need attention across registered repos."
            : projectFilter
            ? `No open issues for ${projectFilter}.`
            : "No open issues across registered repos."}
        </div>
      ) : displayRows ? (
        <table>
          <thead>
            <tr>
              {showProjectColumn && <th>Project</th>}
              <th>#</th>
              <th>Title</th>
              <th>Labels</th>
              <th>Why</th>
              <th>Last run</th>
              <th>Action</th>
            </tr>
          </thead>
          <tbody>
            {displayRows.map((row) => {
              const key = rowKey(row);
              const status = dispatchStatus.kind !== "idle" && dispatchStatus.key === key
                ? dispatchStatus
                : null;
              return (
                <tr key={key}>
                  {showProjectColumn && <td>{row.project}</td>}
                  <td className="mono dim">
                    {row.number !== null ? row.number : "—"}
                  </td>
                  <td>
                    <button
                      type="button"
                      className="link"
                      onClick={() => openDetail(row)}
                      style={{ textAlign: "left" }}
                    >
                      {row.title}
                    </button>
                  </td>
                  <td className="mono dim">
                    {row.labels.length === 0 ? "—" : row.labels.join(", ")}
                  </td>
                  <td>
                    <AttentionReason row={row} />
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
                        || !signedIn
                        || (status?.kind === "dispatching")
                      }
                    >
                      {row.issue_lock_held
                        ? "in flight"
                        : !signedIn
                        ? "sign in"
                        : status?.kind === "dispatching"
                        ? "dispatching…"
                        : "dispatch"}
                    </button>
                    <button
                      type="button"
                      className="link"
                      onClick={() => void discard(row)}
                      disabled={
                        row.issue_lock_held
                        || !signedIn
                        || (issueActionStatus.kind === "discarding" && issueActionStatus.key === key)
                      }
                      style={{ marginLeft: "0.75rem" }}
                    >
                      {issueActionStatus.kind === "discarding" && issueActionStatus.key === key
                        ? "discarding…"
                        : "discard"}
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
                    {issueActionStatus.kind === "error" && issueActionStatus.key === key && (
                      <span className="pill drain" style={{ marginLeft: "0.5rem" }} title={issueActionStatus.message}>
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
  return row.number !== null ? `${row.project}#${row.number}` : `glimmung/${row.id}`;
}

function renderLastRun(row: IssueRow): string {
  if (!row.last_run_id) return "—";
  const label = row.last_run_number !== null ? `run ${row.last_run_number}` : row.last_run_state ?? "?";
  if (row.issue_lock_held) return `${label} (in flight)`;
  return label;
}

function AttentionReason({ row }: { row: IssueRow }) {
  const reason = attentionReason(row);
  return (
    <div className="attention-reason" title={reason.detail ?? reason.label}>
      <span className={`pill ${reason.kind}`}>{reason.label}</span>
      {reason.detail && <div className="attention-detail">{reason.detail}</div>}
    </div>
  );
}

function attentionReason(row: IssueRow): { label: string; detail: string | null; kind: string } {
  if (row.issue_lock_held) {
    return {
      label: "run in flight",
      detail: row.last_run_number !== null ? `run ${row.last_run_number} is still holding the issue lock` : null,
      kind: "busy",
    };
  }
  if (!row.last_run_id) {
    return {
      label: "not dispatched",
      detail: "open issue has not had an agent run yet",
      kind: "info",
    };
  }
  if (row.last_run_state === "aborted" || row.last_run_state === "failed") {
    return {
      label: "last run failed",
      detail: row.last_run_abort_reason,
      kind: "drain",
    };
  }
  if (row.last_run_state === "in_progress" || row.last_run_state === "pending") {
    return {
      label: "run still active",
      detail: row.last_run_number !== null ? `run ${row.last_run_number} is ${row.last_run_state}` : null,
      kind: "busy",
    };
  }
  if (row.last_run_state === "passed") {
    return {
      label: "touchpoint ready",
      detail: "agent run passed and is ready for touchpoint review",
      kind: "free",
    };
  }
  if (row.last_run_state === "needs_review" || row.last_run_state === "review_required") {
    return {
      label: "review needed",
      detail: null,
      kind: "busy",
    };
  }
  return {
    label: row.last_run_state ? `last run ${row.last_run_state}` : "open issue",
    detail: null,
    kind: "info",
  };
}

function pillClass(state: string): string {
  if (state === "dispatched") return "free";
  if (state === "pending") return "busy";
  if (state === "already_running") return "busy";
  return "drain";
}

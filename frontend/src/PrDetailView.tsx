/**
 * PR detail view — reject-with-feedback action lives here.
 *
 * Post-#50: PR meta (title/body/state) sourced from the glimmung `prs`
 * container; runtime fields (run_state, attempt history) come from the
 * linked Run when one exists. Submit POSTs a `glimmung_ui` reject
 * signal; the drain loop processes it through the triage decision
 * engine and (if budget allows) dispatches the triage workflow with
 * the feedback as context.
 */
import { useEffect, useState } from "react";
import { authedFetch } from "./auth";

type PrDetail = {
  id: string;
  project: string;
  repo: string;
  pr_number: number;
  pr_branch: string | null;
  title: string;
  body: string;
  state: string;
  merged: boolean;
  base_ref: string;
  head_sha: string;
  html_url: string | null;
  linked_issue_id: string | null;
  linked_run_id: string | null;
  issue_number: number | null;
  issue_title: string | null;
  run_id: string | null;
  run_state: string | null;
  run_attempts: number;
  run_cumulative_cost_usd: number;
  run_attempt_history: AttemptHistoryEntry[];
  comments: unknown[];
  reviews: unknown[];
  pr_lock_held: boolean;
};

type AttemptHistoryEntry = {
  attempt_index: number;
  phase: string;
  workflow_filename: string;
  workflow_run_id: number | null;
  dispatched_at: string;
  completed_at: string | null;
  verification_status: string | null;
  decision: string | null;
};

type RejectStatus =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "submitted"; signalId: string }
  | { kind: "error"; message: string };

export function PrDetailView({
  repo,
  prNumber,
  onBack,
}: {
  repo: string;
  prNumber: number;
  onBack: () => void;
}) {
  const [detail, setDetail] = useState<PrDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [feedback, setFeedback] = useState("");
  const [reject, setReject] = useState<RejectStatus>({ kind: "idle" });

  const refresh = async () => {
    setError(null);
    try {
      const r = await authedFetch(`/v1/prs/${repo}/${prNumber}`);
      if (!r.ok) throw new Error(`/v1/prs/${repo}/${prNumber} -> ${r.status}`);
      setDetail((await r.json()) as PrDetail);
    } catch (e) {
      setError(String(e));
    }
  };

  useEffect(() => {
    void refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo, prNumber]);

  const submit = async () => {
    if (!feedback.trim()) return;
    setReject({ kind: "submitting" });
    try {
      const r = await authedFetch("/v1/signals", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          target_type: "pr",
          target_repo: repo,
          target_id: String(prNumber),
          source: "glimmung_ui",
          payload: { kind: "reject", feedback: feedback.trim() },
        }),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`/v1/signals -> ${r.status}: ${text}`);
      }
      const sig = await r.json();
      setReject({ kind: "submitted", signalId: sig.id });
      setFeedback("");
      // Poll the detail to reflect the new attempt once the drain
      // processes the signal. Fire-and-forget; user can refresh
      // manually too.
      setTimeout(() => void refresh(), 3000);
    } catch (e) {
      setReject({ kind: "error", message: String(e) });
    }
  };

  return (
    <>
      <h2>
        <button type="button" className="link" onClick={onBack} style={{ marginRight: "1rem" }}>
          ← back
        </button>
        {repo}#{prNumber}
      </h2>
      {error && <div className="empty error">{error}</div>}
      {detail === null && !error ? (
        <div className="empty">Loading…</div>
      ) : detail ? (
        <>
          <div className="project-info">
            <div className="row">
              <span className="key">title</span>
              <span className="val">
                {detail.html_url ? (
                  <a href={detail.html_url} target="_blank" rel="noreferrer">
                    {detail.title || `(no title)`}
                  </a>
                ) : (
                  detail.title || "(no title)"
                )}
              </span>
            </div>
            <div className="row">
              <span className="key">state</span>
              <span className="val mono">
                {detail.merged ? "merged" : detail.state}
              </span>
            </div>
            <div className="row">
              <span className="key">issue</span>
              <span className="val mono">
                {detail.issue_number !== null
                  ? `#${detail.issue_number}${detail.issue_title ? ` — ${detail.issue_title}` : ""}`
                  : "—"}
              </span>
            </div>
            <div className="row">
              <span className="key">branch</span>
              <span className="val mono">
                {detail.pr_branch ?? "—"}{detail.base_ref ? ` → ${detail.base_ref}` : ""}
              </span>
            </div>
            <div className="row">
              <span className="key">run state</span>
              <span className="val">
                {detail.run_state ? (
                  <span className={`pill ${runStatePill(detail.run_state)}`}>{detail.run_state}</span>
                ) : (
                  <span className="dim">no agent run</span>
                )}
                {detail.pr_lock_held && (
                  <span className="pill busy" style={{ marginLeft: "0.5rem" }}>triage in flight</span>
                )}
              </span>
            </div>
            {detail.run_state && (
              <div className="row">
                <span className="key">attempts</span>
                <span className="val mono">
                  {detail.run_attempts} • ${detail.run_cumulative_cost_usd.toFixed(2)} cumulative
                </span>
              </div>
            )}
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

          <h2>Attempt history</h2>
          {detail.run_attempt_history.length === 0 ? (
            <div className="empty">No attempts yet.</div>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>#</th>
                  <th>Phase</th>
                  <th>Workflow</th>
                  <th>Verification</th>
                  <th>Decision</th>
                  <th>Dispatched</th>
                </tr>
              </thead>
              <tbody>
                {detail.run_attempt_history.map((a) => (
                  <tr key={a.attempt_index}>
                    <td className="mono">{a.attempt_index}</td>
                    <td>{a.phase}</td>
                    <td className="mono dim">{a.workflow_filename}</td>
                    <td className="mono dim">{a.verification_status ?? "—"}</td>
                    <td className="mono dim">{a.decision ?? "—"}</td>
                    <td className="mono dim">{a.dispatched_at}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}

          <h2>Reject with feedback</h2>
          <p className="dim">
            Glimmung will dispatch the triage workflow with this feedback as context.
            Subject to the run&apos;s budget. Subsequent rejects on this PR queue cleanly
            — they wait for any in-flight triage to complete.
          </p>
          <textarea
            value={feedback}
            onChange={(e) => setFeedback(e.target.value)}
            placeholder="What needs to change? e.g. 'the date format on the dashboard is wrong, should be ISO-8601'"
            rows={6}
            style={{ width: "100%", fontFamily: "inherit", padding: "0.5rem" }}
            disabled={detail.pr_lock_held || reject.kind === "submitting"}
          />
          <div style={{ marginTop: "0.5rem" }}>
            <button
              type="button"
              className="link"
              onClick={() => void submit()}
              disabled={
                !feedback.trim()
                || detail.pr_lock_held
                || reject.kind === "submitting"
              }
            >
              {reject.kind === "submitting" ? "submitting…" : "submit reject"}
            </button>
            {reject.kind === "submitted" && (
              <span className="pill free" style={{ marginLeft: "0.5rem" }}>
                queued (signal {reject.signalId.slice(0, 8)}…)
              </span>
            )}
            {reject.kind === "error" && (
              <span className="pill drain" style={{ marginLeft: "0.5rem" }} title={reject.message}>
                error
              </span>
            )}
          </div>
        </>
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

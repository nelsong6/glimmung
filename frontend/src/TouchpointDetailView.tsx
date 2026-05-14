/**
 * Touchpoint detail view — reject-with-feedback action lives here.
 *
 * Touchpoint meta comes from Glimmung; GitHub PR coordinates are syndication
 * metadata when present. Runtime fields come from the linked Run.
 *
 * PR-coordinate touchpoints redirect to the canonical issue workspace when a
 * linked Glimmung Issue exists: `/projects/<project>/issues/<n>/touchpoint`.
 */
import { useEffect, useState } from "react";
import { useNavigate, useOutletContext, useParams } from "react-router-dom";
import { authedFetch, publicConfig } from "./auth";

type TouchpointDetail = {
  ref: string;
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
  linked_issue_ref: string | null;
  linked_run_ref: string | null;
  issue_number: number | null;
  issue_title: string | null;
  run_ref: string | null;
  run_state: string | null;
  validation_url: string | null;
  screenshots_markdown: string | null;
  session_launch_url: string | null;
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
  dispatched_at: string;
  completed_at: string | null;
  conclusion: string | null;
  verification_status: string | null;
  evidence_refs: string[];
  summary_markdown: string | null;
  log_archive_url: string | null;
  decision: string | null;
};

type RejectStatus =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "submitted"; signalId: string }
  | { kind: "error"; message: string };

type AuthContext = {
  signedIn: boolean;
  isAdmin: boolean;
};

type TouchpointDetailRouteParams = {
  owner?: string;
  repo?: string;
  n?: string;
};

export function TouchpointDetailView() {
  const navigate = useNavigate();
  const params = useParams<TouchpointDetailRouteParams>();
  const { signedIn, isAdmin } = useOutletContext<AuthContext>();
  const repo = `${params.owner ?? ""}/${params.repo ?? ""}`;
  const prNumber = parseInt(params.n ?? "0", 10);

  const [detail, setDetail] = useState<TouchpointDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [feedback, setFeedback] = useState("");
  const [reject, setReject] = useState<RejectStatus>({ kind: "idle" });
  const [tankBaseUrl, setTankBaseUrl] = useState("");
  const latestSummary = latestAttemptSummary(detail);

  const onBack = () => navigate("/touchpoints");

  const refresh = async () => {
    setError(null);
    try {
      const r = await fetch(`/v1/touchpoints/${repo}/${prNumber}`);
      if (!r.ok) throw new Error(`/v1/touchpoints/${repo}/${prNumber} -> ${r.status}`);
      setDetail((await r.json()) as TouchpointDetail);
    } catch (e) {
      setError(String(e));
    }
  };

  useEffect(() => {
    void refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo, prNumber]);

  useEffect(() => {
    publicConfig()
      .then((cfg) => setTankBaseUrl(cfg.tank_operator_base_url || ""))
      .catch((e) => setError(String(e)));
  }, []);

  const launchSession = () => {
    if (!detail?.run_ref || !detail.linked_issue_ref) return;
    if (detail.session_launch_url) {
      window.open(detail.session_launch_url, "_blank", "noopener,noreferrer");
      return;
    }
    if (!tankBaseUrl) return;
    const url = new URL(tankBaseUrl);
    url.searchParams.set("glimmung_run_ref", detail.run_ref);
    url.searchParams.set("glimmung_issue_ref", detail.linked_issue_ref);
    url.searchParams.set("glimmung_touchpoint_ref", detail.ref);
    if (detail.validation_url) {
      url.searchParams.set("validation_url", detail.validation_url);
    }
    window.open(url.toString(), "_blank", "noopener,noreferrer");
  };

  const submit = async () => {
    if (!feedback.trim() || !detail) return;
    setReject({ kind: "submitting" });
    try {
      // Public signal shape: target_repo is the GitHub repo and target_ref is
      // the PR number. Glimmung stays canonical, but PR signals use GitHub
      // coordinates because the drain resolves the linked run from PR state.
      const r = await authedFetch("/v1/signals", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          target_type: "pr",
          target_repo: detail.repo,
          target_ref: String(detail.pr_number),
          source: "glimmung_ui",
          payload: { kind: "reject", feedback: feedback.trim() },
        }),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`/v1/signals -> ${r.status}: ${text}`);
      }
      const sig = await r.json();
      setReject({ kind: "submitted", signalId: sig.ref });
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
            <div className="row">
              <span className="key">Tank session</span>
              <span className="val">
                <button
                  type="button"
                  className="link"
                  onClick={launchSession}
                  disabled={
                    !detail.run_ref
                    || !detail.linked_issue_ref
                    || (!detail.session_launch_url && !tankBaseUrl)
                  }
                  title={
                    detail.run_ref && detail.linked_issue_ref
                      ? "Start a Tank session with this Glimmung context"
                      : "Requires a linked glimmung run and issue"
                  }
                >
                  start Tank session
                </button>
              </span>
            </div>
          </div>

          <h2>Review evidence</h2>
          <div className="project-info">
            {latestSummary && (
              <div className="row">
                <span className="key">summary</span>
                <span className="val">
                  <pre className="evidence-notes">{latestSummary}</pre>
                </span>
              </div>
            )}
            <div className="row">
              <span className="key">test env</span>
              <span className="val">
                {detail.validation_url ? (
                  <a href={detail.validation_url} target="_blank" rel="noreferrer">
                    {detail.validation_url}
                  </a>
                ) : (
                  <span className="dim">No validation URL was recorded for this run.</span>
                )}
              </span>
            </div>
            <div className="row">
              <span className="key">session URL</span>
              <span className="val">
                {detail.session_launch_url ? (
                  <a href={detail.session_launch_url} target="_blank" rel="noreferrer">
                    {detail.session_launch_url}
                  </a>
                ) : (
                  <span className="dim">No session URL available.</span>
                )}
              </span>
            </div>
            {detail.screenshots_markdown?.trim() ? (
              <div className="row">
                <span className="key">screenshots</span>
                <span className="val">
                  <ScreenshotEvidence markdown={detail.screenshots_markdown} />
                </span>
              </div>
            ) : null}
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
                  <th>Conclusion</th>
                  <th>Verification</th>
                  <th>Summary</th>
                  <th>Evidence</th>
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
                    <td className="mono dim">{a.conclusion ?? "—"}</td>
                    <td className="mono dim">{a.verification_status ?? "—"}</td>
                    <td className="mono dim">{a.summary_markdown?.trim() ? "recorded" : "—"}</td>
                    <td className="mono dim">
                      <EvidenceLinks attempt={a} />
                    </td>
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
            Subject to the run&apos;s budget. Subsequent rejects on this touchpoint queue cleanly
            — they wait for any in-flight triage to complete.
          </p>
          <textarea
            value={feedback}
            onChange={(e) => setFeedback(e.target.value)}
            placeholder="What needs to change? e.g. 'the date format on the dashboard is wrong, should be ISO-8601'"
            rows={6}
            style={{ width: "100%", fontFamily: "inherit", padding: "0.5rem" }}
            disabled={detail.pr_lock_held || reject.kind === "submitting" || !signedIn || !isAdmin}
          />
          <div style={{ marginTop: "0.5rem" }}>
            <button
              type="button"
              className="link"
              onClick={() => void submit()}
              disabled={
                !feedback.trim()
                || !signedIn
                || !isAdmin
                || detail.pr_lock_held
                || reject.kind === "submitting"
              }
            >
              {reject.kind === "submitting" ? "submitting…" : !signedIn ? "sign in" : !isAdmin ? "admin only" : "submit reject"}
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
  if (state === "pending") return "pending";
  if (state === "review_required") return "info";
  if (state === "aborted") return "drain";
  return "dim";
}

function latestAttemptSummary(detail: TouchpointDetail | null): string | null {
  if (!detail) return null;
  for (let i = detail.run_attempt_history.length - 1; i >= 0; i -= 1) {
    const summary = detail.run_attempt_history[i].summary_markdown?.trim();
    if (summary) return summary;
  }
  return null;
}

function ScreenshotEvidence({ markdown }: { markdown: string }) {
  const shots = screenshotLinks(markdown);
  if (shots.length === 0) {
    return <pre className="evidence-notes">{markdown}</pre>;
  }
  return (
    <div className="evidence-gallery">
      {shots.map((shot) => (
        <a key={shot.url} className="evidence-shot" href={shot.url} target="_blank" rel="noreferrer">
          <img src={shot.url} alt={shot.label} loading="lazy" />
          <span>{shot.label}</span>
        </a>
      ))}
    </div>
  );
}

function EvidenceLinks({ attempt }: { attempt: AttemptHistoryEntry }) {
  const links = [
    ...attempt.evidence_refs.map((ref) => ({
      href: evidenceHref(ref),
      label: evidenceLabel(ref),
    })),
    ...(attempt.log_archive_url
      ? [{ href: evidenceHref(attempt.log_archive_url), label: "native events" }]
      : []),
  ];
  if (links.length === 0) return <>—</>;
  return (
    <span className="evidence-list">
      {links.map((link) => (
        <a key={`${link.label}:${link.href}`} href={link.href} target="_blank" rel="noreferrer">
          {link.label}
        </a>
      ))}
    </span>
  );
}

function screenshotLinks(markdown: string): { label: string; url: string }[] {
  return [...markdown.matchAll(/!\[([^\]]*)\]\(([^)]+)\)/g)].map((match) => ({
    label: match[1] || "screenshot",
    url: match[2],
  }));
}

function evidenceHref(ref: string): string {
  if (ref.startsWith("blob://artifacts/")) {
    return `/v1/artifacts/${ref.slice("blob://artifacts/".length)}`;
  }
  if (ref.startsWith("/v1/artifacts/") || /^https?:\/\//.test(ref)) {
    return ref;
  }
  return ref;
}

function evidenceLabel(ref: string): string {
  const clean = ref.split(/[?#]/, 1)[0].replace(/\/$/, "");
  return clean.split("/").pop() || ref;
}

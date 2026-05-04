/**
 * Issue detail view (#42, #81) — issue meta + tabbed content.
 *
 * Tabs (#81): issue / the run / runs.
 *   - issue: title link, body, edit form (was "description")
 *   - the run: workflow's DAG painted with active run state. Phases
 *     as nodes, PR primitive as the trailing node. Cool-toned
 *     definition view when no run is in flight; nodes color in by
 *     state when one is. Click a node to drill into the latest
 *     attempt that exercised it.
 *   - runs: list/timeline of every run on this issue. Click a row
 *     to load that run in the run tab. Replaces the old SVG lineage.
 *
 * Conceptual move per #81: "list of steps that ran" → "graph that
 * runs". Today the DAG is `[phase] → [pr]` (single-phase v1, per
 * #69) — boring but honest, and the layout extends cleanly when
 * multi-phase orchestration lands.
 *
 * Backwards-compat: old slugs (description / in-progress / lineage)
 * still resolve so deep links from before #81 don't 404.
 *
 * Routed via `/issues/<owner>/<repo>/<n>` (GH-anchored) or
 * `/issues/<project>/<issueId>` (native). Target is derived from the
 * URL params so deep-link reloads land directly here.
 */
import { Fragment, useEffect, useMemo, useState } from "react";
import { useLocation, useNavigate, useOutletContext, useParams } from "react-router-dom";
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
  comments: IssueComment[];
  last_run_id: string | null;
  last_run_state: string | null;
  issue_lock_held: boolean;
};

type IssueComment = {
  id: string;
  author: string;
  body: string;
  created_at: string;
  updated_at: string;
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

type IssueGraph = {
  issue_id: string;
  nodes: GraphNode[];
  edges: Array<{
    source: string;
    target: string;
    kind:
      | "spawned"
      | "attempted"
      | "retried"
      | "opened"
      | "feedback"
      | "re_dispatched"
      // Resume primitive (#111) — links a prior Run to a Run that
      // resumed from it (cloned_from_run_id). Renders in the runs
      // table + run-meta panel as "resumed from Run X".
      | "resumed_from";
  }>;
};

type NativeRunEvent = {
  id: string;
  project: string;
  run_id: string;
  attempt_index: number;
  phase: string;
  job_id: string;
  seq: number;
  event: "step_started" | "log" | "step_completed" | "step_skipped" | "step_failed";
  step_slug: string;
  message: string;
  exit_code: number | null;
  metadata: Record<string, unknown>;
  created_at: string;
};

type NativeRunEventsResponse = {
  project: string;
  run_id: string;
  attempt_index: number | null;
  job_id: string | null;
  events: NativeRunEvent[];
  archive_url: string | null;
};

type WorkflowGraphMeta = {
  phases: string[];
  default_entry: { target: string; active: boolean; kind: string } | null;
  recycle_arrows: RecycleArrow[];
  terminal: { kind: string; enabled: boolean };
};

type RecycleArrow = {
  source: string;
  target: string;
  trigger: string;
  max_attempts: number;
  active: boolean;
  kind: "phase_recycle" | "report_recycle";
};

type NativeAttemptJob = {
  job_id: string;
  name?: string | null;
  state?: string | null;
  steps: NativeAttemptStep[];
};

type NativeAttemptStep = {
  slug: string;
  title?: string | null;
  state?: string | null;
  message?: string | null;
  exit_code?: number | null;
};

type DispatchState =
  | { kind: "idle" }
  | { kind: "dispatching" }
  | { kind: "error"; message: string };

type AbortState =
  | { kind: "idle" }
  | { kind: "armed" }       // first click on `abort` — show `abort?` / `keep`
  | { kind: "aborting" }
  | { kind: "error"; message: string };

type AuthContext = {
  signedIn: boolean;
};

type Tab = "issue" | "the_run" | "runs";

const TAB_SLUGS: Record<Tab, string> = {
  issue: "issue",
  the_run: "the-run",
  runs: "runs",
};

// Backwards-compat: old description / in-progress / lineage slugs still
// resolve so links and bookmarks from before #81 keep working.
const SLUG_TO_TAB: Record<string, Tab> = {
  issue: "issue",
  description: "issue",
  "the-run": "the_run",
  "in-progress": "the_run",
  runs: "runs",
  lineage: "runs",
};

const POLL_INTERVAL_MS = 3000;

type IssueDetailRouteParams = {
  owner?: string;
  repo?: string;
  n?: string;
  project?: string;
  issueId?: string;
};

export function IssueDetailView() {
  const navigate = useNavigate();
  const location = useLocation();
  const params = useParams<IssueDetailRouteParams>();
  const { signedIn } = useOutletContext<AuthContext>();

  // Two route shapes land here. GH-anchored has 3 segments after /issues
  // (`:owner/:repo/:n`); native has 2 (`:project/:issueId`). React-router
  // only fills params for the matched route, so `params.n` being set
  // disambiguates.
  const target: IssueDetailTarget = params.n
    ? {
        kind: "gh",
        repo: `${params.owner ?? ""}/${params.repo ?? ""}`,
        issue_number: parseInt(params.n, 10),
      }
    : {
        kind: "native",
        project: params.project ?? "",
        issue_id: params.issueId ?? "",
      };

  const baseUrl =
    target.kind === "gh"
      ? `/issues/${target.repo}/${target.issue_number}`
      : `/issues/${encodeURIComponent(target.project)}/${encodeURIComponent(target.issue_id)}`;

  // Tab is URL-driven so each tab is deep-linkable. Bare `/issues/...`
  // (no tab segment) falls back to the issue tab.
  const lastSeg = location.pathname.split("/").filter(Boolean).pop() ?? "";
  const tab: Tab = SLUG_TO_TAB[lastSeg] ?? "issue";
  const setTab = (t: Tab) => navigate(`${baseUrl}/${TAB_SLUGS[t]}`);

  const [detail, setDetail] = useState<IssueDetail | null>(null);
  const [graph, setGraph] = useState<IssueGraph | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [refreshTick, setRefreshTick] = useState(0);
  const [dispatchState, setDispatchState] = useState<DispatchState>({ kind: "idle" });
  const [abortState, setAbortState] = useState<AbortState>({ kind: "idle" });
  // Which Run the "the run" tab paints. null → fall back to active or
  // most recent. The Runs tab sets this when a row is clicked, then
  // jumps to the run tab. Not URL-encoded yet; deep-linking a specific
  // run gets a follow-up.
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null);

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
  const repoForLinks = target.kind === "gh" ? target.repo : null;

  const onBack = () => navigate("/issues");

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      setError(null);
      try {
        const requests: Promise<Response>[] = [fetch(detailUrl)];
        if (graphUrl) requests.push(fetch(graphUrl));
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

  // While the run tab is open and a run is actually in flight, poll
  // detail+graph so DAG nodes fill in as conclusions / verification /
  // decisions land server-side.
  const isInFlight = !!(detail && (detail.issue_lock_held || detail.last_run_state === "in_progress"));

  const onRedispatch = async () => {
    if (!detail) return;
    setDispatchState({ kind: "dispatching" });
    try {
      const r = await authedFetch("/v1/runs/dispatch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ issue_id: detail.id, project: detail.project }),
      });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`/v1/runs/dispatch -> ${r.status}: ${text}`);
      }
      setDispatchState({ kind: "idle" });
      setRefreshTick((t) => t + 1);
    } catch (e) {
      setDispatchState({ kind: "error", message: String(e) });
    }
  };

  const onAbort = async (runId: string) => {
    if (!detail) return;
    setAbortState({ kind: "aborting" });
    try {
      const url = `/v1/runs/${encodeURIComponent(detail.project)}/${encodeURIComponent(runId)}/abort?reason=aborted_via_dashboard`;
      const r = await authedFetch(url, { method: "POST" });
      if (!r.ok) {
        const text = await r.text();
        throw new Error(`${url} -> ${r.status}: ${text}`);
      }
      setAbortState({ kind: "idle" });
      // Refresh immediately so the in-flight pill flips to aborted and
      // the abort button drops out before the next poll tick lands.
      setRefreshTick((t) => t + 1);
    } catch (e) {
      setAbortState({ kind: "error", message: String(e) });
    }
  };
  useEffect(() => {
    if (tab !== "the_run") return;
    if (!isInFlight) return;
    const id = setInterval(() => setRefreshTick((t) => t + 1), POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [tab, isInFlight]);

  return (
    <>
      <h2>
        <button type="button" className="link" onClick={onBack} style={{ marginRight: "1rem" }}>
          ← back
        </button>
        {heading}
      </h2>
      {error && <div className="empty error">{error}</div>}
      {detail === null && !error ? (
        <div className="empty">Loading…</div>
      ) : detail ? (
        <>
          <IssueHeader detail={detail} />

          <div className="tabs" role="tablist">
            <TabButton current={tab} value="issue" onSelect={setTab}>
              issue
            </TabButton>
            <TabButton current={tab} value="the_run" onSelect={setTab}>
              the run
              {isInFlight && <span className="tab-dot" aria-label="active" />}
            </TabButton>
            <TabButton current={tab} value="runs" onSelect={setTab}>
              runs
            </TabButton>
          </div>

          <div className="tab-panel">
            {tab === "issue" && (
              <DescriptionTab
                detail={detail}
                signedIn={signedIn}
                editing={editing}
                onEdit={() => setEditing(true)}
                onCancelEdit={() => setEditing(false)}
                onSaved={() => {
                  setEditing(false);
                  setRefreshTick((t) => t + 1);
                }}
                onCommentChanged={() => setRefreshTick((t) => t + 1)}
              />
            )}
            {tab === "the_run" && (
              <TheRunTab
                graph={graph}
                graphAvailable={!!graphUrl}
                signedIn={signedIn}
                project={detail.project}
                repo={repoForLinks}
                inFlight={isInFlight}
                dispatchState={dispatchState}
                onRedispatch={() => void onRedispatch()}
                abortState={abortState}
                onArmAbort={() => setAbortState({ kind: "armed" })}
                onCancelAbort={() => setAbortState({ kind: "idle" })}
                onConfirmAbort={(runId) => void onAbort(runId)}
                selectedRunId={selectedRunId}
                onSelectRun={setSelectedRunId}
              />
            )}
            {tab === "runs" && (
              <RunsTab
                graph={graph}
                graphAvailable={!!graphUrl}
                onPickRun={(runId) => {
                  setSelectedRunId(runId);
                  setTab("the_run");
                }}
              />
            )}
          </div>
        </>
      ) : null}
    </>
  );
}

function IssueHeader({ detail }: { detail: IssueDetail }) {
  return (
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
  );
}

function TabButton({
  current,
  value,
  onSelect,
  children,
}: {
  current: Tab;
  value: Tab;
  onSelect: (t: Tab) => void;
  children: React.ReactNode;
}) {
  const selected = current === value;
  return (
    <button
      type="button"
      role="tab"
      aria-selected={selected}
      className={`tab${selected ? " selected" : ""}`}
      onClick={() => onSelect(value)}
    >
      {children}
    </button>
  );
}

function DescriptionTab({
  detail,
  signedIn,
  editing,
  onEdit,
  onCancelEdit,
  onSaved,
  onCommentChanged,
}: {
  detail: IssueDetail;
  signedIn: boolean;
  editing: boolean;
  onEdit: () => void;
  onCancelEdit: () => void;
  onSaved: () => void;
  onCommentChanged: () => void;
}) {
  if (editing && signedIn) {
    return <IssueEditForm detail={detail} onCancel={onCancelEdit} onSaved={onSaved} />;
  }
  return (
    <>
      {signedIn && (
        <div style={{ display: "flex", justifyContent: "flex-end", marginBottom: "0.5rem" }}>
          <button type="button" className="link" onClick={onEdit}>
            edit
          </button>
        </div>
      )}
      {detail.body.trim() ? (
        <pre
          style={{
            whiteSpace: "pre-wrap",
            fontFamily: "inherit",
            background: "#0a0a0c",
            padding: "0.75rem 1rem",
            border: "1px solid #2a2a2e",
            borderRadius: "4px",
            margin: 0,
          }}
        >
          {detail.body}
        </pre>
      ) : (
        <div className="empty dim">(no description)</div>
      )}
      <IssueComments detail={detail} signedIn={signedIn} onChanged={onCommentChanged} />
    </>
  );
}

function IssueComments({
  detail,
  signedIn,
  onChanged,
}: {
  detail: IssueDetail;
  signedIn: boolean;
  onChanged: () => void;
}) {
  const [body, setBody] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editingBody, setEditingBody] = useState("");
  const [deletingId, setDeletingId] = useState<string | null>(null);

  const commentsUrl = `/v1/issues/by-id/${encodeURIComponent(detail.project)}/${encodeURIComponent(detail.id)}/comments`;

  const postComment = async (e: React.FormEvent) => {
    e.preventDefault();
    const text = body.trim();
    if (!text) return;
    setBusy(true);
    setError(null);
    try {
      const r = await authedFetch(commentsUrl, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ body: text }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setBody("");
      onChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const saveEdit = async (commentId: string) => {
    const text = editingBody.trim();
    if (!text) return;
    setBusy(true);
    setError(null);
    try {
      const r = await authedFetch(`${commentsUrl}/${encodeURIComponent(commentId)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ body: text }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setEditingId(null);
      setEditingBody("");
      onChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const deleteComment = async (commentId: string) => {
    setBusy(true);
    setError(null);
    try {
      const r = await authedFetch(`${commentsUrl}/${encodeURIComponent(commentId)}`, {
        method: "DELETE",
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setDeletingId(null);
      onChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="issue-comments">
      <h2>comments</h2>
      {detail.comments.length === 0 ? (
        <div className="empty dim">No comments yet.</div>
      ) : (
        <div className="comment-thread">
          {detail.comments.map((comment) => {
            const isEditing = editingId === comment.id;
            const isDeleting = deletingId === comment.id;
            return (
              <article className="comment-row" key={comment.id}>
                <div className="comment-meta">
                  <span className="mono">{comment.author}</span>
                  <span className="dim">{formatTimestamp(comment.updated_at)}</span>
                </div>
                {isEditing ? (
                  <div className="comment-edit">
                    <textarea
                      value={editingBody}
                      onChange={(e) => setEditingBody(e.target.value)}
                      rows={4}
                      disabled={busy}
                    />
                    <div className="comment-actions">
                      <button
                        type="button"
                        className="link"
                        onClick={() => void saveEdit(comment.id)}
                        disabled={busy || !editingBody.trim()}
                      >
                        save
                      </button>
                      <span className="sep">/</span>
                      <button
                        type="button"
                        className="link"
                        onClick={() => {
                          setEditingId(null);
                          setEditingBody("");
                        }}
                        disabled={busy}
                      >
                        cancel
                      </button>
                    </div>
                  </div>
                ) : (
                  <pre className="comment-body">{comment.body}</pre>
                )}
                {signedIn && !isEditing && (
                  <div className="comment-actions">
                    <button
                      type="button"
                      className="link"
                      onClick={() => {
                        setEditingId(comment.id);
                        setEditingBody(comment.body);
                        setDeletingId(null);
                      }}
                      disabled={busy}
                    >
                      edit
                    </button>
                    <span className="sep">/</span>
                    {isDeleting ? (
                      <>
                        <button
                          type="button"
                          className="link danger-text"
                          onClick={() => void deleteComment(comment.id)}
                          disabled={busy}
                        >
                          delete?
                        </button>
                        <span className="sep">/</span>
                        <button
                          type="button"
                          className="link"
                          onClick={() => setDeletingId(null)}
                          disabled={busy}
                        >
                          keep
                        </button>
                      </>
                    ) : (
                      <button
                        type="button"
                        className="link danger-text"
                        onClick={() => {
                          setDeletingId(comment.id);
                          setEditingId(null);
                        }}
                        disabled={busy}
                      >
                        delete
                      </button>
                    )}
                  </div>
                )}
              </article>
            );
          })}
        </div>
      )}
      {signedIn && (
        <form className="admin-form comment-form" onSubmit={postComment}>
          <label>
            <span>comment</span>
            <textarea
              value={body}
              onChange={(e) => setBody(e.target.value)}
              rows={4}
              disabled={busy}
            />
          </label>
          {error && <div className="error">{error}</div>}
          <button type="submit" disabled={busy || !body.trim()}>
            {busy ? "posting…" : "comment"}
          </button>
        </form>
      )}
    </section>
  );
}

function TheRunTab({
  graph,
  graphAvailable,
  signedIn,
  project,
  repo,
  inFlight,
  dispatchState,
  onRedispatch,
  abortState,
  onArmAbort,
  onCancelAbort,
  onConfirmAbort,
  selectedRunId,
  onSelectRun,
}: {
  graph: IssueGraph | null;
  graphAvailable: boolean;
  signedIn: boolean;
  project: string;
  repo: string | null;
  inFlight: boolean;
  dispatchState: DispatchState;
  onRedispatch: () => void;
  abortState: AbortState;
  onArmAbort: () => void;
  onCancelAbort: () => void;
  onConfirmAbort: (runId: string) => void;
  selectedRunId: string | null;
  onSelectRun: (runId: string | null) => void;
}) {
  // Pick the run we're painting. Caller-selected wins; fall back to
  // active, then most recent. `null` only when there are no runs at
  // all on this issue.
  const focused = useMemo(() => {
    if (!graph) return null;
    if (selectedRunId) {
      const node = graph.nodes.find(
        (n) => n.kind === "run" && runIdFromNode(n) === selectedRunId,
      );
      if (node) return node;
    }
    return findActiveRun(graph) ?? findLastCompletedRun(graph);
  }, [graph, selectedRunId]);

  // Drill-in panel: which DAG node the user clicked. Reset when the
  // focused run changes so we don't carry a stale selection.
  const [drillNodeId, setDrillNodeId] = useState<string | null>(null);
  useEffect(() => {
    setDrillNodeId(null);
  }, [focused?.id]);

  if (!graphAvailable) {
    return (
      <div className="empty">
        Run details aren't available for native issues yet.
      </div>
    );
  }
  if (!graph) {
    return <div className="empty">Loading run state…</div>;
  }

  const dispatching = dispatchState.kind === "dispatching";
  const dispatchDisabled = inFlight || dispatching || !signedIn;
  // Abort button shows only when an actual run record exists in flight.
  // Lock-only state (issue_lock_held but no run yet) doesn't have a run
  // id to target; that case stays as "Run lock held — waiting…".
  // Always targets the currently in-flight run, even when a different
  // historical run is selected for viewing in the run tab.
  const activeRun = findActiveRun(graph);
  const abortableRunId = activeRun ? runIdFromNode(activeRun) : null;
  const aborting = abortState.kind === "aborting";
  const armed = abortState.kind === "armed";
  const actions = (
    <div
      className="run-actions"
      style={{ display: "flex", alignItems: "center", gap: "0.75rem", flexWrap: "wrap" }}
    >
      <button
        type="button"
        className="link"
        onClick={onRedispatch}
        disabled={dispatchDisabled}
      >
        {dispatching ? "dispatching…" : inFlight ? "in flight" : signedIn ? "re-dispatch" : "sign in"}
      </button>
      {dispatchState.kind === "error" && (
        <span className="pill drain" title={dispatchState.message}>
          error
        </span>
      )}
      {signedIn && abortableRunId && (
        <span style={{ marginLeft: "1rem" }}>
          {armed || aborting ? (
            <span className="confirm">
              <button
                type="button"
                className="link danger-text"
                onClick={() => onConfirmAbort(abortableRunId)}
                disabled={aborting}
              >
                {aborting ? "aborting…" : "abort?"}
              </button>
              <span className="sep">/</span>
              <button
                type="button"
                className="link"
                onClick={onCancelAbort}
                disabled={aborting}
              >
                keep
              </button>
            </span>
          ) : (
            <button
              type="button"
              className="link danger-text"
              onClick={onArmAbort}
            >
              abort
            </button>
          )}
        </span>
      )}
      {abortState.kind === "error" && (
        <span
          className="pill drain"
          style={{ marginLeft: "0.5rem" }}
          title={abortState.message}
        >
          abort error
        </span>
      )}
      {selectedRunId && focused && (
        <span className="dim mono">
          showing run {selectedRunId.slice(0, 8)}…{" "}
          <button type="button" className="link" onClick={() => onSelectRun(null)}>
            (clear)
          </button>
        </span>
      )}
    </div>
  );

  if (!focused) {
    if (inFlight) {
      return (
        <>
          {actions}
          <DefinitionDag />
          <div className="empty">
            Run lock held — waiting for the run record to land.
          </div>
        </>
      );
    }
    return (
      <>
        {actions}
        <DefinitionDag />
        <div className="empty">No runs yet — re-dispatch above to start one.</div>
      </>
    );
  }

  const isActive = focused.state === "in_progress";

  return (
    <>
      {actions}
      {!isActive && !selectedRunId && (
        <div className="run-status-banner">
          No run in flight. Showing the last completed run.
        </div>
      )}
      <PipelineDag
        run={focused}
        graph={graph}
        selectedNodeId={drillNodeId}
        onSelectNode={setDrillNodeId}
      />
      <DrillIn
        nodeId={drillNodeId}
        run={focused}
        graph={graph}
        project={project}
        repo={repo}
        onClose={() => setDrillNodeId(null)}
      />
      {drillNodeId === null && (
        <RunMetaSummary run={focused} graph={graph} repo={repo} live={isActive} />
      )}
    </>
  );
}

// Cool-toned definition view of the workflow's DAG when no run has
// landed yet. v1 phases-v1 ships single-phase, so this is `[phase] →
// [pr]`. Rendering richer phase metadata would need a /v1/workflows
// fetch — out of scope for the initial DAG view; this is the floor.
function DefinitionDag() {
  return (
    <div className="dag dag-definition" aria-label="workflow definition">
      <div className="dag-node dag-node-definition">
        <div className="dag-node-label">phase</div>
        <div className="dag-node-state dim mono">not run</div>
      </div>
      <div className="dag-edge" aria-hidden="true">→</div>
      <div className="dag-node dag-node-definition">
        <div className="dag-node-label">pr</div>
        <div className="dag-node-state dim mono">pending</div>
      </div>
    </div>
  );
}

// Pipeline DAG painted with the focused run's state. Builds the node
// list from the run's attempts (one node per distinct phase touched,
// in order) plus a trailing PR node colored by pr linkage. Click a
// node to drill in.
function PipelineDag({
  run,
  graph,
  selectedNodeId,
  onSelectNode,
}: {
  run: GraphNode;
  graph: IssueGraph;
  selectedNodeId: string | null;
  onSelectNode: (id: string | null) => void;
}) {
  const phases = useMemo(() => phaseNodesForRun(graph, run), [graph, run]);
  const meta = run.metadata;
  const workflowGraph = workflowGraphMeta(meta.workflow_graph);
  const activeEntry = stringOrNull(meta.entrypoint_phase)
    ?? workflowGraph?.default_entry?.target
    ?? phases[0]?.phaseName
    ?? null;
  const reportId = stringOrNull(meta.report_id);
  const reportState = stringOrNull(meta.report_state);
  const reportTitle = stringOrNull(meta.report_title);
  const prNumber = numberOrNull(meta.pr_number);
  const prBranch = stringOrNull(meta.pr_branch);
  return (
    <div className="dag-wrap">
      <div className="dag" aria-label="pipeline">
        {activeEntry && (
          <>
            <div className="dag-entry active">
              <span className="mono">entry</span>
              <span className="dim mono">{activeEntry}</span>
            </div>
            <div className="dag-edge" aria-hidden="true">→</div>
          </>
        )}
        {phases.map((p, index) => (
          <Fragment key={p.phaseName}>
            {index > 0 && <div className="dag-edge" aria-hidden="true">→</div>}
            <DagPhaseNode
              phase={p}
              selected={selectedNodeId === `phase:${p.phaseName}`}
              onSelect={() =>
                onSelectNode(selectedNodeId === `phase:${p.phaseName}` ? null : `phase:${p.phaseName}`)
              }
            />
          </Fragment>
        ))}
        <div className="dag-edge" aria-hidden="true">→</div>
        <button
          type="button"
          className={`dag-node dag-node-pr${reportId || prNumber ? " opened" : " pending"}${selectedNodeId === "pr" ? " selected" : ""}`}
          onClick={() => onSelectNode(selectedNodeId === "pr" ? null : "pr")}
          aria-pressed={selectedNodeId === "pr"}
        >
          <div className="dag-node-label">report</div>
          <div className="dag-node-state mono">
            {reportState ?? (prNumber ? `#${prNumber}` : prBranch ? prBranch : "pending")}
          </div>
          {reportTitle && <div className="dag-node-meta dim mono">{reportTitle}</div>}
        </button>
      </div>
      {workflowGraph && workflowGraph.recycle_arrows.length > 0 && (
        <div className="dag-policy-rail" aria-label="recycle policies">
          {workflowGraph.recycle_arrows.map((arrow) => (
            <span
              key={`${arrow.kind}:${arrow.source}:${arrow.target}:${arrow.trigger}`}
              className={`dag-policy ${arrow.active ? "active" : "inactive"}`}
              title={`${arrow.trigger || "recycle"}; max ${arrow.max_attempts}`}
            >
              <span className="mono">{arrow.source}</span>
              <span className="dim mono">↻</span>
              <span className="mono">{arrow.target}</span>
              {arrow.trigger && <span className="dim mono">{arrow.trigger}</span>}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

type PhaseRollup = {
  phaseName: string;
  attempts: GraphNode[];
  latest: GraphNode;
  status: { cls: string; text: string };
};

function phaseNodesForRun(graph: IssueGraph, run: GraphNode): PhaseRollup[] {
  const attempts = attemptsForRun(graph, run.id);
  const byPhase = new Map<string, GraphNode[]>();
  for (const a of attempts) {
    const phase = stringOrNull(a.metadata.phase) ?? "phase";
    const arr = byPhase.get(phase) ?? [];
    arr.push(a);
    byPhase.set(phase, arr);
  }
  const out: PhaseRollup[] = [];
  for (const [phaseName, arr] of byPhase) {
    const latest = arr[arr.length - 1];
    out.push({ phaseName, attempts: arr, latest, status: phaseStatus(latest) });
  }
  if (out.length === 0) {
    // Run exists but no attempts dispatched yet (rare — pre-record_started
    // window). Still render a placeholder so the DAG isn't empty.
    out.push({
      phaseName: stringOrNull(run.metadata.workflow) ?? "phase",
      attempts: [],
      latest: run,
      status: { cls: "info", text: "pending" },
    });
  }
  return out;
}

function phaseStatus(attempt: GraphNode): { cls: string; text: string } {
  const meta = attempt.metadata;
  const completed = stringOrNull(meta.completed_at);
  const conclusion = stringOrNull(meta.conclusion);
  const verification = isRecord(meta.verification) ? meta.verification : null;
  const verStatus = verification ? stringOrNull(verification.status) : null;
  const workflowRunId = meta.workflow_run_id != null ? String(meta.workflow_run_id) : null;
  const nativeJobs = nativeAttemptJobs(meta.jobs);
  const nativeRunning = nativeJobs.some((j) => j.state === "active" || j.steps.some((s) => s.state === "active"));
  // Resume primitive (#111) — synthesized skip-marks render as "skipped"
  // ahead of any other state since they're never dispatched. No pill
  // (skipped isn't one of {free, busy, drain, info}); the caller renders
  // it as dim text. Also propagates upward via the attempt node's
  // `state === "skipped"` value emitted by `_build_issue_graph`.
  if (attempt.state === "skipped" || stringOrNull(meta.skipped_from_run_id)) {
    return { cls: "", text: "skipped" };
  }
  if (!completed) {
    return workflowRunId || nativeRunning
      ? { cls: "busy", text: "running" }
      : { cls: "info", text: "dispatching" };
  }
  if (verStatus === "pass" || conclusion === "success") return { cls: "free", text: "pass" };
  if (verStatus === "fail") return { cls: "drain", text: "fail" };
  if (verStatus === "error") return { cls: "drain", text: "error" };
  if (conclusion === "cancelled") return { cls: "drain", text: "cancelled" };
  if (conclusion) return { cls: "drain", text: conclusion };
  return { cls: "", text: "completed" };
}

function DagPhaseNode({
  phase,
  selected,
  onSelect,
}: {
  phase: PhaseRollup;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      className={`dag-node dag-node-phase${selected ? " selected" : ""}`}
      onClick={onSelect}
      aria-pressed={selected}
    >
      <div className="dag-node-label">{phase.phaseName}</div>
      <div className="dag-node-state">
        <span className={`pill ${phase.status.cls}`}>{phase.status.text}</span>
      </div>
      {phase.attempts.length > 1 && (
        <div className="dag-node-meta dim mono">×{phase.attempts.length}</div>
      )}
    </button>
  );
}

function DrillIn({
  nodeId,
  run,
  graph,
  project,
  repo,
  onClose,
}: {
  nodeId: string | null;
  run: GraphNode;
  graph: IssueGraph;
  project: string;
  repo: string | null;
  onClose: () => void;
}) {
  if (nodeId === null) return null;
  const meta = run.metadata;
  if (nodeId === "pr") {
    const reportId = stringOrNull(meta.report_id);
    const reportState = stringOrNull(meta.report_state);
    const reportTitle = stringOrNull(meta.report_title);
    const reportUrl = stringOrNull(meta.report_url);
    const prNumber = numberOrNull(meta.pr_number);
    const prBranch = stringOrNull(meta.pr_branch);
    return (
      <div className="run-panel">
        <div className="run-panel-header">
          <div>
            <strong>report</strong>
            <span className={`pill ${reportId || prNumber ? "free" : ""}`} style={{ marginLeft: "0.5rem" }}>
              {reportState ?? (prNumber ? "opened" : "pending")}
            </span>
          </div>
          <button type="button" className="link" onClick={onClose}>
            close
          </button>
        </div>
        <div className="run-panel-meta">
          {reportId && (
            <div>
              <span className="key">report</span>{" "}
              <span className="mono" title={reportId}>{reportId.slice(0, 8)}…</span>
            </div>
          )}
          {reportTitle && (
            <div>
              <span className="key">title</span> <span>{reportTitle}</span>
            </div>
          )}
          {prNumber !== null && repo ? (
            <div>
              <span className="key">PR</span>{" "}
              <a className="mono" href={reportUrl || `https://github.com/${repo}/pull/${prNumber}`} target="_blank" rel="noreferrer">
                #{prNumber}
              </a>
            </div>
          ) : (
            <div className="dim mono">No report opened yet for this run.</div>
          )}
          {prBranch && (
            <div>
              <span className="key">branch</span> <span className="mono">{prBranch}</span>
            </div>
          )}
        </div>
      </div>
    );
  }
  if (nodeId.startsWith("phase:")) {
    const phaseName = nodeId.slice("phase:".length);
    const phases = phaseNodesForRun(graph, run);
    const rollup = phases.find((p) => p.phaseName === phaseName);
    if (!rollup) return null;
    return (
      <div className="run-panel">
        <div className="run-panel-header">
          <div>
            <strong>{rollup.phaseName}</strong>
            <span className={`pill ${rollup.status.cls}`} style={{ marginLeft: "0.5rem" }}>
              {rollup.status.text}
            </span>
            <span className="dim mono" style={{ marginLeft: "0.5rem" }}>
              {rollup.attempts.length} attempt{rollup.attempts.length === 1 ? "" : "s"}
            </span>
          </div>
          <button type="button" className="link" onClick={onClose}>
            close
          </button>
        </div>
        {rollup.attempts.length === 0 ? (
          <div className="empty dim">No attempts dispatched yet.</div>
        ) : (
          <div className="attempt-list">
            {rollup.attempts.map((a) => (
              <AttemptCard key={a.id} attempt={a} project={project} repo={repo} />
            ))}
          </div>
        )}
      </div>
    );
  }
  return null;
}

function RunMetaSummary({
  run,
  graph,
  repo,
  live,
}: {
  run: GraphNode;
  graph: IssueGraph;
  repo: string | null;
  live: boolean;
}) {
  // Lightweight tail summary so the user has context without having to
  // drill in. When a node is selected, this gets replaced by the
  // drill-in panel.
  const attempts = attemptsForRun(graph, run.id);
  const meta = run.metadata;
  const cumulativeCost = numberOrNull(meta.cumulative_cost_usd);
  const workflow = stringOrNull(meta.workflow);
  const abortReason = stringOrNull(meta.abort_reason);
  const prNumber = numberOrNull(meta.pr_number);
  // Resume primitive (#111) — set on the resumed Run so the user
  // sees why earlier phases are pre-satisfied and which prior Run's
  // outputs got carried forward.
  const clonedFromRunId = stringOrNull(meta.cloned_from_run_id);
  const entrypointPhase = stringOrNull(meta.entrypoint_phase);
  return (
    <div className="run-panel-meta" style={{ marginTop: "0.5rem" }}>
      <div>
        <span className="key">state</span>{" "}
        <span className={`pill ${runStatePill(run.state ?? "")}`}>{run.state ?? "—"}</span>
        {live && <span className="live-dot" aria-label="live" style={{ marginLeft: "0.5rem" }} />}
      </div>
      {workflow && (
        <div>
          <span className="key">workflow</span> <span className="mono">{workflow}</span>
        </div>
      )}
      {clonedFromRunId && (
        <div>
          <span className="key">resumed from</span>{" "}
          <span className="mono" title={clonedFromRunId}>
            {clonedFromRunId.slice(0, 8)}…
          </span>
          {entrypointPhase && (
            <>
              {" "}
              <span className="dim mono">at {entrypointPhase}</span>
            </>
          )}
        </div>
      )}
      <div>
        <span className="key">attempts</span> <span className="mono">{attempts.length}</span>
      </div>
      {cumulativeCost !== null && (
        <div>
          <span className="key">cost</span> <span className="mono">${cumulativeCost.toFixed(4)}</span>
        </div>
      )}
      {prNumber !== null && repo && (
        <div>
          <span className="key">PR</span>{" "}
          <a className="mono" href={`https://github.com/${repo}/pull/${prNumber}`} target="_blank" rel="noreferrer">
            #{prNumber}
          </a>
        </div>
      )}
      {abortReason && (
        <div>
          <span className="key">abort</span> <span className="mono">{abortReason}</span>
        </div>
      )}
    </div>
  );
}

// Threshold past which a `dispatching` attempt is visually flagged as
// stuck. Post-#79, the workflow's `started` callback fires from the
// route job's first step, which lands within ~10–15s of dispatch under
// normal conditions. 30s gives the runner some slack but flags real
// dispatch failures (the orphan-webhook bug surfaced as exactly this).
const STUCK_DISPATCHING_MS = 30_000;

function RunsTab({
  graph,
  graphAvailable,
  onPickRun,
}: {
  graph: IssueGraph | null;
  graphAvailable: boolean;
  onPickRun: (runId: string) => void;
}) {
  if (!graphAvailable) {
    return (
      <div className="empty">
        Run history isn't available for native issues yet.
      </div>
    );
  }
  if (!graph) {
    return <div className="empty">Loading run history…</div>;
  }
  const runs = graph.nodes
    .filter((n) => n.kind === "run")
    .slice()
    .sort((a, b) => (b.timestamp ?? "").localeCompare(a.timestamp ?? ""));
  if (runs.length === 0) {
    return <div className="empty">No runs yet on this issue.</div>;
  }
  return (
    <table>
      <thead>
        <tr>
          <th>Run</th>
          <th>State</th>
          <th>Started</th>
          <th>Attempts</th>
          <th>Cost</th>
          <th>PR</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {runs.map((r) => {
          const id = runIdFromNode(r);
          const meta = r.metadata;
          const attemptCount = graph.nodes.filter(
            (n) => n.kind === "attempt" && n.id.startsWith(`attempt:${id}:`),
          ).length;
          const cost = numberOrNull(meta.cumulative_cost_usd);
          const prNumber = numberOrNull(meta.pr_number);
          // Resume primitive (#111) — flag rows that started as
          // resumed clones so the lineage is visible at a glance
          // without drilling in. Tooltip carries the prior run id.
          const clonedFrom = stringOrNull(meta.cloned_from_run_id);
          const entrypointPhase = stringOrNull(meta.entrypoint_phase);
          return (
            <tr key={r.id}>
              <td className="mono">
                {id.slice(0, 8)}…
                {clonedFrom && (
                  <span
                    className="dim mono"
                    style={{ marginLeft: "0.5rem" }}
                    title={`resumed from ${clonedFrom}${entrypointPhase ? ` at ${entrypointPhase}` : ""}`}
                  >
                    ↩ {clonedFrom.slice(0, 8)}…
                  </span>
                )}
              </td>
              <td>
                <span className={`pill ${runStatePill(r.state ?? "")}`}>{r.state ?? "—"}</span>
              </td>
              <td className="mono dim">{r.timestamp ? formatTime(r.timestamp) : "—"}</td>
              <td className="mono">{attemptCount}</td>
              <td className="mono">{cost !== null ? `$${cost.toFixed(4)}` : "—"}</td>
              <td className="mono dim">{prNumber !== null ? `#${prNumber}` : "—"}</td>
              <td>
                <button type="button" className="link" onClick={() => onPickRun(id)}>
                  open
                </button>
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

function AttemptCard({
  attempt,
  project,
  repo,
}: {
  attempt: GraphNode;
  project: string;
  repo: string | null;
}) {
  const meta = attempt.metadata;
  const phase = stringOrNull(meta.phase) ?? "attempt";
  const dispatchedAt = attempt.timestamp;
  const completedAt = stringOrNull(meta.completed_at);
  const conclusion = stringOrNull(meta.conclusion);
  const decision = stringOrNull(meta.decision);
  const workflowRunId = meta.workflow_run_id != null ? String(meta.workflow_run_id) : null;
  const workflowFilename = stringOrNull(meta.workflow_filename);
  const verification = isRecord(meta.verification) ? meta.verification : null;
  const verificationStatus = verification ? stringOrNull(verification.status) : null;
  // Resume primitive (#111) — set on synthesized skip-marks. The phase
  // wasn't actually dispatched; outputs were carried from the named
  // prior Run. Renders the card in a dim/dashed style so it reads as
  // "this slot was satisfied, no work happened here."
  const skippedFromRunId = stringOrNull(meta.skipped_from_run_id);
  // Cost prefers the phase-reported top-level cost_usd (#69 — non-verify
  // LLM phases set this directly without a verification.json) and falls
  // back to verification.cost_usd for verify phases that emit the artifact.
  const attemptCost = numberOrNull(meta.cost_usd);
  const verificationCost = verification ? numberOrNull(verification.cost_usd) : null;
  const displayedCost = attemptCost ?? verificationCost;
  const verificationReasons = verification && Array.isArray(verification.reasons)
    ? verification.reasons.filter((r): r is string => typeof r === "string")
    : [];
  const phaseKind = stringOrNull(meta.phase_kind);
  const attemptIndex = numberOrNull(meta.attempt_index);
  const logArchiveUrl = stringOrNull(meta.log_archive_url);
  const nativeJobs = nativeAttemptJobs(meta.jobs);

  // Pre-webhook progression (#83):
  //   no workflow_run_id, no completed_at  → dispatching
  //   workflow_run_id, no completed_at     → running
  //   completed_at                         → terminal (existing logic)
  // Pre-#79 the wire had a separate `queued` state we got from
  // workflow_run.requested. Post-#79 the started callback fires from
  // the workflow's first step, so the "queued at GHA but not yet
  // running" window is no longer separately observable — collapsed
  // into `dispatching`. Stuck-in-dispatching past STUCK_DISPATCHING_MS
  // is visually flagged so the orphan-dispatch shape is obvious.
  const running = !completedAt;
  const nativeRunning = nativeJobs.some((j) => j.state === "active" || j.steps.some((s) => s.state === "active"));
  const dispatching = running && !workflowRunId && !nativeRunning;
  const elapsedMs = dispatchedAt && running ? now() - parseTs(dispatchedAt) : null;
  const stuckDispatching =
    dispatching && elapsedMs !== null && elapsedMs > STUCK_DISPATCHING_MS;
  const elapsedLabel = dispatchedAt
    ? running
      ? `${formatDuration(elapsedMs ?? 0)} elapsed`
      : completedAt
        ? `ran ${formatDuration(parseTs(completedAt) - parseTs(dispatchedAt))}`
        : null
    : null;

  const statusPill = (() => {
    if (skippedFromRunId) return { cls: "", text: "skipped" };
    if (dispatching) return { cls: "info", text: "dispatching" };
    if (running) return { cls: "busy", text: "running" };
    if (verificationStatus === "pass") return { cls: "free", text: "pass" };
    if (verificationStatus === "fail") return { cls: "drain", text: "fail" };
    if (verificationStatus === "error") return { cls: "drain", text: "error" };
    if (conclusion === "success") return { cls: "free", text: "success" };
    if (conclusion === "cancelled") return { cls: "drain", text: "cancelled" };
    if (conclusion) return { cls: "drain", text: conclusion };
    return { cls: "", text: "completed" };
  })();

  // Fallback link for the dispatching window: GHA's actions-by-workflow
  // page filtered to our branch (glimmung/<run_id>) lets the user find
  // their run when the started callback hasn't landed yet. Derives
  // run_id from the attempt id (`attempt:<run_id>:<idx>`).
  const runIdFromAttempt = attempt.id.startsWith("attempt:")
    ? attempt.id.split(":")[1] ?? ""
    : "";
  const branchName = runIdFromAttempt ? `glimmung/${runIdFromAttempt}` : null;
  const dispatchingFallback =
    dispatching && repo && workflowFilename && branchName
      ? `https://github.com/${repo}/actions/workflows/${workflowFilename}?query=${encodeURIComponent(`branch:${branchName}`)}`
      : null;

  return (
    <div
      className={`attempt-card${running ? " running" : ""}${stuckDispatching ? " stuck" : ""}${skippedFromRunId ? " skipped" : ""}`}
    >
      <div className="attempt-card-head">
        <strong>{attempt.label}</strong>
        {skippedFromRunId ? (
          <span
            className="dim mono"
            title={`satisfied by run ${skippedFromRunId} — no dispatch happened on this run`}
          >
            skipped
          </span>
        ) : (
          <span className={`pill ${statusPill.cls}`}>{statusPill.text}</span>
        )}
        <span className="dim mono">{phase}</span>
        {elapsedLabel && !skippedFromRunId && <span className="dim mono">{elapsedLabel}</span>}
        {stuckDispatching && (
          <span className="pill drain" title="No workflow_run_id received from GHA. Possible orphan dispatch.">
            stuck
          </span>
        )}
      </div>
      <div className="attempt-card-body">
        {skippedFromRunId && (
          <div>
            <span className="key">satisfied by</span>{" "}
            <span className="mono">{skippedFromRunId.slice(0, 8)}…</span>
          </div>
        )}
        {dispatchedAt && !skippedFromRunId && (
          <div>
            <span className="key">dispatched</span>{" "}
            <span className="mono">{formatTime(dispatchedAt)}</span>
          </div>
        )}
        {completedAt && !skippedFromRunId && (
          <div>
            <span className="key">completed</span>{" "}
            <span className="mono">{formatTime(completedAt)}</span>
          </div>
        )}
        {workflowFilename && (
          <div>
            <span className="key">workflow</span>{" "}
            <span className="mono">{workflowFilename}</span>
          </div>
        )}
        {workflowRunId && (
          <div>
            <span className="key">gh run</span>{" "}
            {repo ? (
              <a
                className="mono"
                href={`https://github.com/${repo}/actions/runs/${workflowRunId}`}
                target="_blank"
                rel="noreferrer"
              >
                {workflowRunId}
              </a>
            ) : (
              <span className="mono">{workflowRunId}</span>
            )}
          </div>
        )}
        {!workflowRunId && branchName && (
          <div>
            <span className="key">branch</span>{" "}
            <span className="mono">{branchName}</span>
          </div>
        )}
        {dispatchingFallback && (
          <div>
            <span className="key">find run</span>{" "}
            <a
              className="mono"
              href={dispatchingFallback}
              target="_blank"
              rel="noreferrer"
              title="GHA workflow runs filtered to this attempt's branch — useful when the started callback hasn't landed yet"
            >
              gh actions ↗
            </a>
          </div>
        )}
        {displayedCost !== null && (
          <div>
            <span className="key">cost</span>{" "}
            <span className="mono">${displayedCost.toFixed(4)}</span>
          </div>
        )}
        {decision && (
          <div>
            <span className="key">decision</span>{" "}
            <span className="mono">{decision}</span>
          </div>
        )}
      </div>
      {verificationReasons.length > 0 && (
        <ul className="attempt-card-reasons">
          {verificationReasons.map((r, i) => (
            <li key={i} className="mono dim">
              {r}
            </li>
          ))}
        </ul>
      )}
      {nativeJobs.length > 0 && (
        <div className="native-job-list">
          {nativeJobs.map((job) => (
            <div className="native-job" key={job.job_id}>
              <div className="native-job-head">
                <span className="mono">{job.name || job.job_id}</span>
                <span className={`pill ${nativeStatePill(job.state ?? "")}`}>
                  {job.state || "pending"}
                </span>
              </div>
              <div className="native-step-list">
                {job.steps.map((step) => (
                  <div className="native-step" key={step.slug}>
                    <span className={`native-step-rail ${nativeStatePill(step.state ?? "")}`} />
                    <span className="mono">{step.slug}</span>
                    {step.title && <span>{step.title}</span>}
                    <span className="dim mono">{step.state || "pending"}</span>
                    {step.exit_code !== null && step.exit_code !== undefined && (
                      <span className="dim mono">exit {step.exit_code}</span>
                    )}
                    {step.message && <span className="native-step-message">{step.message}</span>}
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>
      )}
      {phaseKind === "k8s_job" && runIdFromAttempt && attemptIndex !== null && (
        <NativeAttemptEvents
          project={project}
          runId={runIdFromAttempt}
          attemptIndex={attemptIndex}
          archiveUrl={logArchiveUrl}
        />
      )}
    </div>
  );
}

function NativeAttemptEvents({
  project,
  runId,
  attemptIndex,
  archiveUrl,
}: {
  project: string;
  runId: string;
  attemptIndex: number;
  archiveUrl: string | null;
}) {
  const [logs, setLogs] = useState<NativeRunEventsResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLogs(null);
    setError(null);
    const url =
      `/v1/runs/${encodeURIComponent(project)}/${encodeURIComponent(runId)}` +
      `/native/events?attempt_index=${attemptIndex}&limit=200`;
    fetch(url)
      .then(async (res) => {
        if (!res.ok) throw new Error(`events ${res.status}`);
        const body = await res.json() as NativeRunEventsResponse;
        if (!cancelled) setLogs(body);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err));
      });
    return () => {
      cancelled = true;
    };
  }, [project, runId, attemptIndex]);

  if (error) {
    return (
      <div className="native-log-panel">
        <span className="pill drain">log error</span>{" "}
        <span className="mono dim">{error}</span>
      </div>
    );
  }
  if (!logs) {
    return <div className="native-log-panel dim mono">loading native events…</div>;
  }
  const events = logs.events;
  return (
    <div className="native-log-panel">
      <div className="native-log-head">
        <span className="key">native events</span>
        <span className="mono dim">{events.length} hot</span>
        {(logs.archive_url || archiveUrl) && (
          <a
            className="mono"
            href={`/v1/artifacts/${artifactPathFromUrl(logs.archive_url || archiveUrl || "")}`}
            target="_blank"
            rel="noreferrer"
          >
            archive
          </a>
        )}
      </div>
      {events.length === 0 ? (
        <div className="dim mono">No hot native events recorded for this attempt.</div>
      ) : (
        <div className="native-log-lines">
          {events.map((event) => (
            <div key={event.id} className={`native-log-line ${event.event}`}>
              <span className="mono dim">{event.seq}</span>
              <span className="mono">{event.job_id}</span>
              <span className="mono">{event.step_slug || "—"}</span>
              <span className="mono">{event.event}</span>
              {event.message && <span className="native-log-message">{event.message}</span>}
              {event.exit_code !== null && (
                <span className="mono dim">exit {event.exit_code}</span>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function findActiveRun(graph: IssueGraph): GraphNode | null {
  return graph.nodes.find((n) => n.kind === "run" && n.state === "in_progress") ?? null;
}

function findLastCompletedRun(graph: IssueGraph): GraphNode | null {
  const completed = graph.nodes
    .filter((n) => n.kind === "run" && n.state !== "in_progress")
    .sort((a, b) => (b.timestamp ?? "").localeCompare(a.timestamp ?? ""));
  return completed[0] ?? null;
}

function attemptsForRun(graph: IssueGraph, runNodeId: string): GraphNode[] {
  // run node id is `run:<run_id>`; attempt ids are `attempt:<run_id>:<index>`.
  const runId = runNodeId.startsWith("run:") ? runNodeId.slice(4) : runNodeId;
  const prefix = `attempt:${runId}:`;
  return graph.nodes
    .filter((n) => n.kind === "attempt" && n.id.startsWith(prefix))
    .sort((a, b) => {
      const ai = parseInt(a.id.split(":").pop() ?? "0", 10);
      const bi = parseInt(b.id.split(":").pop() ?? "0", 10);
      return ai - bi;
    });
}

// Run nodes are keyed `run:<run_id>` in the graph endpoint; the abort
// endpoint and selection state both take the bare ULID.
function runIdFromNode(n: GraphNode): string {
  return n.id.startsWith("run:") ? n.id.slice(4) : n.id;
}

function isRecord(x: unknown): x is Record<string, unknown> {
  return typeof x === "object" && x !== null && !Array.isArray(x);
}

function workflowGraphMeta(x: unknown): WorkflowGraphMeta | null {
  if (!isRecord(x)) return null;
  const phases = Array.isArray(x.phases)
    ? x.phases.filter((p): p is string => typeof p === "string")
    : [];
  const defaultEntry = isRecord(x.default_entry)
    && typeof x.default_entry.target === "string"
    ? {
        target: x.default_entry.target,
        active: Boolean(x.default_entry.active),
        kind: String(x.default_entry.kind ?? "default"),
      }
    : null;
  const terminal = isRecord(x.terminal)
    ? {
        kind: String(x.terminal.kind ?? "report"),
        enabled: Boolean(x.terminal.enabled),
      }
    : { kind: "report", enabled: false };
  const recycle_arrows = Array.isArray(x.recycle_arrows)
    ? x.recycle_arrows.flatMap((raw): RecycleArrow[] => {
        if (!isRecord(raw)) return [];
        const kind = raw.kind === "report_recycle" ? "report_recycle" : "phase_recycle";
        return [{
          source: String(raw.source ?? ""),
          target: String(raw.target ?? ""),
          trigger: String(raw.trigger ?? ""),
          max_attempts: numberOrNull(raw.max_attempts) ?? 0,
          active: Boolean(raw.active),
          kind,
        }];
      }).filter((a) => a.source && a.target)
    : [];
  return { phases, default_entry: defaultEntry, recycle_arrows, terminal };
}

function nativeAttemptJobs(x: unknown): NativeAttemptJob[] {
  if (!Array.isArray(x)) return [];
  return x.flatMap((raw): NativeAttemptJob[] => {
    if (!isRecord(raw)) return [];
    const jobId = stringOrNull(raw.job_id) ?? stringOrNull(raw.id);
    if (!jobId) return [];
    const steps = Array.isArray(raw.steps)
      ? raw.steps.flatMap((s): NativeAttemptStep[] => {
          if (!isRecord(s)) return [];
          const slug = stringOrNull(s.slug);
          if (!slug) return [];
          return [{
            slug,
            title: stringOrNull(s.title),
            state: stringOrNull(s.state),
            message: stringOrNull(s.message),
            exit_code: numberOrNull(s.exit_code),
          }];
        })
      : [];
    return [{
      job_id: jobId,
      name: stringOrNull(raw.name),
      state: stringOrNull(raw.state),
      steps,
    }];
  });
}

function nativeStatePill(state: string): string {
  if (state === "succeeded") return "free";
  if (state === "active") return "busy";
  if (state === "failed") return "drain";
  if (state === "skipped") return "info";
  return "";
}

function numberOrNull(x: unknown): number | null {
  return typeof x === "number" && Number.isFinite(x) ? x : null;
}

function stringOrNull(x: unknown): string | null {
  return typeof x === "string" && x.length > 0 ? x : null;
}

function artifactPathFromUrl(url: string): string {
  const prefix = "blob://artifacts/";
  if (url.startsWith(prefix)) return url.slice(prefix.length);
  return url.replace(/^\/v1\/artifacts\//, "");
}

function parseTs(s: string): number {
  const n = Date.parse(s);
  return Number.isFinite(n) ? n : 0;
}

function now(): number {
  return Date.now();
}

function formatTime(s: string): string {
  const n = parseTs(s);
  if (!n) return s;
  return new Date(n).toLocaleString();
}

function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "—";
  const sec = Math.floor(ms / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  const remSec = sec % 60;
  if (min < 60) return `${min}m ${remSec}s`;
  const hr = Math.floor(min / 60);
  const remMin = min % 60;
  return `${hr}h ${remMin}m`;
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

function runStatePill(state: string): string {
  if (state === "passed") return "free";
  if (state === "in_progress") return "busy";
  if (state === "review_required") return "info";
  if (state === "aborted") return "drain";
  return "";
}

function formatTimestamp(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { authedFetch } from "./auth";

type PlaybookIssueSpec = {
  title: string;
  body: string;
  labels: string[];
  workflow?: string | null;
  metadata: Record<string, unknown>;
};

type PlaybookEntry = {
  id: string;
  title?: string | null;
  issue: PlaybookIssueSpec;
  depends_on: string[];
  manual_gate: boolean;
  state: string;
  created_issue_ref?: string | null;
  run_ref?: string | null;
  completed_at?: string | null;
  metadata: Record<string, unknown>;
};

type Playbook = {
  schema_version: number;
  ref: string;
  project: string;
  title: string;
  description: string;
  entries: PlaybookEntry[];
  concurrency_limit?: number | null;
  integration_strategy: string;
  state: string;
  metadata: Record<string, unknown>;
  created_at: string;
  updated_at: string;
};

type ActionState =
  | { kind: "idle" }
  | { kind: "running" }
  | { kind: "gating"; entryId: string }
  | { kind: "error"; message: string }
  | { kind: "done"; message: string };

export function PlaybooksView({
  signedIn,
  isAdmin,
  projectFilter,
  playbookRef,
}: {
  signedIn: boolean;
  isAdmin: boolean;
  projectFilter: string | null;
  playbookRef?: string | null;
}) {
  const [rows, setRows] = useState<Playbook[] | null>(null);
  const [detail, setDetail] = useState<Playbook | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [action, setAction] = useState<ActionState>({ kind: "idle" });

  const detailProject = projectFilter;
  const detailMode = Boolean(detailProject && playbookRef);

  const refresh = async () => {
    setLoading(true);
    setError(null);
    try {
      if (detailMode && detailProject && playbookRef) {
        const url = `/v1/playbooks/${encodeURIComponent(detailProject)}/${encodeURIComponent(playbookRef)}`;
        const r = await fetch(url);
        if (!r.ok) throw new Error(`${url} -> ${r.status}`);
        setDetail((await r.json()) as Playbook);
        setRows(null);
      } else {
        const params = new URLSearchParams();
        if (projectFilter) params.set("project", projectFilter);
        params.set("limit", "200");
        const url = `/v1/playbooks?${params.toString()}`;
        const r = await fetch(url);
        if (!r.ok) throw new Error(`${url} -> ${r.status}`);
        setRows((await r.json()) as Playbook[]);
        setDetail(null);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setRows(null);
      setDetail(null);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectFilter, playbookRef]);

  const runPlaybook = async () => {
    if (!detail) return;
    setAction({ kind: "running" });
    try {
      const url = `/v1/playbooks/${encodeURIComponent(detail.project)}/${encodeURIComponent(detail.ref)}/run`;
      const r = await authedFetch(url, { method: "POST" });
      if (!r.ok) throw new Error(`${url} -> ${r.status}: ${await r.text()}`);
      setDetail((await r.json()) as Playbook);
      setAction({ kind: "done", message: "advanced" });
    } catch (e) {
      setAction({ kind: "error", message: e instanceof Error ? e.message : String(e) });
    }
  };

  const patchGate = async (entry: PlaybookEntry, manualGate: boolean) => {
    if (!detail) return;
    setAction({ kind: "gating", entryId: entry.id });
    try {
      const url = `/v1/playbooks/${encodeURIComponent(detail.project)}/${encodeURIComponent(detail.ref)}` +
        `/entries/${encodeURIComponent(entry.id)}/gate`;
      const r = await authedFetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ manual_gate: manualGate }),
      });
      if (!r.ok) throw new Error(`${url} -> ${r.status}: ${await r.text()}`);
      setDetail((await r.json()) as Playbook);
      setAction({ kind: "done", message: manualGate ? "held" : "cleared" });
    } catch (e) {
      setAction({ kind: "error", message: e instanceof Error ? e.message : String(e) });
    }
  };

  if (error) return <div className="empty error">{error}</div>;
  if (loading && !rows && !detail) return <div className="empty">Loading playbooks...</div>;
  if (detailMode) {
    if (!detail) return <div className="empty">No playbook found.</div>;
    return (
      <PlaybookDetail
        playbook={detail}
        signedIn={signedIn}
        isAdmin={isAdmin}
        action={action}
        onRefresh={() => void refresh()}
        onRun={() => void runPlaybook()}
        onPatchGate={(entry, manualGate) => void patchGate(entry, manualGate)}
      />
    );
  }
  return (
    <PlaybookIndex
      rows={rows ?? []}
      projectFilter={projectFilter}
      loading={loading}
      onRefresh={() => void refresh()}
    />
  );
}

function PlaybookIndex({
  rows,
  projectFilter,
  loading,
  onRefresh,
}: {
  rows: Playbook[];
  projectFilter: string | null;
  loading: boolean;
  onRefresh: () => void;
}) {
  const counts = useMemo(() => countByState(rows), [rows]);
  return (
    <>
      <h2>
        Playbooks{rows ? ` (${rows.length})` : ""}
        {projectFilter && <span className="filter-hint"> - filtered to {projectFilter}</span>}
        <button type="button" className="inline-action" onClick={onRefresh} disabled={loading}>
          {loading ? "refreshing..." : "refresh"}
        </button>
      </h2>
      <KpiCounts counts={counts} />
      {rows.length === 0 ? (
        <div className="empty">No playbooks yet.</div>
      ) : (
        <table>
          <thead>
            <tr>
              {!projectFilter && <th>Project</th>}
              <th>Playbook</th>
              <th>State</th>
              <th>Strategy</th>
              <th>Entries</th>
              <th>Gated</th>
              <th>Running</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => {
              const gated = row.entries.filter((entry) => entry.manual_gate).length;
              const running = row.entries.filter((entry) => entry.state === "running" || entry.state === "created").length;
              return (
                <tr key={`${row.project}:${row.ref}`}>
                  {!projectFilter && <td className="mono">{row.project}</td>}
                  <td>
                    <strong>{row.title}</strong>
                    <div className="mono dim">{row.ref}</div>
                  </td>
                  <td><span className={`pill ${playbookStatePill(row.state)}`}>{row.state}</span></td>
                  <td className="mono dim">{row.integration_strategy}</td>
                  <td className="mono">{row.entries.length}</td>
                  <td className="mono">{gated}</td>
                  <td className="mono">{running}</td>
                  <td>
                    <Link className="link" to={`/projects/${encodeURIComponent(row.project)}/playbooks/${encodeURIComponent(row.ref)}`}>
                      view
                    </Link>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </>
  );
}

function PlaybookDetail({
  playbook,
  signedIn,
  isAdmin,
  action,
  onRefresh,
  onRun,
  onPatchGate,
}: {
  playbook: Playbook;
  signedIn: boolean;
  isAdmin: boolean;
  action: ActionState;
  onRefresh: () => void;
  onRun: () => void;
  onPatchGate: (entry: PlaybookEntry, manualGate: boolean) => void;
}) {
  const counts = countEntriesByState(playbook.entries);
  const runDisabled = !signedIn || !isAdmin || action.kind === "running";
  return (
    <>
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">playbook</div>
          <h2>{playbook.title}</h2>
          <div className="project-repo mono">{playbook.ref}</div>
        </div>
        <div className="project-facts">
          <div className="project-fact">
            <span>state</span>
            <strong>{playbook.state}</strong>
          </div>
          <div className="project-fact">
            <span>entries</span>
            <strong>{playbook.entries.length}</strong>
          </div>
          <div className="project-fact">
            <span>strategy</span>
            <strong>{playbook.integration_strategy}</strong>
          </div>
        </div>
      </section>

      <div className="section-actions playbook-actions">
        <button
          type="button"
          className="link"
          onClick={onRun}
          disabled={runDisabled}
          title={!signedIn ? "sign in" : !isAdmin ? "admin only" : undefined}
        >
          {action.kind === "running" ? "advancing..." : !signedIn ? "sign in" : !isAdmin ? "admin only" : "advance"}
        </button>
        <span className="sep">/</span>
        <button type="button" className="link" onClick={onRefresh}>
          refresh
        </button>
        {action.kind === "done" && <span className="pill free">{action.message}</span>}
        {action.kind === "error" && <span className="pill drain" title={action.message}>error</span>}
      </div>

      <KpiCounts counts={counts} />

      {playbook.description.trim() && <pre className="evidence-notes playbook-description">{playbook.description}</pre>}

      <h2>Entries</h2>
      {playbook.entries.length === 0 ? (
        <div className="empty">No entries yet.</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Entry</th>
              <th>State</th>
              <th>Dependencies</th>
              <th>Gate</th>
              <th>Issue</th>
              <th>Run</th>
              <th>Workflow</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {playbook.entries.map((entry) => {
              const issueNumber = issueNumberFromRef(entry.created_issue_ref);
              const runTarget = runTargetFromRef(entry.run_ref);
              const gating = action.kind === "gating" && action.entryId === entry.id;
              return (
                <tr key={entry.id}>
                  <td>
                    <strong>{entry.title || entry.issue.title || entry.id}</strong>
                    <div className="mono dim">{entry.id}</div>
                  </td>
                  <td><span className={`pill ${entryStatePill(entry.state)}`}>{entry.state}</span></td>
                  <td className="mono dim">{entry.depends_on.length > 0 ? entry.depends_on.join(", ") : "-"}</td>
                  <td>
                    <span className={`pill ${entry.manual_gate ? "busy" : "free"}`}>
                      {entry.manual_gate ? "held" : "clear"}
                    </span>
                  </td>
                  <td className="mono">
                    {issueNumber ? (
                      <Link className="link" to={`/projects/${encodeURIComponent(playbook.project)}/issues/${issueNumber}/summary`}>
                        {entry.created_issue_ref}
                      </Link>
                    ) : (
                      <span className="dim">pending</span>
                    )}
                  </td>
                  <td className="mono">
                    {runTarget ? (
                      <Link
                        className="link"
                        to={`/projects/${encodeURIComponent(playbook.project)}/issues/${runTarget.issueNumber}/runs/${encodeURIComponent(runTarget.runNumber)}`}
                      >
                        {entry.run_ref}
                      </Link>
                    ) : (
                      <span className="dim">pending</span>
                    )}
                  </td>
                  <td className="mono dim">{entry.issue.workflow ?? "-"}</td>
                  <td>
                    <button
                      type="button"
                      className="link"
                      onClick={() => onPatchGate(entry, !entry.manual_gate)}
                      disabled={!signedIn || !isAdmin || gating}
                    >
                      {gating ? "saving..." : entry.manual_gate ? "clear gate" : "hold"}
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </>
  );
}

function KpiCounts({ counts }: { counts: Record<string, number> }) {
  const keys = Object.keys(counts).sort();
  if (keys.length === 0) return null;
  return (
    <div className="kpi-strip">
      {keys.map((key) => (
        <div className="kpi" key={key}>
          <span className="k">{key}</span>
          <span className={`v ${kpiValueClass(key)}`}>{counts[key]}</span>
        </div>
      ))}
    </div>
  );
}

function countByState(rows: Playbook[]): Record<string, number> {
  return rows.reduce<Record<string, number>>((acc, row) => {
    acc[row.state] = (acc[row.state] ?? 0) + 1;
    return acc;
  }, {});
}

function countEntriesByState(entries: PlaybookEntry[]): Record<string, number> {
  return entries.reduce<Record<string, number>>((acc, entry) => {
    acc[entry.state] = (acc[entry.state] ?? 0) + 1;
    if (entry.manual_gate) acc.gated = (acc.gated ?? 0) + 1;
    return acc;
  }, {});
}

function playbookStatePill(state: string): string {
  if (state === "complete" || state === "completed") return "free";
  if (state === "running" || state === "active") return "busy";
  if (state === "failed" || state === "blocked") return "drain";
  return "info";
}

function entryStatePill(state: string): string {
  if (state === "succeeded" || state === "skipped" || state === "completed") return "free";
  if (state === "running" || state === "created") return "busy";
  if (state === "failed" || state === "blocked") return "drain";
  return "info";
}

function kpiValueClass(state: string): string {
  if (["succeeded", "skipped", "completed", "complete"].includes(state)) return "green";
  if (["running", "created", "active", "gated"].includes(state)) return "amber";
  if (["failed", "blocked"].includes(state)) return "red";
  return "";
}

function issueNumberFromRef(ref?: string | null): number | null {
  if (!ref) return null;
  const match = ref.match(/#(\d+)$/);
  return match ? Number(match[1]) : null;
}

function runTargetFromRef(ref?: string | null): { issueNumber: number; runNumber: string } | null {
  if (!ref) return null;
  const match = ref.match(/#(\d+)\/runs\/(.+)$/);
  if (!match) return null;
  return { issueNumber: Number(match[1]), runNumber: match[2] };
}

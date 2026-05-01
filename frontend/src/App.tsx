import { useEffect, useMemo, useState } from "react";
import { AdminPanel } from "./AdminPanel";
import { IssuesView } from "./IssuesView";
import { PrsView } from "./PrsView";
import { authedFetch, currentAccount, initAuth, signIn, signOut } from "./auth";
import type { AccountInfo } from "@azure/msal-browser";

type Host = {
  name: string;
  capabilities: Record<string, unknown>;
  current_lease_id: string | null;
  last_heartbeat: string | null;
  last_used_at: string | null;
  drained: boolean;
  created_at: string;
};

type Lease = {
  id: string;
  project: string;
  workflow: string | null;
  host: string | null;
  state: "pending" | "active" | "released" | "expired";
  requirements: Record<string, unknown>;
  metadata: Record<string, unknown>;
  requested_at: string;
  assigned_at: string | null;
  released_at: string | null;
  ttl_seconds: number;
};

type Project = {
  id: string;
  name: string;
  github_repo: string;
  metadata: Record<string, unknown>;
  created_at: string;
};

type Workflow = {
  id: string;
  project: string;
  name: string;
  workflow_filename: string;
  workflow_ref: string;
  trigger_label: string;
  default_requirements: Record<string, unknown>;
  metadata: Record<string, unknown>;
  created_at: string;
};

type Snapshot = {
  hosts: Host[];
  pending_leases: Lease[];
  active_leases: Lease[];
  projects: Project[];
  workflows: Workflow[];
};

type Connection = "live" | "stale" | "dead";

type Selection =
  | { kind: "all" }
  | { kind: "project"; project: string }
  | { kind: "workflow"; project: string; workflow: string };

type ViewMode = "capacity" | "issues" | "prs";

const ALL: Selection = { kind: "all" };

export function App() {
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [conn, setConn] = useState<Connection>("dead");
  const [lastUpdate, setLastUpdate] = useState<number>(0);
  const [selected, setSelected] = useState<Selection>(ALL);
  const [account, setAccount] = useState<AccountInfo | null>(null);
  const [authReady, setAuthReady] = useState(false);
  const [showAdmin, setShowAdmin] = useState(false);
  const [viewMode, setViewMode] = useState<ViewMode>("capacity");

  useEffect(() => {
    initAuth()
      .then(() => {
        setAccount(currentAccount());
        setAuthReady(true);
      })
      .catch((e) => {
        console.error("auth init failed", e);
        setAuthReady(true);
      });
  }, []);

  useEffect(() => {
    let es: EventSource | null = null;
    let staleTimer: number | null = null;

    const connect = () => {
      es = new EventSource("/v1/events");
      es.addEventListener("state", (e) => {
        try {
          setSnap(JSON.parse((e as MessageEvent).data));
          setLastUpdate(Date.now());
          setConn("live");
        } catch (err) {
          console.error("bad snapshot", err);
        }
      });
      es.onerror = () => setConn("dead");
    };

    connect();
    staleTimer = window.setInterval(() => {
      if (Date.now() - lastUpdate > 5000) setConn((c) => (c === "live" ? "stale" : c));
    }, 1000);

    return () => {
      es?.close();
      if (staleTimer !== null) window.clearInterval(staleTimer);
    };
  }, [lastUpdate]);

  const matchesSelection = (l: Lease): boolean => {
    if (selected.kind === "all") return true;
    if (selected.kind === "project") return l.project === selected.project;
    return l.project === selected.project && l.workflow === selected.workflow;
  };

  const filteredPending = useMemo(
    () => (snap ? snap.pending_leases.filter(matchesSelection) : []),
    [snap, selected]
  );
  const filteredActive = useMemo(
    () => (snap ? snap.active_leases.filter(matchesSelection) : []),
    [snap, selected]
  );

  const selectedWorkflow = useMemo(() => {
    if (!snap || selected.kind !== "workflow") return null;
    return snap.workflows.find((w) => w.project === selected.project && w.name === selected.workflow) ?? null;
  }, [snap, selected]);

  const selectedProject = useMemo(() => {
    if (!snap || selected.kind === "all") return null;
    return snap.projects.find((p) => p.name === selected.project) ?? null;
  }, [snap, selected]);

  const matchesRequirements = (host: Host, reqs: Record<string, unknown>): boolean => {
    for (const [k, want] of Object.entries(reqs)) {
      const have = (host.capabilities as Record<string, unknown>)[k];
      if (Array.isArray(want)) {
        if (!Array.isArray(have)) return false;
        for (const v of want) if (!have.includes(v)) return false;
      } else if (have !== want) {
        return false;
      }
    }
    return true;
  };

  const eligibilityReqs = selectedWorkflow?.default_requirements ?? null;

  return (
    <div className="layout">
      <aside className="sidebar">
        <div className="sidebar-title">Projects</div>
        <button
          type="button"
          className={`project-row ${selected.kind === "all" ? "selected" : ""}`}
          onClick={() => setSelected(ALL)}
        >
          <span className="name">All</span>
          <span className="count">
            {(snap?.pending_leases.length ?? 0) + (snap?.active_leases.length ?? 0)}
          </span>
        </button>
        {snap?.projects
          .slice()
          .sort((a, b) => a.name.localeCompare(b.name))
          .map((p) => {
            const projWorkflows = snap.workflows.filter((w) => w.project === p.name);
            const isProjectSelected =
              selected.kind === "project" && selected.project === p.name;
            const isWorkflowOfProjectSelected =
              selected.kind === "workflow" && selected.project === p.name;
            const projActive = snap.active_leases.filter((l) => l.project === p.name).length;
            const projPending = snap.pending_leases.filter((l) => l.project === p.name).length;
            return (
              <div key={p.name} className="project-group">
                <button
                  type="button"
                  className={`project-row ${isProjectSelected ? "selected" : ""}`}
                  onClick={() => setSelected({ kind: "project", project: p.name })}
                >
                  <span className="name">{p.name}</span>
                  <span className="count">{projActive + projPending}</span>
                </button>
                {(isProjectSelected || isWorkflowOfProjectSelected) &&
                  projWorkflows
                    .slice()
                    .sort((a, b) => a.name.localeCompare(b.name))
                    .map((w) => {
                      const wActive = snap.active_leases.filter(
                        (l) => l.project === p.name && l.workflow === w.name
                      ).length;
                      const wPending = snap.pending_leases.filter(
                        (l) => l.project === p.name && l.workflow === w.name
                      ).length;
                      const isSel =
                        selected.kind === "workflow" &&
                        selected.project === p.name &&
                        selected.workflow === w.name;
                      return (
                        <button
                          type="button"
                          key={w.name}
                          className={`workflow-row ${isSel ? "selected" : ""}`}
                          onClick={() =>
                            setSelected({ kind: "workflow", project: p.name, workflow: w.name })
                          }
                        >
                          <span className="name">{w.name}</span>
                          <span className="count">{wActive + wPending}</span>
                        </button>
                      );
                    })}
                {(isProjectSelected || isWorkflowOfProjectSelected) && projWorkflows.length === 0 && (
                  <div className="workflow-empty">no workflows</div>
                )}
              </div>
            );
          })}
      </aside>

      <main className="content">
        <header>
          <h1>glimmung</h1>
          <span className={`connection ${conn}`}>{conn}</span>
          <div className="quote">
            “The Glimmung scanned the assembled list of beings he had summoned. From a thousand worlds they had come, each with a craft to contribute.”
          </div>
          <div className="auth">
            <button
              type="button"
              className={`link ${viewMode === "capacity" ? "selected" : ""}`}
              onClick={() => setViewMode("capacity")}
              style={{ marginRight: "0.5rem" }}
            >
              capacity
            </button>
            <button
              type="button"
              className={`link ${viewMode === "issues" ? "selected" : ""}`}
              onClick={() => setViewMode("issues")}
              style={{ marginRight: "0.5rem" }}
            >
              issues
            </button>
            <button
              type="button"
              className={`link ${viewMode === "prs" ? "selected" : ""}`}
              onClick={() => setViewMode("prs")}
              style={{ marginRight: "1rem" }}
            >
              prs
            </button>
            {!authReady ? null : account ? (
              <>
                <span className="user">{account.username}</span>
                <button type="button" className="link" onClick={() => setShowAdmin((s) => !s)}>
                  {showAdmin ? "hide admin" : "admin"}
                </button>
                <button
                  type="button"
                  className="link"
                  onClick={async () => {
                    await signOut();
                    setAccount(null);
                    setShowAdmin(false);
                  }}
                >
                  sign out
                </button>
              </>
            ) : (
              <button
                type="button"
                className="link"
                onClick={async () => {
                  try {
                    setAccount(await signIn());
                  } catch (e) {
                    console.error("sign-in failed", e);
                  }
                }}
              >
                sign in
              </button>
            )}
          </div>
        </header>

        {account && showAdmin && (
          <AdminPanel projects={snap?.projects ?? []} onSuccess={() => setShowAdmin(false)} />
        )}

        {viewMode === "issues" ? (
          <IssuesView
            signedIn={!!account}
            projectFilter={selected.kind === "all" ? null : selected.project}
          />
        ) : viewMode === "prs" ? (
          <PrsView
            signedIn={!!account}
            projectFilter={selected.kind === "all" ? null : selected.project}
          />
        ) : (
          <CapacityView
            snap={snap}
            filteredPending={filteredPending}
            filteredActive={filteredActive}
            selected={selected}
            selectedWorkflow={selectedWorkflow}
            selectedProject={selectedProject}
            eligibilityReqs={eligibilityReqs}
            matchesRequirements={matchesRequirements}
            signedIn={!!account}
          />
        )}
      </main>
    </div>
  );
}

type CapacityViewProps = {
  snap: Snapshot | null;
  filteredPending: Lease[];
  filteredActive: Lease[];
  selected: Selection;
  selectedWorkflow: Workflow | null;
  selectedProject: Project | null;
  eligibilityReqs: Record<string, unknown> | null;
  matchesRequirements: (host: Host, reqs: Record<string, unknown>) => boolean;
  signedIn: boolean;
};

function CapacityView({
  snap,
  filteredPending,
  filteredActive,
  selected,
  selectedWorkflow,
  selectedProject,
  eligibilityReqs,
  matchesRequirements,
  signedIn,
}: CapacityViewProps) {
  // Two-click confirm pattern for the cancel button (#30): first click
  // arms the row (replaces the button with [Cancel?] [Keep]); second
  // click on Cancel? POSTs. The row drops out via the next /v1/state
  // SSE snapshot, so no manual list mutation is needed.
  const [confirmId, setConfirmId] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [cancelError, setCancelError] = useState<string | null>(null);

  const fireCancel = async (lease: Lease) => {
    setBusyId(lease.id);
    setCancelError(null);
    try {
      const url = `/v1/lease/${encodeURIComponent(lease.id)}/cancel?project=${encodeURIComponent(lease.project)}`;
      const r = await authedFetch(url, { method: "POST" });
      if (!r.ok) {
        const text = await r.text().catch(() => "");
        setCancelError(`cancel ${lease.id.slice(0, 8)}…: ${r.status} ${text || r.statusText}`);
      }
    } catch (e) {
      setCancelError(String(e));
    } finally {
      setBusyId(null);
      setConfirmId(null);
    }
  };

  return (
    <>
      <h2>Hosts</h2>
        {snap === null ? (
          <div className="empty">Connecting…</div>
        ) : snap.hosts.length === 0 ? (
          <div className="empty">No hosts registered. Sign in and use the admin panel to add one.</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Capabilities</th>
                <th>State</th>
                <th>Current lease</th>
                <th>Last heartbeat</th>
                <th>Last used</th>
              </tr>
            </thead>
            <tbody>
              {snap.hosts.map((h) => {
                const eligible = eligibilityReqs !== null && matchesRequirements(h, eligibilityReqs);
                return (
                  <tr key={h.name} className={eligible ? "eligible" : ""}>
                    <td className="mono">{h.name}</td>
                    <td className="mono">{JSON.stringify(h.capabilities)}</td>
                    <td>
                      {h.drained ? (
                        <span className="pill drain">drained</span>
                      ) : h.current_lease_id ? (
                        <span className="pill busy">busy</span>
                      ) : (
                        <span className="pill free">free</span>
                      )}
                    </td>
                    <td className="mono dim">{h.current_lease_id ?? "—"}</td>
                    <td className="mono dim">{relTime(h.last_heartbeat)}</td>
                    <td className="mono dim">{relTime(h.last_used_at)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}

        <h2>
          Pending queue ({filteredPending.length})
          <FilterHint selected={selected} />
        </h2>
        {filteredPending.length === 0 ? (
          <div className="empty">No leases waiting.</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Lease</th>
                <th>Project</th>
                <th>Workflow</th>
                <th>Requirements</th>
                <th>Metadata</th>
                <th>Requested</th>
              </tr>
            </thead>
            <tbody>
              {filteredPending.map((l) => (
                <tr key={l.id}>
                  <td className="mono">{l.id.slice(0, 8)}…</td>
                  <td>{l.project}</td>
                  <td className="mono dim">{l.workflow ?? "—"}</td>
                  <td className="mono">{JSON.stringify(l.requirements)}</td>
                  <td className="mono dim">{JSON.stringify(l.metadata)}</td>
                  <td className="mono dim">{relTime(l.requested_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}

        <h2>
          Active ({filteredActive.length})
          <FilterHint selected={selected} />
        </h2>
        {filteredActive.length === 0 ? (
          <div className="empty">No active leases.</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Lease</th>
                <th>Project</th>
                <th>Workflow</th>
                <th>Host</th>
                <th>Metadata</th>
                <th>Assigned</th>
                {signedIn && <th></th>}
              </tr>
            </thead>
            <tbody>
              {filteredActive.map((l) => (
                <tr key={l.id}>
                  <td className="mono">{l.id.slice(0, 8)}…</td>
                  <td>{l.project}</td>
                  <td className="mono dim">{l.workflow ?? "—"}</td>
                  <td className="mono">{l.host ?? "—"}</td>
                  <td className="mono dim">{JSON.stringify(l.metadata)}</td>
                  <td className="mono dim">{relTime(l.assigned_at)}</td>
                  {signedIn && (
                    <td>
                      {confirmId === l.id ? (
                        <>
                          <button
                            type="button"
                            className="link"
                            onClick={() => void fireCancel(l)}
                            disabled={busyId === l.id}
                            style={{ color: "var(--danger, #c44)" }}
                          >
                            {busyId === l.id ? "cancelling…" : "cancel?"}
                          </button>
                          {" / "}
                          <button
                            type="button"
                            className="link"
                            onClick={() => setConfirmId(null)}
                            disabled={busyId === l.id}
                          >
                            keep
                          </button>
                        </>
                      ) : (
                        <button
                          type="button"
                          className="link"
                          onClick={() => setConfirmId(l.id)}
                        >
                          cancel
                        </button>
                      )}
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {cancelError && (
          <div className="empty" style={{ color: "var(--danger, #c44)" }}>
            {cancelError}
          </div>
        )}

        {selectedWorkflow && (
          <>
            <h2>Workflow info</h2>
            <div className="project-info">
              <div className="row">
                <span className="key">project</span>
                <span className="val mono">{selectedWorkflow.project}</span>
              </div>
              <div className="row">
                <span className="key">workflow</span>
                <span className="val mono">{selectedWorkflow.name}</span>
              </div>
              <div className="row">
                <span className="key">file</span>
                <span className="val mono">
                  {selectedWorkflow.workflow_filename}@{selectedWorkflow.workflow_ref}
                </span>
              </div>
              <div className="row">
                <span className="key">trigger label</span>
                <span className="val mono">{selectedWorkflow.trigger_label}</span>
              </div>
              <div className="row">
                <span className="key">requires</span>
                <span className="val mono">
                  {JSON.stringify(selectedWorkflow.default_requirements)}
                </span>
              </div>
            </div>
          </>
        )}

        {selectedProject && !selectedWorkflow && (
          <>
            <h2>Project info</h2>
            <div className="project-info">
              <div className="row">
                <span className="key">name</span>
                <span className="val mono">{selectedProject.name}</span>
              </div>
              <div className="row">
                <span className="key">github</span>
                <span className="val mono">{selectedProject.github_repo}</span>
              </div>
            </div>
          </>
        )}
    </>
  );
}

function FilterHint({ selected }: { selected: Selection }) {
  if (selected.kind === "all") return null;
  const text =
    selected.kind === "project"
      ? `filtered to ${selected.project}`
      : `filtered to ${selected.project}.${selected.workflow}`;
  return <span className="filter-hint"> — {text}</span>;
}

function relTime(iso: string | null): string {
  if (!iso) return "never";
  const ms = Date.now() - new Date(iso).getTime();
  if (ms < 0) return "just now";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

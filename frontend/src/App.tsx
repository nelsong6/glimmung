import { useEffect, useMemo, useState } from "react";
import { AdminPanel } from "./AdminPanel";
import { currentAccount, initAuth, signIn, signOut } from "./auth";
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
};

type Connection = "live" | "stale" | "dead";

const ALL = "__all__";

export function App() {
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [conn, setConn] = useState<Connection>("dead");
  const [lastUpdate, setLastUpdate] = useState<number>(0);
  const [selected, setSelected] = useState<string>(ALL);
  const [account, setAccount] = useState<AccountInfo | null>(null);
  const [authReady, setAuthReady] = useState(false);
  const [showAdmin, setShowAdmin] = useState(false);

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

  const filteredPending = useMemo(() => {
    if (!snap) return [];
    return selected === ALL ? snap.pending_leases : snap.pending_leases.filter((l) => l.project === selected);
  }, [snap, selected]);

  const filteredActive = useMemo(() => {
    if (!snap) return [];
    return selected === ALL ? snap.active_leases : snap.active_leases.filter((l) => l.project === selected);
  }, [snap, selected]);

  const selectedProject = useMemo(() => {
    if (!snap || selected === ALL) return null;
    return snap.projects.find((p) => p.name === selected) ?? null;
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

  return (
    <div className="layout">
      <aside className="sidebar">
        <div className="sidebar-title">Projects</div>
        <button
          type="button"
          className={`project-row ${selected === ALL ? "selected" : ""}`}
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
            const active = snap.active_leases.filter((l) => l.project === p.name).length;
            const pending = snap.pending_leases.filter((l) => l.project === p.name).length;
            return (
              <button
                type="button"
                key={p.name}
                className={`project-row ${selected === p.name ? "selected" : ""}`}
                onClick={() => setSelected(p.name)}
              >
                <span className="name">{p.name}</span>
                <span className="count">{active + pending}</span>
              </button>
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
            {!authReady ? null : account ? (
              <>
                <span className="user">{account.username}</span>
                <button
                  type="button"
                  className="link"
                  onClick={() => setShowAdmin((s) => !s)}
                >
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
                    const acc = await signIn();
                    setAccount(acc);
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

        {account && showAdmin && <AdminPanel onSuccess={() => setShowAdmin(false)} />}

        <h2>Hosts</h2>
        {snap === null ? (
          <div className="empty">Connecting…</div>
        ) : snap.hosts.length === 0 ? (
          <div className="empty">No hosts registered. POST /v1/hosts to add one.</div>
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
                const eligible =
                  selectedProject !== null &&
                  matchesRequirements(h, selectedProject.default_requirements);
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
          {selected !== ALL && <span className="filter-hint"> — filtered to {selected}</span>}
        </h2>
        {filteredPending.length === 0 ? (
          <div className="empty">No leases waiting.</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Lease</th>
                <th>Project</th>
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
          {selected !== ALL && <span className="filter-hint"> — filtered to {selected}</span>}
        </h2>
        {filteredActive.length === 0 ? (
          <div className="empty">No active leases.</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Lease</th>
                <th>Project</th>
                <th>Host</th>
                <th>Metadata</th>
                <th>Assigned</th>
              </tr>
            </thead>
            <tbody>
              {filteredActive.map((l) => (
                <tr key={l.id}>
                  <td className="mono">{l.id.slice(0, 8)}…</td>
                  <td>{l.project}</td>
                  <td className="mono">{l.host ?? "—"}</td>
                  <td className="mono dim">{JSON.stringify(l.metadata)}</td>
                  <td className="mono dim">{relTime(l.assigned_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}

        {selectedProject && (
          <>
            <h2>Project info</h2>
            <div className="project-info">
              <div className="row">
                <span className="key">github</span>
                <span className="val mono">{selectedProject.github_repo}</span>
              </div>
              <div className="row">
                <span className="key">workflow</span>
                <span className="val mono">
                  {selectedProject.workflow_filename}@{selectedProject.workflow_ref}
                </span>
              </div>
              <div className="row">
                <span className="key">trigger label</span>
                <span className="val mono">{selectedProject.trigger_label}</span>
              </div>
              <div className="row">
                <span className="key">requires</span>
                <span className="val mono">
                  {JSON.stringify(selectedProject.default_requirements)}
                </span>
              </div>
            </div>
          </>
        )}
      </main>
    </div>
  );
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

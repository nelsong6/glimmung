import { useEffect, useState } from "react";

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

type Snapshot = {
  hosts: Host[];
  pending_leases: Lease[];
  active_leases: Lease[];
};

type Connection = "live" | "stale" | "dead";

export function App() {
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [conn, setConn] = useState<Connection>("dead");
  const [lastUpdate, setLastUpdate] = useState<number>(0);

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

  return (
    <>
      <header>
        <h1>glimmung</h1>
        <span className={`connection ${conn}`}>{conn}</span>
        <div className="quote">
          “The Glimmung scanned the assembled list of beings he had summoned. From a thousand worlds they had come, each with a craft to contribute.”
        </div>
      </header>

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
            {snap.hosts.map((h) => (
              <tr key={h.name}>
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
            ))}
          </tbody>
        </table>
      )}

      <h2>Pending queue ({snap?.pending_leases.length ?? 0})</h2>
      {!snap || snap.pending_leases.length === 0 ? (
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
            {snap.pending_leases.map((l) => (
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

      <h2>Active ({snap?.active_leases.length ?? 0})</h2>
      {!snap || snap.active_leases.length === 0 ? (
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
            {snap.active_leases.map((l) => (
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
    </>
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

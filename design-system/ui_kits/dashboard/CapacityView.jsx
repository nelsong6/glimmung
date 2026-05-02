/* global React, Pill, CountPill, Empty, MonoCell, EyebrowKey, relTime */
const { useState: useStateCV } = React;

function CapacityView({ snap, signedIn, selected, selectedWorkflow, selectedProject }) {
  const [confirmId, setConfirmId] = useStateCV(null);

  const filteredPending = snap.pending_leases.filter(matchesSelection(selected));
  const filteredActive = snap.active_leases.filter(matchesSelection(selected));

  const free = snap.hosts.filter(h => !h.drained && !h.current_lease_id).length;
  const busy = snap.hosts.filter(h => h.current_lease_id).length;
  const drained = snap.hosts.filter(h => h.drained).length;

  return (
    <>
      <h2>Hosts</h2>
      <div className="kpi-strip">
        <div className="kpi"><span className="k">hosts</span><span className="v">{snap.hosts.length}</span></div>
        <div className="kpi"><span className="k">free</span><span className="v green">{free}</span></div>
        <div className="kpi"><span className="k">busy</span><span className="v amber">{busy}</span></div>
        <div className="kpi"><span className="k">drained</span><span className="v red">{drained}</span></div>
        <div className="kpi"><span className="k">pending</span><span className="v">{snap.pending_leases.length}</span></div>
        <div className="kpi"><span className="k">active</span><span className="v">{snap.active_leases.length}</span></div>
      </div>
      <table>
        <thead><tr>
          <th>Name</th><th>Capabilities</th><th>State</th><th>Current lease</th><th>Last heartbeat</th><th>Last used</th>
        </tr></thead>
        <tbody>
          {snap.hosts.map(h => {
            const eligible = selectedWorkflow && matchesReqs(h, selectedWorkflow.default_requirements);
            return (
              <tr key={h.name} className={eligible ? "eligible" : ""}>
                <MonoCell>{h.name}</MonoCell>
                <MonoCell>{JSON.stringify(h.capabilities)}</MonoCell>
                <td>
                  {h.drained ? <Pill kind="drain">drained</Pill> :
                   h.current_lease_id ? <Pill kind="busy">busy</Pill> :
                   <Pill kind="free">free</Pill>}
                </td>
                <MonoCell dim>{h.current_lease_id ? h.current_lease_id.slice(0,8)+"…" : "—"}</MonoCell>
                <MonoCell dim>{relTime(h.last_heartbeat)}</MonoCell>
                <MonoCell dim>{relTime(h.last_used_at)}</MonoCell>
              </tr>
            );
          })}
        </tbody>
      </table>

      <h2>Pending queue ({filteredPending.length})<FilterHint selected={selected} /></h2>
      {filteredPending.length === 0 ? <Empty>No leases waiting.</Empty> : (
        <table>
          <thead><tr><th>Lease</th><th>Project</th><th>Workflow</th><th>Requires</th><th>Metadata</th><th>Requested</th></tr></thead>
          <tbody>
            {filteredPending.map(l => (
              <tr key={l.id}>
                <MonoCell>{l.id.slice(0,8)}…</MonoCell>
                <td>{l.project}</td>
                <MonoCell dim>{l.workflow ?? "—"}</MonoCell>
                <MonoCell>{JSON.stringify(l.requirements)}</MonoCell>
                <MonoCell dim>{JSON.stringify(l.metadata)}</MonoCell>
                <MonoCell dim>{relTime(l.requested_at)}</MonoCell>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2>Active ({filteredActive.length})<FilterHint selected={selected} /></h2>
      {filteredActive.length === 0 ? <Empty>No active leases.</Empty> : (
        <table>
          <thead><tr><th>Lease</th><th>Project</th><th>Workflow</th><th>Host</th><th>Metadata</th><th>Assigned</th>{signedIn && <th></th>}</tr></thead>
          <tbody>
            {filteredActive.map(l => (
              <tr key={l.id}>
                <MonoCell>{l.id.slice(0,8)}…</MonoCell>
                <td>{l.project}</td>
                <MonoCell dim>{l.workflow ?? "—"}</MonoCell>
                <MonoCell>{l.host ?? "—"}</MonoCell>
                <MonoCell dim>{JSON.stringify(l.metadata)}</MonoCell>
                <MonoCell dim>{relTime(l.assigned_at)}</MonoCell>
                {signedIn && (
                  <td>
                    {confirmId === l.id ? (
                      <>
                        <button className="link danger" onClick={() => setConfirmId(null)}>cancel?</button>
                        <span style={{ color: "var(--fg-dimmer)" }}> / </span>
                        <button className="link" onClick={() => setConfirmId(null)}>keep</button>
                      </>
                    ) : (
                      <button className="link" onClick={() => setConfirmId(l.id)}>cancel</button>
                    )}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {selectedWorkflow && (
        <>
          <h2>Workflow info</h2>
          <div className="project-info">
            <span className="key">project</span><span className="val mono">{selectedWorkflow.project}</span>
            <span className="key">workflow</span><span className="val mono">{selectedWorkflow.name}</span>
            <span className="key">file</span><span className="val mono">{selectedWorkflow.workflow_filename}@{selectedWorkflow.workflow_ref}</span>
            <span className="key">trigger label</span><span className="val mono">{selectedWorkflow.trigger_label || "—"}</span>
            <span className="key">requires</span><span className="val mono">{JSON.stringify(selectedWorkflow.default_requirements)}</span>
          </div>
        </>
      )}
      {selectedProject && !selectedWorkflow && (
        <>
          <h2>Project info</h2>
          <div className="project-info">
            <span className="key">name</span><span className="val mono">{selectedProject.name}</span>
            <span className="key">github</span><span className="val mono">{selectedProject.github_repo}</span>
          </div>
        </>
      )}
    </>
  );
}

function FilterHint({ selected }) {
  if (selected.kind === "all") return null;
  const text = selected.kind === "project"
    ? `filtered to ${selected.project}`
    : `filtered to ${selected.project}.${selected.workflow}`;
  return <span className="filter-hint"> — {text}</span>;
}

function matchesSelection(selected) {
  return (l) => {
    if (selected.kind === "all") return true;
    if (selected.kind === "project") return l.project === selected.project;
    return l.project === selected.project && l.workflow === selected.workflow;
  };
}

function matchesReqs(host, reqs) {
  for (const [k, v] of Object.entries(reqs)) {
    if (host.capabilities[k] !== v) return false;
  }
  return true;
}

window.CapacityView = CapacityView;

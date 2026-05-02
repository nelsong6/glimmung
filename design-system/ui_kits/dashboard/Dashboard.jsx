/* global React, ReactDOM, GlimmungData, CapacityView, IssuesView, PrsView, AdminPanel, Pill, LiveDot */
const { useState } = React;

const ALL = { kind: "all" };

function Dashboard() {
  const [tab, setTab] = useState("capacity");
  const [selected, setSelected] = useState(ALL);
  const [signedIn, setSignedIn] = useState(true);
  const [showAdmin, setShowAdmin] = useState(false);
  const data = GlimmungData;

  const projWorkflows = (name) => data.workflows.filter(w => w.project === name);
  const countFor = (name) =>
    data.active_leases.filter(l => l.project === name).length +
    data.pending_leases.filter(l => l.project === name).length;

  const selectedProject = selected.kind === "all" ? null : data.projects.find(p => p.name === selected.project);
  const selectedWorkflow = selected.kind === "workflow"
    ? data.workflows.find(w => w.project === selected.project && w.name === selected.workflow)
    : null;

  const projectFilter = selected.kind === "all" ? null : selected.project;

  return (
    <div className="layout">
      <aside className="sidebar">
        <div className="sidebar-title">Projects</div>
        <button className={`project-row ${selected.kind === "all" ? "selected" : ""}`} onClick={() => setSelected(ALL)}>
          <span className="name">All</span>
          <span className="count">{data.active_leases.length + data.pending_leases.length}</span>
        </button>
        {data.projects.slice().sort((a,b)=>a.name.localeCompare(b.name)).map(p => {
          const isProj = selected.kind === "project" && selected.project === p.name;
          const isWfOf = selected.kind === "workflow" && selected.project === p.name;
          const wfs = projWorkflows(p.name);
          return (
            <div key={p.name} className="project-group">
              <button className={`project-row ${isProj ? "selected" : ""}`} onClick={() => setSelected({ kind: "project", project: p.name })}>
                <span className="name">{p.name}</span>
                <span className="count">{countFor(p.name)}</span>
              </button>
              {(isProj || isWfOf) && wfs.map(w => {
                const isSel = selected.kind === "workflow" && selected.project === p.name && selected.workflow === w.name;
                return (
                  <button key={w.name} className={`workflow-row ${isSel ? "selected" : ""}`} onClick={() => setSelected({ kind: "workflow", project: p.name, workflow: w.name })}>
                    <span className="name">{w.name}</span>
                    <span className="count">{
                      data.active_leases.filter(l => l.project === p.name && l.workflow === w.name).length +
                      data.pending_leases.filter(l => l.project === p.name && l.workflow === w.name).length
                    }</span>
                  </button>
                );
              })}
              {(isProj || isWfOf) && wfs.length === 0 && <div className="workflow-empty">no workflows</div>}
            </div>
          );
        })}
      </aside>

      <main className="content">
        <header className="glimmung-header">
          <div className="header-left">
            <div className="header-title">
              <h1 className="wordmark">glimmung</h1>
              <Pill kind="free">live</Pill>
            </div>
            <div className="epigraph">"The Glimmung scanned the assembled list of beings he had summoned. From a thousand worlds they had come, each with a craft to contribute."</div>
          </div>
          <div className="header-right">
            {signedIn ? (
              <div className="user-chip">
                <button
                  className={`chip-btn ${showAdmin ? "active" : ""}`}
                  onClick={() => setShowAdmin(s => !s)}
                  title={showAdmin ? "hide admin" : "admin"}
                  aria-label="admin"
                >admin</button>
                <span className="chip-divider" />
                <span className="user-id">
                  <span className="user-dot" />
                  <span className="user-handle">nelson</span>
                </span>
                <span className="chip-divider" />
                <button
                  className="chip-btn quiet"
                  onClick={() => { setSignedIn(false); setShowAdmin(false); }}
                  title="sign out"
                  aria-label="sign out"
                >sign out</button>
              </div>
            ) : (
              <button className="chip-btn solo" onClick={() => setSignedIn(true)}>sign in</button>
            )}
          </div>
        </header>

        <div className="tabs">
          <button className={`tab ${tab === "capacity" ? "selected" : ""}`} onClick={() => setTab("capacity")}>capacity</button>
          <button className={`tab ${tab === "issues" ? "selected" : ""}`} onClick={() => setTab("issues")}>
            issues {data.issues.some(i => i.issue_lock_held) && <span className="tab-dot" />}
          </button>
          <button className={`tab ${tab === "prs" ? "selected" : ""}`} onClick={() => setTab("prs")}>
            prs {data.prs.some(p => p.pr_lock_held) && <span className="tab-dot" />}
          </button>
        </div>

        {signedIn && showAdmin && <AdminPanel projects={data.projects} onClose={() => setShowAdmin(false)} />}

        <div className="tab-panel">
          {tab === "capacity" && (
            <CapacityView snap={data} signedIn={signedIn} selected={selected}
              selectedWorkflow={selectedWorkflow} selectedProject={selectedProject} />
          )}
          {tab === "issues" && <IssuesView data={data} projectFilter={projectFilter} />}
          {tab === "prs" && <PrsView data={data} projectFilter={projectFilter} />}
        </div>
      </main>
    </div>
  );
}

ReactDOM.createRoot(document.getElementById("root")).render(<Dashboard />);

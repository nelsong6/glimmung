import { useEffect, useMemo, useState } from "react";
import { Link, Navigate, NavLink, Outlet, Route, Routes, useLocation, useOutletContext, useParams } from "react-router-dom";
import { AdminPanel } from "./AdminPanel";
import { GraphView } from "./GraphView";
import { IssueDetailView } from "./IssueDetailView";
import { IssuesView } from "./IssuesView";
import { ReportDetailView } from "./ReportDetailView";
import { ReportsView } from "./ReportsView";
import { StyleguideView } from "./StyleguideView";
import { authedFetch, currentAccount, initAuth, signIn, signOut } from "./auth";
import { isMockMode, mockSnapshot } from "./mockApi";
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

type Inflight = { issues: boolean };

type Selection =
  | { kind: "all" }
  | { kind: "project"; project: string }
  | { kind: "workflow"; project: string; workflow: string };

type LayoutContext = {
  snap: Snapshot | null;
  signedIn: boolean;
  selected: Selection;
  filteredPending: Lease[];
  filteredActive: Lease[];
  selectedWorkflow: Workflow | null;
  selectedProject: Project | null;
  eligibilityReqs: Record<string, unknown> | null;
  matchesRequirements: (host: Host, reqs: Record<string, unknown>) => boolean;
};

const ALL: Selection = { kind: "all" };

export function App() {
  // Routes-only — Layout owns the SSE/auth state and provides it via Outlet
  // context so deep-link reloads (e.g. /issues/<owner>/<repo>/<n>) land
  // straight on the right view without a viewMode flip.
  return (
    <Routes>
      {/* Platform route: visual catalog of components, served outside
          Layout so the validation-step curl check (#86) doesn't need
          auth or SSE. Contract: docs/styleguide-contract.md. */}
      <Route path="/_styleguide" element={<StyleguideView />} />
      <Route path="/_design-portfolio" element={<StyleguideView />} />
      <Route path="/_mock/*" element={<MockModeRedirect />} />
      <Route path="/" element={<Layout />}>
        <Route index element={<CapacityRoute />} />
        <Route path="graph" element={<GraphRoute />} />
        <Route path="projects" element={<ProjectsRoute />} />
        <Route path="projects/:project" element={<ProjectRoute />} />
        <Route path="issues" element={<IssuesRoute />} />
        <Route path="issues/:owner/:repo/:n" element={<IssueDetailView />}>
          {/* Issue workspace tabs. Old slugs are still accepted by
              IssueDetailView so existing links keep working. */}
          <Route path="issue" element={null} />
          <Route path="the-run" element={null} />
          <Route path="runs" element={null} />
          <Route path="touchpoint" element={null} />
          {/* Backwards-compat: pre-#81 tab slugs. SLUG_TO_TAB in
              IssueDetailView maps these to the new tabs so deep links
              from before the rename keep working. */}
          <Route path="description" element={null} />
          <Route path="in-progress" element={null} />
          <Route path="lineage" element={null} />
        </Route>
        <Route path="issues/:project/:issueId" element={<IssueDetailView />}>
          <Route path="issue" element={null} />
          <Route path="the-run" element={null} />
          <Route path="runs" element={null} />
          <Route path="touchpoint" element={null} />
          <Route path="description" element={null} />
          <Route path="in-progress" element={null} />
          <Route path="lineage" element={null} />
        </Route>
        <Route path="reports" element={<ReportsRoute />} />
        <Route path="reports/:owner/:repo/:n" element={<ReportDetailView />} />
      </Route>
    </Routes>
  );
}

function MockModeRedirect() {
  isMockMode();
  return <Navigate to="/" replace />;
}

function Layout() {
  const location = useLocation();
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [conn, setConn] = useState<Connection>("dead");
  const [lastUpdate, setLastUpdate] = useState<number>(0);
  const selected = ALL;
  const [account, setAccount] = useState<AccountInfo | null>(null);
  const [authReady, setAuthReady] = useState(false);
  const [showAdmin, setShowAdmin] = useState(false);
  const [inflight, setInflight] = useState<Inflight>({ issues: false });

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
    if (isMockMode()) {
      setSnap(mockSnapshot as Snapshot);
      setLastUpdate(Date.now());
      setConn("live");
      return;
    }

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

  // Poll /v1/issues + /v1/reports to drive the issue-workspace pulse
  // when issue work or touchpoint review is in flight. Reports are no
  // longer primary navigation, but their locks still matter to issues.
  useEffect(() => {
    let cancelled = false;
    const check = async () => {
      try {
        const [iRes, pRes] = await Promise.all([
          fetch("/v1/issues"),
          fetch("/v1/reports"),
        ]);
        const issues = iRes.ok ? ((await iRes.json()) as Array<{ issue_lock_held?: boolean }>) : [];
        const reports = pRes.ok ? ((await pRes.json()) as Array<{ pr_lock_held?: boolean }>) : [];
        if (cancelled) return;
        setInflight({
          issues:
            (Array.isArray(issues) && issues.some((x) => x.issue_lock_held))
            || (Array.isArray(reports) && reports.some((x) => x.pr_lock_held)),
        });
      } catch {
        // keep last value on transient failures
      }
    };
    void check();
    const t = window.setInterval(() => void check(), 20000);
    return () => {
      cancelled = true;
      window.clearInterval(t);
    };
  }, []);

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

  const ctx: LayoutContext = {
    snap,
    signedIn: !!account,
    selected,
    filteredPending,
    filteredActive,
    selectedWorkflow,
    selectedProject,
    eligibilityReqs,
    matchesRequirements,
  };

  const dashboardLinkClass = ({ isActive }: { isActive: boolean }) =>
    `dashboard-nav-link ${isActive ? "selected" : ""}`;
  const dashboardWorkspace =
    location.pathname === "/" || location.pathname === "/issues" || location.pathname === "/graph" || location.pathname === "/projects";
  const breadcrumbs = buildBreadcrumbs(location.pathname);

  return (
    <div className="layout">
      <main className="content">
        <header>
          <div className="header-left">
            <div className="header-title">
              <h1>glimmung</h1>
              {isMockMode() && <span className="connection info">mock</span>}
              <span className={`connection ${conn}`}>{conn}</span>
            </div>
            <div className="epigraph">
              “The Glimmung scanned the assembled list of beings he had summoned. From a thousand worlds they had come, each with a craft to contribute.”
            </div>
          </div>
          <div className="header-right">
            {!authReady ? null : account ? (
              <div className="user-cluster">
                <button
                  type="button"
                  className={`gb sm${showAdmin ? " active" : ""}`}
                  onClick={() => setShowAdmin((s) => !s)}
                  aria-label="admin"
                  title={showAdmin ? "hide admin" : "admin"}
                >
                  <span className="sigil">∷</span>
                  <span className="label">admin</span>
                </button>
                <span className="user-id">
                  <span className="user-dot" />
                  <span className="user-handle">{account.username}</span>
                </span>
                <button
                  type="button"
                  className="gb sm quiet"
                  onClick={async () => {
                    await signOut();
                    setAccount(null);
                    setShowAdmin(false);
                  }}
                  aria-label="sign out"
                  title="sign out"
                >
                  <span className="label">sign out</span>
                </button>
              </div>
            ) : (
              <button
                type="button"
                className="gb primary"
                onClick={async () => {
                  try {
                    setAccount(await signIn());
                  } catch (e) {
                    console.error("sign-in failed", e);
                  }
                }}
              >
                <span className="sigil">›</span>
                <span className="label">sign in</span>
              </button>
            )}
          </div>
        </header>

        <nav className="workspace-breadcrumb app-breadcrumb" aria-label="breadcrumb">
          {breadcrumbs.map((crumb, index) => (
            <span className="breadcrumb-segment" key={`${crumb.label}:${index}`}>
              {index > 0 && <span className="breadcrumb-sep">/</span>}
              {crumb.to && index < breadcrumbs.length - 1 ? (
                <Link to={crumb.to}>{crumb.label}</Link>
              ) : (
                <strong>{crumb.label}</strong>
              )}
            </span>
          ))}
        </nav>

        {dashboardWorkspace && (
            <nav className="dashboard-nav" aria-label="dashboard views">
              <NavLink to="/" end className={dashboardLinkClass}>
                capacity
              </NavLink>
              <NavLink to="/issues" className={dashboardLinkClass}>
                issues
                {inflight.issues && <span className="tab-dot" />}
              </NavLink>
              <NavLink to="/graph" className={dashboardLinkClass}>
                graph
              </NavLink>
              <NavLink to="/projects" className={dashboardLinkClass}>
                projects
              </NavLink>
            </nav>
        )}

        {account && showAdmin && (
          <AdminPanel projects={snap?.projects ?? []} onSuccess={() => setShowAdmin(false)} />
        )}

        <Outlet context={ctx} />
      </main>
    </div>
  );
}

type Breadcrumb = {
  label: string;
  to?: string;
};

function buildBreadcrumbs(pathname: string): Breadcrumb[] {
  const parts = pathname.split("/").filter(Boolean).map(decodeURIComponent);
  if (parts.length === 0) return [{ label: "Dashboard" }];
  if (parts[0] === "projects") {
    const crumbs: Breadcrumb[] = [
      { label: "Dashboard", to: "/" },
      { label: "Projects", to: "/projects" },
    ];
    if (parts[1]) crumbs.push({ label: parts[1] });
    return crumbs;
  }
  if (parts[0] === "issues") {
    const crumbs: Breadcrumb[] = [{ label: "Dashboard", to: "/" }];
    if (parts.length >= 4) {
      const owner = parts[1];
      const repo = parts[2];
      const issue = parts[3];
      crumbs.push({ label: "Issues", to: "/issues" });
      crumbs.push({ label: `${owner}/${repo}` });
      crumbs.push({ label: `#${issue}` });
      return crumbs;
    }
    if (parts.length >= 3) {
      crumbs.push({ label: "Projects", to: "/projects" });
      crumbs.push({ label: parts[1], to: `/projects/${encodeURIComponent(parts[1])}` });
      crumbs.push({ label: parts[2] });
      return crumbs;
    }
    return [{ label: "Dashboard", to: "/" }, { label: "Issues" }];
  }
  if (parts[0] === "graph") return [{ label: "Dashboard", to: "/" }, { label: "Graph" }];
  if (parts[0] === "reports") return [{ label: "Dashboard", to: "/" }, { label: "Touchpoint evidence" }];
  return [{ label: "Dashboard", to: "/" }, { label: parts[0] }];
}

function CapacityRoute() {
  const ctx = useOutletContext<LayoutContext>();
  return <CapacityView {...ctx} />;
}

function IssuesRoute() {
  const { signedIn, selected } = useOutletContext<LayoutContext>();
  return (
    <IssuesView
      signedIn={signedIn}
      projectFilter={selected.kind === "all" ? null : selected.project}
    />
  );
}

function GraphRoute() {
  const { selected } = useOutletContext<LayoutContext>();
  return (
    <GraphView
      projectFilter={selected.kind === "all" ? null : selected.project}
    />
  );
}

function ProjectsRoute() {
  const ctx = useOutletContext<LayoutContext>();
  return <ProjectsView {...ctx} />;
}

function ProjectRoute() {
  const params = useParams<{ project?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return <ProjectView {...ctx} projectName={decodeURIComponent(params.project ?? "")} />;
}

function ReportsRoute() {
  const { selected } = useOutletContext<LayoutContext>();
  return (
    <ReportsView
      projectFilter={selected.kind === "all" ? null : selected.project}
    />
  );
}

function ProjectsView({ snap }: LayoutContext) {
  if (snap === null) return <div className="empty">Connecting…</div>;

  const projects = snap.projects
    .slice()
    .sort((a, b) => a.name.localeCompare(b.name));

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">dashboard</div>
          <h2>Projects</h2>
          <div className="project-repo mono">registered repos and project-scoped workspaces</div>
        </div>
        <div className="project-facts">
          <div className="project-fact">
            <span>projects</span>
            <strong>{projects.length}</strong>
          </div>
          <div className="project-fact">
            <span>workflows</span>
            <strong>{snap.workflows.length}</strong>
          </div>
          <div className="project-fact">
            <span>active</span>
            <strong>{snap.active_leases.length}</strong>
          </div>
          <div className="project-fact">
            <span>pending</span>
            <strong>{snap.pending_leases.length}</strong>
          </div>
        </div>
      </section>

      {projects.length === 0 ? (
        <div className="empty">No projects registered.</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Project</th>
              <th>GitHub</th>
              <th>Workflows</th>
              <th>Work</th>
              <th>Hosts</th>
            </tr>
          </thead>
          <tbody>
            {projects.map((project) => {
              const workflows = snap.workflows.filter((w) => w.project === project.name);
              const pending = snap.pending_leases.filter((l) => l.project === project.name);
              const active = snap.active_leases.filter((l) => l.project === project.name);
              const activeHosts = new Set(active.flatMap((l) => (l.host ? [l.host] : [])));
              return (
                <tr key={project.id}>
                  <td>
                    <Link className="link" to={`/projects/${encodeURIComponent(project.name)}`}>
                      {project.name}
                    </Link>
                  </td>
                  <td className="mono dim">{project.github_repo}</td>
                  <td className="mono">{workflows.length}</td>
                  <td className="mono dim">{active.length} active / {pending.length} pending</td>
                  <td className="mono dim">
                    {activeHosts.size > 0 ? Array.from(activeHosts).join(", ") : "—"}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

function ProjectView({
  snap,
  signedIn,
  projectName,
}: LayoutContext & { projectName: string }) {
  if (snap === null) return <div className="empty">Connecting…</div>;

  const project = snap.projects.find((p) => p.name === projectName);
  if (!project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }

  const workflows = snap.workflows
    .filter((w) => w.project === project.name)
    .slice()
    .sort((a, b) => a.name.localeCompare(b.name));
  const pending = snap.pending_leases.filter((l) => l.project === project.name);
  const active = snap.active_leases.filter((l) => l.project === project.name);
  const activeHosts = new Set(active.flatMap((l) => (l.host ? [l.host] : [])));
  const currentWork = [...active, ...pending];
  const nextWork = currentWork[0] ?? null;

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">project</div>
          <h2>{project.name}</h2>
          <div className="project-repo mono">{project.github_repo}</div>
        </div>
        <div className="project-facts">
          <div className="project-fact">
            <span>workflows</span>
            <strong>{workflows.length}</strong>
          </div>
          <div className="project-fact">
            <span>active</span>
            <strong>{active.length}</strong>
          </div>
          <div className="project-fact">
            <span>pending</span>
            <strong>{pending.length}</strong>
          </div>
          <div className="project-fact">
            <span>hosts</span>
            <strong>{activeHosts.size}</strong>
          </div>
        </div>
      </section>

      <section className="project-focus">
        <div>
          <span className="key">current focus</span>
          {nextWork ? (
            <strong>{String(nextWork.metadata.title ?? nextWork.metadata.issue ?? nextWork.workflow ?? nextWork.id)}</strong>
          ) : (
            <strong>No active project work</strong>
          )}
        </div>
        <div>
          <span className="key">assigned hosts</span>
          <span className="mono">{activeHosts.size > 0 ? Array.from(activeHosts).join(", ") : "none assigned"}</span>
        </div>
      </section>

      <h2>Workflows</h2>
      {workflows.length === 0 ? (
        <div className="empty">No workflows registered for {project.name}.</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>File</th>
              <th>Trigger</th>
              <th>Requires</th>
              <th>Work</th>
            </tr>
          </thead>
          <tbody>
            {workflows.map((w) => {
              const wPending = pending.filter((l) => l.workflow === w.name).length;
              const wActive = active.filter((l) => l.workflow === w.name).length;
              return (
                <tr key={w.id}>
                  <td>{w.name}</td>
                  <td className="mono dim">{w.workflow_filename}@{w.workflow_ref}</td>
                  <td className="mono dim">{w.trigger_label}</td>
                  <td className="mono">{JSON.stringify(w.default_requirements)}</td>
                  <td className="mono dim">{wActive} active / {wPending} pending</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      <h2>Current work</h2>
      {currentWork.length === 0 ? (
        <div className="empty">No active or pending work for {project.name}.</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Lease</th>
              <th>Workflow</th>
              <th>State</th>
              <th>Host</th>
              <th>Metadata</th>
              <th>Requested</th>
            </tr>
          </thead>
          <tbody>
            {currentWork.map((l) => (
              <tr key={l.id}>
                <td className="mono">{l.id.slice(0, 8)}…</td>
                <td className="mono dim">{l.workflow ?? "—"}</td>
                <td><span className={`pill ${l.state === "active" ? "busy" : "info"}`}>{l.state}</span></td>
                <td className="mono">{l.host ?? "—"}</td>
                <td className="mono dim">{JSON.stringify(l.metadata)}</td>
                <td className="mono dim">{relTime(l.requested_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <IssuesView signedIn={signedIn} projectFilter={project.name} />
    </div>
  );
}

type CapacityViewProps = {
  snap: Snapshot | null;
  signedIn: boolean;
  filteredPending: Lease[];
  filteredActive: Lease[];
  selected: Selection;
  selectedWorkflow: Workflow | null;
  selectedProject: Project | null;
  eligibilityReqs: Record<string, unknown> | null;
  matchesRequirements: (host: Host, reqs: Record<string, unknown>) => boolean;
};

function CapacityView({
  snap,
  signedIn,
  filteredPending,
  filteredActive,
  selected,
  selectedWorkflow,
  selectedProject,
  eligibilityReqs,
  matchesRequirements,
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

  const free = snap?.hosts.filter((h) => !h.drained && !h.current_lease_id).length ?? 0;
  const busy = snap?.hosts.filter((h) => !h.drained && h.current_lease_id).length ?? 0;
  const drained = snap?.hosts.filter((h) => h.drained).length ?? 0;

  return (
    <>
      {snap !== null && (
        <div className="kpi-strip">
          <div className="kpi"><span className="k">hosts</span><span className="v">{snap.hosts.length}</span></div>
          <div className="kpi"><span className="k">free</span><span className="v green">{free}</span></div>
          <div className="kpi"><span className="k">busy</span><span className="v amber">{busy}</span></div>
          <div className="kpi"><span className="k">drained</span><span className="v red">{drained}</span></div>
          <div className="kpi"><span className="k">pending</span><span className="v">{snap.pending_leases.length}</span></div>
          <div className="kpi"><span className="k">active</span><span className="v">{snap.active_leases.length}</span></div>
        </div>
      )}
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
                          <span className="confirm">
                            <button
                              type="button"
                              className="link danger-text"
                              onClick={() => void fireCancel(l)}
                              disabled={busyId === l.id}
                            >
                              {busyId === l.id ? "cancelling…" : "cancel?"}
                            </button>
                            <span className="sep">/</span>
                            <button
                              type="button"
                              className="link"
                              onClick={() => setConfirmId(null)}
                              disabled={busyId === l.id}
                            >
                              keep
                            </button>
                          </span>
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
          <div className="empty" style={{ color: "var(--state-danger-fg)" }}>
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

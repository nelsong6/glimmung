import { Fragment, useEffect, useMemo, useState } from "react";
import { Link, Navigate, NavLink, Outlet, Route, Routes, useLocation, useNavigate, useOutletContext, useParams } from "react-router-dom";
import { AdminPanel } from "./AdminPanel";
import { IssueDetailView, RunViewer, type AbortState, type DispatchState, type IssueGraph } from "./IssueDetailView";
import { IssuesView } from "./IssuesView";
import { ReportDetailView } from "./ReportDetailView";
import { ReportsView } from "./ReportsView";
import { StyleguideView } from "./StyleguideView";
import { authedFetch, currentAccount, initAuth, signIn, signOut } from "./auth";
import { isMockMode, mockRuns, mockSnapshot } from "./mockApi";
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
  phases: PhaseSpec[];
  pr: PrPrimitiveSpec;
  workflow_filename: string;
  workflow_ref: string;
  trigger_label: string;
  default_requirements: Record<string, unknown>;
  metadata: Record<string, unknown>;
  created_at: string;
};

type PhaseSpec = {
  name: string;
  kind: string;
  workflow_filename: string;
  workflow_ref: string;
  inputs: Record<string, string>;
  outputs: string[];
  requirements: Record<string, unknown> | null;
  verify: boolean;
  recycle_policy: RecyclePolicy | null;
};

type PrPrimitiveSpec = {
  enabled: boolean;
  recycle_policy: RecyclePolicy | null;
};

type RecyclePolicy = {
  max_attempts: number;
  on: string[];
  lands_at: string;
};

type ProjectRun = {
  id: string;
  project: string;
  workflow: string;
  issue_number: number | null;
  title: string;
  state: string;
  cycles: number;
  current_phase: string;
  cost_usd: number;
  started_at: string;
  updated_at: string;
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
        <Route index element={<HomeRoute />} />
        <Route path="dashboard" element={<DashboardRoute />} />
        <Route path="needs-attention" element={<NeedsAttentionRoute />} />
        <Route path="graph" element={<Navigate to="/dashboard" replace />} />
        <Route path="projects" element={<ProjectsRoute />} />
        <Route path="projects/new" element={<ProjectOnboardingRoute />} />
        <Route path="projects/:project" element={<ProjectRoute />} />
        <Route path="projects/:project/workflows" element={<ProjectWorkflowsRoute />} />
        <Route path="projects/:project/workflows/:workflow" element={<ProjectWorkflowRoute />} />
        <Route path="projects/:project/issues" element={<ProjectIssuesRoute />} />
        <Route path="projects/:project/issues/new" element={<IssueOnboardingRoute />} />
        <Route path="projects/:project/issues/:issueNumber/runs/:runId" element={<ProjectRunRoute />} />
        <Route path="projects/:project/issues/:issueNumber" element={<IssueDetailView />}>
          <Route path="summary" element={null} />
          <Route path="issue" element={null} />
          <Route path="runs" element={null} />
          <Route path="touchpoint" element={null} />
          <Route path="description" element={null} />
          <Route path="the-run" element={null} />
          <Route path="in-progress" element={null} />
          <Route path="lineage" element={null} />
        </Route>
        <Route path="projects/:project/needs-attention" element={<ProjectNeedsAttentionRoute />} />
        <Route path="projects/:project/runs" element={<ProjectRunsRoute />} />
        <Route path="projects/:project/runs/:runId" element={<ProjectRunRedirectRoute />} />
        <Route path="issues" element={<Navigate to="/needs-attention" replace />} />
        <Route path="issues/:owner/:repo/:n" element={<IssueDetailView />}>
          {/* Issue workspace tabs. Old slugs are still accepted by
              IssueDetailView so existing links keep working. */}
          <Route path="summary" element={null} />
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
          <Route path="summary" element={null} />
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
  const homeRoute = location.pathname === "/";
  const breadcrumbs = buildBreadcrumbs(location.pathname, snap?.projects ?? []);
  const returnTarget = returnTargetFromState(location.state, location.pathname);

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
          <div className="breadcrumb-trail">
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
          </div>
          {returnTarget && (
            <Link className="breadcrumb-return" to={returnTarget.to}>
              ← return to {returnTarget.label}
            </Link>
          )}
        </nav>

        {homeRoute && (
            <nav className="dashboard-nav" aria-label="dashboard views">
              <NavLink to="/dashboard" className={dashboardLinkClass}>
                dashboard
              </NavLink>
              <NavLink to="/needs-attention" className={dashboardLinkClass}>
                needs attention
                {inflight.issues && <span className="tab-dot" />}
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

type ReturnNavigationState = {
  returnTo?: unknown;
  returnLabel?: unknown;
};

function returnTargetFromState(state: unknown, currentPath: string): { to: string; label: string } | null {
  if (typeof state !== "object" || state === null) return null;
  const candidate = state as ReturnNavigationState;
  if (typeof candidate.returnTo !== "string" || !candidate.returnTo.startsWith("/")) return null;
  if (candidate.returnTo === currentPath) return null;
  const label = typeof candidate.returnLabel === "string" && candidate.returnLabel.trim()
    ? candidate.returnLabel
    : "previous view";
  return { to: candidate.returnTo, label };
}

function buildBreadcrumbs(pathname: string, projects: Project[]): Breadcrumb[] {
  const parts = pathname.split("/").filter(Boolean).map(decodeURIComponent);
  if (parts.length === 0) return [{ label: "Home" }];
  if (parts[0] === "dashboard") {
    return [{ label: "Home", to: "/" }, { label: "Dashboard" }];
  }
  if (parts[0] === "needs-attention") {
    return [{ label: "Home", to: "/" }, { label: "Needs attention" }];
  }
  if (parts[0] === "projects") {
    const crumbs: Breadcrumb[] = [
      { label: "Home", to: "/" },
      { label: "Projects", to: "/projects" },
    ];
    if (parts[1]) crumbs.push({ label: parts[1], to: `/projects/${encodeURIComponent(parts[1])}` });
    if (parts[1] === "new") {
      crumbs[crumbs.length - 1] = { label: "New project" };
    } else if (parts[2] === "workflows") {
      crumbs.push({ label: "Workflows", to: `/projects/${encodeURIComponent(parts[1] ?? "")}/workflows` });
      if (parts[3]) crumbs.push({ label: parts[3] });
    } else if (parts[2] === "issues") {
      crumbs.push({ label: "Issues", to: `/projects/${encodeURIComponent(parts[1] ?? "")}/issues` });
      if (parts[3] === "new") {
        crumbs.push({ label: "New issue" });
      } else if (parts[3]) {
        crumbs.push({
          label: `#${parts[3]}`,
          to: parts[4] ? `/projects/${encodeURIComponent(parts[1] ?? "")}/issues/${encodeURIComponent(parts[3])}` : undefined,
        });
      }
      if (parts[4] === "runs") {
        crumbs.push({
          label: "Runs",
          to: `/projects/${encodeURIComponent(parts[1] ?? "")}/issues/${encodeURIComponent(parts[3] ?? "")}/runs`,
        });
        if (parts[5]) crumbs.push({ label: runSlugDisplay(parts[5]) });
      } else if (parts[4]) {
        crumbs.push({ label: titleCase(parts[4]) });
      }
    } else if (parts[2] === "needs-attention") {
      crumbs.push({ label: "Needs attention" });
    } else if (parts[2] === "runs") {
      crumbs.push({ label: "Runs", to: `/projects/${encodeURIComponent(parts[1] ?? "")}/runs` });
      if (parts[3]) crumbs.push({ label: runSlugDisplay(parts[3]) });
    }
    return crumbs;
  }
  if (parts[0] === "issues") {
    const crumbs: Breadcrumb[] = [{ label: "Home", to: "/" }];
    if (parts.length >= 4) {
      const owner = parts[1];
      const repo = parts[2];
      const issue = parts[3];
      const githubRepo = `${owner}/${repo}`;
      const project = projects.find((p) => p.github_repo === githubRepo);
      if (project) {
        crumbs.push({ label: "Projects", to: "/projects" });
        crumbs.push({ label: project.name, to: `/projects/${encodeURIComponent(project.name)}` });
        crumbs.push({ label: "Issues", to: `/projects/${encodeURIComponent(project.name)}/issues` });
      } else {
        crumbs.push({ label: "Needs attention", to: "/needs-attention" });
        crumbs.push({ label: githubRepo });
      }
      crumbs.push({ label: `#${issue}` });
      if (parts[4]) crumbs.push({ label: titleCase(parts[4]) });
      return crumbs;
    }
    if (parts.length >= 3) {
      crumbs.push({ label: "Projects", to: "/projects" });
      crumbs.push({ label: parts[1], to: `/projects/${encodeURIComponent(parts[1])}` });
      crumbs.push({ label: "Issues", to: `/projects/${encodeURIComponent(parts[1])}/issues` });
      crumbs.push({ label: parts[2] });
      if (parts[3]) crumbs.push({ label: titleCase(parts[3]) });
      return crumbs;
    }
    return [{ label: "Home", to: "/" }, { label: "Needs attention" }];
  }
  if (parts[0] === "reports") return [{ label: "Home", to: "/" }, { label: "Touchpoint evidence" }];
  return [{ label: "Home", to: "/" }, { label: parts[0] }];
}

function HomeRoute() {
  const ctx = useOutletContext<LayoutContext>();
  return <HomeView {...ctx} />;
}

function DashboardRoute() {
  const ctx = useOutletContext<LayoutContext>();
  return <CapacityView {...ctx} />;
}

function NeedsAttentionRoute() {
  const { signedIn } = useOutletContext<LayoutContext>();
  return <IssuesView signedIn={signedIn} projectFilter={null} headingLabel="Needs attention" />;
}

function ProjectsRoute() {
  const ctx = useOutletContext<LayoutContext>();
  return <ProjectsView {...ctx} />;
}

function ProjectOnboardingRoute() {
  const ctx = useOutletContext<LayoutContext>();
  return <ProjectOnboardingView {...ctx} />;
}

function ProjectRoute() {
  const params = useParams<{ project?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return <ProjectView {...ctx} projectName={decodeURIComponent(params.project ?? "")} />;
}

function ProjectWorkflowsRoute() {
  const params = useParams<{ project?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return <ProjectWorkflowsView {...ctx} projectName={decodeURIComponent(params.project ?? "")} />;
}

function ProjectWorkflowRoute() {
  const params = useParams<{ project?: string; workflow?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return (
    <ProjectWorkflowView
      {...ctx}
      projectName={decodeURIComponent(params.project ?? "")}
      workflowName={decodeURIComponent(params.workflow ?? "")}
    />
  );
}

function ProjectIssuesRoute() {
  const params = useParams<{ project?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return <ProjectIssuesView {...ctx} projectName={decodeURIComponent(params.project ?? "")} />;
}

function IssueOnboardingRoute() {
  const params = useParams<{ project?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return <IssueOnboardingView {...ctx} projectName={decodeURIComponent(params.project ?? "")} />;
}

function ProjectNeedsAttentionRoute() {
  const params = useParams<{ project?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return <ProjectNeedsAttentionView {...ctx} projectName={decodeURIComponent(params.project ?? "")} />;
}

function ProjectRunsRoute() {
  const params = useParams<{ project?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return <ProjectRunsView {...ctx} projectName={decodeURIComponent(params.project ?? "")} />;
}

function ProjectRunRoute() {
  const params = useParams<{ project?: string; issueNumber?: string; runId?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return (
    <ProjectRunView
      {...ctx}
      projectName={decodeURIComponent(params.project ?? "")}
      issueNumber={params.issueNumber ? parseInt(params.issueNumber, 10) : null}
      runId={decodeURIComponent(params.runId ?? "")}
    />
  );
}

function ProjectRunRedirectRoute() {
  const params = useParams<{ project?: string; runId?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  const projectName = decodeURIComponent(params.project ?? "");
  const runId = decodeURIComponent(params.runId ?? "");
  if (ctx.snap === null) return <div className="empty">Connecting…</div>;
  const runs = isMockMode()
    ? mockRuns.filter((candidate) => candidate.project === projectName)
    : [];
  const run = isMockMode() ? resolveProjectRun(runs, runId) : null;
  const index = run ? runs.findIndex((candidate) => candidate.id === run.id) : -1;
  if (run?.issue_number !== null && run?.issue_number !== undefined) {
    const slug = projectRunSlug(run, runs, index >= 0 ? index : 0);
    return (
      <Navigate
        to={`/projects/${encodeURIComponent(projectName)}/issues/${run.issue_number}/runs/${encodeURIComponent(slug)}`}
        replace
      />
    );
  }
  return (
    <ProjectRunView
      {...ctx}
      projectName={projectName}
      issueNumber={null}
      runId={runId}
    />
  );
}

function ReportsRoute() {
  const { selected } = useOutletContext<LayoutContext>();
  return (
    <ReportsView
      projectFilter={selected.kind === "all" ? null : selected.project}
    />
  );
}

function HomeView({ snap }: LayoutContext) {
  const projects = snap?.projects.length ?? 0;
  const workflows = snap?.workflows.length ?? 0;
  const active = snap?.active_leases.length ?? 0;
  const pending = snap?.pending_leases.length ?? 0;

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">home</div>
          <h2>Glimmung coordinates agent work across projects</h2>
          <div className="project-repo mono">
            capacity, issue runs, touchpoints, and project-scoped workflow state
          </div>
        </div>
        <div className="project-facts">
          <div className="project-fact">
            <span>projects</span>
            <strong>{projects}</strong>
          </div>
          <div className="project-fact">
            <span>workflows</span>
            <strong>{workflows}</strong>
          </div>
          <div className="project-fact">
            <span>active</span>
            <strong>{active}</strong>
          </div>
          <div className="project-fact">
            <span>pending</span>
            <strong>{pending}</strong>
          </div>
        </div>
      </section>

      <section className="home-links" aria-label="primary destinations">
        <Link to="/dashboard" className="home-link">
          <span className="key">Dashboard</span>
          <strong>System health, hosts, and queue state</strong>
        </Link>
        <Link to="/needs-attention" className="home-link">
          <span className="key">Needs attention</span>
          <strong>Open work that needs a decision or follow-up</strong>
        </Link>
        <Link to="/projects" className="home-link">
          <span className="key">Projects</span>
          <strong>Project workspaces, workflows, and scoped issues</strong>
        </Link>
      </section>
    </div>
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

      <div className="section-actions">
        <Link className="link" to="/projects/new">new project</Link>
      </div>

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
                  <td className="mono dim">
                    <a
                      className="link"
                      href={`https://github.com/${project.github_repo}`}
                    >
                      {project.github_repo}
                    </a>
                  </td>
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
  const projectPath = `/projects/${encodeURIComponent(project.name)}`;

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">project</div>
          <h2>{project.name}</h2>
          <div className="project-repo mono">
            <a className="link" href={`https://github.com/${project.github_repo}`}>
              {project.github_repo}
            </a>
          </div>
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

      <section className="home-links" aria-label={`${project.name} destinations`}>
        <Link to={`${projectPath}/workflows`} className="home-link">
          <span className="key">Workflows</span>
          <strong>Definitions, triggers, requirements, and workflow-scoped work</strong>
        </Link>
        <Link to={`${projectPath}/issues`} className="home-link">
          <span className="key">Issues</span>
          <strong>All open issues for {project.name}</strong>
        </Link>
        <Link to={`${projectPath}/needs-attention`} className="home-link">
          <span className="key">Needs attention</span>
          <strong>Project work that needs a decision or follow-up</strong>
        </Link>
        <Link to={`${projectPath}/runs`} className="home-link">
          <span className="key">Runs</span>
          <strong>Run and cycle history for project work</strong>
        </Link>
      </section>

      <IssuesView
        signedIn={signedIn}
        projectFilter={project.name}
        headingLabel="Issue preview"
        maxRows={3}
        showProjectColumn={false}
      />
    </div>
  );
}

function ProjectWorkflowsView({
  snap,
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

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">project workflows</div>
          <h2>{project.name} workflows</h2>
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
        </div>
      </section>

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
              const fileUrl = githubFileUrl(project.github_repo, w.workflow_ref, w.workflow_filename);
              return (
                <tr key={w.id}>
                  <td>
                    <Link className="link" to={`/projects/${encodeURIComponent(project.name)}/workflows/${encodeURIComponent(w.name)}`}>
                      {w.name}
                    </Link>
                  </td>
                  <td className="mono dim">
                    <a className="link" href={fileUrl}>
                      {w.workflow_filename}@{w.workflow_ref}
                    </a>
                  </td>
                  <td className="mono dim">{w.trigger_label}</td>
                  <td><RequirementPills requirements={w.default_requirements} /></td>
                  <td className="mono dim">{wActive} active / {wPending} pending</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

function githubFileUrl(repo: string, ref: string, path: string): string {
  return `https://github.com/${repo}/blob/${encodeURIComponent(ref)}/${path.split("/").map(encodeURIComponent).join("/")}`;
}

function RequirementPills({ requirements }: { requirements: Record<string, unknown> }) {
  const entries = Object.entries(requirements);
  if (entries.length === 0) {
    return <span className="mono dim">none</span>;
  }

  return (
    <span className="requirement-pills">
      {entries.flatMap(([key, value]) => {
        const values = Array.isArray(value) ? value : [value];
        return values.map((item, index) => (
          <span className="pill info" key={`${key}:${index}:${String(item)}`}>
            {key}:{String(item)}
          </span>
        ));
      })}
    </span>
  );
}

function WorkflowDefinitionGraph({ workflow }: { workflow: Workflow }) {
  const phases = workflow.phases.length > 0
    ? workflow.phases
    : [{
        name: workflow.name,
        kind: "gha_dispatch",
        workflow_filename: workflow.workflow_filename,
        workflow_ref: workflow.workflow_ref,
        inputs: {},
        outputs: [],
        requirements: workflow.default_requirements,
        verify: false,
        recycle_policy: null,
      }];
  const policies = [
    ...phases.flatMap((phase) => phase.recycle_policy ? [{
      source: phase.name,
      target: phase.recycle_policy.lands_at,
      trigger: phase.recycle_policy.on.join(" / ") || "recycle",
      max: phase.recycle_policy.max_attempts,
    }] : []),
    ...(workflow.pr.recycle_policy ? [{
      source: "touchpoint",
      target: workflow.pr.recycle_policy.lands_at,
      trigger: workflow.pr.recycle_policy.on.join(" / ") || "feedback",
      max: workflow.pr.recycle_policy.max_attempts,
    }] : []),
  ];

  return (
    <section>
      <h2>Workflow graph</h2>
      <div className="dag-wrap">
        <div className="dag dag-definition" aria-label={`${workflow.name} workflow graph`}>
          <div className="dag-entry active">
            <span className="mono">entry</span>
            <span className="dim mono">{workflow.trigger_label}</span>
          </div>
          <span className="dag-edge" aria-hidden="true">→</span>
          {phases.map((phase, index) => (
            <Fragment key={phase.name}>
              <button
                type="button"
                className="dag-node dag-node-phase dag-node-definition"
                aria-disabled="true"
              >
                <div className="dag-node-label">{phase.name}</div>
                <div className="dag-node-state">
                  <span className="pill info">not run</span>
                </div>
                <div className="dag-node-meta dim mono">
                  {phase.verify ? "verify" : phase.kind}
                </div>
              </button>
              {(index < phases.length - 1 || workflow.pr.enabled) && (
                <span className="dag-edge" aria-hidden="true">→</span>
              )}
            </Fragment>
          ))}
          {workflow.pr.enabled && (
            <button
              type="button"
              className="dag-node dag-node-definition dag-node-pr pending"
              aria-disabled="true"
            >
              <div className="dag-node-label">touchpoint</div>
              <div className="dag-node-state mono">pending</div>
              <div className="dag-node-meta dim mono">PR primitive</div>
            </button>
          )}
        </div>
        {policies.length > 0 && (
          <div className="dag-policy-rail" aria-label="recycle policies">
            {policies.map((policy) => (
              <span
                className="dag-policy inactive"
                key={`${policy.source}:${policy.target}:${policy.trigger}`}
                title={`${policy.trigger}; max ${policy.max}`}
              >
                <span className="mono">{policy.source}</span>
                <span className="dim mono">↻</span>
                <span className="mono">{policy.target}</span>
                <span className="dim mono">{policy.trigger}</span>
              </span>
            ))}
          </div>
        )}
      </div>
    </section>
  );
}

function ProjectWorkflowView({
  snap,
  signedIn,
  projectName,
  workflowName,
}: LayoutContext & { projectName: string; workflowName: string }) {
  if (snap === null) return <div className="empty">Connecting…</div>;

  const project = snap.projects.find((p) => p.name === projectName);
  if (!project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }

  const workflow = snap.workflows.find((w) => w.project === project.name && w.name === workflowName);
  if (!workflow) {
    return <div className="empty">Workflow {workflowName || "(missing)"} was not found.</div>;
  }

  const pending = snap.pending_leases.filter((l) => l.project === project.name && l.workflow === workflow.name);
  const active = snap.active_leases.filter((l) => l.project === project.name && l.workflow === workflow.name);
  const currentWork = [...active, ...pending];

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">workflow</div>
          <h2>{workflow.name}</h2>
          <div className="project-repo mono">{workflow.workflow_filename}@{workflow.workflow_ref}</div>
        </div>
        <div className="project-facts">
          <div className="project-fact">
            <span>active</span>
            <strong>{active.length}</strong>
          </div>
          <div className="project-fact">
            <span>pending</span>
            <strong>{pending.length}</strong>
          </div>
          <div className="project-fact">
            <span>trigger</span>
            <strong>{workflow.trigger_label}</strong>
          </div>
        </div>
      </section>

      <section className="project-focus">
        <div>
          <span className="key">requires</span>
          <RequirementPills requirements={workflow.default_requirements} />
        </div>
        <div>
          <span className="key">project</span>
          <span className="mono">{project.name}</span>
        </div>
      </section>

      <WorkflowDefinitionGraph workflow={workflow} />

      <h2>Current work</h2>
      <CurrentWorkTable leases={currentWork} emptyText={`No active or pending work for ${workflow.name}.`} />

      <IssuesView
        signedIn={signedIn}
        projectFilter={project.name}
        workflowFilter={workflow.name}
        headingLabel="Workflow issues"
        showProjectColumn={false}
      />
    </div>
  );
}

function ProjectIssuesView({
  snap,
  signedIn,
  projectName,
}: LayoutContext & { projectName: string }) {
  if (snap === null) return <div className="empty">Connecting…</div>;
  const project = snap.projects.find((p) => p.name === projectName);
  if (!project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">project issues</div>
          <h2>{project.name} issues</h2>
          <div className="project-repo mono">{project.github_repo}</div>
        </div>
      </section>
      <div className="section-actions">
        <Link className="link" to={`/projects/${encodeURIComponent(project.name)}/issues/new`}>
          new issue
        </Link>
      </div>
      <IssuesView signedIn={signedIn} projectFilter={project.name} showProjectColumn={false} />
    </div>
  );
}

function ProjectOnboardingView({ signedIn }: LayoutContext) {
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [githubRepo, setGithubRepo] = useState("");
  const [appType, setAppType] = useState<"native_web_app" | "non_web">("native_web_app");
  const [uiValidation, setUiValidation] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const uiValidationEnabled = appType === "native_web_app";

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const r = await authedFetch("/v1/projects", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name,
          github_repo: githubRepo,
          metadata: {
            app_type: appType,
            ui_validation_default: uiValidationEnabled && uiValidation,
          },
        }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      navigate(`/projects/${encodeURIComponent(name)}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">project onboarding</div>
          <h2>New project</h2>
          <div className="project-repo mono">register the workspace Glimmung will track</div>
        </div>
      </section>
      {!signedIn ? (
        <div className="empty error">Sign in to register projects.</div>
      ) : (
        <form className="admin-form" onSubmit={submit}>
          <label>
            <span>Name</span>
            <input value={name} onChange={(e) => setName(e.target.value)} placeholder="ambience" required />
          </label>
          <label>
            <span>Repository</span>
            <input value={githubRepo} onChange={(e) => setGithubRepo(e.target.value)} placeholder="nelsong6/ambience" required />
          </label>
          <label>
            <span>App type</span>
            <select
              value={appType}
              onChange={(e) => {
                const value = e.target.value as "native_web_app" | "non_web";
                setAppType(value);
                if (value === "non_web") setUiValidation(false);
              }}
            >
              <option value="native_web_app">Native web app</option>
              <option value="non_web">Non-web</option>
            </select>
          </label>
          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={uiValidationEnabled && uiValidation}
              disabled={!uiValidationEnabled}
              onChange={(e) => setUiValidation(e.target.checked)}
            />
            <span>UI validation</span>
            {!uiValidationEnabled && (
              <span className="help-dot" title="Disabled because this project is not marked as a native web app.">?</span>
            )}
          </label>
          {error && <div className="error">{error}</div>}
          <button type="submit" disabled={busy}>{busy ? "Creating..." : "Create project"}</button>
        </form>
      )}
    </div>
  );
}

function IssueOnboardingView({
  snap,
  signedIn,
  projectName,
}: LayoutContext & { projectName: string }) {
  const navigate = useNavigate();
  const project = snap?.projects.find((p) => p.name === projectName) ?? null;
  const workflows = useMemo(
    () => project ? snap?.workflows.filter((w) => w.project === project.name) ?? [] : [],
    [project, snap?.workflows],
  );
  const isWeb = project
    ? (project.metadata?.app_type ?? (project.name === "spirelens" ? "non_web" : "native_web_app")) === "native_web_app"
    : true;
  const defaultUiValidation = isWeb && project?.metadata?.ui_validation_default !== false;
  const [workflow, setWorkflow] = useState("");
  const [title, setTitle] = useState("");
  const [objective, setObjective] = useState("");
  const [context, setContext] = useState("");
  const [acceptance, setAcceptance] = useState("");
  const [constraints, setConstraints] = useState("");
  const [labels, setLabels] = useState("");
  const [uiValidation, setUiValidation] = useState(defaultUiValidation);
  const [startRun, setStartRun] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!project) return;
    setWorkflow((current) => current || workflows[0]?.name || "");
    setUiValidation(defaultUiValidation);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [defaultUiValidation, project?.name]);

  if (snap === null) return <div className="empty">Connecting…</div>;
  if (!project) return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const body = [
        ["Objective", objective],
        ["Context", context],
        ["Acceptance criteria", acceptance],
        ["Constraints / links", constraints],
      ]
        .filter(([, value]) => value.trim())
        .map(([label, value]) => `${label}\n${value.trim()}`)
        .join("\n\n");
      const labelList = labels.split(",").map((s) => s.trim()).filter(Boolean);
      const r = await authedFetch("/v1/issues", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          project: project.name,
          workflow: workflow || null,
          title,
          body,
          labels: labelList,
          ui_validation_requested: isWeb && uiValidation,
        }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      const detail = (await r.json()) as { id: string; number: number | null };
      if (startRun) {
        await authedFetch("/v1/runs/dispatch", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ issue_id: detail.id, project: project.name, workflow: workflow || undefined }),
        });
      }
      navigate(`/projects/${encodeURIComponent(project.name)}/issues/${detail.number ?? detail.id}/summary`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">issue onboarding</div>
          <h2>New issue</h2>
          <div className="project-repo mono">{project.name}</div>
        </div>
      </section>
      {!signedIn ? (
        <div className="empty error">Sign in to create issues.</div>
      ) : (
        <form className="admin-form" onSubmit={submit}>
          <label>
            <span>Workflow</span>
            <select value={workflow} onChange={(e) => setWorkflow(e.target.value)}>
              <option value="">Decide later</option>
              {workflows.map((w) => <option key={w.id} value={w.name}>{w.name}</option>)}
            </select>
          </label>
          <label><span>Title</span><input value={title} onChange={(e) => setTitle(e.target.value)} required /></label>
          <label><span>Objective</span><textarea value={objective} onChange={(e) => setObjective(e.target.value)} rows={4} /></label>
          <label><span>Context</span><textarea value={context} onChange={(e) => setContext(e.target.value)} rows={4} /></label>
          <label><span>Acceptance criteria</span><textarea value={acceptance} onChange={(e) => setAcceptance(e.target.value)} rows={4} /></label>
          <label><span>Constraints / links</span><textarea value={constraints} onChange={(e) => setConstraints(e.target.value)} rows={3} /></label>
          <label><span>Labels</span><input value={labels} onChange={(e) => setLabels(e.target.value)} placeholder="design, ui" /></label>
          <label className="checkbox-row">
            <input type="checkbox" checked={isWeb && uiValidation} disabled={!isWeb} onChange={(e) => setUiValidation(e.target.checked)} />
            <span>UI validation</span>
            {!isWeb && <span className="help-dot" title="Disabled because this project is not marked as a native web app.">?</span>}
          </label>
          <label className="checkbox-row">
            <input type="checkbox" checked={startRun} onChange={(e) => setStartRun(e.target.checked)} />
            <span>Start run now</span>
          </label>
          {error && <div className="error">{error}</div>}
          <button type="submit" disabled={busy}>{busy ? "Creating..." : "Create issue"}</button>
        </form>
      )}
    </div>
  );
}

function ProjectNeedsAttentionView({
  snap,
  signedIn,
  projectName,
}: LayoutContext & { projectName: string }) {
  if (snap === null) return <div className="empty">Connecting…</div>;
  const project = snap.projects.find((p) => p.name === projectName);
  if (!project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">project attention</div>
          <h2>{project.name} needs attention</h2>
          <div className="project-repo mono">{project.github_repo}</div>
        </div>
      </section>
      <IssuesView
        signedIn={signedIn}
        projectFilter={project.name}
        headingLabel="Needs attention"
        showProjectColumn={false}
      />
    </div>
  );
}

function ProjectRunsView({
  snap,
  projectName,
}: LayoutContext & { projectName: string }) {
  if (snap === null) return <div className="empty">Connecting…</div>;
  const project = snap.projects.find((p) => p.name === projectName);
  if (!project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }

  const active = snap.active_leases.filter((l) => l.project === project.name);
  const pending = snap.pending_leases.filter((l) => l.project === project.name);
  const currentWork = [...active, ...pending];
  const runs = isMockMode()
    ? mockRuns.filter((run) => run.project === project.name)
    : [];

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">project runs</div>
          <h2>{project.name} runs</h2>
          <div className="project-repo mono">{project.github_repo}</div>
        </div>
        <div className="project-facts">
          <div className="project-fact">
            <span>active</span>
            <strong>{active.length}</strong>
          </div>
          <div className="project-fact">
            <span>pending</span>
            <strong>{pending.length}</strong>
          </div>
          {runs.length > 0 && (
            <div className="project-fact">
              <span>runs</span>
              <strong>{runs.length}</strong>
            </div>
          )}
        </div>
      </section>

      {runs.length > 0 ? (
        <ProjectRunsTable runs={runs} project={project} />
      ) : (
        <>
          <h2>Work in flight</h2>
          <CurrentWorkTable
            leases={currentWork}
            emptyText={`No active or pending runs for ${project.name}.`}
          />
        </>
      )}
    </div>
  );
}

function ProjectRunsTable({ runs, project }: { runs: ProjectRun[]; project: Project }) {
  const runsPath = `/projects/${encodeURIComponent(project.name)}/runs`;
  return (
    <>
      <h2>Run history</h2>
      <table>
        <thead>
          <tr>
            <th>Run</th>
            <th>Workflow</th>
            <th>Issue</th>
            <th>State</th>
            <th>Cycle</th>
            <th>Phase</th>
            <th>Cost</th>
            <th>Updated</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((run, index) => {
            const runLabel = projectRunLabel(run, runs, index);
            const runSlug = projectRunSlug(run, runs, index);
            return (
              <tr key={run.id}>
                <td>
                  <Link
                    className="link mono"
                    to={`/projects/${encodeURIComponent(run.project)}/issues/${run.issue_number}/runs/${encodeURIComponent(runSlug)}`}
                    state={{ returnTo: runsPath, returnLabel: "runs" }}
                    title={run.id}
                  >
                    {runLabel}
                  </Link>
                  <div className="dim">{run.title}</div>
                </td>
              <td className="mono dim">
                <Link
                  className="link mono"
                  to={`/projects/${encodeURIComponent(run.project)}/workflows/${encodeURIComponent(run.workflow)}`}
                >
                  {run.workflow}
                </Link>
              </td>
              <td className="mono dim">
                {run.issue_number ? (
                  <Link
                    className="link mono"
                    to={`/projects/${encodeURIComponent(project.name)}/issues/${run.issue_number}/summary`}
                    state={{ returnTo: runsPath, returnLabel: "runs" }}
                  >
                    #{run.issue_number}
                  </Link>
                ) : (
                  "-"
                )}
              </td>
              <td><span className={`pill ${runStatePill(run.state)}`}>{run.state}</span></td>
              <td className="mono">{run.cycles}</td>
              <td className="mono dim">{run.current_phase}</td>
              <td className="mono dim">${run.cost_usd.toFixed(2)}</td>
              <td className="mono dim">{relTime(run.updated_at)}</td>
            </tr>
            );
          })}
        </tbody>
      </table>
    </>
  );
}

function projectRunLabel(run: ProjectRun, runs: ProjectRun[], index: number): string {
  if (run.issue_number === null) return `run-${index + 1}`;
  const sameIssue = runs.filter((candidate) => candidate.issue_number === run.issue_number);
  const ordinal = sameIssue.findIndex((candidate) => candidate.id === run.id) + 1;
  return `#${run.issue_number}-${Math.max(ordinal, 1)}`;
}

function projectRunSlug(run: ProjectRun, runs: ProjectRun[], index: number): string {
  return projectRunLabel(run, runs, index).replace(/^#/, "");
}

function runSlugDisplay(slug: string): string {
  return /^\d+-\d+$/.test(slug) ? `#${slug}` : slug;
}

function titleCase(value: string): string {
  return value
    .split("-")
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ");
}

function resolveProjectRun(runs: ProjectRun[], runIdOrSlug: string): ProjectRun | null {
  return runs.find((candidate, index) => (
    candidate.id === runIdOrSlug || projectRunSlug(candidate, runs, index) === runIdOrSlug
  )) ?? null;
}

const RUN_VIEWER_IDLE_DISPATCH: DispatchState = { kind: "idle" };
const RUN_VIEWER_IDLE_ABORT: AbortState = { kind: "idle" };

function projectRunGraph(run: ProjectRun, workflow: Workflow | undefined, project: Project): IssueGraph {
  const issueNode = run.issue_number
    ? {
        id: `issue:${project.name}-${run.issue_number}`,
        kind: "issue" as const,
        label: `#${run.issue_number}`,
        state: "open",
        timestamp: run.started_at,
        metadata: {
          project: project.name,
          repo: project.github_repo,
          number: run.issue_number,
          issue_id: `${project.name}-${run.issue_number}`,
        },
      }
    : null;
  const phaseNames = workflow?.phases.map((phase) => phase.name) ?? [run.current_phase];
  const recycleArrows = workflow?.phases.flatMap((phase) => {
    if (!phase.recycle_policy) return [];
    return phase.recycle_policy.on.map((trigger) => ({
      source: phase.name,
      target: phase.recycle_policy?.lands_at ?? phase.name,
      trigger,
      max_attempts: phase.recycle_policy?.max_attempts ?? 1,
      active: false,
      kind: "phase_recycle" as const,
    }));
  }) ?? [];
  const runNode = {
    id: `run:${run.id}`,
    kind: "run" as const,
    label: run.id,
    state: run.state,
    timestamp: run.started_at,
    metadata: {
      workflow: run.workflow,
      cycles_count: run.cycles,
      cumulative_cost_usd: run.cost_usd,
      entrypoint_phase: phaseNames[0] ?? run.current_phase,
      workflow_graph: {
        phases: phaseNames,
        default_entry: { target: phaseNames[0] ?? run.current_phase, active: true, kind: "phase" },
        recycle_arrows: recycleArrows,
        terminal: { kind: "report", enabled: workflow?.pr.enabled ?? true },
      },
    },
  };
  const attempts = phaseNames
    .slice(0, Math.max(run.cycles, run.state === "pending" ? 0 : 1))
    .map((phase, index) => ({
      id: `attempt:${run.id}:${index}`,
      kind: "attempt" as const,
      label: phase,
      state: index === phaseNames.indexOf(run.current_phase) ? run.state : "completed",
      timestamp: run.started_at,
      metadata: {
        attempt_index: index,
        phase,
        phase_kind: workflow?.phases.find((candidate) => candidate.name === phase)?.kind ?? "agent",
        completed_at: run.state === "pending" || run.state === "in_progress" ? null : run.updated_at,
        verification_status: run.state === "passed" ? "pass" : null,
        steps: [
          {
            slug: `${phase}-start`,
            title: `${phase} started`,
            state: run.state === "pending" ? "pending" : "completed",
            message: `Mock ${phase} execution for ${run.title}.`,
            exit_code: run.state === "aborted" ? 1 : 0,
          },
        ],
      },
    }));
  const nodes = [
    ...(issueNode ? [issueNode] : []),
    runNode,
    ...attempts,
  ];
  return {
    issue_id: issueNode?.metadata.issue_id ?? run.id,
    nodes,
    edges: [
      ...(issueNode ? [{ source: issueNode.id, target: runNode.id, kind: "spawned" as const }] : []),
      ...attempts.map((attempt) => ({ source: runNode.id, target: attempt.id, kind: "attempted" as const })),
    ],
  };
}

function ProjectRunView({
  snap,
  signedIn,
  projectName,
  issueNumber,
  runId,
}: LayoutContext & { projectName: string; issueNumber: number | null; runId: string }) {
  if (snap === null) return <div className="empty">Connecting…</div>;
  const project = snap.projects.find((p) => p.name === projectName);
  if (!project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }

  const runs = isMockMode()
    ? mockRuns.filter((candidate) => candidate.project === project.name)
    : [];
  const run = isMockMode() ? resolveProjectRun(runs, runId) : null;
  const workflow = snap.workflows.find((w) => w.project === (run?.project ?? project.name) && w.name === run?.workflow);
  const graph = run ? projectRunGraph(run, workflow, project) : null;

  if (!run) {
    return (
      <div className="project-workspace">
        <section className="project-hero">
          <div className="project-hero-main">
            <div className="project-kicker mono">run</div>
            <h2>{runId || "(missing)"}</h2>
            <div className="project-repo mono">{project.github_repo}</div>
          </div>
        </section>
        <div className="empty">
          Run detail is not available yet for live runs.
        </div>
      </div>
    );
  }
  if (issueNumber !== null && run.issue_number !== issueNumber) {
    return (
      <div className="project-workspace">
        <section className="project-hero">
          <div className="project-hero-main">
            <div className="project-kicker mono">run</div>
            <h2>{runSlugDisplay(runId)}</h2>
            <div className="project-repo mono">{project.github_repo}</div>
          </div>
        </section>
        <div className="empty">
          Run {runSlugDisplay(runId)} does not belong to issue #{issueNumber}.
        </div>
      </div>
    );
  }

  const runIndex = runs.findIndex((candidate) => candidate.id === run.id);
  const runLabel = projectRunLabel(run, runs, runIndex >= 0 ? runIndex : 0);

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">run</div>
          <h2 title={run.id}>{runLabel}</h2>
          <div className="project-repo mono">{run.title}</div>
        </div>
        <div className="project-facts">
          <div className="project-fact">
            <span>state</span>
            <strong>{run.state}</strong>
          </div>
          <div className="project-fact">
            <span>cycles</span>
            <strong>{run.cycles}</strong>
          </div>
          <div className="project-fact">
            <span>phase</span>
            <strong>{run.current_phase}</strong>
          </div>
          <div className="project-fact">
            <span>cost</span>
            <strong>${run.cost_usd.toFixed(2)}</strong>
          </div>
        </div>
      </section>

      <section className="project-focus">
        <div>
          <span className="key">workflow</span>
          <strong>
            <Link
              className="link"
              to={`/projects/${encodeURIComponent(project.name)}/workflows/${encodeURIComponent(run.workflow)}`}
            >
              {run.workflow}
            </Link>
          </strong>
        </div>
        <div>
          <span className="key">issue</span>
          <span className="mono">
            {run.issue_number ? (
              <Link className="link mono" to={`/projects/${encodeURIComponent(project.name)}/issues/${run.issue_number}/summary`}>
                #{run.issue_number}
              </Link>
            ) : (
              "none"
            )}
          </span>
        </div>
        <div>
          <span className="key">updated</span>
          <span className="mono">{relTime(run.updated_at)}</span>
        </div>
      </section>

      <RunViewer
        graph={graph}
        graphAvailable={true}
        signedIn={signedIn}
        project={project.name}
        repo={project.github_repo}
        inFlight={run.state === "in_progress"}
        dispatchState={RUN_VIEWER_IDLE_DISPATCH}
        onRedispatch={() => undefined}
        abortState={RUN_VIEWER_IDLE_ABORT}
        onArmAbort={() => undefined}
        onCancelAbort={() => undefined}
        onConfirmAbort={() => undefined}
        selectedRunId={run.id}
        onBackToRuns={() => undefined}
        actionsVisible={false}
      />
    </div>
  );
}

function runStatePill(state: string): string {
  if (state === "passed") return "free";
  if (state === "in_progress" || state === "pending" || state === "needs_review") return "busy";
  if (state === "aborted" || state === "failed") return "drain";
  return "info";
}

function CurrentWorkTable({ leases, emptyText }: { leases: Lease[]; emptyText: string }) {
  if (leases.length === 0) {
    return <div className="empty">{emptyText}</div>;
  }

  return (
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
        {leases.map((l) => (
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
                <span className="val"><RequirementPills requirements={selectedWorkflow.default_requirements} /></span>
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

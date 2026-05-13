import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Link, Navigate, NavLink, Outlet, Route, Routes, useLocation, useNavigate, useOutletContext, useParams } from "react-router-dom";
import { AdminPanel } from "./AdminPanel";
import { IssueDetailView, RunViewer, type AbortState, type DispatchState, type IssueGraph } from "./IssueDetailView";
import { IssuesView } from "./IssuesView";
import { PlaybooksView } from "./PlaybooksView";
import { PortfolioView } from "./PortfolioView";
import { TouchpointDetailView } from "./TouchpointDetailView";
import { TouchpointsView } from "./TouchpointsView";
import { StyleguideView } from "./StyleguideView";
import { PhaseGraph, type PhaseGraphPhase } from "./PhaseGraph";
import { workflowToPhaseGraphModel } from "./workflowGraphModel";
import { resolveProjectWorkflow } from "./workflowLookup";
import { authedFetch, currentAccount, initAuth, signIn, signOut } from "./auth";
import { isMockMode, mockRuns, mockSnapshot } from "./mockApi";
import type { AccountInfo } from "@azure/msal-browser";

type Host = {
  name: string;
  capabilities: Record<string, unknown>;
  current_lease_ref: string | null;
  last_heartbeat: string | null;
  last_used_at: string | null;
  drained: boolean;
  created_at: string;
};

type Lease = {
  ref: string;
  lease_number?: number | null;
  project: string;
  workflow: string | null;
  host: string | null;
  state: "pending" | "active" | "claimed" | "released" | "expired";
  requirements: Record<string, unknown>;
  metadata: Record<string, unknown>;
  requester: Record<string, unknown> | null;
  requested_at: string;
  assigned_at: string | null;
  released_at: string | null;
  ttl_seconds: number;
};

type LeaseKind = "test" | "agent";

type TestSlotRequest = {
  ref: string;
  project: string;
  workflow: string;
  state: "waiting" | "fulfilled" | "cancelled";
  requested_slot_index: number | null;
  requester: Record<string, unknown> | null;
  metadata: Record<string, unknown>;
  requested_at: string;
  fulfilled_at: string | null;
  fulfilled_lease_ref: string | null;
  ttl_seconds: number;
};

type TestEnvironment = {
  project: string;
  slot_index: number;
  slot_name: string;
  state: "available" | "warming" | "activating" | "active" | "cleaning" | "claimed" | "error";
  usable?: boolean;
  detail?: string | null;
  updated_at?: string | null;
  ready_at?: string | null;
  activation_attempt?: number | null;
  activation_state?: string | null;
  activation_started_at?: string | null;
  activation_completed_at?: string | null;
  activation_job_name?: string | null;
  activation_error?: string | null;
  cleanup_state?: string | null;
  cleanup_started_at?: string | null;
  cleanup_completed_at?: string | null;
  cleanup_error?: string | null;
  lease: Lease | null;
  waiting_requests: TestSlotRequest[];
};

type Project = {
  id: string;
  name: string;
  github_repo: string;
  argocd_app?: string;
  metadata: Record<string, unknown>;
  created_at: string;
};

type Workflow = {
  id: string;
  project: string;
  name: string;
  phases: PhaseSpec[];
  pr: PrPrimitiveSpec;
  workflow_filename: string | null;
  workflow_ref: string | null;
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
  run_number?: number | null;
  issue_number: number | null;
  title: string;
  state: string;
  cycles: number;
  current_phase: string;
  cost_usd: number;
  started_at: string;
  updated_at: string;
};

type RunReportAttempt = {
  attempt_index: number;
  phase: string;
  phase_kind: string;
  workflow_filename: string;
  workflow_run_id: number | null;
  dispatched_at: string;
  completed_at: string | null;
  conclusion: string | null;
  verification_status: string | null;
  evidence_refs: string[];
  summary_markdown: string | null;
  decision: string | null;
  cost_usd: number | null;
  log_archive_url: string | null;
  skipped_from_run_ref: string | null;
};

type RunReport = {
  ref: string;
  project: string;
  run_ref: string;
  run_number: number | null;
  run_display_number: string | null;
  parent_run_ref: string | null;
  root_run_ref: string | null;
  origin_kind: string | null;
  is_cycle: boolean;
  cycle_number: number | null;
  workflow: string;
  issue_ref: string | null;
  issue_repo: string | null;
  issue_number: number | null;
  state: string;
  current_phase: string | null;
  attempts_count: number;
  cumulative_cost_usd: number;
  validation_url: string | null;
  screenshots_markdown: string | null;
  abort_reason: string | null;
  started_at: string;
  completed_at: string | null;
  updated_at: string;
  attempts: RunReportAttempt[];
};

type Snapshot = {
  hosts: Host[];
  pending_leases: Lease[];
  active_leases: Lease[];
  test_environments?: TestEnvironment[];
  waiting_test_slot_requests?: TestSlotRequest[];
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
  isAdmin: boolean;
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
        <Route path="dashboard" element={<Navigate to="/leases/test" replace />} />
        <Route path="leases" element={<Navigate to="/leases/test" replace />} />
        <Route path="leases/test" element={<GlobalLeaseRoute kind="test" />} />
        <Route path="leases/test/:leaseId" element={<GlobalLeaseDetailRoute kind="test" />} />
        <Route path="leases/agent" element={<GlobalLeaseRoute kind="agent" />} />
        <Route path="leases/agent/:leaseId" element={<GlobalLeaseDetailRoute kind="agent" />} />
        <Route path="needs-attention" element={<NeedsAttentionRoute />} />
        <Route path="playbooks" element={<PlaybooksRoute />} />
        <Route path="graph" element={<Navigate to="/leases/test" replace />} />
        <Route path="projects" element={<ProjectsRoute />} />
        <Route path="projects/new" element={<ProjectOnboardingRoute />} />
        <Route path="projects/:project" element={<ProjectRoute />} />
        <Route path="projects/:project/leases" element={<ProjectLeaseRedirectRoute />} />
        <Route path="projects/:project/leases/test" element={<ProjectLeaseRoute kind="test" />} />
        <Route path="projects/:project/leases/test/:leaseId" element={<ProjectLeaseDetailRoute kind="test" />} />
        <Route path="projects/:project/leases/agent" element={<ProjectLeaseRoute kind="agent" />} />
        <Route path="projects/:project/leases/agent/:leaseId" element={<ProjectLeaseDetailRoute kind="agent" />} />
        <Route path="projects/:project/workflows" element={<ProjectWorkflowsRoute />} />
        <Route path="projects/:project/workflows/:workflow" element={<ProjectWorkflowRoute />} />
        <Route path="projects/:project/issues" element={<ProjectIssuesRoute />} />
        <Route path="projects/:project/playbooks" element={<ProjectPlaybooksRoute />} />
        <Route path="projects/:project/playbooks/:playbookRef" element={<ProjectPlaybookDetailRoute />} />
        <Route path="projects/:project/portfolio" element={<ProjectPortfolioRoute />} />
        <Route path="projects/:project/issues/new" element={<IssueOnboardingRoute />} />
        <Route path="projects/:project/issues/:issueNumber" element={<IssueDetailView />}>
          <Route path="summary" element={null} />
          <Route path="issue" element={null} />
          <Route path="runs" element={null} />
          <Route path="runs/:runId" element={null} />
          <Route path="workflow" element={null} />
          <Route path="workflow/:workflowRunId" element={null} />
          <Route path="touchpoint" element={null} />
          <Route path="description" element={null} />
          <Route path="the-run" element={null} />
          <Route path="in-progress" element={null} />
          <Route path="lineage" element={null} />
        </Route>
        <Route path="projects/:project/needs-attention" element={<ProjectNeedsAttentionRoute />} />
        <Route path="projects/:project/hosts" element={<ProjectHostsRoute />} />
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
          <Route path="runs/:runId" element={null} />
          <Route path="workflow" element={null} />
          <Route path="workflow/:workflowRunId" element={null} />
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
          <Route path="runs/:runId" element={null} />
          <Route path="workflow" element={null} />
          <Route path="workflow/:workflowRunId" element={null} />
          <Route path="touchpoint" element={null} />
          <Route path="description" element={null} />
          <Route path="in-progress" element={null} />
          <Route path="lineage" element={null} />
        </Route>
        <Route path="touchpoints" element={<TouchpointsRoute />} />
        <Route path="portfolio" element={<PortfolioRoute />} />
        <Route path="touchpoints/:owner/:repo/:n" element={<LegacyTouchpointRedirectRoute />} />
        <Route path="reports" element={<Navigate to="/touchpoints" replace />} />
        <Route path="reports/:owner/:repo/:n" element={<LegacyTouchpointRedirectRoute />} />
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
  const selected = ALL;
  const [account, setAccount] = useState<AccountInfo | null>(null);
  const [authReady, setAuthReady] = useState(false);
  const [isAdmin, setIsAdmin] = useState(false);
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

  // Admin status comes from /v1/auth/me, which is a soft check — non-allowlisted
  // signed-in users get 200 with is_admin=false. Drives whether admin actions
  // (dispatch / abort / etc.) render disabled or clickable.
  useEffect(() => {
    if (!authReady) return;
    if (!account) {
      setIsAdmin(false);
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const r = await authedFetch("/v1/auth/me");
        if (!r.ok) {
          if (!cancelled) setIsAdmin(false);
          return;
        }
        const me = (await r.json()) as { is_admin?: boolean };
        if (!cancelled) setIsAdmin(!!me.is_admin);
      } catch {
        if (!cancelled) setIsAdmin(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [authReady, account]);

  useEffect(() => {
    if (isMockMode()) {
      setSnap(mockSnapshot as Snapshot);
      setConn("live");
      return;
    }

    let es: EventSource | null = null;
    let staleTimer: number | null = null;
    let lastSeen = 0;

    const connect = () => {
      es = new EventSource("/v1/events");
      es.addEventListener("state", (e) => {
        try {
          setSnap(JSON.parse((e as MessageEvent).data));
          lastSeen = Date.now();
          setConn("live");
        } catch (err) {
          console.error("bad snapshot", err);
        }
      });
      es.onerror = () => setConn("dead");
    };

    connect();
    staleTimer = window.setInterval(() => {
      if (lastSeen > 0 && Date.now() - lastSeen > 5000) {
        setConn((c) => (c === "live" ? "stale" : c));
      }
    }, 1000);

    return () => {
      es?.close();
      if (staleTimer !== null) window.clearInterval(staleTimer);
    };
  }, []);

  // Poll /v1/issues + /v1/touchpoints to drive the issue-workspace pulse
  // when issue work or touchpoint review is in flight. Touchpoints are no
  // longer primary navigation, but their locks still matter to issues.
  useEffect(() => {
    let cancelled = false;
    const check = async () => {
      try {
        const [iRes, pRes] = await Promise.all([
          fetch("/v1/issues"),
          fetch("/v1/touchpoints"),
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
    isAdmin,
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
              <NavLink to="/leases/test" className={dashboardLinkClass}>
                test leases
              </NavLink>
              <NavLink to="/leases/agent" className={dashboardLinkClass}>
                agent leases
              </NavLink>
              <NavLink to="/needs-attention" className={dashboardLinkClass}>
                needs attention
                {inflight.issues && <span className="tab-dot" />}
              </NavLink>
              <NavLink to="/projects" className={dashboardLinkClass}>
                projects
              </NavLink>
              <NavLink to="/portfolio" className={dashboardLinkClass}>
                portfolio
              </NavLink>
              <NavLink to="/playbooks" className={dashboardLinkClass}>
                playbooks
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
    return [{ label: "Home", to: "/" }, { label: "Test leases" }];
  }
  if (parts[0] === "leases") {
    const kind = parts[1] === "agent" ? "Agent leases" : "Test leases";
    const crumbs: Breadcrumb[] = [{ label: "Home", to: "/" }, { label: kind, to: `/leases/${parts[1] ?? "test"}` }];
    if (parts[2]) crumbs.push({ label: `Lease ${parts[2]}` });
    return crumbs;
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
    } else if (parts[2] === "leases") {
      const leaseKind = parts[3] === "agent" ? "Agent leases" : "Test leases";
      crumbs.push({
        label: leaseKind,
        to: `/projects/${encodeURIComponent(parts[1] ?? "")}/leases/${parts[3] ?? "test"}`,
      });
      if (parts[4]) crumbs.push({ label: `Lease ${parts[4]}` });
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
      } else if (parts[4] === "workflow") {
        crumbs.push({
          label: "Workflow",
          to: `/projects/${encodeURIComponent(parts[1] ?? "")}/issues/${encodeURIComponent(parts[3] ?? "")}/workflow`,
        });
        if (parts[5]) crumbs.push({ label: runSlugDisplay(parts[5]) });
      } else if (parts[4]) {
        crumbs.push({ label: titleCase(parts[4]) });
      }
    } else if (parts[2] === "playbooks") {
      crumbs.push({ label: "Playbooks", to: `/projects/${encodeURIComponent(parts[1] ?? "")}/playbooks` });
      if (parts[3]) crumbs.push({ label: parts[3] });
    } else if (parts[2] === "needs-attention") {
      crumbs.push({ label: "Needs attention" });
    } else if (parts[2] === "portfolio") {
      crumbs.push({ label: "Portfolio" });
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
  if (parts[0] === "touchpoints" || parts[0] === "reports") {
    return [{ label: "Home", to: "/" }, { label: "Touchpoints", to: "/touchpoints" }];
  }
  if (parts[0] === "portfolio") {
    return [{ label: "Home", to: "/" }, { label: "Portfolio", to: "/portfolio" }];
  }
  if (parts[0] === "playbooks") {
    return [{ label: "Home", to: "/" }, { label: "Playbooks", to: "/playbooks" }];
  }
  return [{ label: "Home", to: "/" }, { label: parts[0] }];
}

function HomeRoute() {
  const ctx = useOutletContext<LayoutContext>();
  return <HomeView {...ctx} />;
}

function GlobalLeaseRoute({ kind }: { kind: LeaseKind }) {
  const ctx = useOutletContext<LayoutContext>();
  return <LeaseIndexView {...ctx} kind={kind} />;
}

function GlobalLeaseDetailRoute({ kind }: { kind: LeaseKind }) {
  const params = useParams<{ leaseId?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return <LeaseDetailView {...ctx} kind={kind} leaseId={decodeURIComponent(params.leaseId ?? "")} />;
}

function ProjectLeaseRoute({ kind }: { kind: LeaseKind }) {
  const params = useParams<{ project?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return <LeaseIndexView {...ctx} kind={kind} projectName={decodeURIComponent(params.project ?? "")} />;
}

function ProjectLeaseRedirectRoute() {
  const params = useParams<{ project?: string }>();
  return <Navigate to={`/projects/${encodeURIComponent(decodeURIComponent(params.project ?? ""))}/leases/test`} replace />;
}

function ProjectLeaseDetailRoute({ kind }: { kind: LeaseKind }) {
  const params = useParams<{ project?: string; leaseId?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return (
    <LeaseDetailView
      {...ctx}
      kind={kind}
      projectName={decodeURIComponent(params.project ?? "")}
      leaseId={decodeURIComponent(params.leaseId ?? "")}
    />
  );
}

function NeedsAttentionRoute() {
  const { signedIn } = useOutletContext<LayoutContext>();
  return (
    <IssuesView
      signedIn={signedIn}
      projectFilter={null}
      headingLabel="Needs attention"
      needsAttentionOnly
    />
  );
}

function PlaybooksRoute() {
  const { signedIn, isAdmin, selected } = useOutletContext<LayoutContext>();
  return (
    <PlaybooksView
      signedIn={signedIn}
      isAdmin={isAdmin}
      projectFilter={selected.kind === "all" ? null : selected.project}
    />
  );
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

function ProjectPlaybooksRoute() {
  const params = useParams<{ project?: string }>();
  const { signedIn, isAdmin } = useOutletContext<LayoutContext>();
  return (
    <PlaybooksView
      signedIn={signedIn}
      isAdmin={isAdmin}
      projectFilter={decodeURIComponent(params.project ?? "")}
    />
  );
}

function ProjectPlaybookDetailRoute() {
  const params = useParams<{ project?: string; playbookRef?: string }>();
  const { signedIn, isAdmin } = useOutletContext<LayoutContext>();
  return (
    <PlaybooksView
      signedIn={signedIn}
      isAdmin={isAdmin}
      projectFilter={decodeURIComponent(params.project ?? "")}
      playbookRef={decodeURIComponent(params.playbookRef ?? "")}
    />
  );
}

function ProjectPortfolioRoute() {
  const params = useParams<{ project?: string }>();
  const { signedIn } = useOutletContext<LayoutContext>();
  return (
    <PortfolioView
      signedIn={signedIn}
      projectFilter={decodeURIComponent(params.project ?? "")}
    />
  );
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

function ProjectHostsRoute() {
  const params = useParams<{ project?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return <ProjectHostsView {...ctx} projectName={decodeURIComponent(params.project ?? "")} />;
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

function TouchpointsRoute() {
  const { selected } = useOutletContext<LayoutContext>();
  return (
    <TouchpointsView
      projectFilter={selected.kind === "all" ? null : selected.project}
    />
  );
}

function PortfolioRoute() {
  const { signedIn, selected } = useOutletContext<LayoutContext>();
  return (
    <PortfolioView
      signedIn={signedIn}
      projectFilter={selected.kind === "all" ? null : selected.project}
    />
  );
}

function LegacyTouchpointRedirectRoute() {
  const params = useParams<{ owner?: string; repo?: string; n?: string }>();
  const [target, setTarget] = useState<string | null>(null);
  const [fallback, setFallback] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const repo = `${params.owner ?? ""}/${params.repo ?? ""}`;
  const prNumber = params.n ?? "";

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      setError(null);
      setFallback(false);
      try {
        const r = await fetch(`/v1/touchpoints/${repo}/${prNumber}`);
        if (!r.ok) throw new Error(`/v1/touchpoints/${repo}/${prNumber} -> ${r.status}`);
        const detail = await r.json() as {
          project?: string;
          issue_number?: number | null;
        };
        if (cancelled) return;
        if (detail.project && detail.issue_number !== null && detail.issue_number !== undefined) {
          setTarget(
            `/projects/${encodeURIComponent(detail.project)}/issues/${detail.issue_number}/touchpoint`,
          );
        } else {
          setFallback(true);
        }
      } catch (e) {
        if (!cancelled) setError(String(e));
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, [repo, prNumber]);

  if (target) return <Navigate to={target} replace />;
  if (fallback) return <TouchpointDetailView />;
  if (error) return <div className="empty error">{error}</div>;
  return <div className="empty">Loading touchpoint…</div>;
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
        <Link to="/projects" className="home-link">
          <span className="key">Projects</span>
          <strong>Project workspaces, workflows, and scoped issues</strong>
        </Link>
        <Link to="/leases/test" className="home-link">
          <span className="key">Test leases</span>
          <strong>Current test environments and queued checkouts</strong>
        </Link>
        <Link to="/leases/agent" className="home-link">
          <span className="key">Agent leases</span>
          <strong>Active agent capacity and pending work leases</strong>
        </Link>
        <Link to="/needs-attention" className="home-link">
          <span className="key">Needs attention</span>
          <strong>Open work that needs a decision or follow-up</strong>
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
              <th>Test leases</th>
              <th>Agent leases</th>
              <th>Legacy hosts</th>
            </tr>
          </thead>
          <tbody>
            {projects.map((project) => {
              const workflows = snap.workflows.filter((w) => w.project === project.name);
              const pending = snap.pending_leases.filter((l) => l.project === project.name);
              const active = snap.active_leases.filter((l) => l.project === project.name);
              const leases = [...active, ...pending];
              const testLeases = leases.filter((l) => leaseKind(l) === "test");
              const agentLeases = leases.filter((l) => leaseKind(l) === "agent");
              const isNativeK8sProject = projectUsesNativeWorkflows(project);
              const activeHosts = new Set(isNativeK8sProject ? [] : active.flatMap((l) => (l.host ? [l.host] : [])));
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
                  <td className="mono dim">
                    <Link className="link mono" to={`/projects/${encodeURIComponent(project.name)}/leases/test`}>
                      {testLeases.length}
                    </Link>
                  </td>
                  <td className="mono dim">
                    {isNativeK8sProject ? (
                      "—"
                    ) : (
                      <Link className="link mono" to={`/projects/${encodeURIComponent(project.name)}/leases/agent`}>
                        {agentLeases.length}
                      </Link>
                    )}
                  </td>
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
  matchesRequirements,
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
  const projectLeases = [...active, ...pending];
  const testLeases = projectLeases.filter((l) => leaseKind(l) === "test");
  const agentLeases = projectLeases.filter((l) => leaseKind(l) === "agent");
  const projectPath = `/projects/${encodeURIComponent(project.name)}`;
  const isNativeK8sProject = projectUsesNativeWorkflows(project);

  const nonEmptyReqs = workflows
    .map((w) => w.default_requirements)
    .filter((r) => Object.keys(r).length > 0);
  const hasHosts = !isNativeK8sProject && snap.hosts.some((h) =>
    nonEmptyReqs.some((reqs) => matchesRequirements(h, reqs))
  );

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
            {project.argocd_app && (
              <>
                {" · "}
                <a
                  className="link"
                  href={`https://argocd.romaine.life/applications/argocd/${project.argocd_app}`}
                  target="_blank"
                  rel="noreferrer"
                >
                  argocd/{project.argocd_app}
                </a>
              </>
            )}
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
        </div>
      </section>

      <section className="home-links" aria-label={`${project.name} destinations`}>
        <Link to={`${projectPath}/leases/test`} className="home-link">
          <span className="key">Test leases</span>
          <strong>{testLeases.length} active or pending test environment lease{testLeases.length === 1 ? "" : "s"}</strong>
        </Link>
        {!isNativeK8sProject && (
          <Link to={`${projectPath}/leases/agent`} className="home-link">
            <span className="key">Agent leases</span>
            <strong>{agentLeases.length} active or pending agent lease{agentLeases.length === 1 ? "" : "s"}</strong>
          </Link>
        )}
        <Link to={`${projectPath}/workflows`} className="home-link">
          <span className="key">Workflows</span>
          <strong>Definitions, triggers, requirements, and workflow-scoped work</strong>
        </Link>
        <Link to={`${projectPath}/issues`} className="home-link">
          <span className="key">Issues</span>
          <strong>All open issues for {project.name}</strong>
        </Link>
        <Link to={`${projectPath}/playbooks`} className="home-link">
          <span className="key">Playbooks</span>
          <strong>Executable plans, gates, dependencies, and linked runs</strong>
        </Link>
        <Link to={`${projectPath}/needs-attention`} className="home-link">
          <span className="key">Needs attention</span>
          <strong>Project work that needs a decision or follow-up</strong>
        </Link>
        <Link to={`${projectPath}/portfolio`} className="home-link">
          <span className="key">Portfolio</span>
          <strong>Review UI package rows and dispatch explicit follow-up</strong>
        </Link>
        <Link to={`${projectPath}/runs`} className="home-link">
          <span className="key">Runs</span>
          <strong>Run and cycle history for project work</strong>
        </Link>
        {hasHosts && (
          <Link to={`${projectPath}/hosts`} className="home-link">
            <span className="key">Legacy hosts</span>
            <strong>Self-hosted gha_dispatch capacity for exception workflows</strong>
          </Link>
        )}
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
              const sourceLabel = workflowSourceLabel(w);
              return (
                <tr key={w.id}>
                  <td>
                    <Link className="link" to={`/projects/${encodeURIComponent(project.name)}/workflows/${encodeURIComponent(w.name)}`}>
                      {w.name}
                    </Link>
                  </td>
                  <td className="mono dim">
                    {fileUrl ? (
                      <a className="link" href={fileUrl}>
                        {sourceLabel}
                      </a>
                    ) : sourceLabel}
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

function githubFileUrl(repo: string, ref: string | null, path: string | null): string | null {
  if (!repo || !ref || !path) return null;
  return `https://github.com/${repo}/blob/${encodeURIComponent(ref)}/${path.split("/").map(encodeURIComponent).join("/")}`;
}

function workflowSourceLabel(workflow: Workflow): string {
  if (workflow.workflow_filename) {
    return `${workflow.workflow_filename}@${workflow.workflow_ref ?? "main"}`;
  }
  const nativeKinds = Array.from(new Set(workflow.phases.map((phase) => phase.kind).filter(Boolean)));
  return nativeKinds.length > 0 ? nativeKinds.join(" + ") : "native";
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
  const graphModel = workflowToPhaseGraphModel(workflow);

  const renderPhase = (phase: PhaseGraphPhase) => {
    const meta = phase.evidence_verification_gate
      ? "verify-gate"
      : phase.always
        ? "always"
        : phase.verify
          ? "verify"
          : phase.kind;
    return (
      <div className="dag-node dag-node-phase dag-node-definition">
        <div className="dag-job-head">
          <span className="dag-job-title">{phase.name}</span>
          <span className="dag-job-kicker">job</span>
        </div>
        <div className="dag-node-meta dim mono">{meta}</div>
      </div>
    );
  };

  const renderTouchpoint = () => (
    <div className="dag-node dag-node-definition dag-node-pr">
      <div className="dag-node-label">touchpoint</div>
      <div className="dag-node-meta dim mono">PR primitive</div>
    </div>
  );

  return (
    <section>
      <h2>Workflow graph</h2>
      <div className="dag-wrap">
        <PhaseGraph
          phases={graphModel.phases}
          prEnabled={graphModel.prEnabled}
          dagClassName="dag-definition"
          ariaLabel={`${workflow.name} workflow graph`}
          renderPhase={renderPhase}
          renderTouchpoint={renderTouchpoint}
          recycleArrows={graphModel.recycleArrows}
        />
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
          <div className="project-repo mono">{workflowSourceLabel(workflow)}</div>
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
      const detail = (await r.json()) as { ref: string; number: number | null };
      if (startRun) {
        if (detail.number === null) throw new Error("Created issue did not receive a project issue number");
        await authedFetch("/v1/runs/dispatch", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ issue_number: detail.number, project: project.name, workflow: workflow || undefined }),
        });
      }
      if (detail.number === null) throw new Error("Created issue did not receive a project issue number");
      navigate(`/projects/${encodeURIComponent(project.name)}/issues/${detail.number}/summary`);
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
        needsAttentionOnly
      />
    </div>
  );
}

function ProjectRunsView({
  snap,
  projectName,
}: LayoutContext & { projectName: string }) {
  const project = snap?.projects.find((p) => p.name === projectName);
  const [liveRuns, setLiveRuns] = useState<ProjectRun[]>([]);
  const [runsLoading, setRunsLoading] = useState(!isMockMode());
  const [runsError, setRunsError] = useState<string | null>(null);

  useEffect(() => {
    if (isMockMode() || !project) return;
    let cancelled = false;
    setRunsLoading(true);
    setRunsError(null);
    fetch(`/v1/projects/${encodeURIComponent(project.name)}/runs?limit=200`)
      .then(async (res) => {
        if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
        return await res.json() as RunReport[];
      })
      .then((reports) => {
        if (!cancelled) setLiveRuns(reports.map(projectRunFromReport));
      })
      .catch((err) => {
        if (!cancelled) setRunsError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!cancelled) setRunsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [project?.name]);

  if (snap === null) return <div className="empty">Connecting…</div>;
  if (!project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }

  const active = snap.active_leases.filter((l) => l.project === project.name);
  const pending = snap.pending_leases.filter((l) => l.project === project.name);
  const currentWork = [...active, ...pending];

  const runs = isMockMode()
    ? mockRuns.filter((run) => run.project === project.name)
    : liveRuns;
  const completedRuns = runs.filter((run) => run.state !== "in_progress" && run.state !== "pending");

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

      {runsLoading && <div className="empty">Loading run history…</div>}
      {runsError && <div className="empty">Run history could not be loaded: {runsError}</div>}
      {!runsLoading && !runsError && completedRuns.length > 0 && (
        <ProjectRunsTable title="Completed runs" runs={completedRuns} project={project} />
      )}

      {currentWork.length > 0 && (
        <>
          <h2>Work in flight</h2>
          <CurrentWorkTable leases={currentWork} emptyText="" />
        </>
      )}

      {!runsLoading && !runsError && (
        runs.length > 0 ? (
          <ProjectRunsTable title="All runs" runs={runs} project={project} />
        ) : currentWork.length === 0 ? (
          <CurrentWorkTable
            leases={currentWork}
            emptyText={`No active, pending, or completed runs for ${project.name}.`}
          />
        ) : null
      )}
    </div>
  );
}

function ProjectHostsView({
  snap,
  projectName,
  matchesRequirements,
}: LayoutContext & { projectName: string }) {
  if (snap === null) return <div className="empty">Connecting…</div>;

  const project = snap.projects.find((p) => p.name === projectName);
  if (!project) return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;

  const workflows = snap.workflows.filter((w) => w.project === project.name);
  const nonEmptyReqs = workflows
    .map((w) => w.default_requirements)
    .filter((r) => Object.keys(r).length > 0);

  const hosts = snap.hosts.filter((h) =>
    nonEmptyReqs.some((reqs) => matchesRequirements(h, reqs))
  );
  const leaseLabels = new Map(
    [...snap.active_leases, ...snap.pending_leases].map((lease) => [
      lease.ref,
      leaseDisplayName(lease),
    ]),
  );

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">legacy gha capacity / {project.name}</div>
          <h2>Legacy hosts</h2>
          <div className="project-repo mono">self-hosted runner pool for explicit gha_dispatch workflows</div>
        </div>
        <div className="project-facts">
          <div className="project-fact"><span>total</span><strong>{hosts.length}</strong></div>
          <div className="project-fact"><span>free</span><strong>{hosts.filter((h) => !h.drained && !h.current_lease_ref).length}</strong></div>
          <div className="project-fact"><span>busy</span><strong>{hosts.filter((h) => !h.drained && h.current_lease_ref).length}</strong></div>
          <div className="project-fact"><span>drained</span><strong>{hosts.filter((h) => h.drained).length}</strong></div>
        </div>
      </section>

      {hosts.length === 0 ? (
        <div className="empty">No legacy hosts match this project's workflow requirements.</div>
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
            {hosts.map((h) => (
              <tr key={h.name}>
                <td className="mono">{h.name}</td>
                <td className="mono">{JSON.stringify(h.capabilities)}</td>
                <td>
                  {h.drained ? (
                    <span className="pill drain">drained</span>
                  ) : h.current_lease_ref ? (
                    <span className="pill busy">busy</span>
                  ) : (
                    <span className="pill free">free</span>
                  )}
                </td>
                <td className="mono dim">
                  {h.current_lease_ref ? leaseLabels.get(h.current_lease_ref) ?? "active lease" : "—"}
                </td>
                <td className="mono dim">{relTime(h.last_heartbeat)}</td>
                <td className="mono dim">{relTime(h.last_used_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function ProjectRunsTable({ title, runs, project }: { title: string; runs: ProjectRun[]; project: Project }) {
  const runsPath = `/projects/${encodeURIComponent(project.name)}/runs`;
  return (
    <>
      <h2>{title}</h2>
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
                    to={projectRunHref(run, runSlug)}
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
                  state={{ returnTo: runsPath, returnLabel: "runs" }}
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
  if (run.issue_number !== null && run.run_number) return `#${run.issue_number}-${run.run_number}`;
  if (run.issue_number === null) return `run-${index + 1}`;
  const sameIssue = runs.filter((candidate) => candidate.issue_number === run.issue_number);
  const ordinal = sameIssue.findIndex((candidate) => candidate.id === run.id) + 1;
  return `#${run.issue_number}-${Math.max(ordinal, 1)}`;
}

function projectRunSlug(run: ProjectRun, runs: ProjectRun[], index: number): string {
  return projectRunLabel(run, runs, index).replace(/^#/, "");
}

function projectRunHref(run: ProjectRun, runSlug: string): string {
  if (run.issue_number !== null) {
    return `/projects/${encodeURIComponent(run.project)}/issues/${run.issue_number}/runs/${encodeURIComponent(runSlug)}`;
  }
  return `/projects/${encodeURIComponent(run.project)}/runs/${encodeURIComponent(run.id)}`;
}

function runSlugDisplay(slug: string): string {
  return /^\d+-\d+$/.test(slug) ? `#${slug}` : slug;
}

function issueScopedRunNumberFromSlug(issueNumber: number | null, slug: string): number | null {
  if (issueNumber === null) return null;
  if (/^\d+$/.test(slug)) return parseInt(slug, 10);
  const match = slug.match(/^(\d+)-(\d+)$/);
  if (!match) return null;
  return parseInt(match[1], 10) === issueNumber ? parseInt(match[2], 10) : null;
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
        id: `issue:${project.name}#${run.issue_number}`,
        kind: "issue" as const,
        label: `#${run.issue_number}`,
        state: "open",
        timestamp: run.started_at,
        metadata: {
          project: project.name,
          repo: project.github_repo,
          number: run.issue_number,
          issue_ref: `${project.name}#${run.issue_number}`,
        },
      }
    : null;
  const graphModel = workflow ? workflowToPhaseGraphModel(workflow) : null;
  const phaseNames = graphModel?.phases.map((phase) => phase.name) ?? [run.current_phase];
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
        recycle_arrows: graphModel?.recycleArrows ?? [],
        terminal: { kind: "report", enabled: graphModel?.prEnabled ?? true },
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
    issue_ref: String(issueNode?.metadata.issue_ref ?? run.id),
    nodes,
    edges: [
      ...(issueNode ? [{ source: issueNode.id, target: runNode.id, kind: "spawned" as const }] : []),
      ...attempts.map((attempt) => ({ source: runNode.id, target: attempt.id, kind: "attempted" as const })),
    ],
  };
}

function projectRunFromReport(report: RunReport): ProjectRun {
  return {
    id: report.run_ref,
    project: report.project,
    workflow: report.workflow,
    run_number: report.run_number,
    issue_number: report.issue_number,
    title: report.issue_number ? `Issue #${report.issue_number}` : report.run_ref,
    state: report.state,
    cycles: report.attempts_count,
    current_phase: report.current_phase ?? "pending",
    cost_usd: report.cumulative_cost_usd,
    started_at: report.started_at,
    updated_at: report.updated_at,
  };
}

function projectRunReportGraph(report: RunReport, workflow: Workflow | undefined, project: Project): IssueGraph {
  const run = projectRunFromReport(report);
  const graphModel = workflow ? workflowToPhaseGraphModel(workflow) : null;
  const phaseNames = graphModel?.phases.map((phase) => phase.name)
    ?? report.attempts.map((attempt) => attempt.phase)
    ?? [run.current_phase];
  const uniquePhases = Array.from(new Set(phaseNames.length > 0 ? phaseNames : [run.current_phase]));
  const reportIssueRef = report.issue_number !== null ? `${project.name}#${report.issue_number}` : null;
  const issueNode = reportIssueRef ? {
    id: `issue:${reportIssueRef}`,
    kind: "issue" as const,
    label: report.issue_number ? `#${report.issue_number}` : "issue",
    state: null,
    timestamp: report.started_at,
    metadata: {
      project: project.name,
      repo: report.issue_repo ?? project.github_repo,
      issue_ref: reportIssueRef,
      issue_number: report.issue_number,
    },
  } : null;
  const runNode = {
    id: `run:${report.run_ref}`,
    kind: "run" as const,
    label: report.run_display_number ?? (report.run_number !== null ? `Run ${report.run_number}` : report.run_ref),
    state: report.state,
    timestamp: report.started_at,
    metadata: {
      run_ref: report.run_ref,
      run_number: report.run_number,
      run_display_number: report.run_display_number,
      parent_run_ref: report.parent_run_ref,
      root_run_ref: report.root_run_ref,
      origin_kind: report.origin_kind,
      is_cycle: report.is_cycle,
      cycle_number: report.cycle_number,
      workflow: report.workflow,
      cost_usd: report.cumulative_cost_usd,
      issue_ref: report.issue_ref,
      issue_number: report.issue_number,
      validation_url: report.validation_url,
      screenshots_markdown: report.screenshots_markdown,
      abort_reason: report.abort_reason,
      pr_primitive_state: report.abort_reason?.startsWith("PR primitive:") ? "failed" : "pending",
      pr_primitive_error: report.abort_reason?.startsWith("PR primitive:") ? report.abort_reason : null,
      entrypoint_phase: uniquePhases[0] ?? report.current_phase,
      workflow_graph: {
        phases: uniquePhases,
        default_entry: { target: uniquePhases[0] ?? report.current_phase ?? "phase", active: true, kind: "phase" },
        recycle_arrows: graphModel?.recycleArrows ?? [],
        terminal: { kind: "pr", enabled: graphModel?.prEnabled ?? false },
      },
    },
  };
  const attempts = report.attempts.map((attempt) => ({
    id: `attempt:${report.run_ref}:${attempt.attempt_index}`,
    kind: "attempt" as const,
    label: `${attempt.phase} attempt ${attempt.attempt_index}`,
    state: attempt.completed_at ? "completed" : report.state,
    timestamp: attempt.dispatched_at,
    metadata: {
      run_ref: report.run_ref,
      attempt_index: attempt.attempt_index,
      phase: attempt.phase,
      phase_kind: attempt.phase_kind,
      workflow_filename: attempt.workflow_filename,
      workflow_run_id: attempt.workflow_run_id,
      completed_at: attempt.completed_at,
      conclusion: attempt.conclusion,
      verification_status: attempt.verification_status,
      evidence_refs: attempt.evidence_refs,
      summary_markdown: attempt.summary_markdown,
      decision: attempt.decision,
      cost_usd: attempt.cost_usd,
      log_archive_url: attempt.log_archive_url,
      skipped_from_run_ref: attempt.skipped_from_run_ref,
    },
  }));
  return {
    issue_ref: reportIssueRef ?? `${report.project}/runs/${report.run_number ?? "unknown"}`,
    nodes: [
      ...(issueNode ? [issueNode] : []),
      runNode,
      ...attempts,
    ],
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
  const navigate = useNavigate();
  const location = useLocation();
  const [liveReport, setLiveReport] = useState<RunReport | null>(null);
  const [liveError, setLiveError] = useState<string | null>(null);
  const [liveLoading, setLiveLoading] = useState(false);

  useEffect(() => {
    if (isMockMode() || !projectName || !runId) return;
    let cancelled = false;
    setLiveReport(null);
    setLiveError(null);
    setLiveLoading(true);
    const issueScopedRunNumber = issueScopedRunNumberFromSlug(issueNumber, runId);
    if (issueScopedRunNumber === null) {
      setLiveLoading(false);
      setLiveError("Issue-scoped run number required");
      return;
    }
    const reportUrl = `/v1/projects/${encodeURIComponent(projectName)}/issues/${issueNumber}/runs/${issueScopedRunNumber}/report`;
    fetch(reportUrl)
      .then(async (res) => {
        if (!res.ok) throw new Error(`run report ${res.status}`);
        const body = await res.json() as RunReport;
        if (!cancelled) setLiveReport(body);
      })
      .catch((err: unknown) => {
        if (!cancelled) setLiveError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!cancelled) setLiveLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [projectName, issueNumber, runId]);

  if (snap === null) return <div className="empty">Connecting…</div>;
  const project = snap.projects.find((p) => p.name === projectName);
  if (!project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }

  const runs = isMockMode()
    ? mockRuns.filter((candidate) => candidate.project === project.name)
    : [];
  const run = isMockMode() ? resolveProjectRun(runs, runId) : liveReport ? projectRunFromReport(liveReport) : null;
  const workflow = resolveProjectWorkflow(
    snap.workflows,
    run?.project ?? project.name,
    [run?.workflow],
  );
  const graph = liveReport ? projectRunReportGraph(liveReport, workflow ?? undefined, project) : run ? projectRunGraph(run, workflow ?? undefined, project) : null;

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
        {liveLoading && <div className="empty">Loading run detail…</div>}
        {liveError && <div className="empty">Run detail could not be loaded: {liveError}</div>}
        {!liveLoading && !liveError && <div className="empty">Run {runSlugDisplay(runId)} was not found.</div>}
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
              to={`/projects/${encodeURIComponent(project.name)}/workflows/${encodeURIComponent(workflow?.name ?? run.workflow)}`}
              state={{ returnTo: location.pathname, returnLabel: "run" }}
            >
              {workflow?.name ?? run.workflow}
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
        workflow={workflow}
        inFlight={run.state === "in_progress"}
        dispatchState={RUN_VIEWER_IDLE_DISPATCH}
        onRedispatch={() => undefined}
        abortState={RUN_VIEWER_IDLE_ABORT}
        onArmAbort={() => undefined}
        onCancelAbort={() => undefined}
        onConfirmAbort={() => undefined}
        selectedRunId={run.id}
        onBackToRuns={() => undefined}
        onOpenTouchpoint={() => {
          if (run.issue_number) {
            navigate(`/projects/${encodeURIComponent(project.name)}/issues/${run.issue_number}/touchpoint`);
          }
        }}
        actionsVisible={false}
      />
    </div>
  );
}

function runStatePill(state: string): string {
  if (state === "passed") return "free";
  if (state === "in_progress" || state === "needs_review") return "busy";
  if (state === "pending") return "pending";
  if (state === "aborted" || state === "failed") return "drain";
  return "info";
}

function CurrentWorkTable({ leases, emptyText }: { leases: Lease[]; emptyText: string }) {
  return (
    <LeaseTable
      leases={leases}
      emptyText={emptyText}
      detailBasePath={(lease) => `/projects/${encodeURIComponent(lease.project)}/leases/${leaseKind(lease)}`}
      showProject={false}
      signedIn={false}
    />
  );
}

function LeaseIndexView({
  snap,
  signedIn,
  isAdmin,
  kind,
  projectName,
}: LayoutContext & { kind: LeaseKind; projectName?: string }) {
  if (snap === null) return <div className="empty">Connecting…</div>;

  const project = projectName ? snap.projects.find((p) => p.name === projectName) : null;
  if (projectName && !project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }
  if (kind === "test") {
    return (
      <TestEnvironmentIndexView
        snap={snap}
        projectName={projectName}
        signedIn={signedIn}
        isAdmin={isAdmin}
      />
    );
  }

  const leases = leasesFor(snap, kind, projectName);
  const pending = leases.filter((l) => l.state === "pending");
  const active = leases.filter((l) => l.state === "active");
  const basePath = projectName
    ? `/projects/${encodeURIComponent(projectName)}/leases/${kind}`
    : `/leases/${kind}`;

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">{projectName ? `project / ${projectName}` : "global leases"}</div>
          <h2>{leaseKindTitle(kind)}</h2>
          <div className="project-repo mono">{leaseKindDescription(kind)}</div>
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
        </div>
      </section>

      <h2>Active ({active.length})</h2>
      <LeaseTable
        leases={active}
        emptyText={`No active ${leaseKindNoun(kind)} leases.`}
        detailBasePath={basePath}
        showProject={!projectName}
        signedIn={signedIn}
      />

      <h2>Pending ({pending.length})</h2>
      <LeaseTable
        leases={pending}
        emptyText={`No pending ${leaseKindNoun(kind)} leases.`}
        detailBasePath={basePath}
        showProject={!projectName}
        signedIn={signedIn}
      />
    </div>
  );
}

function LeaseDetailView({
  snap,
  signedIn,
  kind,
  leaseId,
  projectName,
}: LayoutContext & { kind: LeaseKind; leaseId: string; projectName?: string }) {
  if (snap === null) return <div className="empty">Connecting…</div>;

  const project = projectName ? snap.projects.find((p) => p.name === projectName) : null;
  if (projectName && !project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }

  const lease = leasesFor(snap, kind, projectName).find((candidate) =>
    leaseIdMatches(candidate, leaseId)
  );

  if (!lease) {
    return <div className="empty">Lease {leaseId || "(missing)"} was not found.</div>;
  }

  const requester = leaseRequester(lease);
  const purpose = leasePurpose(lease);
  const slot = leaseSlot(lease);
  const detailRows = leaseDetailRows(lease);
  const metadata = sanitizeLeaseMetadata(lease.metadata ?? {});

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">{leaseKindNoun(kind)} lease</div>
          <h2>{leaseDisplayName(lease)}</h2>
          <div className="project-repo mono">{lease.ref}</div>
        </div>
        <div className="project-facts">
          <div className="project-fact">
            <span>state</span>
            <strong>{lease.state}</strong>
          </div>
          <div className="project-fact">
            <span>project</span>
            <strong>{lease.project}</strong>
          </div>
          <div className="project-fact">
            <span>workflow</span>
            <strong>{lease.workflow ?? "none"}</strong>
          </div>
        </div>
      </section>

      <section className="project-focus">
        <div>
          <span className="key">requester</span>
          <strong>{requester.label}</strong>
        </div>
        <div>
          <span className="key">purpose</span>
          <span className="mono">{purpose}</span>
        </div>
        <div>
          <span className="key">environment</span>
          <span className="mono">{slot}</span>
        </div>
      </section>

      <h2>Lease</h2>
      <div className="project-info">
        {detailRows.map((row) => (
          <div className="row" key={row.key}>
            <span className="key">{row.key}</span>
            <span className="val mono">{row.value}</span>
          </div>
        ))}
      </div>

      <h2>Requirements</h2>
      <pre className="json-block">{formatJson(lease.requirements)}</pre>

      <h2>Consumer metadata</h2>
      <pre className="json-block">{formatJson(metadata)}</pre>

      {signedIn && (
        <LeaseCancelAction lease={lease} />
      )}
    </div>
  );
}

function TestEnvironmentIndexView({
  snap,
  projectName,
  signedIn,
  isAdmin,
}: {
  snap: Snapshot;
  projectName?: string;
  signedIn: boolean;
  isAdmin: boolean;
}) {
  const environments = (snap.test_environments ?? [])
    .filter((env) => !projectName || env.project === projectName);
  const available = environments.filter((env) => env.state === "available");
  const activating = environments.filter((env) => env.state === "activating");
  const active = environments.filter((env) => env.state === "active");
  const cleaning = environments.filter((env) => env.state === "cleaning");
  const claimed = environments.filter((env) => env.state === "claimed");
  const errored = environments.filter((env) => env.state === "error");
  const project = projectName ? snap.projects.find((p) => p.name === projectName) : null;

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">{projectName ? `project / ${projectName}` : "global test environments"}</div>
          <h2>Test environments</h2>
          <div className="project-repo mono">warm slots and leased runtime</div>
        </div>
        <div className="project-facts">
          <div className="project-fact"><span>available</span><strong>{available.length}</strong></div>
          <div className="project-fact"><span>activating</span><strong>{activating.length}</strong></div>
          <div className="project-fact"><span>active</span><strong>{active.length}</strong></div>
          {cleaning.length > 0 && <div className="project-fact"><span>cleaning</span><strong>{cleaning.length}</strong></div>}
          {claimed.length > 0 && <div className="project-fact"><span>claimed</span><strong>{claimed.length}</strong></div>}
          {errored.length > 0 && <div className="project-fact"><span>error</span><strong>{errored.length}</strong></div>}
          {projectName && <div className="project-fact"><span>configured</span><strong>{environments.length}</strong></div>}
        </div>
      </section>

      {projectName && project && (
        <TestEnvironmentScaleControl
          project={project}
          currentCount={environments.length}
          signedIn={signedIn}
          isAdmin={isAdmin}
        />
      )}

      <h2>Environments ({environments.length})</h2>
      {environments.length === 0 ? (
        <div className="empty">No test environments are registered.</div>
      ) : (
        <table>
          <thead>
            <tr>
              {!projectName && <th>Project</th>}
              <th>Slot</th>
              <th>State</th>
              <th>Lease</th>
              <th>Requester</th>
              <th>Purpose</th>
            </tr>
          </thead>
          <tbody>
            {environments.map((env) => {
              const requester = env.lease ? leaseRequester(env.lease) : { label: "-", title: env.detail ?? env.state };
              const detailTo = env.lease
                ? `/projects/${encodeURIComponent(env.project)}/leases/test/${encodeURIComponent(leaseRouteId(env.lease))}`
                : null;
              return (
                <tr key={`${env.project}:${env.slot_index}`}>
                  {!projectName && (
                    <td><Link className="link" to={`/projects/${encodeURIComponent(env.project)}`}>{env.project}</Link></td>
                  )}
                  <td className="mono">{env.slot_name}</td>
                  <td><span className={`pill ${testEnvironmentPillClass(env.state)}`} title={env.detail ?? env.state}>{env.state}</span></td>
                  <td className="mono dim">
                    {env.lease && detailTo ? (
                      <Link className="link mono" to={detailTo}>{leaseDisplayName(env.lease)}</Link>
                    ) : "-"}
                  </td>
                  <td className="mono dim" title={requester.title}>{requester.label}</td>
                  <td className="lease-purpose" title={env.lease ? leasePurpose(env.lease) : "-"}>{env.lease ? leasePurpose(env.lease) : "-"}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

function TestEnvironmentScaleControl({
  project,
  currentCount,
  signedIn,
  isAdmin,
}: {
  project: Project;
  currentCount: number;
  signedIn: boolean;
  isAdmin: boolean;
}) {
  const [draft, setDraft] = useState(() => String(projectTestEnvironmentCount(project, currentCount)));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const configuredCount = projectTestEnvironmentCount(project, currentCount);
  const canSave = signedIn && isAdmin;
  const authRedirectStatus = nativeAuthRedirectStatus(project);

  useEffect(() => {
    if (!saving) setDraft(String(configuredCount));
  }, [configuredCount, saving]);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!canSave) return;
    const count = Number.parseInt(draft, 10);
    if (!Number.isFinite(count) || count < 0 || count > 50) {
      setError("Count must be between 0 and 50.");
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const response = await authedFetch(`/v1/projects/${encodeURIComponent(project.name)}/test-environments/count`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ count }),
      });
      if (!response.ok) {
        const text = await response.text().catch(() => "");
        setError(`${response.status} ${text || response.statusText}`);
      }
    } catch (err) {
      setError(String(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <form className="test-env-scale" onSubmit={submit}>
      <label htmlFor="test-env-count">Test environment count</label>
      <input
        id="test-env-count"
        type="number"
        min={0}
        max={50}
        value={draft}
        onChange={(event) => setDraft(event.target.value)}
        disabled={!canSave || saving}
      />
      <button type="submit" disabled={!canSave || saving || draft === String(configuredCount)}>
        {saving ? "saving..." : "apply"}
      </button>
      {authRedirectStatus && (
        <span className="test-env-auth-status mono" title={authRedirectStatus.title}>
          auth redirects <span className={`pill ${authRedirectStatus.pill}`}>{authRedirectStatus.state}</span>
        </span>
      )}
      {!canSave && <span className="dim mono">admin sign-in required</span>}
      {error && <span className="danger-text mono">{error}</span>}
    </form>
  );
}

function LeaseTable({
  leases,
  emptyText,
  detailBasePath,
  showProject,
  signedIn,
}: {
  leases: Lease[];
  emptyText: string;
  detailBasePath: string | ((lease: Lease) => string) | null;
  showProject: boolean;
  signedIn: boolean;
}) {
  if (leases.length === 0) {
    return <div className="empty">{emptyText}</div>;
  }

  return (
    <table>
      <thead>
        <tr>
          <th>Lease</th>
          {showProject && <th>Project</th>}
          <th>Workflow</th>
          <th>State</th>
          <th>Environment</th>
          <th>Requester</th>
          <th>Purpose</th>
          <th>Requested</th>
          {signedIn && <th></th>}
        </tr>
      </thead>
      <tbody>
        {leases.map((lease) => {
          const requester = leaseRequester(lease);
          const detailBase = typeof detailBasePath === "function" ? detailBasePath(lease) : detailBasePath;
          const detailTo = detailBase ? `${detailBase}/${encodeURIComponent(leaseRouteId(lease))}` : null;
          return (
            <tr key={lease.ref}>
              <td className="mono">
                {detailTo ? (
                  <Link className="link mono" to={detailTo}>
                    {leaseDisplayName(lease)}
                  </Link>
                ) : (
                  leaseDisplayName(lease)
                )}
              </td>
              {showProject && (
                <td>
                  <Link className="link" to={`/projects/${encodeURIComponent(lease.project)}`}>
                    {lease.project}
                  </Link>
                </td>
              )}
              <td className="mono dim">{lease.workflow ?? "-"}</td>
              <td><span className={`pill ${lease.state === "claimed" ? "busy" : "info"}`}>{lease.state}</span></td>
              <td className="mono dim">{leaseSlot(lease)}</td>
              <td className="mono dim" title={requester.title}>{requester.label}</td>
              <td className="lease-purpose" title={leasePurpose(lease)}>{leasePurpose(lease)}</td>
              <td className="mono dim">{relTime(lease.requested_at)}</td>
              {signedIn && (
                <td>
                  <LeaseCancelAction lease={lease} compact />
                </td>
              )}
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

function LeaseCancelAction({ lease, compact = false }: { lease: Lease; compact?: boolean }) {
  const [confirmId, setConfirmId] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [cancelError, setCancelError] = useState<string | null>(null);

  const fireCancel = async (lease: Lease) => {
    setBusyId(lease.ref);
    setCancelError(null);
    try {
      const r = await authedFetch("/v1/leases/cancel", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ project: lease.project, lease_ref: lease.ref }),
      });
      if (!r.ok) {
        const text = await r.text().catch(() => "");
        setCancelError(`cancel ${leaseDisplayName(lease)}: ${r.status} ${text || r.statusText}`);
      }
    } catch (e) {
      setCancelError(String(e));
    } finally {
      setBusyId(null);
      setConfirmId(null);
    }
  };

  if (confirmId === lease.ref) {
    return (
      <>
        <span className="confirm">
          <button
            type="button"
            className="link danger-text"
            onClick={() => void fireCancel(lease)}
            disabled={busyId === lease.ref}
          >
            {busyId === lease.ref ? "cancelling..." : "cancel?"}
          </button>
          <span className="sep">/</span>
          <button
            type="button"
            className="link"
            onClick={() => setConfirmId(null)}
            disabled={busyId === lease.ref}
          >
            keep
          </button>
        </span>
        {cancelError && !compact && <div className="empty error">{cancelError}</div>}
      </>
    );
  }

  return (
    <>
      <button type="button" className="link" onClick={() => setConfirmId(lease.ref)}>
        cancel
      </button>
      {cancelError && !compact && <div className="empty error">{cancelError}</div>}
    </>
  );
}

function leasesFor(snap: Snapshot, kind: LeaseKind, projectName?: string): Lease[] {
  return [...snap.active_leases, ...snap.pending_leases]
    .filter((lease) => leaseKind(lease) === kind)
    .filter((lease) => !projectName || lease.project === projectName)
    .sort((a, b) => {
      if (a.state !== b.state) return a.state === "claimed" ? -1 : 1;
      return new Date(b.requested_at).getTime() - new Date(a.requested_at).getTime();
    });
}

function leaseKind(lease: Lease): LeaseKind {
  if (lease.workflow === "test-slot-checkout" || lease.metadata?.test_slot_checkout === true) {
    return "test";
  }
  return "agent";
}

function leaseKindTitle(kind: LeaseKind): string {
  return kind === "test" ? "Test leases" : "Agent leases";
}

function leaseKindNoun(kind: LeaseKind): string {
  return kind === "test" ? "test" : "agent";
}

function leaseKindDescription(kind: LeaseKind): string {
  return kind === "test"
    ? "test environments and native slots"
    : "agent runners and work leases that are not test environments";
}

function testEnvironmentPillClass(state: TestEnvironment["state"]): "free" | "busy" | "drain" | "info" {
  switch (state) {
    case "available":
      return "free";
    case "active":
    case "claimed":
      return "busy";
    case "error":
      return "drain";
    case "warming":
    case "activating":
    case "cleaning":
    default:
      return "info";
  }
}

function projectUsesNativeWorkflows(project: Project): boolean {
  const metadata = project.metadata ?? {};
  return metadata.native_webapp === true
    || metadata.nativeWebapp === true
    || isNativeWebappMetadataValue(metadata.app_kind)
    || isNativeWebappMetadataValue(metadata.appKind)
    || isNativeWebappMetadataValue(metadata.app_type)
    || isNativeWebappMetadataValue(metadata.appType)
    || isNativeWebappMetadataValue(metadata.kind);
}

function isNativeWebappMetadataValue(value: unknown): boolean {
  if (typeof value !== "string") return false;
  switch (value.trim().toLowerCase()) {
    case "native_webapp":
    case "native-webapp":
    case "native webapp":
    case "native_web_app":
    case "native-web-app":
    case "native web app":
      return true;
    default:
      return false;
  }
}

function projectTestEnvironmentCount(project: Project, fallback: number): number {
  const standby = project.metadata?.native_standby_dns;
  if (isRecord(standby)) {
    const count = standby.count;
    if (typeof count === "number" && Number.isFinite(count)) return count;
    if (typeof count === "string" && count.trim()) {
      const parsed = Number.parseInt(count, 10);
      if (Number.isFinite(parsed)) return parsed;
    }
  }
  return fallback;
}

function nativeAuthRedirectStatus(project: Project): { state: string; pill: "free" | "drain" | "info"; title: string } | null {
  const raw = project.metadata?.native_auth_redirects_status;
  if (!isRecord(raw)) return null;
  const state = valueLabel(raw.state).toLowerCase();
  if (!state) return null;
  const desiredCount = valueLabel(raw.desired_count);
  const lastError = valueLabel(raw.last_error);
  const title = [
    desiredCount ? `desired ${desiredCount}` : "",
    lastError,
  ].filter(Boolean).join(" / ") || state;
  return {
    state,
    pill: state === "ok" ? "free" : state === "failed" ? "drain" : "info",
    title,
  };
}

function leaseRouteId(lease: Lease): string {
  if (leaseKind(lease) === "test") {
    return lease.ref;
  }
  if (lease.lease_number !== null && lease.lease_number !== undefined) {
    return String(lease.lease_number);
  }
  return lease.ref;
}

function leaseIdMatches(lease: Lease, leaseId: string): boolean {
  return leaseRouteId(lease) === leaseId || lease.ref === leaseId || encodeURIComponent(lease.ref) === leaseId;
}

function leaseRequester(lease: Lease): { label: string; title: string } {
  if (isRecord(lease.requester)) {
    const ref = valueLabel(lease.requester.ref);
    const label = ref || valueLabel(lease.requester.label) || valueLabel(lease.requester.consumer);
    const detail = [lease.requester.consumer, lease.requester.kind, lease.requester.ref]
      .map(valueLabel)
      .filter(Boolean)
      .join(" / ");
    if (label) return { label, title: detail || label };
  }
  const metadata = lease.metadata ?? {};
  const requester = metadata.requester;
  if (isRecord(requester)) {
    const ref = valueLabel(requester.ref);
    const label = ref || valueLabel(requester.label) || valueLabel(requester.consumer);
    const detail = [requester.consumer, requester.kind, requester.ref]
      .map(valueLabel)
      .filter(Boolean)
      .join(" / ");
    if (label) return { label, title: detail || label };
  }
  const requesterRef = valueLabel(metadata.requester_ref) || valueLabel(metadata.requesterRef);
  if (requesterRef) return { label: requesterRef, title: requesterRef };
  const tankSessionId = valueLabel(metadata.tank_session_id) || valueLabel(metadata.tankSessionId);
  if (tankSessionId) return { label: `tank session ${tankSessionId}`, title: tankSessionId };
  const issueNumber = metadata.issue_number ?? metadata.issueNumber;
  if (typeof issueNumber === "number" || typeof issueNumber === "string") {
    return { label: `issue #${issueNumber}`, title: `issue #${issueNumber}` };
  }
  return { label: "-", title: "No requester recorded on this lease" };
}

function leasePurpose(lease: Lease): string {
  const metadata = lease.metadata ?? {};
  const phaseInputs = metadata.phase_inputs;
  if (isRecord(phaseInputs)) {
    const purpose = valueLabel(phaseInputs.purpose);
    if (purpose) return purpose;
  }
  return valueLabel(metadata.purpose) || valueLabel(metadata.reason) || "-";
}

function leaseSlot(lease: Lease): string {
  const metadata = lease.metadata ?? {};
  return (
    valueLabel(metadata.native_slot_name)
    || valueLabel(metadata.slot_name)
    || valueLabel(metadata.validation_url)
    || lease.host
    || "-"
  );
}

function leaseDisplayName(lease: Lease): string {
  if (leaseKind(lease) === "test") {
    return lease.ref;
  }
  if (lease.lease_number !== null && lease.lease_number !== undefined) {
    return `#${lease.lease_number}`;
  }
  const slotName = lease.metadata?.native_slot_name;
  if (typeof slotName === "string" && slotName) {
    return slotName;
  }
  const issueNumber = lease.metadata?.issue_number ?? lease.metadata?.issueNumber;
  if (typeof issueNumber === "number" || typeof issueNumber === "string") {
    return `issue #${issueNumber}`;
  }
  return "lease";
}

function leaseDetailRows(lease: Lease): Array<{ key: string; value: string }> {
  return [
    { key: "ref", value: lease.ref },
    { key: "type", value: leaseKindTitle(leaseKind(lease)) },
    { key: "project", value: lease.project },
    { key: "workflow", value: lease.workflow ?? "-" },
    { key: "state", value: lease.state },
    { key: "host", value: lease.host ?? "-" },
    { key: "requested", value: formatDateTime(lease.requested_at) },
    { key: "assigned", value: formatDateTime(lease.assigned_at) },
    { key: "released", value: formatDateTime(lease.released_at) },
    { key: "ttl", value: `${lease.ttl_seconds}s` },
  ];
}

function sanitizeLeaseMetadata(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sanitizeLeaseMetadata);
  if (!isRecord(value)) return value;
  return Object.fromEntries(
    Object.entries(value).map(([key, entry]) => {
      const lowered = key.toLowerCase();
      if (lowered.includes("token") || lowered.includes("secret") || lowered.includes("password")) {
        return [key, "[redacted]"];
      }
      return [key, sanitizeLeaseMetadata(entry)];
    })
  );
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function valueLabel(value: unknown): string {
  if (typeof value === "string") return value.trim();
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return "";
}

function formatJson(value: unknown): string {
  return JSON.stringify(value, null, 2);
}

function formatDateTime(iso: string | null): string {
  return iso ? new Date(iso).toLocaleString() : "-";
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

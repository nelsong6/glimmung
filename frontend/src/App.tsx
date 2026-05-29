import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Link, Navigate, NavLink, Outlet, Route, Routes, useLocation, useNavigate, useOutletContext, useParams } from "react-router-dom";
import { AdminPanel } from "./AdminPanel";
import { IssueDetailView, RunViewer, type AbortState, type DispatchState, type IssueGraph } from "./IssueDetailView";
import { IssuesView } from "./IssuesView";
import { PlaybooksView } from "./PlaybooksView";
import { PortfolioView } from "./PortfolioView";
import { TouchpointsView } from "./TouchpointsView";
import { StyleguideView } from "./StyleguideView";
import { PhaseGraph, type PhaseGraphPhase } from "./PhaseGraph";
import { workflowToPhaseGraphModel } from "./workflowGraphModel";
import { resolveProjectWorkflow } from "./workflowLookup";
import { authedFetch, currentAccount, initAuth, signIn, signOut, type Account } from "./auth";
import { isMockMode, mockRuns, mockSnapshot } from "./mockApi";

type Lease = {
  ref: string;
  lease_number?: number | null;
  project: string;
  workflow: string | null;
  host: string | null;
  state: "active" | "claimed" | "released" | "expired";
  requirements: Record<string, unknown>;
  metadata: Record<string, unknown>;
  requester: Record<string, unknown> | null;
  requested_at: string;
  assigned_at: string | null;
  released_at: string | null;
  ttl_seconds: number;
  playwright_ws_endpoint?: string | null;
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

type TestSlotReturnHistoryEntry = {
  event: string;
  created_at: string;
  project: string;
  slot_index?: number | null;
  slot_name?: string | null;
  lease_ref: string;
  lease_number?: number | null;
  lease_requester?: string | null;
  caller_pod_ip?: string | null;
  caller_session_id?: string | null;
  source: string;
  reason?: string | null;
  cleanup_started: boolean;
};

type TestEnvironment = {
  project: string;
  slot_index: number;
  slot_name: string;
  // "" means no durable slot status record exists yet (count was bumped and
  // the reconciler has not yet seeded this slot). It is not synonymous with
  // "warming" — warming means preliminary reconciliation is actually running.
  state: "" | "available" | "warming" | "activating" | "active" | "cleaning" | "claimed" | "reserved" | "error";
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
  test_slot_return_history?: TestSlotReturnHistoryEntry[];
  playwright_ws_endpoint?: string | null;
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
  always?: boolean;
  evidence_verification_gate?: boolean;
  depends_on?: string[];
  recycle_policy: RecyclePolicy | null;
  jobs?: NativeJobSpec[];
};

type NativeJobSpec = {
  id: string;
  name?: string | null;
  image?: string;
  primitive?: string;
};

type PrPrimitiveSpec = {
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
  run_display_number?: string | null;
  cycle_number?: number | null;
  run_cycle_number?: number | null;
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
  dispatched_at: string;
  completed_at: string | null;
  conclusion: string | null;
  verification_status: string | null;
  evidence_refs: string[];
  summary_markdown: string | null;
  decision: string | null;
  cost_usd: number | null;
  log_archive_url: string | null;
};

type RunReport = {
  ref: string;
  project: string;
  run_ref: string;
  run_number: number | null;
  run_display_number: string | null;
  run_cycle_number: number | null;
  parent_run_ref: string | null;
  root_run_ref: string | null;
  origin_kind: string | null;
  is_cycle: boolean;
  cycle_number: number | null;
  workflow_schema_ref: string | null;
  queue_state: string | null;
  admission_error: string | null;
  slot_lease_ref: string | null;
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
  active_leases: Lease[];
  test_environments?: TestEnvironment[];
  waiting_test_slot_requests?: TestSlotRequest[];
  test_lease_defaults?: TestLeaseDefaults;
  projects: Project[];
  workflows: Workflow[];
  inflight_locks?: InflightLocks;
};

type TestLeaseDefaults = {
  global_ttl_seconds: number;
  hot_swap_min_ttl_seconds?: number;
};

export type Connection = "live" | "stale" | "dead";

export const CONNECTION_STALE_AFTER_MS = 5000;
export const CONNECTION_DEAD_AFTER_MS = 30000;

export function connectionStateFromSnapshotClock(now: number, startedAt: number, lastSeen: number): Connection {
  if (lastSeen <= 0) {
    return now - startedAt >= CONNECTION_DEAD_AFTER_MS ? "dead" : "stale";
  }
  const age = now - lastSeen;
  if (age >= CONNECTION_DEAD_AFTER_MS) return "dead";
  if (age >= CONNECTION_STALE_AFTER_MS) return "stale";
  return "live";
}

// Server pushes this on every SSE snapshot tick. Drives the "needs
// attention" nav dot. Optional in the type because the snapshot may
// arrive from an older server during a rolling deploy; treat as
// all-false when missing.
type InflightLocks = {
  issues: boolean;
  prs: boolean;
};

type Selection =
  | { kind: "all" }
  | { kind: "project"; project: string }
  | { kind: "workflow"; project: string; workflow: string };

type LayoutContext = {
  snap: Snapshot | null;
  signedIn: boolean;
  isAdmin: boolean;
  selected: Selection;
};

const ALL: Selection = { kind: "all" };
const testSlotBuiltInDefaultTTLSeconds = 3600;
const testSlotBuiltInHotSwapMinTTLSeconds = 1800;

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
        <Route path="projects/:project/leases/test/slots/:slotId" element={<ProjectTestEnvironmentDetailRoute />} />
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
          <Route path="runs" element={null} />
          <Route path="runs/:runId" element={null} />
          <Route path="runs/:runId/cycles/:cycleId" element={null} />
          <Route path="runs/:runId/cycles/:cycleId/phases/:phaseId" element={null} />
          <Route path="runs/:runId/cycles/:cycleId/phases/:phaseId/jobs/:jobId" element={null} />
          <Route path="runs/:runId/cycles/:cycleId/phases/:phaseId/jobs/:jobId/steps/:stepId" element={null} />
          <Route path="workflow" element={null} />
          <Route path="workflow/:workflowRunId" element={null} />
          <Route path="touchpoint" element={null} />
        </Route>
        <Route path="projects/:project/needs-attention" element={<ProjectNeedsAttentionRoute />} />
        <Route path="projects/:project/runs" element={<ProjectRunsRoute />} />
        <Route path="projects/:project/runs/:runId" element={<ProjectRunRedirectRoute />} />
        <Route path="issues" element={<Navigate to="/needs-attention" replace />} />
        <Route path="touchpoints" element={<TouchpointsRoute />} />
        <Route path="portfolio" element={<PortfolioRoute />} />
      </Route>
    </Routes>
  );
}

function MockModeRedirect() {
  const location = useLocation();
  const targetPath = location.pathname.replace(/^\/_mock/, "") || "/";
  const params = new URLSearchParams(location.search);
  params.set("mock", "1");
  const search = params.toString();
  return <Navigate to={`${targetPath}${search ? `?${search}` : ""}${location.hash}`} replace />;
}

function Layout() {
  const location = useLocation();
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const selected = ALL;
  const [account, setAccount] = useState<Account | null>(null);
  const [authReady, setAuthReady] = useState(false);
  const [isAdmin, setIsAdmin] = useState(false);
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
      return;
    }

    let es: EventSource | null = null;

    const connect = () => {
      es = new EventSource("/v1/events");
      es.addEventListener("state", (e) => {
        try {
          setSnap(JSON.parse((e as MessageEvent).data));
        } catch (err) {
          console.error("bad snapshot", err);
        }
      });
    };

    connect();

    return () => {
      es?.close();
    };
  }, []);

  // The "needs attention" nav dot derives from snap.inflight_locks,
  // which the server pushes on every SSE snapshot tick. The 20-second
  // poll of /v1/issues + /v1/touchpoints that previously fed this
  // boolean was deleted because it forced unnecessary cross-project work on
  // every tick only to compute a single bool.
  const inflightIssues = Boolean(snap?.inflight_locks?.issues || snap?.inflight_locks?.prs);

  const ctx: LayoutContext = {
    snap,
    signedIn: !!account,
    isAdmin,
    selected,
  };

  const dashboardLinkClass = ({ isActive }: { isActive: boolean }) =>
    `dashboard-nav-link ${isActive ? "selected" : ""}`;
  const homeRoute = location.pathname === "/";
  const breadcrumbs = buildBreadcrumbs(location.pathname);
  const returnTarget = returnTargetFromState(location.state, location.pathname);

  return (
    <div className="layout">
      <main className="content">
        <header className="app-header">
          <div className="header-left">
            <div className="header-title">
              <h1>glimmung</h1>
              {isMockMode() && <span className="connection info">mock</span>}
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
                className="gb sm primary"
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
                {inflightIssues && <span className="tab-dot" />}
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

export function buildBreadcrumbs(pathname: string): Breadcrumb[] {
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
      if (parts[4] === "slots") {
        if (parts[5]) crumbs.push({ label: `Slot ${parts[5]}` });
      } else if (parts[4]) {
        crumbs.push({ label: `Lease ${parts[4]}` });
      }
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
        appendIssueRunBreadcrumbs(crumbs, parts);
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
  if (parts[0] === "touchpoints") {
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

function appendIssueRunBreadcrumbs(crumbs: Breadcrumb[], parts: string[]): void {
  const project = encodeURIComponent(parts[1] ?? "");
  const issue = encodeURIComponent(parts[3] ?? "");
  const runId = parts[5];
  if (!runId) return;

  const runsPath = `/projects/${project}/issues/${issue}/runs`;
  const runPath = `${runsPath}/${encodeURIComponent(runId)}`;
  const hasCycle = parts[6] === "cycles" && Boolean(parts[7]);
  crumbs.push({
    label: hasCycle ? `run ${runId}` : runSlugDisplay(runId),
    to: parts[6] ? runPath : undefined,
  });

  if (!hasCycle) return;
  const cycleId = parts[7] ?? "";
  const cyclePath = `${runPath}/cycles/${encodeURIComponent(cycleId)}`;
  crumbs.push({
    label: `cycle ${cycleId}`,
    to: parts[8] ? cyclePath : undefined,
  });

  if (parts[8] !== "phases" || !parts[9]) return;
  const phaseId = parts[9];
  const phasePath = `${cyclePath}/phases/${encodeURIComponent(phaseId)}`;
  crumbs.push({
    label: `phase ${phaseId}`,
    to: parts[10] ? phasePath : undefined,
  });

  if (parts[10] !== "jobs" || !parts[11]) return;
  const jobId = parts[11];
  const jobPath = `${phasePath}/jobs/${encodeURIComponent(jobId)}`;
  crumbs.push({
    label: `job ${jobId}`,
    to: parts[12] ? jobPath : undefined,
  });

  if (parts[12] !== "steps" || !parts[13]) return;
  crumbs.push({ label: `step ${parts[13]}` });
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

function ProjectTestEnvironmentDetailRoute() {
  const params = useParams<{ project?: string; slotId?: string }>();
  const ctx = useOutletContext<LayoutContext>();
  return (
    <TestEnvironmentDetailView
      {...ctx}
      projectName={decodeURIComponent(params.project ?? "")}
      slotId={decodeURIComponent(params.slotId ?? "")}
    />
  );
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

function HomeView({ snap }: LayoutContext) {
  const projects = snap?.projects.length ?? 0;
  const workflows = snap?.workflows.length ?? 0;
  const active = snap?.active_leases.length ?? 0;

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
          <strong>Active native agent work leases</strong>
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
            </tr>
          </thead>
          <tbody>
            {projects.map((project) => {
              const workflows = snap.workflows.filter((w) => w.project === project.name);
              const active = snap.active_leases.filter((l) => l.project === project.name);
              const testLeases = active.filter((l) => leaseKind(l) === "test");
              const agentLeases = active.filter((l) => leaseKind(l) === "agent");
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
                    <Link className="link mono" to={`/projects/${encodeURIComponent(project.name)}/leases/agent`}>
                      {agentLeases.length}
                    </Link>
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

  const active = snap.active_leases.filter((l) => l.project === project.name);
  const testLeases = active.filter((l) => leaseKind(l) === "test");
  const agentLeases = active.filter((l) => leaseKind(l) === "agent");
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
            <span>active</span>
            <strong>{active.length}</strong>
          </div>
        </div>
      </section>

      <section className="home-links" aria-label={`${project.name} destinations`}>
        <Link to={`${projectPath}/issues`} className="home-link">
          <span className="key">Issues</span>
          <strong>All open issues for {project.name}</strong>
        </Link>
        <Link to={`${projectPath}/leases/test`} className="home-link">
          <span className="key">Test leases</span>
          <strong>{testLeases.length} active test environment lease{testLeases.length === 1 ? "" : "s"}</strong>
        </Link>
        <Link to={`${projectPath}/leases/agent`} className="home-link">
          <span className="key">Agent leases</span>
          <strong>{agentLeases.length} active agent lease{agentLeases.length === 1 ? "" : "s"}</strong>
        </Link>
        <Link to={`${projectPath}/workflows`} className="home-link">
          <span className="key">Workflows</span>
          <strong>Definitions, triggers, requirements, and workflow-scoped work</strong>
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
              <th>Requires</th>
              <th>Work</th>
            </tr>
          </thead>
          <tbody>
            {workflows.map((w) => {
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
                  <td><RequirementPills requirements={w.default_requirements} /></td>
                  <td className="mono dim">{wActive} active</td>
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
    const jobs = phase.jobs && phase.jobs.length > 0
      ? phase.jobs
      : [{ id: phase.name, name: phase.name }];
    return (
      <>
        {jobs.map((job) => (
          <div className="dag-node dag-node-phase dag-node-definition" key={job.id}>
            <div className="dag-job-head">
              <span className="dag-job-title">{job.name || job.id}</span>
              <span className="dag-job-kicker">job</span>
            </div>
            <div className="dag-node-meta dim mono">{job.id === phase.name ? meta : job.id}</div>
          </div>
        ))}
      </>
    );
  };

  return (
    <section>
      <h2>Workflow graph</h2>
      <div className="dag-wrap">
        <PhaseGraph
          phases={graphModel.phases}
          dagClassName="dag-definition"
          ariaLabel={`${workflow.name} workflow graph`}
          renderPhase={renderPhase}
          entryArrows={graphModel.entryArrows}
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

  const active = snap.active_leases.filter((l) => l.project === project.name && l.workflow === workflow.name);
  const currentWork = active;

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
      <CurrentWorkTable leases={currentWork} emptyText={`No active work for ${workflow.name}.`} />

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
      <IssuesView
        signedIn={signedIn}
        projectFilter={project.name}
        headingLabel="Issues"
        showProjectColumn={false}
        allowStateFilter
      />
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
  const currentWork = active;

  const runs = isMockMode()
    ? mockRuns.filter((run) => run.project === project.name)
    : liveRuns;
  const completedRuns = runs.filter((run) => !runStateIsActive(run.state));

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
            emptyText={`No active or completed runs for ${project.name}.`}
          />
        ) : null
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
            <th>Cycle</th>
            <th>Run</th>
            <th>Run cycle</th>
            <th>Workflow</th>
            <th>Issue</th>
            <th>State</th>
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
              <td className="mono">{run.run_number ?? "-"}</td>
              <td className="mono">{run.run_cycle_number ?? "-"}</td>
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
  if (run.cycle_number !== null && run.cycle_number !== undefined) return `cycle ${run.cycle_number}`;
  if (run.run_display_number) return `cycle ${run.run_display_number}`;
  if (run.issue_number === null) return `run-${index + 1}`;
  const sameIssue = runs.filter((candidate) => candidate.issue_number === run.issue_number);
  const ordinal = sameIssue.findIndex((candidate) => candidate.id === run.id) + 1;
  return `cycle ${Math.max(ordinal, 1)}`;
}

function projectRunSlug(run: ProjectRun, runs: ProjectRun[], index: number): string {
  if (run.run_display_number) return run.run_display_number;
  if (run.cycle_number !== null && run.cycle_number !== undefined) return String(run.cycle_number);
  return projectRunLabel(run, runs, index).replace(/^cycle\s+/, "").replace(/^#/, "");
}

function projectRunHref(run: ProjectRun, runSlug: string): string {
  if (run.issue_number !== null) {
    return `/projects/${encodeURIComponent(run.project)}/issues/${run.issue_number}/runs/${encodeURIComponent(runSlug)}`;
  }
  return `/projects/${encodeURIComponent(run.project)}/runs/${encodeURIComponent(run.id)}`;
}

function runSlugDisplay(slug: string): string {
  return /^\d+(\.\d+)?$/.test(slug) ? `cycle ${slug}` : slug;
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
      run_number: run.run_number,
      run_display_number: run.run_display_number,
      cycle_number: run.cycle_number,
      run_cycle_number: run.run_cycle_number,
      cycles_count: run.cycles,
      cumulative_cost_usd: run.cost_usd,
      entrypoint_phase: phaseNames[0] ?? run.current_phase,
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
        completed_at: runStateIsActive(run.state) ? null : run.updated_at,
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
    run_display_number: report.run_display_number,
    cycle_number: report.cycle_number,
    run_cycle_number: report.run_cycle_number,
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
    label: report.cycle_number !== null ? `cycle ${report.cycle_number}` : report.run_display_number ?? report.run_ref,
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
      run_cycle_number: report.run_cycle_number,
      workflow_schema_ref: report.workflow_schema_ref,
      queue_state: report.queue_state,
      admission_error: report.admission_error,
      slot_lease_ref: report.slot_lease_ref,
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
      completed_at: attempt.completed_at,
      conclusion: attempt.conclusion,
      verification_status: attempt.verification_status,
      evidence_refs: attempt.evidence_refs,
      summary_markdown: attempt.summary_markdown,
      decision: attempt.decision,
      cost_usd: attempt.cost_usd,
      log_archive_url: attempt.log_archive_url,
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
    if (issueNumber === null) {
      setLiveLoading(false);
      setLiveError("Issue-scoped cycle required");
      return;
    }
    const reportUrl = `/v1/projects/${encodeURIComponent(projectName)}/issues/${issueNumber}/runs/${encodeURIComponent(runId)}/report`;
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
        inFlight={runStateIsActive(run.state)}
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
  if (state === "in_progress" || state === "queued" || state === "pending" || state === "needs_review") return "busy";
  if (state === "aborted" || state === "failed") return "drain";
  return "info";
}

function runStateIsActive(state: string): boolean {
  return state === "in_progress" || state === "queued" || state === "pending";
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
            <strong>{leases.length}</strong>
          </div>
        </div>
      </section>

      <h2>Active ({leases.length})</h2>
      <LeaseTable
        leases={leases}
        emptyText={`No active ${leaseKindNoun(kind)} leases.`}
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
  isAdmin,
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

      {leaseKind(lease) === "test" && (
        <LeaseTTLAction lease={lease} signedIn={signedIn} isAdmin={isAdmin} />
      )}

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
  // "" means the count is set but the reconciler has not yet seeded a status
  // record. It's expected for ~15s after a count bump or rollout; persistently
  // non-zero suggests the reconciler is failing — surface it as its own KPI.
  const unseeded = environments.filter((env) => env.state === "");
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
          {unseeded.length > 0 && <div className="project-fact"><span>unseeded</span><strong>{unseeded.length}</strong></div>}
          {projectName && <div className="project-fact"><span>configured</span><strong>{environments.length}</strong></div>}
        </div>
      </section>

      <TestLeaseDefaultTTLSettings
        snap={snap}
        project={project ?? undefined}
        signedIn={signedIn}
        isAdmin={isAdmin}
      />

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
              <th>Work</th>
              <th>Lease</th>
              <th>TTL</th>
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
              const slotTo = testEnvironmentDetailPath(env);
              return (
                <tr key={`${env.project}:${env.slot_index}`}>
                  {!projectName && (
                    <td><Link className="link" to={`/projects/${encodeURIComponent(env.project)}`}>{env.project}</Link></td>
                  )}
                  <td className="mono"><Link className="link mono" to={slotTo}>{env.slot_name}</Link></td>
                  <td><span className={`pill ${testEnvironmentPillClass(env.state)}`} title={env.detail ?? testEnvironmentStateLabel(env.state)}>{testEnvironmentStateLabel(env.state)}</span></td>
                  <td className="mono dim" title={testEnvironmentWorkTitle(env)}>{testEnvironmentWorkLabel(env)}</td>
                  <td className="mono dim">
                    {env.lease && detailTo ? (
                      <Link className="link mono" to={detailTo}>{leaseDisplayName(env.lease)}</Link>
                    ) : "-"}
                  </td>
                  <td>
                    {env.lease ? (
                      <LeaseTTLAction lease={env.lease} signedIn={signedIn} isAdmin={isAdmin} compact />
                    ) : (
                      <span className="mono dim">-</span>
                    )}
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

type DetailRow = {
  key: string;
  value: ReactNode;
  title?: string;
};

function DetailRows({ rows }: { rows: DetailRow[] }) {
  return (
    <div className="project-info">
      {rows.map((row) => (
        <div className="row" key={row.key}>
          <span className="key">{row.key}</span>
          <span className="val mono" title={row.title}>{row.value}</span>
        </div>
      ))}
    </div>
  );
}

function TestEnvironmentDetailView({
  snap,
  signedIn,
  isAdmin,
  projectName,
  slotId,
}: LayoutContext & { projectName: string; slotId: string }) {
  if (snap === null) return <div className="empty">Connecting…</div>;

  const project = snap.projects.find((p) => p.name === projectName);
  if (!project) {
    return <div className="empty">Project {projectName || "(missing)"} was not found.</div>;
  }

  const env = (snap.test_environments ?? [])
    .filter((candidate) => candidate.project === projectName)
    .find((candidate) => testEnvironmentIdMatches(candidate, slotId));
  if (!env) {
    return <div className="empty">Slot {slotId || "(missing)"} was not found.</div>;
  }

  const waitingRequests = env.waiting_requests ?? [];
  const history = env.test_slot_return_history ?? [];
  const lease = env.lease;
  const workLabel = testEnvironmentWorkLabel(env);
  const leaseDetailTo = lease
    ? `/projects/${encodeURIComponent(lease.project)}/leases/test/${encodeURIComponent(leaseRouteId(lease))}`
    : null;
  const requester = lease ? leaseRequester(lease) : null;

  return (
    <div className="project-workspace">
      <section className="project-hero">
        <div className="project-hero-main">
          <div className="project-kicker mono">project / {env.project} / slot {env.slot_index}</div>
          <h2>{env.slot_name}</h2>
          <div className="project-repo mono">test environment slot</div>
        </div>
        <div className="project-facts">
          <div className="project-fact"><span>state</span><strong>{testEnvironmentStateLabel(env.state)}</strong></div>
          <div className="project-fact"><span>usable</span><strong>{env.usable ? "yes" : "no"}</strong></div>
          <div className="project-fact"><span>waiting</span><strong>{waitingRequests.length}</strong></div>
          <div className="project-fact"><span>lease</span><strong>{lease ? "active" : "none"}</strong></div>
        </div>
      </section>

      <section className="project-focus test-env-focus">
        <div>
          <span className="key">work</span>
          <strong className="mono" title={testEnvironmentWorkTitle(env)}>{workLabel}</strong>
        </div>
        <div>
          <span className="key">detail</span>
          <span className="mono">{env.detail ?? "-"}</span>
        </div>
        <div>
          <span className="key">playwright</span>
          <span className="mono">{env.playwright_ws_endpoint ? "ready" : "-"}</span>
        </div>
      </section>

      <h2>Slot</h2>
      <DetailRows rows={testEnvironmentDetailRows(env)} />

      <h2>Lifecycle</h2>
      <DetailRows rows={testEnvironmentLifecycleRows(env)} />

      <h2>Lease</h2>
      {lease ? (
        <>
          <div className="project-info">
            {leaseDetailTo && (
              <div className="row">
                <span className="key">detail</span>
                <span className="val mono"><Link className="link mono" to={leaseDetailTo}>{leaseDisplayName(lease)}</Link></span>
              </div>
            )}
            {leaseDetailRows(lease).map((row) => (
              <div className="row" key={row.key}>
                <span className="key">{row.key}</span>
                <span className="val mono">{row.value}</span>
              </div>
            ))}
            {requester && (
              <div className="row">
                <span className="key">requester</span>
                <span className="val mono" title={requester.title}>{requester.label}</span>
              </div>
            )}
            <div className="row">
              <span className="key">purpose</span>
              <span className="val mono">{leasePurpose(lease)}</span>
            </div>
            <div className="row">
              <span className="key">playwright</span>
              <span className="val mono">{lease.playwright_ws_endpoint ?? "-"}</span>
            </div>
          </div>
          <div className="lease-detail-actions">
            <LeaseTTLAction lease={lease} signedIn={signedIn} isAdmin={isAdmin} />
            {signedIn && <LeaseCancelAction lease={lease} />}
          </div>

          <h2>Requirements</h2>
          <pre className="json-block">{formatJson(lease.requirements)}</pre>

          <h2>Consumer metadata</h2>
          <pre className="json-block">{formatJson(sanitizeLeaseMetadata(lease.metadata ?? {}))}</pre>
        </>
      ) : (
        <div className="empty">No active lease on this slot.</div>
      )}

      <h2>Waiting requests ({waitingRequests.length})</h2>
      <TestSlotRequestTable requests={waitingRequests} />

      <h2>History ({history.length})</h2>
      <TestSlotHistoryTable history={history} />

      <h2>Raw slot snapshot</h2>
      <pre className="json-block">{formatJson(sanitizeLeaseMetadata(env))}</pre>
    </div>
  );
}

function TestSlotRequestTable({ requests }: { requests: TestSlotRequest[] }) {
  if (requests.length === 0) {
    return <div className="empty">No waiting requests for this slot.</div>;
  }

  return (
    <table>
      <thead>
        <tr>
          <th>Request</th>
          <th>Workflow</th>
          <th>State</th>
          <th>Requester</th>
          <th>Requested</th>
          <th>TTL</th>
        </tr>
      </thead>
      <tbody>
        {requests.map((request) => {
          const requester = testSlotRequestRequester(request);
          return (
            <tr key={request.ref}>
              <td className="mono dim">{request.ref}</td>
              <td className="mono dim">{request.workflow}</td>
              <td><span className={`pill ${testSlotRequestPillClass(request.state)}`}>{request.state}</span></td>
              <td className="mono dim" title={requester.title}>{requester.label}</td>
              <td className="mono dim" title={formatDateTime(request.requested_at)}>{relTime(request.requested_at)}</td>
              <td className="mono dim">{formatTTL(request.ttl_seconds)}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

function TestSlotHistoryTable({ history }: { history: TestSlotReturnHistoryEntry[] }) {
  if (history.length === 0) {
    return <div className="empty">No slot history yet.</div>;
  }

  return (
    <table>
      <thead>
        <tr>
          <th>Event</th>
          <th>Created</th>
          <th>Lease</th>
          <th>Source</th>
          <th>Cleanup</th>
          <th>Reason</th>
        </tr>
      </thead>
      <tbody>
        {history.map((entry, index) => (
          <tr key={`${entry.created_at}:${entry.lease_ref}:${entry.event}:${index}`}>
            <td><span className={`pill ${slotHistoryPillClass(entry)}`}>{entry.event || "event"}</span></td>
            <td className="mono dim" title={formatDateTime(entry.created_at)}>{relTime(entry.created_at)}</td>
            <td className="mono dim">{entry.lease_ref || "-"}</td>
            <td className="mono dim">{entry.source || "-"}</td>
            <td className="mono dim">{entry.cleanup_started ? "started" : "-"}</td>
            <td className="lease-purpose" title={entry.reason ?? "-"}>{entry.reason ?? "-"}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function TestLeaseDefaultTTLSettings({
  snap,
  project,
  signedIn,
  isAdmin,
}: {
  snap: Snapshot;
  project?: Project;
  signedIn: boolean;
  isAdmin: boolean;
}) {
  const globalTTL = snapshotGlobalTestLeaseDefaultTTL(snap);
  const globalHotSwapMinTTL = snapshotGlobalTestLeaseHotSwapMinTTL(snap);
  return (
    <section className="test-lease-defaults" aria-label="test lease defaults">
      <TestLeaseDefaultTTLControl
        label="global default TTL"
        endpoint="/v1/test-slots/default-ttl"
        idBase="test-lease-default"
        valueSeconds={globalTTL}
        effectiveSeconds={globalTTL}
        builtInSeconds={testSlotBuiltInDefaultTTLSeconds}
        builtInResetLabel="use built-in"
        signedIn={signedIn}
        isAdmin={isAdmin}
      />
      <TestLeaseDefaultTTLControl
        label="global hot-swap min TTL"
        endpoint="/v1/test-slots/hot-swap-min-ttl"
        idBase="test-lease-hot-swap-min"
        valueSeconds={globalHotSwapMinTTL}
        effectiveSeconds={globalHotSwapMinTTL}
        builtInSeconds={testSlotBuiltInHotSwapMinTTLSeconds}
        builtInResetLabel="use built-in"
        signedIn={signedIn}
        isAdmin={isAdmin}
      />
      {project && (
        <TestLeaseDefaultTTLControl
          label="project default TTL"
          endpoint="/v1/test-slots/default-ttl"
          idBase="test-lease-default"
          project={project}
          valueSeconds={projectTestLeaseDefaultTTLSeconds(project)}
          effectiveSeconds={projectTestLeaseDefaultTTLSeconds(project) ?? globalTTL}
          inheritedSeconds={globalTTL}
          builtInSeconds={testSlotBuiltInDefaultTTLSeconds}
          projectResetLabel="use global"
          signedIn={signedIn}
          isAdmin={isAdmin}
        />
      )}
      {project && (
        <TestLeaseDefaultTTLControl
          label="project hot-swap min TTL"
          endpoint="/v1/test-slots/hot-swap-min-ttl"
          idBase="test-lease-hot-swap-min"
          project={project}
          valueSeconds={projectTestLeaseHotSwapMinTTLSeconds(project)}
          effectiveSeconds={projectTestLeaseHotSwapMinTTLSeconds(project) ?? globalHotSwapMinTTL}
          inheritedSeconds={globalHotSwapMinTTL}
          builtInSeconds={testSlotBuiltInHotSwapMinTTLSeconds}
          projectResetLabel="use global"
          signedIn={signedIn}
          isAdmin={isAdmin}
        />
      )}
    </section>
  );
}

function TestLeaseDefaultTTLControl({
  label,
  endpoint,
  idBase,
  project,
  valueSeconds,
  effectiveSeconds,
  inheritedSeconds,
  builtInSeconds,
  builtInResetLabel = "use built-in",
  projectResetLabel = "use global",
  signedIn,
  isAdmin,
}: {
  label: string;
  endpoint: string;
  idBase: string;
  project?: Project;
  valueSeconds: number | null;
  effectiveSeconds: number;
  inheritedSeconds?: number;
  builtInSeconds: number;
  builtInResetLabel?: string;
  projectResetLabel?: string;
  signedIn: boolean;
  isAdmin: boolean;
}) {
  const [draftMinutes, setDraftMinutes] = useState(() => String(ttlMinutesFromSeconds(effectiveSeconds)));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [localValueSeconds, setLocalValueSeconds] = useState<number | null | undefined>(undefined);
  const configuredSeconds = localValueSeconds !== undefined ? localValueSeconds : valueSeconds;
  const displaySeconds = configuredSeconds ?? inheritedSeconds ?? effectiveSeconds;
  const canSave = signedIn && isAdmin;
  const isProject = Boolean(project);
  const hasProjectOverride = isProject && configuredSeconds !== null && configuredSeconds !== undefined;
  const isGlobalResettable = !isProject && configuredSeconds !== builtInSeconds;
  const inputID = `${idBase}-${project?.name ?? "global"}`;

  useEffect(() => {
    if (!saving) {
      setDraftMinutes(String(ttlMinutesFromSeconds(displaySeconds)));
    }
  }, [displaySeconds, saving]);

  useEffect(() => {
    setLocalValueSeconds(undefined);
  }, [valueSeconds]);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!canSave) return;
    const minutes = Number.parseInt(draftMinutes, 10);
    if (!Number.isFinite(minutes) || minutes <= 0) {
      setError("ttl must be positive minutes");
      return;
    }
    const nextSeconds = minutes * 60;
    setSaving(true);
    setError(null);
    try {
      const response = await authedFetch(endpoint, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          ...(project ? { project: project.name } : {}),
          ttl_seconds: nextSeconds,
        }),
      });
      if (!response.ok) {
        const text = await response.text().catch(() => "");
        setError(`${response.status} ${text || response.statusText}`);
        return;
      }
      setLocalValueSeconds(nextSeconds);
    } catch (err) {
      setError(String(err));
    } finally {
      setSaving(false);
    }
  };

  const reset = async () => {
    if (!canSave || (!hasProjectOverride && !isGlobalResettable)) return;
    setSaving(true);
    setError(null);
    try {
      const response = await authedFetch(endpoint, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          ...(project ? { project: project.name } : {}),
          reset: true,
        }),
      });
      if (!response.ok) {
        const text = await response.text().catch(() => "");
        setError(`${response.status} ${text || response.statusText}`);
        return;
      }
      setLocalValueSeconds(project ? null : builtInSeconds);
    } catch (err) {
      setError(String(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <form className="test-lease-default" onSubmit={submit}>
      <label htmlFor={inputID}>{label}</label>
      <strong className="mono">{formatTTL(displaySeconds)}</strong>
      {isProject && !hasProjectOverride && (
        <span className="mono dim">inherits {formatTTL(inheritedSeconds ?? effectiveSeconds)}</span>
      )}
      <input
        id={inputID}
        type="number"
        min={1}
        step={1}
        value={draftMinutes}
        onChange={(event) => setDraftMinutes(event.target.value)}
        disabled={!canSave || saving}
      />
      <span className="mono dim">min</span>
      <button type="submit" disabled={!canSave || saving || draftMinutes === String(ttlMinutesFromSeconds(displaySeconds))}>
        {saving ? "saving..." : "apply"}
      </button>
      {(hasProjectOverride || isGlobalResettable) && (
        <button type="button" className="link" onClick={reset} disabled={!canSave || saving}>
          {isProject ? projectResetLabel : builtInResetLabel}
        </button>
      )}
      {!canSave && <span className="dim mono">admin sign-in required</span>}
      {error && <span className="danger-text mono">{error}</span>}
    </form>
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

function LeaseTTLAction({
  lease,
  signedIn,
  isAdmin,
  compact = false,
}: {
  lease: Lease;
  signedIn: boolean;
  isAdmin: boolean;
  compact?: boolean;
}) {
  const [editing, setEditing] = useState(false);
  const [draftMinutes, setDraftMinutes] = useState(() => String(ttlMinutesFromSeconds(lease.ttl_seconds)));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [localTTLSeconds, setLocalTTLSeconds] = useState<number | null>(null);
  const displayTTLSeconds = localTTLSeconds ?? lease.ttl_seconds;
  const canEdit = signedIn && isAdmin && lease.state === "claimed" && leaseKind(lease) === "test";

  useEffect(() => {
    if (!editing && !saving) {
      setDraftMinutes(String(ttlMinutesFromSeconds(displayTTLSeconds)));
    }
  }, [displayTTLSeconds, editing, saving]);

  useEffect(() => {
    setLocalTTLSeconds(null);
  }, [lease.ttl_seconds]);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!canEdit) return;
    const minutes = Number.parseInt(draftMinutes, 10);
    if (!Number.isFinite(minutes) || minutes <= 0) {
      setError("ttl must be positive minutes");
      return;
    }
    const ttlSeconds = minutes * 60;
    setSaving(true);
    setError(null);
    try {
      const response = await authedFetch("/v1/leases/ttl", {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ project: lease.project, lease_ref: lease.ref, ttl_seconds: ttlSeconds }),
      });
      if (!response.ok) {
        const text = await response.text().catch(() => "");
        setError(`${response.status} ${text || response.statusText}`);
        return;
      }
      setLocalTTLSeconds(ttlSeconds);
      setEditing(false);
    } catch (err) {
      setError(String(err));
    } finally {
      setSaving(false);
    }
  };

  if (!canEdit && !error) {
    return <span className="mono dim">{formatTTL(displayTTLSeconds)}</span>;
  }

  if (!editing) {
    return (
      <span className={`lease-ttl-control ${compact ? "compact" : ""}`}>
        <button type="button" className="link mono" onClick={() => setEditing(true)} disabled={saving || !canEdit}>
          ttl {formatTTL(displayTTLSeconds)}
        </button>
        {error && <span className="danger-text mono">{error}</span>}
      </span>
    );
  }

  return (
    <form className={`lease-ttl-editor ${compact ? "compact" : ""}`} onSubmit={submit}>
      {!compact && <label htmlFor={`ttl-${lease.ref}`}>ttl</label>}
      <input
        id={`ttl-${lease.ref}`}
        type="number"
        min={1}
        step={1}
        value={draftMinutes}
        onChange={(event) => setDraftMinutes(event.target.value)}
        disabled={saving}
      />
      <span className="mono dim">min</span>
      <button type="submit" className="link" disabled={saving || !canEdit}>
        {saving ? "applying..." : "apply"}
      </button>
      <span className="sep">/</span>
      <button
        type="button"
        className="link"
        onClick={() => {
          setEditing(false);
          setError(null);
          setDraftMinutes(String(ttlMinutesFromSeconds(displayTTLSeconds)));
        }}
        disabled={saving}
      >
        keep
      </button>
      {error && <span className="danger-text mono">{error}</span>}
    </form>
  );
}

function leasesFor(snap: Snapshot, kind: LeaseKind, projectName?: string): Lease[] {
  return snap.active_leases
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
    case "reserved":
      return "busy";
    case "error":
      return "drain";
    case "":
    case "warming":
    case "activating":
    case "cleaning":
    default:
      return "info";
  }
}

function testEnvironmentStateLabel(state: TestEnvironment["state"]): string {
  // Empty state means the reconciler has not yet seeded this slot. Surface
  // that as "unseeded" rather than synthesizing one of the lifecycle states —
  // those are reserved for real recorded transitions.
  return state === "" ? "unseeded" : state;
}

function testEnvironmentWorkLabel(env: TestEnvironment): string {
  // The Work column shows what is happening on the slot *right now*. Errors
  // surface unconditionally because an erroring slot needs an operator's
  // attention regardless of its current lifecycle state. Otherwise, only
  // states that represent in-flight work get a label; settled slots (active,
  // available, claimed-and-idle) show `-`. Past-tense fields like
  // activation_attempt or cleanup_state="ready" describe history, not
  // current work — per the slot status field contract in
  // docs/test-slot-lifecycle.md they live in the doc but must not be
  // promoted to a "currently doing" label by the UI.
  if (env.cleanup_error) return "cleanup error";
  if (env.activation_error) return "activation error";
  if (env.state === "cleaning") return compactWorkLabel("cleanup", env.cleanup_state, env.cleanup_started_at);
  if (env.state === "activating") return compactWorkLabel("activation", env.activation_state, env.activation_started_at, env.activation_attempt);
  return "-";
}

function testEnvironmentWorkTitle(env: TestEnvironment): string {
  const parts = [
    env.activation_attempt ? `activation attempt ${env.activation_attempt}` : null,
    env.activation_state ? `activation ${env.activation_state}` : null,
    env.activation_job_name ? `job ${env.activation_job_name}` : null,
    env.activation_started_at ? `activation started ${env.activation_started_at}` : null,
    env.activation_completed_at ? `activation completed ${env.activation_completed_at}` : null,
    env.activation_error ? `activation error ${env.activation_error}` : null,
    env.cleanup_state ? `cleanup ${env.cleanup_state}` : null,
    env.cleanup_started_at ? `cleanup started ${env.cleanup_started_at}` : null,
    env.cleanup_completed_at ? `cleanup completed ${env.cleanup_completed_at}` : null,
    env.cleanup_error ? `cleanup error ${env.cleanup_error}` : null,
  ].filter(Boolean);
  return parts.length > 0 ? parts.join(" | ") : env.detail ?? env.state;
}

function testEnvironmentDetailPath(env: TestEnvironment): string {
  return `/projects/${encodeURIComponent(env.project)}/leases/test/slots/${encodeURIComponent(String(env.slot_index))}`;
}

function testEnvironmentIdMatches(env: TestEnvironment, slotId: string): boolean {
  return String(env.slot_index) === slotId
    || env.slot_name === slotId
    || encodeURIComponent(env.slot_name) === slotId;
}

function testEnvironmentDetailRows(env: TestEnvironment): DetailRow[] {
  return [
    { key: "project", value: env.project },
    { key: "slot index", value: String(env.slot_index) },
    { key: "slot name", value: env.slot_name },
    { key: "state", value: testEnvironmentStateLabel(env.state), title: env.detail ?? undefined },
    { key: "usable", value: env.usable ? "yes" : "no" },
    { key: "updated", value: detailTime(env.updated_at ?? null), title: formatDateTime(env.updated_at ?? null) },
    { key: "ready", value: detailTime(env.ready_at ?? null), title: formatDateTime(env.ready_at ?? null) },
    { key: "detail", value: env.detail ?? "-" },
    { key: "playwright ws", value: env.playwright_ws_endpoint ?? "-" },
  ];
}

function testEnvironmentLifecycleRows(env: TestEnvironment): DetailRow[] {
  return [
    { key: "work", value: testEnvironmentWorkLabel(env), title: testEnvironmentWorkTitle(env) },
    { key: "activation attempt", value: env.activation_attempt ? String(env.activation_attempt) : "-" },
    { key: "activation state", value: env.activation_state ?? "-" },
    { key: "activation job", value: env.activation_job_name ?? "-" },
    { key: "activation started", value: detailTime(env.activation_started_at ?? null), title: formatDateTime(env.activation_started_at ?? null) },
    { key: "activation completed", value: detailTime(env.activation_completed_at ?? null), title: formatDateTime(env.activation_completed_at ?? null) },
    { key: "activation error", value: env.activation_error ?? "-" },
    { key: "cleanup state", value: env.cleanup_state ?? "-" },
    { key: "cleanup started", value: detailTime(env.cleanup_started_at ?? null), title: formatDateTime(env.cleanup_started_at ?? null) },
    { key: "cleanup completed", value: detailTime(env.cleanup_completed_at ?? null), title: formatDateTime(env.cleanup_completed_at ?? null) },
    { key: "cleanup error", value: env.cleanup_error ?? "-" },
  ];
}

function compactWorkLabel(kind: string, state?: string | null, startedAt?: string | null, attempt?: number | null): string {
  const suffix = attempt ? ` #${attempt}` : "";
  if (state) return `${kind}${suffix} ${state}`;
  if (startedAt) return `${kind}${suffix} running`;
  return `${kind}${suffix}`;
}

function testSlotRequestRequester(request: TestSlotRequest): { label: string; title: string } {
  if (isRecord(request.requester)) {
    const ref = valueLabel(request.requester.ref);
    const label = ref || valueLabel(request.requester.label) || valueLabel(request.requester.consumer);
    const detail = [request.requester.consumer, request.requester.kind, request.requester.ref]
      .map(valueLabel)
      .filter(Boolean)
      .join(" / ");
    if (label) return { label, title: detail || label };
  }
  const metadata = request.metadata ?? {};
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
  return { label: "-", title: "No requester recorded on this request" };
}

function testSlotRequestPillClass(state: TestSlotRequest["state"]): "free" | "busy" | "drain" | "info" {
  switch (state) {
    case "waiting":
      return "busy";
    case "fulfilled":
      return "free";
    case "cancelled":
      return "drain";
    default:
      return "info";
  }
}

function slotHistoryPillClass(entry: TestSlotReturnHistoryEntry): "free" | "busy" | "drain" | "info" {
  const event = entry.event.toLowerCase();
  if (event.includes("error") || event.includes("fail")) return "drain";
  if (entry.cleanup_started) return "busy";
  if (event === "return" || event.includes("complete")) return "free";
  return "info";
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

function snapshotGlobalTestLeaseDefaultTTL(snap: Snapshot): number {
  const ttl = snap.test_lease_defaults?.global_ttl_seconds;
  return typeof ttl === "number" && Number.isFinite(ttl) && ttl > 0
    ? ttl
    : testSlotBuiltInDefaultTTLSeconds;
}

function snapshotGlobalTestLeaseHotSwapMinTTL(snap: Snapshot): number {
  const ttl = snap.test_lease_defaults?.hot_swap_min_ttl_seconds;
  return typeof ttl === "number" && Number.isFinite(ttl) && ttl > 0
    ? ttl
    : testSlotBuiltInHotSwapMinTTLSeconds;
}

function projectTestLeaseDefaultTTLSeconds(project: Project): number | null {
  const raw = project.metadata?.test_lease_default_ttl_seconds ?? project.metadata?.testLeaseDefaultTTLSeconds;
  if (typeof raw === "number" && Number.isFinite(raw) && raw > 0) return raw;
  if (typeof raw === "string" && raw.trim()) {
    const parsed = Number.parseInt(raw, 10);
    if (Number.isFinite(parsed) && parsed > 0) return parsed;
  }
  return null;
}

function projectTestLeaseHotSwapMinTTLSeconds(project: Project): number | null {
  const raw = project.metadata?.test_lease_hot_swap_min_ttl_seconds ?? project.metadata?.testLeaseHotSwapMinTTLSeconds;
  if (typeof raw === "number" && Number.isFinite(raw) && raw > 0) return raw;
  if (typeof raw === "string" && raw.trim()) {
    const parsed = Number.parseInt(raw, 10);
    if (Number.isFinite(parsed) && parsed > 0) return parsed;
  }
  return null;
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

function detailTime(iso: string | null): string {
  if (!iso) return "-";
  return `${relTime(iso)} (${formatDateTime(iso)})`;
}

function ttlMinutesFromSeconds(seconds: number): number {
  if (!Number.isFinite(seconds) || seconds <= 0) return 1;
  return Math.max(1, Math.ceil(seconds / 60));
}

function formatTTL(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "-";
  if (seconds % 3600 === 0) return `${seconds / 3600}h`;
  if (seconds % 60 === 0) return `${seconds / 60}m`;
  return `${seconds}s`;
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

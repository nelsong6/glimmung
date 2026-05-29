import { generatePath, matchRoutes, type RouteMatch, type RouteObject } from "react-router-dom";

export type Breadcrumb = {
  label: string;
  to?: string;
};

export const ISSUE_DETAIL_CHILD_ROUTES = {
  summary: "summary",
  runs: "runs",
  run: "runs/:runId",
  runCycle: "runs/:runId/cycles/:cycleId",
  runPhase: "runs/:runId/cycles/:cycleId/phases/:phaseId",
  runJob: "runs/:runId/cycles/:cycleId/phases/:phaseId/jobs/:jobId",
  runStep: "runs/:runId/cycles/:cycleId/phases/:phaseId/jobs/:jobId/steps/:stepId",
  workflow: "workflow",
  workflowRun: "workflow/:workflowRunId",
  touchpoint: "touchpoint",
} as const;

const APP_ROUTES = {
  home: "/",
  leases: "/leases/:kind",
  leaseDetail: "/leases/:kind/:leaseId",
  needsAttention: "/needs-attention",
  projects: "/projects",
  newProject: "/projects/new",
  project: "/projects/:project",
  projectLeases: "/projects/:project/leases/:kind",
  projectLeaseDetail: "/projects/:project/leases/:kind/:leaseId",
  projectTestSlot: "/projects/:project/leases/test/slots/:slotId",
  projectWorkflows: "/projects/:project/workflows",
  projectWorkflow: "/projects/:project/workflows/:workflow",
  projectIssues: "/projects/:project/issues",
  newIssue: "/projects/:project/issues/new",
  issue: "/projects/:project/issues/:issueNumber",
  issueRuns: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.runs}`,
  issueRun: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.run}`,
  issueRunCycle: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.runCycle}`,
  issueRunPhase: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.runPhase}`,
  issueRunJob: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.runJob}`,
  issueRunStep: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.runStep}`,
  issueWorkflow: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.workflow}`,
  issueWorkflowRun: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.workflowRun}`,
  issueTouchpoint: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.touchpoint}`,
  projectPlaybooks: "/projects/:project/playbooks",
  projectPlaybook: "/projects/:project/playbooks/:playbookRef",
  projectNeedsAttention: "/projects/:project/needs-attention",
  projectPortfolio: "/projects/:project/portfolio",
  projectRuns: "/projects/:project/runs",
  projectRun: "/projects/:project/runs/:runId",
  touchpoints: "/touchpoints",
  portfolio: "/portfolio",
  playbooks: "/playbooks",
} as const;

type IssueRunSelectionParams = {
  runId: string;
  cycleId: string;
  phaseId?: string | null;
  jobId?: string | null;
  stepId?: string | null;
};

type BreadcrumbMatch = RouteMatch<string, RouteObject>;

type BreadcrumbHandle = {
  crumb?: (match: BreadcrumbMatch) => Breadcrumb | null;
};

type BreadcrumbRouteObject = {
  path?: string;
  index?: boolean;
  handle?: BreadcrumbHandle;
  children?: BreadcrumbRouteObject[];
};

function param(value: string | number | null | undefined): string {
  return encodeURIComponent(String(value ?? ""));
}

function routePath(pattern: string, params: Record<string, string | number | null | undefined> = {}): string {
  const encoded = Object.fromEntries(Object.entries(params).map(([key, value]) => [key, param(value)]));
  return generatePath(pattern, encoded);
}

export function issueRunSelectionPath(baseUrl: string, selection: IssueRunSelectionParams): string {
  const prefix = baseUrl || "";
  const params = {
    runId: selection.runId,
    cycleId: selection.cycleId,
    phaseId: selection.phaseId ?? undefined,
    jobId: selection.jobId ?? undefined,
    stepId: selection.stepId ?? undefined,
  };
  if (selection.phaseId && selection.jobId && selection.stepId) {
    return `${prefix}/${routePath(ISSUE_DETAIL_CHILD_ROUTES.runStep, params)}`;
  }
  if (selection.phaseId && selection.jobId) {
    return `${prefix}/${routePath(ISSUE_DETAIL_CHILD_ROUTES.runJob, params)}`;
  }
  if (selection.phaseId) {
    return `${prefix}/${routePath(ISSUE_DETAIL_CHILD_ROUTES.runPhase, params)}`;
  }
  return `${prefix}/${routePath(ISSUE_DETAIL_CHILD_ROUTES.runCycle, params)}`;
}

const breadcrumbRoutes: BreadcrumbRouteObject[] = [
  {
    path: APP_ROUTES.home,
    handle: { crumb: () => ({ label: "Home", to: APP_ROUTES.home }) },
    children: [
      { path: "dashboard", handle: { crumb: () => ({ label: "Test leases", to: "/leases/test" }) } },
      {
        path: "leases",
        children: [
          {
            path: ":kind",
            handle: { crumb: (match) => ({ label: leaseKindLabel(match.params.kind), to: routePath(APP_ROUTES.leases, match.params) }) },
            children: [
              { path: ":leaseId", handle: { crumb: (match) => ({ label: `Lease ${match.params.leaseId ?? ""}` }) } },
            ],
          },
        ],
      },
      { path: "needs-attention", handle: { crumb: () => ({ label: "Needs attention" }) } },
      {
        path: "projects",
        handle: { crumb: () => ({ label: "Projects", to: APP_ROUTES.projects }) },
        children: [
          { path: "new", handle: { crumb: () => ({ label: "New project" }) } },
          {
            path: ":project",
            handle: { crumb: (match) => ({ label: match.params.project ?? "", to: routePath(APP_ROUTES.project, match.params) }) },
            children: [
              {
                path: "leases",
                children: [
                  {
                    path: ":kind",
                    handle: { crumb: (match) => ({ label: leaseKindLabel(match.params.kind), to: routePath(APP_ROUTES.projectLeases, match.params) }) },
                    children: [
                      { path: "slots/:slotId", handle: { crumb: (match) => ({ label: `Slot ${match.params.slotId ?? ""}` }) } },
                      { path: ":leaseId", handle: { crumb: (match) => ({ label: `Lease ${match.params.leaseId ?? ""}` }) } },
                    ],
                  },
                ],
              },
              {
                path: "workflows",
                handle: { crumb: (match) => ({ label: "Workflows", to: routePath(APP_ROUTES.projectWorkflows, match.params) }) },
                children: [
                  { path: ":workflow", handle: { crumb: (match) => ({ label: match.params.workflow ?? "" }) } },
                ],
              },
              {
                path: "issues",
                handle: { crumb: (match) => ({ label: "Issues", to: routePath(APP_ROUTES.projectIssues, match.params) }) },
                children: [
                  { path: "new", handle: { crumb: () => ({ label: "New issue" }) } },
                  {
                    path: ":issueNumber",
                    handle: { crumb: (match) => ({ label: `#${match.params.issueNumber ?? ""}`, to: routePath(APP_ROUTES.issue, match.params) }) },
                    children: [
                      {
                        path: ISSUE_DETAIL_CHILD_ROUTES.summary,
                        handle: { crumb: () => ({ label: "Summary" }) },
                      },
                      {
                        path: "runs",
                        handle: { crumb: (match) => ({ label: "Runs", to: routePath(APP_ROUTES.issueRuns, match.params) }) },
                        children: [
                          {
                            path: ":runId",
                            handle: { crumb: (match) => ({ label: runCrumbLabel(match.params.runId), to: routePath(APP_ROUTES.issueRun, match.params) }) },
                            children: [
                              {
                                path: "cycles/:cycleId",
                                handle: { crumb: (match) => ({ label: cycleCrumbLabel(match.params.cycleId), to: routePath(APP_ROUTES.issueRunCycle, match.params) }) },
                                children: [
                                  {
                                    path: "phases/:phaseId",
                                    handle: { crumb: (match) => ({ label: `phase ${match.params.phaseId ?? ""}`, to: routePath(APP_ROUTES.issueRunPhase, match.params) }) },
                                    children: [
                                      {
                                        path: "jobs/:jobId",
                                        handle: { crumb: (match) => ({ label: `job ${match.params.jobId ?? ""}`, to: routePath(APP_ROUTES.issueRunJob, match.params) }) },
                                        children: [
                                          {
                                            path: "steps/:stepId",
                                            handle: { crumb: (match) => ({ label: `step ${match.params.stepId ?? ""}` }) },
                                          },
                                        ],
                                      },
                                    ],
                                  },
                                ],
                              },
                            ],
                          },
                        ],
                      },
                      {
                        path: "workflow",
                        handle: { crumb: (match) => ({ label: "Workflow", to: routePath(APP_ROUTES.issueWorkflow, match.params) }) },
                        children: [
                          {
                            path: ":workflowRunId",
                            handle: { crumb: (match) => ({ label: runSlugDisplay(match.params.workflowRunId ?? "") }) },
                          },
                        ],
                      },
                      {
                        path: ISSUE_DETAIL_CHILD_ROUTES.touchpoint,
                        handle: { crumb: () => ({ label: "Touchpoint" }) },
                      },
                    ],
                  },
                ],
              },
              {
                path: "playbooks",
                handle: { crumb: (match) => ({ label: "Playbooks", to: routePath(APP_ROUTES.projectPlaybooks, match.params) }) },
                children: [
                  { path: ":playbookRef", handle: { crumb: (match) => ({ label: match.params.playbookRef ?? "" }) } },
                ],
              },
              { path: "needs-attention", handle: { crumb: () => ({ label: "Needs attention" }) } },
              { path: "portfolio", handle: { crumb: () => ({ label: "Portfolio" }) } },
              {
                path: "runs",
                handle: { crumb: (match) => ({ label: "Runs", to: routePath(APP_ROUTES.projectRuns, match.params) }) },
                children: [
                  { path: ":runId", handle: { crumb: (match) => ({ label: runSlugDisplay(match.params.runId ?? "") }) } },
                ],
              },
            ],
          },
        ],
      },
      { path: "touchpoints", handle: { crumb: () => ({ label: "Touchpoints", to: APP_ROUTES.touchpoints }) } },
      { path: "portfolio", handle: { crumb: () => ({ label: "Portfolio" }) } },
      { path: "playbooks", handle: { crumb: () => ({ label: "Playbooks" }) } },
    ],
  },
];

export function buildBreadcrumbs(pathname: string): Breadcrumb[] {
  const matches = matchRoutes(breadcrumbRoutes as RouteObject[], pathname);
  if (!matches) return fallbackBreadcrumbs(pathname);
  const crumbs = matches
    .map((match) => (match.route.handle as BreadcrumbHandle | undefined)?.crumb?.(match))
    .filter((crumb): crumb is Breadcrumb => Boolean(crumb));
  return crumbs.length > 0 ? crumbs : [{ label: "Home" }];
}

function fallbackBreadcrumbs(pathname: string): Breadcrumb[] {
  const parts = pathname.split("/").filter(Boolean).map(decodeURIComponent);
  if (parts.length === 0) return [{ label: "Home" }];
  return [{ label: "Home", to: "/" }, { label: titleCase(parts[0]) }];
}

function leaseKindLabel(kind: string | undefined): string {
  return kind === "agent" ? "Agent leases" : "Test leases";
}

function runCrumbLabel(runId: string | undefined): string {
  return runId ? `run ${runId}` : "run";
}

function cycleCrumbLabel(cycleId: string | undefined): string {
  return cycleId ? `cycle ${cycleId}` : "cycle";
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

type MockResponseInit = {
  status?: number;
  headers?: HeadersInit;
};

const MOCK_SESSION_KEY = "glimmung.mock.enabled";

const nowIso = (offsetMs = 0) => new Date(Date.now() + offsetMs).toISOString();
const ago = (minutes: number) => nowIso(-minutes * 60 * 1000);

export function isMockMode(): boolean {
  const params = new URLSearchParams(window.location.search);
  if (params.get("mock") === "0") {
    sessionStorage.removeItem(MOCK_SESSION_KEY);
    return false;
  }
  if (params.get("mock") === "1" || window.location.pathname.startsWith("/_mock")) {
    sessionStorage.setItem(MOCK_SESSION_KEY, "1");
    return true;
  }
  return sessionStorage.getItem(MOCK_SESSION_KEY) === "1";
}

export function mockAccount() {
  return {
    homeAccountId: "mock",
    environment: "mock.glimmung",
    tenantId: "mock",
    username: "mock.designer@glimmung.local",
    localAccountId: "mock",
    name: "Mock Designer",
  };
}

export function installMockFetch(): void {
  if (!isMockMode()) return;
  const realFetch = window.fetch.bind(window);
  window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
    const url = requestUrl(input);
    if (url && url.pathname.startsWith("/v1/")) {
      return Promise.resolve(handleMockRequest(url, init));
    }
    return realFetch(input, init);
  };
}

export const mockSnapshot = {
  hosts: [
    {
      name: "nelsonpc",
      capabilities: { os: "windows", apps: ["sts2", "visual-studio"], gpu: false },
      current_lease_id: "lease-active-glimmung-206",
      last_heartbeat: ago(1),
      last_used_at: ago(4),
      drained: false,
      created_at: ago(1440),
    },
    {
      name: "buildbox-01",
      capabilities: { os: "linux", apps: ["node", "python", "kubectl"], gpu: false },
      current_lease_id: null,
      last_heartbeat: ago(2),
      last_used_at: ago(38),
      drained: false,
      created_at: ago(2200),
    },
    {
      name: "design-lab",
      capabilities: { os: "linux", apps: ["node", "playwright", "figma-export"], gpu: true },
      current_lease_id: null,
      last_heartbeat: ago(5),
      last_used_at: ago(71),
      drained: false,
      created_at: ago(3200),
    },
    {
      name: "legacy-runner",
      capabilities: { os: "windows", apps: ["sts2"], gpu: false },
      current_lease_id: null,
      last_heartbeat: ago(90),
      last_used_at: ago(560),
      drained: true,
      created_at: ago(8200),
    },
  ],
  pending_leases: [
    {
      id: "lease-pending-portfolio-217",
      project: "glimmung",
      workflow: "issue-agent",
      host: null,
      state: "pending",
      requirements: { os: "linux", apps: ["node", "playwright"] },
      metadata: { issue: 217, title: "Generate UI portfolio for existing repo" },
      requested_at: ago(7),
      assigned_at: null,
      released_at: null,
      ttl_seconds: 7200,
    },
    {
      id: "lease-pending-ambience-44",
      project: "ambience",
      workflow: "preview-agent",
      host: null,
      state: "pending",
      requirements: { os: "linux", apps: ["node"] },
      metadata: { issue: 44, preview: "mock checkout flow" },
      requested_at: ago(16),
      assigned_at: null,
      released_at: null,
      ttl_seconds: 3600,
    },
  ],
  active_leases: [
    {
      id: "lease-active-glimmung-206",
      project: "glimmung",
      workflow: "issue-agent",
      host: "nelsonpc",
      state: "active",
      requirements: { os: "windows", apps: ["sts2"] },
      metadata: { issue: 206, run: "run-glimmung-206-live" },
      requested_at: ago(24),
      assigned_at: ago(22),
      released_at: null,
      ttl_seconds: 7200,
    },
  ],
  projects: [
    {
      id: "project-glimmung",
      name: "glimmung",
      github_repo: "nelsong6/glimmung",
      metadata: { owner: "platform", portfolio: true },
      created_at: ago(9000),
    },
    {
      id: "project-ambience",
      name: "ambience",
      github_repo: "nelsong6/ambience",
      metadata: { owner: "apps" },
      created_at: ago(6000),
    },
  ],
  workflows: [
    {
      id: "workflow-glimmung-issue-agent",
      project: "glimmung",
      name: "issue-agent",
      phases: [
        {
          name: "design",
          kind: "gha_dispatch",
          workflow_filename: "issue-agent.yaml",
          workflow_ref: "main",
          inputs: {},
          outputs: ["design_brief"],
          requirements: { os: "windows", apps: ["sts2"] },
          verify: false,
          recycle_policy: null,
        },
        {
          name: "implement",
          kind: "gha_dispatch",
          workflow_filename: "issue-agent.yaml",
          workflow_ref: "main",
          inputs: { design_brief: "${{ phases.design.outputs.design_brief }}" },
          outputs: ["branch", "summary"],
          requirements: { os: "windows", apps: ["sts2"] },
          verify: false,
          recycle_policy: null,
        },
        {
          name: "verify",
          kind: "gha_dispatch",
          workflow_filename: "issue-agent.yaml",
          workflow_ref: "main",
          inputs: { branch: "${{ phases.implement.outputs.branch }}" },
          outputs: ["verification"],
          requirements: { os: "linux", apps: ["node", "playwright"] },
          verify: true,
          recycle_policy: { max_attempts: 2, on: ["reject"], lands_at: "implement" },
        },
      ],
      pr: {
        enabled: true,
        recycle_policy: { max_attempts: 2, on: ["changes_requested"], lands_at: "implement" },
      },
      workflow_filename: "issue-agent.yaml",
      workflow_ref: "main",
      trigger_label: "issue-agent",
      default_requirements: { os: "windows", apps: ["sts2"] },
      metadata: { phases: ["design", "implement", "verify"] },
      created_at: ago(8600),
    },
    {
      id: "workflow-glimmung-portfolio-agent",
      project: "glimmung",
      name: "portfolio-agent",
      phases: [
        {
          name: "generate",
          kind: "gha_dispatch",
          workflow_filename: "design-portfolio.yaml",
          workflow_ref: "main",
          inputs: {},
          outputs: ["portfolio_url"],
          requirements: { os: "linux", apps: ["node", "playwright"] },
          verify: true,
          recycle_policy: { max_attempts: 2, on: ["reject"], lands_at: "generate" },
        },
      ],
      pr: { enabled: false, recycle_policy: null },
      workflow_filename: "design-portfolio.yaml",
      workflow_ref: "main",
      trigger_label: "design-portfolio",
      default_requirements: { os: "linux", apps: ["node", "playwright"] },
      metadata: { output: "design files" },
      created_at: ago(1800),
    },
    {
      id: "workflow-ambience-preview-agent",
      project: "ambience",
      name: "preview-agent",
      phases: [
        {
          name: "preview",
          kind: "gha_dispatch",
          workflow_filename: "preview.yaml",
          workflow_ref: "main",
          inputs: {},
          outputs: ["preview_url"],
          requirements: { os: "linux", apps: ["node"] },
          verify: false,
          recycle_policy: null,
        },
      ],
      pr: { enabled: false, recycle_policy: null },
      workflow_filename: "preview.yaml",
      workflow_ref: "main",
      trigger_label: "preview",
      default_requirements: { os: "linux", apps: ["node"] },
      metadata: { output: "mock site" },
      created_at: ago(4200),
    },
  ],
};

const mockIssues = [
  {
    id: "issue-glimmung-206",
    project: "glimmung",
    workflow: "issue-agent",
    repo: "nelsong6/glimmung",
    number: 206,
    title: "Display native run graph and step-level execution",
    state: "open",
    labels: ["design-system", "run-graph", "issue-agent"],
    html_url: "https://github.com/nelsong6/glimmung/issues/206",
    last_run_id: "run-glimmung-206-live",
    last_run_number: 1,
    last_run_state: "in_progress",
    last_run_abort_reason: null,
    issue_lock_held: true,
  },
  {
    id: "issue-glimmung-217",
    project: "glimmung",
    workflow: "portfolio-agent",
    repo: "nelsong6/glimmung",
    number: 217,
    title: "Generate reusable design portfolio from an existing repo",
    state: "open",
    labels: ["portfolio", "needs-design"],
    html_url: "https://github.com/nelsong6/glimmung/issues/217",
    last_run_id: "run-glimmung-217-review",
    last_run_number: 2,
    last_run_state: "review_required",
    last_run_abort_reason: null,
    issue_lock_held: false,
  },
  {
    id: "issue-ambience-44",
    project: "ambience",
    workflow: "checkout-agent",
    repo: "nelsong6/ambience",
    number: 44,
    title: "Mock checkout flow before wiring real payment data",
    state: "open",
    labels: ["preview", "frontend"],
    html_url: "https://github.com/nelsong6/ambience/issues/44",
    last_run_id: null,
    last_run_number: null,
    last_run_state: null,
    last_run_abort_reason: null,
    issue_lock_held: false,
  },
];

const mockPortfolioElements = [
  {
    id: "portfolio-glimmung-sidebar",
    project: "glimmung",
    route: "/_design-portfolio",
    element_id: "sidebar.nav",
    title: "Sidebar navigation",
    screenshot_url: "/mock/portfolio/sidebar.png",
    preview_url: "/_design-portfolio?mock=1",
    status: "needs_review",
    notes: "Spacing changed around the active project state.",
    last_touched_run_id: "run-glimmung-217-review",
    metadata: { package: "@glimmung/ui" },
    created_at: ago(180),
    updated_at: ago(38),
  },
  {
    id: "portfolio-glimmung-toolbar",
    project: "glimmung",
    route: "/_design-portfolio",
    element_id: "toolbar.actions",
    title: "Toolbar actions",
    screenshot_url: null,
    preview_url: "/_design-portfolio?mock=1",
    status: "approved",
    notes: "Matches the review baseline.",
    last_touched_run_id: "run-glimmung-217-review",
    metadata: { package: "@glimmung/ui" },
    created_at: ago(180),
    updated_at: ago(42),
  },
];

export const mockRuns = [
  {
    id: "run-glimmung-206-live",
    project: "glimmung",
    workflow: "issue-agent",
    issue_number: 206,
    title: "Display native run graph and step-level execution",
    state: "in_progress",
    cycles: 2,
    current_phase: "verify",
    cost_usd: 8.91,
    started_at: ago(24),
    updated_at: ago(2),
  },
  {
    id: "run-glimmung-217-review",
    project: "glimmung",
    workflow: "portfolio-agent",
    issue_number: 217,
    title: "Generate reusable design portfolio from an existing repo",
    state: "needs_review",
    cycles: 1,
    current_phase: "generate",
    cost_usd: 3.42,
    started_at: ago(180),
    updated_at: ago(38),
  },
  {
    id: "run-glimmung-184-passed",
    project: "glimmung",
    workflow: "issue-agent",
    issue_number: 184,
    title: "Wire native runner log archive links",
    state: "passed",
    cycles: 1,
    current_phase: "touchpoint",
    cost_usd: 2.18,
    started_at: ago(860),
    updated_at: ago(790),
  },
  {
    id: "run-ambience-44-pending",
    project: "ambience",
    workflow: "preview-agent",
    issue_number: 44,
    title: "Mock checkout flow before wiring real payment data",
    state: "pending",
    cycles: 0,
    current_phase: "preview",
    cost_usd: 0,
    started_at: ago(16),
    updated_at: ago(16),
  },
  {
    id: "run-ambience-39-aborted",
    project: "ambience",
    workflow: "preview-agent",
    issue_number: 39,
    title: "Prototype inventory empty-state screens",
    state: "aborted",
    cycles: 1,
    current_phase: "preview",
    cost_usd: 1.07,
    started_at: ago(1440),
    updated_at: ago(1380),
  },
];

const issueDetails = mockIssues.map((issue) => ({
  ...issue,
  body:
    issue.number === 206
      ? [
          "We need the run display to move from a tabbed status page to a full issue workspace.",
          "",
          "The visible model should be stages across the graph, jobs as sparse boxes in each stage, and step logs in a side/detail panel. The graph should make parentage evident without over-labeling everything as stage/job.",
          "",
          "Open question: how much of this becomes the default issue view versus a drill-in route from the issue.",
        ].join("\n")
      : "Mock issue used to evaluate how Glimmung can discover UI elements after an app is already built.",
  comments: [
    {
      id: `${issue.id}-comment-1`,
      author: "nelsong6",
      body: issue.number === 206
        ? "Let's learn from Azure DevOps and GitHub: step list on the left, terminal output for the selected step."
        : "Mark generated portfolio elements for review, but do not trigger an agent automatically.",
      created_at: ago(180),
      updated_at: ago(180),
    },
  ],
}));

const mockReports = [
  {
    id: "report-glimmung-216",
    project: "glimmung",
    repo: "nelsong6/glimmung",
    pr_number: 216,
    pr_branch: "codex/design-portfolio-preview",
    title: "Add design portfolio and mock preview surfaces",
    state: "ready",
    merged: false,
    html_url: "https://github.com/nelsong6/glimmung/pull/216",
    linked_issue_id: "issue-glimmung-217",
    linked_run_id: "run-glimmung-217-review",
    issue_number: 217,
    run_id: "run-glimmung-217-review",
    run_state: "review_required",
    run_attempts: 2,
    run_cumulative_cost_usd: 3.42,
    pr_lock_held: false,
  },
  {
    id: "report-glimmung-206",
    project: "glimmung",
    repo: "nelsong6/glimmung",
    pr_number: 218,
    pr_branch: "codex/native-run-graph",
    title: "Render native run graph detail view",
    state: "open",
    merged: false,
    html_url: "https://github.com/nelsong6/glimmung/pull/218",
    linked_issue_id: "issue-glimmung-206",
    linked_run_id: "run-glimmung-206-live",
    issue_number: 206,
    run_id: "run-glimmung-206-live",
    run_state: "in_progress",
    run_attempts: 3,
    run_cumulative_cost_usd: 8.91,
    pr_lock_held: true,
  },
];

const issueGraph = {
  issue_id: "issue-glimmung-206",
  nodes: [
    {
      id: "issue:issue-glimmung-206",
      kind: "issue",
      label: "#206 run graph display",
      state: "open",
      timestamp: ago(240),
      metadata: { project: "glimmung", repo: "nelsong6/glimmung", number: 206, issue_id: "issue-glimmung-206" },
    },
    {
      id: "run:run-glimmung-206-live",
      kind: "run",
      label: "run-glimmung-206-live",
      state: "in_progress",
      timestamp: ago(24),
      metadata: {
        workflow: "issue-agent",
        cycles_count: 2,
        cumulative_cost_usd: 8.91,
        entrypoint_phase: "design",
        report_id: "report-glimmung-206",
        report_state: "open",
        report_title: "Render native run graph detail view",
        report_url: "https://github.com/nelsong6/glimmung/pull/218",
        pr_number: 218,
        pr_branch: "codex/native-run-graph",
        workflow_graph: {
          phases: ["design", "implement", "verify"],
          default_entry: { target: "design", active: true, kind: "phase" },
          recycle_arrows: [
            { source: "verify", target: "implement", trigger: "reject", max_attempts: 2, active: true, kind: "phase_recycle" },
          ],
          terminal: { kind: "report", enabled: true },
        },
      },
    },
    attempt("run-glimmung-206-live", 0, "design", "completed", ago(23), ago(18), "success", "pass", [
      step("read-docs", "Read issue and design docs", "completed", "Captured stage/job/step constraints.", 0),
      step("draft-dag", "Draft graph shape", "completed", "Stage columns and sparse job boxes selected.", 0),
    ]),
    attempt("run-glimmung-206-live", 1, "implement", "completed", ago(17), ago(5), "success", "pass", [
      step("inspect-ui", "Inspect existing issue view", "completed", "Found tabbed layout and route contracts.", 0),
      step("patch-components", "Patch run graph components", "completed", "Added side inspector and step list.", 0),
      step("build", "Run frontend build", "completed", "Build completed.", 0),
    ]),
    attempt("run-glimmung-206-live", 2, "verify", "active", ago(4), null, null, null, [
      step("unit", "Run typecheck and tests", "completed", "TypeScript build passed.", 0),
      step("screenshot", "Capture desktop preview", "active", "Waiting on Playwright screenshot comparison.", null),
      step("summarize", "Summarize review notes", "pending", null, null),
    ]),
    {
      id: "pr:report-glimmung-206",
      kind: "pr",
      label: "PR #218",
      state: "open",
      timestamp: ago(3),
      metadata: { project: "glimmung", repo: "nelsong6/glimmung", number: 218 },
    },
  ],
  edges: [
    { source: "issue:issue-glimmung-206", target: "run:run-glimmung-206-live", kind: "spawned" },
    { source: "run:run-glimmung-206-live", target: "attempt:run-glimmung-206-live:0", kind: "attempted" },
    { source: "run:run-glimmung-206-live", target: "attempt:run-glimmung-206-live:1", kind: "attempted" },
    { source: "run:run-glimmung-206-live", target: "attempt:run-glimmung-206-live:2", kind: "attempted" },
    { source: "run:run-glimmung-206-live", target: "pr:report-glimmung-206", kind: "opened" },
  ],
};

const systemGraph = {
  issue_id: "all",
  nodes: [
    ...issueGraph.nodes,
    {
      id: "issue:issue-glimmung-217",
      kind: "issue",
      label: "#217 design portfolio generator",
      state: "open",
      timestamp: ago(160),
      metadata: { project: "glimmung", repo: "nelsong6/glimmung", number: 217, issue_id: "issue-glimmung-217" },
    },
    {
      id: "run:run-glimmung-217-review",
      kind: "run",
      label: "portfolio review",
      state: "review_required",
      timestamp: ago(88),
      metadata: { workflow: "portfolio-agent", cycles_count: 1, cumulative_cost_usd: 3.42, pr_number: 216 },
    },
    {
      id: "pr:report-glimmung-216",
      kind: "pr",
      label: "PR #216",
      state: "ready",
      timestamp: ago(30),
      metadata: { project: "glimmung", repo: "nelsong6/glimmung", number: 216 },
    },
    {
      id: "signal:review-glimmung-217",
      kind: "signal",
      label: "needs review",
      state: "pending",
      timestamp: ago(2),
      metadata: { source: "design_portfolio" },
    },
  ],
  edges: [
    ...issueGraph.edges,
    { source: "issue:issue-glimmung-217", target: "run:run-glimmung-217-review", kind: "spawned" },
    { source: "run:run-glimmung-217-review", target: "pr:report-glimmung-216", kind: "opened" },
    { source: "pr:report-glimmung-216", target: "signal:review-glimmung-217", kind: "feedback" },
  ],
};

const nativeEvents = {
  project: "glimmung",
  run_id: "run-glimmung-206-live",
  attempt_index: 2,
  job_id: null,
  archive_url: "mock://artifacts/run-glimmung-206-live/verify.log",
  events: [
    event(1, "verify-ui", "unit", "step_started", "npm run build", null),
    event(2, "verify-ui", "unit", "log", "vite v5 building production bundle", null),
    event(3, "verify-ui", "unit", "step_completed", "build passed", 0),
    event(4, "verify-ui", "screenshot", "step_started", "opening mock preview", null),
    event(5, "verify-ui", "screenshot", "log", "capturing issue detail at 1440x1000", null),
  ],
};

function handleMockRequest(url: URL, init?: RequestInit): Response {
  const method = (init?.method ?? "GET").toUpperCase();
  const path = url.pathname;

  if (path === "/v1/config") {
    return json({ entra_client_id: "mock", authority: "https://login.microsoftonline.com/mock", tank_operator_base_url: "https://tank.mock.local" });
  }
  if (path === "/v1/issues" && method === "GET") {
    const project = url.searchParams.get("project");
    const workflow = url.searchParams.get("workflow");
    return json(mockIssues.filter((issue) => (
      (!project || issue.project === project)
      && (!workflow || issue.workflow === workflow)
    )));
  }
  if ((path === "/v1/reports" || path === "/v1/touchpoints") && method === "GET") {
    return json(mockReports);
  }
  if (path === "/v1/portfolio/elements" && method === "GET") {
    const project = url.searchParams.get("project");
    const status = url.searchParams.get("status");
    return json(mockPortfolioElements.filter((row) => (
      (!project || row.project === project)
      && (!status || row.status === status)
    )));
  }
  if (path === "/v1/graph" && method === "GET") return json(filterGraph(url.searchParams.get("project")));
  if (path === "/v1/runs/dispatch" && method === "POST") {
    return json({ state: "dispatched", lease_id: "lease-mock-dispatch", run_id: `run-mock-${Date.now()}`, host: null, workflow: "issue-agent", issue_lock_holder_id: "mock-lock", detail: null });
  }
  if (path === "/v1/portfolio/elements/dispatch" && method === "POST") {
    return json({ state: "dispatched", lease_id: "lease-mock-portfolio", run_id: `run-mock-${Date.now()}`, host: null, workflow: "portfolio-agent", issue_lock_holder_id: "mock-lock", detail: null });
  }
  if (path.startsWith("/v1/lease/") && path.endsWith("/cancel") && method === "POST") return json({ ok: true });
  if (path === "/v1/signals" && method === "POST") return json({ id: `signal-mock-${Date.now()}`, state: "pending" });
  if (["/v1/projects", "/v1/workflows", "/v1/hosts", "/v1/issues"].includes(path) && method === "POST") {
    return json({ id: `mock-${Date.now()}`, ok: true }, { status: 201 });
  }

  const reportMatch = path.match(/^\/v1\/(?:reports|touchpoints)\/([^/]+)\/([^/]+)\/(\d+)$/);
  if (reportMatch) {
    const repo = `${decodeURIComponent(reportMatch[1])}/${decodeURIComponent(reportMatch[2])}`;
    const pr = Number(reportMatch[3]);
    const row = mockReports.find((r) => r.repo === repo && r.pr_number === pr);
    if (!row) return json({ error: "not found" }, { status: 404 });
    return json({
      ...row,
      body: "This mock report shows how Glimmung can keep a PR review loop attached to the originating run.",
      base_ref: "main",
      head_sha: "c0ffee1234567890",
      validation_url: "https://design-portfolio-pr-216.glimmung.dev.romaine.life/?mock=1",
      session_launch_intent: row.pr_lock_held ? "warm" : "cold",
      session_launch_url: null,
      run_attempt_history: [
        { attempt_index: 0, phase: "design", workflow_filename: "issue-agent.yaml", workflow_run_id: 9123401, dispatched_at: ago(88), completed_at: ago(75), verification_status: "pass", decision: "continue" },
        { attempt_index: 1, phase: "implement", workflow_filename: "issue-agent.yaml", workflow_run_id: 9123448, dispatched_at: ago(70), completed_at: row.pr_lock_held ? null : ago(42), verification_status: row.pr_lock_held ? null : "pass", decision: row.pr_lock_held ? null : "report" },
      ],
      comments: [],
      reviews: [],
    });
  }

  const ghIssueGraphMatch = path.match(/^\/v1\/issues\/([^/]+)\/([^/]+)\/(\d+)\/graph$/);
  if (ghIssueGraphMatch) return json(issueGraph);

  const ghIssueMatch = path.match(/^\/v1\/issues\/([^/]+)\/([^/]+)\/(\d+)$/);
  if (ghIssueMatch) {
    const repo = `${decodeURIComponent(ghIssueMatch[1])}/${decodeURIComponent(ghIssueMatch[2])}`;
    const number = Number(ghIssueMatch[3]);
    const detail = issueDetails.find((i) => i.repo === repo && i.number === number);
    return detail ? json(detail) : json({ error: "not found" }, { status: 404 });
  }

  const nativeIssueMatch = path.match(/^\/v1\/issues\/by-id\/([^/]+)\/([^/]+)$/);
  if (nativeIssueMatch) {
    const project = decodeURIComponent(nativeIssueMatch[1]);
    const id = decodeURIComponent(nativeIssueMatch[2]);
    const detail = issueDetails.find((i) => i.project === project && i.id === id);
    return detail ? json(detail) : json({ error: "not found" }, { status: 404 });
  }

  if (path.includes("/comments")) return json({ id: `comment-mock-${Date.now()}`, ok: true });

  if (path.match(/^\/v1\/runs\/[^/]+\/[^/]+\/native\/events$/)) return json(nativeEvents);
  if (path.match(/^\/v1\/runs\/[^/]+\/[^/]+\/abort$/) && method === "POST") return json({ ok: true });

  return json({ error: `mock route not implemented: ${method} ${path}` }, { status: 404 });
}

function requestUrl(input: RequestInfo | URL): URL | null {
  if (typeof input === "string") return new URL(input, window.location.origin);
  if (input instanceof URL) return input;
  if (input instanceof Request) return new URL(input.url);
  return null;
}

function json(body: unknown, init: MockResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: init.status ?? 200,
    headers: { "Content-Type": "application/json", ...(init.headers ?? {}) },
  });
}

function filterGraph(project: string | null) {
  if (!project) return systemGraph;
  const issueIds = new Set(
    systemGraph.nodes
      .filter((n) => n.kind === "issue" && (n.metadata as Record<string, unknown>).project === project)
      .map((n) => n.id),
  );
  const keep = new Set<string>(issueIds);
  let changed = true;
  while (changed) {
    changed = false;
    for (const edge of systemGraph.edges) {
      if (keep.has(edge.source) && !keep.has(edge.target)) {
        keep.add(edge.target);
        changed = true;
      }
    }
  }
  return {
    issue_id: project,
    nodes: systemGraph.nodes.filter((n) => keep.has(n.id)),
    edges: systemGraph.edges.filter((e) => keep.has(e.source) && keep.has(e.target)),
  };
}

function attempt(
  runId: string,
  index: number,
  phase: string,
  state: string,
  timestamp: string,
  completedAt: string | null,
  conclusion: string | null,
  verificationStatus: string | null,
  steps: Array<{ slug: string; title: string; state: string; message: string | null; exit_code: number | null }>,
) {
  return {
    id: `attempt:${runId}:${index}`,
    kind: "attempt",
    label: `${phase} attempt ${index}`,
    state,
    timestamp,
    metadata: {
      phase,
      phase_kind: "k8s_job",
      attempt_index: index,
      workflow_filename: "issue-agent.yaml",
      workflow_run_id: index < 2 ? 9123400 + index : null,
      completed_at: completedAt,
      conclusion,
      cost_usd: index === 0 ? 1.12 : index === 1 ? 5.37 : null,
      verification: verificationStatus ? { status: verificationStatus, reasons: ["mock validation signal"] } : null,
      log_archive_url: `mock://artifacts/${runId}/${phase}.log`,
      jobs: [
        {
          job_id: `${phase}-ui`,
          name: `${phase} UI`,
          state,
          steps,
        },
      ],
    },
  };
}

function step(slug: string, title: string, state: string, message: string | null, exit_code: number | null) {
  return { slug, title, state, message, exit_code };
}

function event(seq: number, jobId: string, stepSlug: string, eventName: string, message: string, exitCode: number | null) {
  return {
    id: `event-${seq}`,
    project: "glimmung",
    run_id: "run-glimmung-206-live",
    attempt_index: 2,
    phase: "verify",
    job_id: jobId,
    seq,
    event: eventName,
    step_slug: stepSlug,
    message,
    exit_code: exitCode,
    metadata: {},
    created_at: ago(4 - seq * 0.25),
  };
}

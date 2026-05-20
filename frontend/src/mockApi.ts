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
  active_leases: [
    {
      ref: "glimmung/leases/11",
      lease_number: 11,
      project: "glimmung",
      workflow: "issue-agent",
      host: "native-k8s",
      state: "claimed",
      requirements: { os: "windows", apps: ["sts2"] },
      metadata: { issue: 206, run: "run-glimmung-206-live" },
      requester: null,
      requested_at: ago(24),
      assigned_at: ago(22),
      released_at: null,
      ttl_seconds: 7200,
    },
  ],
  test_lease_defaults: {
    global_ttl_seconds: 3600,
  },
  projects: [
    {
      id: "project-glimmung",
      name: "glimmung",
      github_repo: "nelsong6/glimmung",
      argocd_app: "glimmung",
      metadata: { owner: "platform", portfolio: true },
      created_at: ago(9000),
    },
    {
      id: "project-ambience",
      name: "ambience",
      github_repo: "nelsong6/ambience",
      argocd_app: "ambience",
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
          kind: "k8s_job",
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
          kind: "k8s_job",
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
          kind: "k8s_job",
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
          kind: "k8s_job",
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
          kind: "k8s_job",
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
      default_requirements: { os: "linux", apps: ["node"] },
      metadata: { output: "mock site" },
      created_at: ago(4200),
    },
  ],
  inflight_locks: { issues: false, prs: false },
};

const mockIssues = [
  {
    id: "issue-glimmung-206",
    project: "glimmung",
    workflow: "issue-agent",
    repo: null,
    number: 206,
    title: "Display native run graph and step-level execution",
    state: "open",
    labels: ["design-system", "run-graph", "issue-agent"],
    html_url: null,
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
    repo: null,
    number: 217,
    title: "Generate reusable design portfolio from an existing repo",
    state: "open",
    labels: ["portfolio", "needs-design"],
    html_url: null,
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
    repo: null,
    number: 44,
    title: "Mock checkout flow before wiring real payment data",
    state: "open",
    labels: ["preview", "frontend"],
    html_url: null,
    last_run_id: null,
    last_run_number: null,
    last_run_state: null,
    last_run_abort_reason: null,
    issue_lock_held: false,
  },
];

const mockPortfolioElements = [
  {
    ref: "design-portfolio--sidebar.nav",
    project: "glimmung",
    route: "/_design-portfolio",
    element_id: "sidebar.nav",
    title: "Sidebar navigation",
    screenshot_url: "/mock/portfolio/sidebar.png",
    preview_url: "/_design-portfolio?mock=1",
    status: "needs_review",
    notes: "Spacing changed around the active project state.",
    last_touched_run_ref: "glimmung#217/runs/2",
    metadata: { package: "@glimmung/ui" },
    created_at: ago(180),
    updated_at: ago(38),
  },
  {
    ref: "design-portfolio--toolbar.actions",
    project: "glimmung",
    route: "/_design-portfolio",
    element_id: "toolbar.actions",
    title: "Toolbar actions",
    screenshot_url: null,
    preview_url: "/_design-portfolio?mock=1",
    status: "approved",
    notes: "Matches the review baseline.",
    last_touched_run_ref: "glimmung#217/runs/2",
    metadata: { package: "@glimmung/ui" },
    created_at: ago(180),
    updated_at: ago(42),
  },
];

const mockPlaybooks = [
  {
    schema_version: 1,
    ref: "sdlc-control-plane-20260512110000",
    project: "glimmung",
    title: "sdlc control plane sweep",
    description: "Mock executable plan for the integrated issue/run/touchpoint sweep.",
    entries: [
      {
        id: "projection",
        title: "run projection",
        issue: { title: "Expose RunGraphProjection", body: "", labels: ["backend"], workflow: "issue-agent", metadata: {} },
        depends_on: [],
        manual_gate: false,
        state: "succeeded",
        created_issue_ref: "glimmung#206",
        run_ref: "glimmung#206/runs/1",
        completed_at: ago(28),
        metadata: {},
      },
      {
        id: "playbook-ui",
        title: "operator ui",
        issue: { title: "Add Playbook operator UI", body: "", labels: ["frontend"], workflow: "issue-agent", metadata: {} },
        depends_on: ["projection"],
        manual_gate: true,
        state: "pending",
        created_issue_ref: null,
        run_ref: null,
        completed_at: null,
        metadata: {},
      },
    ],
    concurrency_limit: 1,
    integration_strategy: "isolated_prs",
    state: "claimed",
    metadata: {},
    created_at: ago(90),
    updated_at: ago(5),
  },
];

export const mockRuns = [
  {
    id: "run-glimmung-206-live",
    project: "glimmung",
    workflow: "issue-agent",
    run_number: 1,
    run_display_number: "1.1",
    cycle_number: 1,
    run_cycle_number: 1,
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
    run_number: 1,
    run_display_number: "1.1",
    cycle_number: 2,
    run_cycle_number: 1,
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
    run_number: 1,
    run_display_number: "1.1",
    cycle_number: 3,
    run_cycle_number: 1,
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
    run_number: 1,
    run_display_number: "1.1",
    cycle_number: 1,
    run_cycle_number: 1,
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
    run_number: 1,
    run_display_number: "1.1",
    cycle_number: 2,
    run_cycle_number: 1,
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

const mockTouchpoints = [
  {
    ref: "nelsong6/glimmung#216",
    project: "glimmung",
    repo: "nelsong6/glimmung",
    pr_number: 216,
    pr_branch: "codex/design-portfolio-preview",
    title: "Add design portfolio and mock preview surfaces",
    state: "ready",
    merged: false,
    html_url: "https://github.com/nelsong6/glimmung/pull/216",
    linked_issue_ref: "glimmung#217",
    linked_run_ref: "glimmung#217/runs/1",
    issue_number: 217,
    run_ref: "glimmung#217/runs/1",
    run_state: "review_required",
    run_attempts: 2,
    run_cumulative_cost_usd: 3.42,
    pr_lock_held: false,
  },
  {
    ref: "nelsong6/glimmung#218",
    project: "glimmung",
    repo: "nelsong6/glimmung",
    pr_number: 218,
    pr_branch: "codex/native-run-graph",
    title: "Render native run graph detail view",
    state: "open",
    merged: false,
    html_url: "https://github.com/nelsong6/glimmung/pull/218",
    linked_issue_ref: "glimmung#206",
    linked_run_ref: "glimmung#206/runs/1",
    issue_number: 206,
    run_ref: "glimmung#206/runs/1",
    run_state: "in_progress",
    run_attempts: 3,
    run_cumulative_cost_usd: 8.91,
    pr_lock_held: true,
  },
];

const issueGraph = {
  issue_ref: "glimmung#206",
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
        touchpoint_ref: "touchpoint-glimmung-206",
        touchpoint_state: "open",
        touchpoint_title: "Render native run graph detail view",
        touchpoint_url: "https://github.com/nelsong6/glimmung/pull/218",
        pr_number: 218,
        pr_branch: "codex/native-run-graph",
        workflow_graph: {
          phases: ["design", "implement", "verify"],
          default_entry: { target: "design", active: true, kind: "phase" },
          recycle_arrows: [
            { source: "verify", target: "implement", trigger: "reject", max_attempts: 2, active: true, kind: "phase_recycle" },
          ],
          terminal: { kind: "touchpoint", enabled: true },
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
      id: "pr:touchpoint-glimmung-206",
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
    { source: "run:run-glimmung-206-live", target: "pr:touchpoint-glimmung-206", kind: "opened" },
  ],
  projection: {
    issue_ref: "glimmung#206",
    current_run_ref: "glimmung#206/runs/1",
    default_focus: { kind: "phase", ref: "glimmung#206/runs/1#verify" },
    next_action: { kind: "watch_run", label: "watch run", target_ref: "glimmung#206/runs/1" },
    edges: [
      { source: "run:glimmung#206/runs/1", target: "phase:glimmung#206/runs/1:design", kind: "contains" },
      { source: "phase:glimmung#206/runs/1:design", target: "phase:glimmung#206/runs/1:implement", kind: "depends_on" },
      { source: "phase:glimmung#206/runs/1:implement", target: "phase:glimmung#206/runs/1:verify", kind: "depends_on" },
    ],
    runs: [{
      run_ref: "glimmung#206/runs/1",
      run_number: 1,
      run_display_number: "1.1",
      cycle_number: 1,
      run_cycle_number: 1,
      workflow: "issue-agent",
      state: "in_progress",
      current_phase: "verify",
      validation_url: "https://design-portfolio-pr-216.glimmung.dev.romaine.life/?mock=1",
      cost_usd: 8.91,
      attempts_count: 3,
      started_at: ago(24),
      updated_at: ago(2),
      completed_at: null,
      topology: {
        phases: [
          { name: "design", kind: "k8s_job", verify: false, always: false, depends_on: [], jobs: [{ id: "design", name: "design" }] },
          { name: "implement", kind: "k8s_job", verify: false, always: false, depends_on: ["design"], jobs: [{ id: "implement", name: "implement" }] },
          { name: "verify", kind: "k8s_job", verify: true, always: false, depends_on: ["implement"], jobs: [{ id: "verify-ui", name: "verify ui" }] },
        ],
        default_entry: { target: "design", active: true, kind: "default" },
        recycle_arrows: [
          { source: "verify", target: "implement", trigger: "reject", max_attempts: 2, active: true, kind: "phase_recycle" },
        ],
        terminal: { kind: "touchpoint", enabled: true },
      },
      phases: [
        { name: "design", kind: "k8s_job", state: "succeeded", verify: false, always: false, depends_on: [], jobs: [{ id: "design", name: "design", state: "succeeded", steps: [{ slug: "read-docs", title: "read docs", state: "succeeded" }] }], attempts: [] },
        { name: "implement", kind: "k8s_job", state: "succeeded", verify: false, always: false, depends_on: ["design"], jobs: [{ id: "implement", name: "implement", state: "succeeded", steps: [{ slug: "build", title: "build", state: "succeeded" }] }], attempts: [] },
        { name: "verify", kind: "k8s_job", state: "claimed", verify: true, always: false, depends_on: ["implement"], jobs: [{ id: "verify-ui", name: "verify ui", state: "claimed", steps: [{ slug: "screenshot", title: "screenshot", state: "claimed" }] }], attempts: [] },
      ],
      evidence: [
        { kind: "validation", ref: "https://design-portfolio-pr-216.glimmung.dev.romaine.life/?mock=1", label: "validation", url: "https://design-portfolio-pr-216.glimmung.dev.romaine.life/?mock=1" },
        { kind: "pull_request", ref: "https://github.com/nelsong6/glimmung/pull/218", label: "PR #218", url: "https://github.com/nelsong6/glimmung/pull/218" },
      ],
    }],
    touchpoints: [{
      ref: "nelsong6/glimmung#218",
      repo: "nelsong6/glimmung",
      pr_number: 218,
      title: "Render native run graph detail view",
      state: "open",
      html_url: "https://github.com/nelsong6/glimmung/pull/218",
      linked_run_ref: "glimmung#206/runs/1",
      validation_url: "https://design-portfolio-pr-216.glimmung.dev.romaine.life/?mock=1",
    }],
    signals: [],
  },
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
      id: "pr:touchpoint-glimmung-216",
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
    { source: "run:run-glimmung-217-review", target: "pr:touchpoint-glimmung-216", kind: "opened" },
    { source: "pr:touchpoint-glimmung-216", target: "signal:review-glimmung-217", kind: "feedback" },
  ],
};

const nativeEvents = {
  project: "glimmung",
  run_ref: "glimmung#206/runs/1",
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
  if (path === "/v1/touchpoints" && method === "GET") {
    return json(mockTouchpoints);
  }
  if (path === "/v1/portfolio/elements" && method === "GET") {
    const project = url.searchParams.get("project");
    const status = url.searchParams.get("status");
    return json(mockPortfolioElements.filter((row) => (
      (!project || row.project === project)
      && (!status || row.status === status)
    )));
  }
  if (path === "/v1/playbooks" && method === "GET") {
    const project = url.searchParams.get("project");
    return json(mockPlaybooks.filter((row) => !project || row.project === project));
  }
  if (path === "/v1/graph" && method === "GET") return json(filterGraph(url.searchParams.get("project")));
  if (path === "/v1/runs/dispatch" && method === "POST") {
    return json({ state: "dispatched", lease: "claimed", issue_ref: "glimmung#206", issue_number: 206, run_number: 1, cycle_number: 1, run_cycle_number: 1, run_ref: "glimmung#206/runs/1.1", host: null, workflow: "issue-agent", detail: null });
  }
  if (path === "/v1/portfolio/elements/dispatch" && method === "POST") {
    return json({ state: "dispatched", lease: "claimed", issue_ref: "glimmung#217", issue_number: 217, run_number: 1, cycle_number: 2, run_cycle_number: 1, run_ref: "glimmung#217/runs/1.1", host: null, workflow: "portfolio-agent", detail: null });
  }
  const playbookRunMatch = path.match(/^\/v1\/playbooks\/([^/]+)\/([^/]+)\/run$/);
  if (playbookRunMatch && method === "POST") {
    const project = decodeURIComponent(playbookRunMatch[1]);
    const ref = decodeURIComponent(playbookRunMatch[2]);
    const row = mockPlaybooks.find((p) => p.project === project && p.ref === ref);
    return row ? json(row) : json({ error: "not found" }, { status: 404 });
  }
  const playbookGateMatch = path.match(/^\/v1\/playbooks\/([^/]+)\/([^/]+)\/entries\/([^/]+)\/gate$/);
  if (playbookGateMatch && method === "POST") {
    const project = decodeURIComponent(playbookGateMatch[1]);
    const ref = decodeURIComponent(playbookGateMatch[2]);
    const row = mockPlaybooks.find((p) => p.project === project && p.ref === ref);
    return row ? json(row) : json({ error: "not found" }, { status: 404 });
  }
  if (path === "/v1/leases/cancel" && method === "POST") return json({ state: "cancelled", lease_ref: "glimmung/leases/11" });
  if (path === "/v1/test-slots/default-ttl" && method === "PATCH") {
    const body = parseMockBody(init?.body);
    const ttl = typeof body.ttl_seconds === "number" && body.ttl_seconds > 0 ? body.ttl_seconds : mockSnapshot.test_lease_defaults.global_ttl_seconds;
    return json({ defaults: { global_ttl_seconds: body.reset ? 3600 : ttl } });
  }
  if (path === "/v1/signals" && method === "POST") return json({ ref: `signal:mock:${Date.now()}`, state: "pending" });
  if (["/v1/projects", "/v1/workflows", "/v1/issues"].includes(path) && method === "POST") {
    return json({ id: `mock-${Date.now()}`, ok: true }, { status: 201 });
  }

  const nativeIssueGraphMatch = path.match(/^\/v1\/issues\/by-number\/([^/]+)\/(\d+)\/graph$/);
  if (nativeIssueGraphMatch) return json(issueGraph);

  const nativeIssueNumberMatch = path.match(/^\/v1\/issues\/by-number\/([^/]+)\/(\d+)$/);
  if (nativeIssueNumberMatch) {
    const project = decodeURIComponent(nativeIssueNumberMatch[1]);
    const number = Number(nativeIssueNumberMatch[2]);
    const detail = issueDetails.find((i) => i.project === project && i.number === number);
    return detail ? json(detail) : json({ error: "not found" }, { status: 404 });
  }

  const playbookDetailMatch = path.match(/^\/v1\/playbooks\/([^/]+)\/([^/]+)$/);
  if (playbookDetailMatch) {
    const project = decodeURIComponent(playbookDetailMatch[1]);
    const ref = decodeURIComponent(playbookDetailMatch[2]);
    const row = mockPlaybooks.find((p) => p.project === project && p.ref === ref);
    return row ? json(row) : json({ error: "not found" }, { status: 404 });
  }

  if (path.includes("/comments")) return json({ id: `comment-mock-${Date.now()}`, ok: true });

  if (path.match(/^\/v1\/runs\/[^/]+\/[^/]+\/native\/events$/)) return json(nativeEvents);
  if (path.match(/^\/v1\/projects\/[^/]+\/issues\/\d+\/runs\/[^/]+\/native\/events$/)) return json(nativeEvents);
  if (path.match(/^\/v1\/projects\/[^/]+\/issues\/\d+\/runs\/[^/]+\/abort$/) && method === "POST") return json({ ok: true });

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

function parseMockBody(body: BodyInit | null | undefined): Record<string, any> {
  if (typeof body !== "string") return {};
  try {
    const parsed = JSON.parse(body);
    return parsed && typeof parsed === "object" ? parsed : {};
  } catch {
    return {};
  }
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
      workflow_filename: `k8s_job:${phase}`,
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
    project: "glimmung",
    run_ref: "glimmung#206/runs/1",
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

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter, Outlet, Route, Routes, useLocation } from "react-router-dom";

import { IssueDetailView } from "./IssueDetailView";
import { ISSUE_DETAIL_CHILD_ROUTES } from "./routes";

const issueDetail = {
  ref: "ambience#172",
  project: "ambience",
  repo: "nelsong6/ambience",
  number: 172,
  title: "Effect: Distant storm at sea horizon",
  body: "storm",
  state: "open",
  labels: ["ambient-effects"],
  html_url: null,
  metadata: {},
  comments: [],
  last_run_ref: "ambience#172/runs/7.1",
  last_run_number: 7,
  last_run_state: "in_progress",
  issue_lock_held: true,
};

const runProjection = {
  issue_ref: "ambience#172",
  current_run_ref: "ambience#172/runs/7.1",
  default_focus: { kind: "run", ref: "ambience#172/runs/7.1" },
  next_action: { kind: "watch_run", label: "watch run", target_ref: "ambience#172/runs/7.1" },
  touchpoints: [],
  signals: [],
  edges: [],
  runs: [{
    run_ref: "ambience#172/runs/7.1",
    run_number: 7,
    run_display_number: "7.1",
    cycle_number: 7,
    run_cycle_number: 1,
    workflow: "default",
    state: "in_progress",
    current_phase: "env-prep",
    validation_url: null,
    cost_usd: 0,
    attempts_count: 1,
    started_at: "2026-05-20T17:24:09.336Z",
    updated_at: "2026-05-20T17:24:09.696Z",
    completed_at: null,
    evidence: [],
    topology: {
      phases: [{
        name: "env-prep",
        kind: "k8s_job",
        verify: false,
        run_on: "success",
        purpose: "work",
        depends_on: [],
        jobs: [{ id: "env-prep", name: "Environment prep" }],
	      }, {
	        name: "agent-execute",
	        kind: "k8s_job",
	        verify: false,
	        run_on: "success",
	        purpose: "work",
	        depends_on: ["env-prep"],
	        jobs: [{ id: "agent", name: "Run agent" }],
	      }, {
	        name: "touchpoint",
	        kind: "k8s_job",
	        verify: false,
	        run_on: "success",
	        purpose: "review_touchpoint",
	        depends_on: ["agent-execute"],
	        jobs: [{ id: "pr-touchpoint", name: "PR touchpoint" }],
	      }],
	      default_entry: { target: "env-prep", active: true, kind: "default" },
	      recycle_arrows: [{
	        source: "touchpoint",
        target: "env-prep",
        trigger: "changes_requested",
        max_attempts: 3,
        active: false,
        kind: "touchpoint_recycle",
      }],
    },
    phases: [{
      name: "env-prep",
      kind: "k8s_job",
      state: "dispatching",
      verify: false,
      run_on: "success",
      purpose: "work",
      depends_on: [],
      jobs: [{
        id: "env-prep",
        name: "Environment prep",
        state: "dispatching",
        k8s_job_name: "glim-ambience-172-runs-7-1-0-env-prep",
        steps: [
          { slug: "clone-repo", title: "Clone repository", state: "not_started" },
          { slug: "build-validation-image", title: "Build validation image", state: "not_started" },
        ],
      }],
      attempts: [{
        attempt_index: 0,
        state: "dispatching",
        conclusion: null,
        verification_status: null,
        decision: null,
        log_archive_url: null,
        evidence_refs: [],
        job_completions: [],
      }],
    }, {
      name: "agent-execute",
      kind: "k8s_job",
      state: "not_started",
      verify: false,
      run_on: "success",
      purpose: "work",
      depends_on: ["env-prep"],
	      jobs: [{
	        id: "agent",
	        name: "Run agent",
	        state: "not_started",
        steps: [
          { slug: "checkout", title: "Checkout workspace", state: "not_started" },
          { slug: "run-agent", title: "Run agent", state: "not_started" },
        ],
	      }],
	      attempts: [],
	    }, {
	      name: "touchpoint",
	      kind: "k8s_job",
	      state: "not_started",
	      verify: false,
	      run_on: "success",
	      purpose: "review_touchpoint",
	      depends_on: ["agent-execute"],
	      jobs: [{
	        id: "pr-touchpoint",
	        name: "PR touchpoint",
	        state: "not_started",
	        steps: [
	          { slug: "ensure-pr-touchpoint", title: "Ensure PR touchpoint", state: "not_started" },
	        ],
	      }],
	      attempts: [],
	    }],
  }],
};

const issueGraph = {
  issue_ref: "ambience#172",
  nodes: [
    {
      id: "issue:ambience#172",
      kind: "issue",
      label: "#172 Effect: Distant storm at sea horizon",
      state: "open",
      timestamp: null,
      metadata: { project: "ambience", number: 172 },
    },
    {
      id: "run:ambience#172/runs/7.1",
      kind: "run",
      label: "Run 7.1",
      state: "in_progress",
      timestamp: "2026-05-20T17:24:09.336Z",
      metadata: {
        run_number: 7,
        run_display_number: "7.1",
        cycle_number: 7,
        run_cycle_number: 1,
        workflow: "default",
      },
    },
  ],
  edges: [
    { source: "issue:ambience#172", target: "run:ambience#172/runs/7.1", kind: "spawned" },
  ],
  projection: runProjection,
};

const nativeEvents = {
  project: "ambience",
  run_ref: "ambience#172/runs/7.1",
  attempt_index: 0,
  job_id: "env-prep",
  archive_url: null,
  events: [
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "env-prep",
      job_id: "env-prep",
      seq: 1,
      event: "log",
      step_slug: "clone-repo",
      message: "cloning repo",
      exit_code: null,
      metadata: {},
      created_at: "2026-05-20T17:24:10.000Z",
    },
  ],
};

const agentNativeEvents = {
  project: "ambience",
  run_ref: "ambience#172/runs/7.1",
  attempt_index: 0,
  job_id: "agent",
  archive_url: null,
  events: [
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "agent-execute",
      job_id: "agent",
      seq: 1,
      event: "log",
      step_slug: "run-agent",
      message: JSON.stringify({ type: "system", subtype: "init", cwd: "/workspace" }),
      exit_code: null,
      metadata: { stream: "stdout" },
      created_at: "2026-05-20T17:24:10.000Z",
    },
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "agent-execute",
      job_id: "agent",
      seq: 2,
      event: "log",
      step_slug: "run-agent",
      message: JSON.stringify({
        type: "assistant",
        message: {
          content: [
            { type: "text", text: "I will inspect the file." },
            { type: "tool_use", id: "toolu_1", name: "Read", input: { file_path: "src/App.tsx" } },
          ],
        },
      }),
      exit_code: null,
      metadata: { stream: "stdout" },
      created_at: "2026-05-20T17:24:11.000Z",
    },
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "agent-execute",
      job_id: "agent",
      seq: 3,
      event: "log",
      step_slug: "run-agent",
      message: JSON.stringify({
        type: "user",
        message: {
          content: [{
            type: "tool_result",
            tool_use_id: "toolu_1",
            content: JSON.stringify({ stdout: "line one\nline two" }),
          }],
        },
      }),
      exit_code: null,
      metadata: { stream: "stdout" },
      created_at: "2026-05-20T17:24:12.000Z",
    },
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "agent-execute",
      job_id: "agent",
      seq: 4,
      event: "log",
      step_slug: "run-agent",
      message: JSON.stringify({
        type: "assistant",
        message: {
          content: [
            { type: "thinking", signature: "very-large-signature" },
            { type: "text", text: "Done." },
          ],
        },
      }),
      exit_code: null,
      metadata: { stream: "stdout" },
      created_at: "2026-05-20T17:24:13.000Z",
    },
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "agent-execute",
      job_id: "agent",
      seq: 5,
      event: "log",
      step_slug: "run-agent",
      message: JSON.stringify({ type: "result", subtype: "success", duration_ms: 1250, total_cost_usd: 0.0123 }),
      exit_code: null,
      metadata: { stream: "stdout" },
      created_at: "2026-05-20T17:24:14.000Z",
    },
  ],
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("IssueDetailView run execution graph", () => {
  it("keeps issue labels inline with the issue title", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/touchpoint");

    const heading = await screen.findByRole("heading", { name: issueDetail.title });
    const titleRow = heading.closest(".issue-title-row");
    if (!titleRow) throw new Error("missing issue title row");

    const labels = within(titleRow as HTMLElement).getByLabelText("issue labels");
    expect(within(labels).getByText("ambient-effects")).toBeInTheDocument();
    expect(within(labels).getByText("in flight")).toBeInTheDocument();
    expect(document.querySelector(".project-hero > .dag-policy-rail")).not.toBeInTheDocument();
    expect(document.querySelector(".issue-hero .project-facts")).not.toBeInTheDocument();
  });

  it("shows run history as flat run counts, base cycle values, and run-cycle ordinals", async () => {
    const baseRun = runProjection.runs[0];
    const historyRuns = [
      {
        ...baseRun,
        run_ref: "ambience#172/runs/1.1",
        run_number: 1,
        run_display_number: "1.1",
        cycle_number: 1,
        run_cycle_number: 1,
        state: "recycled",
        started_at: "2026-05-20T17:24:09.336Z",
      },
      {
        ...baseRun,
        run_ref: "ambience#172/runs/2.1",
        run_number: 2,
        run_display_number: "2.1",
        cycle_number: 2,
        run_cycle_number: 1,
        state: "recycled",
        started_at: "2026-05-20T18:24:09.336Z",
      },
      {
        ...baseRun,
        run_ref: "ambience#172/runs/2.2",
        run_number: 2,
        run_display_number: "2.2",
        cycle_number: 3,
        run_cycle_number: 2,
        state: "in_progress",
        started_at: "2026-05-20T19:24:09.336Z",
      },
    ];
    const historyProjection = {
      ...runProjection,
      current_run_ref: "ambience#172/runs/2.2",
      default_focus: { kind: "run", ref: "ambience#172/runs/2.2" },
      next_action: { kind: "watch_run", label: "watch run", target_ref: "ambience#172/runs/2.2" },
      runs: historyRuns,
    };
    const historyGraph = {
      ...issueGraph,
      nodes: [
        issueGraph.nodes[0],
        ...historyRuns.map((run) => ({
          id: `run:${run.run_ref}`,
          kind: "run",
          label: `Run ${run.run_display_number}`,
          state: run.state,
          timestamp: run.started_at,
          metadata: {
            run_number: run.run_number,
            run_display_number: run.run_display_number,
            cycle_number: run.cycle_number,
            run_cycle_number: run.run_cycle_number,
            workflow: run.workflow,
          },
        })),
      ],
      edges: [],
      projection: historyProjection,
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(historyGraph);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs");

    const table = await screen.findByRole("table");
    const rows = within(table).getAllByRole("row");
    const newestCells = within(rows[1]).getAllByRole("cell");
    const middleCells = within(rows[2]).getAllByRole("cell");
    const oldestCells = within(rows[3]).getAllByRole("cell");

    expect(newestCells[0]).toHaveTextContent(/^3$/);
    expect(within(newestCells[1]).getByRole("button")).toHaveTextContent(/^2$/);
    expect(newestCells[1]).not.toHaveTextContent(/cycle/i);
    expect(newestCells[1]).not.toHaveTextContent(/\./);
    expect(newestCells[2]).toHaveTextContent(/^2$/);

    expect(middleCells[0]).toHaveTextContent(/^2$/);
    expect(within(middleCells[1]).getByRole("button")).toHaveTextContent(/^2$/);
    expect(middleCells[2]).toHaveTextContent(/^1$/);

    expect(oldestCells[0]).toHaveTextContent(/^1$/);
    expect(within(oldestCells[1]).getByRole("button")).toHaveTextContent(/^1$/);
  });

  it("routes a dispatching job click to its job path and keeps step clicks specific", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(runProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/native/events") return json(nativeEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    const jobLabel = await screen.findByText("Environment prep");
    const jobButton = jobLabel.closest("button");
    if (!jobButton) throw new Error("missing graph job button");
    await userEvent.click(jobButton);

    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/env-prep/jobs/env-prep",
      );
    });
    expect(await screen.findByText("native job inspector")).toBeInTheDocument();
    expect(await screen.findByText(/cloning repo/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /Build validation image/ }));
    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/env-prep/jobs/env-prep/steps/build-validation-image",
      );
    });
  });

  it("routes a phase header click to its phase breadcrumb path", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(runProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/native/events") return json(nativeEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    const phaseTitle = await screen.findByText("env-prep", { selector: ".dag-phase-title" });
    const phaseButton = phaseTitle.closest("button");
    if (!phaseButton) throw new Error("missing phase header button");
    await userEvent.click(phaseButton);

    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/env-prep",
      );
    });
    expect(await screen.findByText("native job inspector")).toBeInTheDocument();
  });

  it("surfaces completed job cost in the selected job log section", async () => {
    const selectedProjection = {
      ...runProjection,
      runs: [{
        ...runProjection.runs[0],
        phases: runProjection.runs[0].phases.map((phase) => phase.name === "env-prep"
          ? {
              ...phase,
              jobs: phase.jobs.map((job) => job.id === "env-prep"
                ? { ...job, state: "succeeded", cost_usd: 2.3456 }
                : job),
            }
          : phase),
      }],
    };
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(selectedProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/native/events") return json(nativeEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    const jobLabel = await screen.findByText("Environment prep");
    expect(screen.queryByText("$2.3456", { selector: ".dag-node-cost" })).not.toBeInTheDocument();
    const jobButton = jobLabel.closest("button");
    if (!jobButton) throw new Error("missing graph job button");
    await userEvent.click(jobButton);

    expect(await screen.findByText("job cost")).toBeInTheDocument();
    expect(screen.getAllByText("$2.3456").length).toBeGreaterThanOrEqual(2);
  });

  it("renders LLM native step JSON as a transcript while keeping raw logs available", async () => {
    const agentProjection = {
      ...runProjection,
      runs: [{
        ...runProjection.runs[0],
        current_phase: "agent-execute",
        phases: runProjection.runs[0].phases.map((phase) => {
          if (phase.name === "env-prep") {
            return {
              ...phase,
              state: "succeeded",
              jobs: phase.jobs.map((job) => ({
                ...job,
                state: "succeeded",
                steps: job.steps.map((step) => ({ ...step, state: "succeeded" })),
              })),
            };
          }
          if (phase.name === "agent-execute") {
            return {
              ...phase,
              state: "active",
              jobs: phase.jobs.map((job) => ({
                ...job,
                state: "active",
                steps: job.steps.map((step) => ({
                  ...step,
                  state: step.slug === "run-agent" ? "active" : "succeeded",
                })),
              })),
              attempts: [{
                attempt_index: 0,
                state: "active",
                conclusion: null,
                verification_status: null,
                decision: null,
                log_archive_url: null,
                evidence_refs: [],
                job_completions: [],
              }],
            };
          }
          return phase;
        }),
      }],
    };
    const agentGraph = { ...issueGraph, projection: agentProjection };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(agentGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(agentProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/native/events") return json(agentNativeEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent/steps/run-agent");

    expect(await screen.findByLabelText("agent transcript")).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "native log view" })).toBeInTheDocument();
    expect(screen.getByText("I will inspect the file.")).toBeInTheDocument();
    expect(screen.getAllByText("Read").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText(/Thinking\/signature content hidden/)).toBeInTheDocument();

    const toolResultSummary = screen.getByText("tool result").closest("summary");
    if (!toolResultSummary) throw new Error("missing tool result summary");
    await userEvent.click(toolResultSummary);
    expect(screen.getByText(/line two/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "raw" }));
    expect(screen.getByText((content) => content.includes("\"tool_use\""))).toBeInTheDocument();
    expect(screen.getByText((content) => content.includes("\\nline two"))).toBeInTheDocument();
  });

  it("keeps raw stdout fragments out of the LLM transcript", async () => {
    const agentProjection = activeAgentProjection();
    const agentGraph = { ...issueGraph, projection: agentProjection };
    const noisyAgentEvents = {
      ...agentNativeEvents,
      events: [
        {
          ...agentNativeEvents.events[0],
          seq: 0,
          message: "{",
          metadata: { stream: "stdout" },
        },
        {
          ...agentNativeEvents.events[0],
          seq: 1,
          message: "  \"namespace\": \"ambience-slot-2\",",
          metadata: { stream: "stdout" },
        },
        ...agentNativeEvents.events.map((event) => ({ ...event, seq: event.seq + 10 })),
      ],
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(agentGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(agentProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/native/events") return json(noisyAgentEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent/steps/run-agent");

    const transcript = await screen.findByLabelText("agent transcript");
    const firstEntry = transcript.querySelector(".agent-transcript-entry");
    expect(firstEntry).toHaveTextContent("assistant");
    expect(within(transcript).queryByText(/stdout log/i)).not.toBeInTheDocument();
    expect(within(transcript).queryByText(/system init/i)).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "raw" }));
    expect(screen.getByText((content, element) => (
      element?.tagName === "PRE" && content.includes("{")
    ))).toBeInTheDocument();
  });

  it("keeps non-agent steps in an LLM job on the raw terminal view", async () => {
    const agentProjection = activeAgentProjection();
    const agentGraph = { ...issueGraph, projection: agentProjection };
    const checkoutNativeEvents = {
      ...agentNativeEvents,
      events: [{
        project: "ambience",
        run_ref: "ambience#172/runs/7.1",
        attempt_index: 0,
        phase: "agent-execute",
        job_id: "agent",
        seq: 1,
        event: "log",
        step_slug: "checkout",
        message: "{",
        exit_code: null,
        metadata: { stream: "stdout" },
        created_at: "2026-05-20T17:24:10.000Z",
      }],
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(agentGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(agentProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/native/events") return json(checkoutNativeEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent/steps/checkout");

    expect(await screen.findByText((content, element) => (
      element?.tagName === "PRE" && content.includes("$ step checkout")
    ))).toBeInTheDocument();
    expect(screen.queryByLabelText("agent transcript")).not.toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "native log view" })).not.toBeInTheDocument();
    expect(screen.getByText((content, element) => (
      element?.tagName === "PRE" && content.includes("{")
    ))).toBeInTheDocument();
  });

  it("pages native events in fixed batches without accumulating prior rows", async () => {
    const agentProjection = activeAgentProjection();
    const agentGraph = { ...issueGraph, projection: agentProjection };
    const firstPageEvents = {
      ...agentNativeEvents,
      events: Array.from({ length: 200 }, (_, index) => {
        const seq = index + 1;
        return agentPageEvent(seq, [{
          type: "tool_use",
          id: `toolu_${seq}`,
          name: "Read",
          input: { seq },
        }]);
      }),
    };
    const secondPageEvents = {
      ...agentNativeEvents,
      events: [
        agentPageEvent(201, [{ type: "text", text: "Readable line on the second batch." }]),
      ],
    };
    const eventSearches: string[] = [];

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(agentGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(agentProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/native/events") {
        eventSearches.push(url.search);
        return json(url.searchParams.get("after_seq") === "200" ? secondPageEvents : firstPageEvents);
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent/steps/run-agent");

    expect(await screen.findByLabelText("agent transcript")).toBeInTheDocument();
    expect(screen.getByText(/200 events/)).toHaveTextContent(/batch 1/);
    expect(screen.queryByText("Readable line on the second batch.")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "next batch" }));

    expect(await screen.findByText("Readable line on the second batch.")).toBeInTheDocument();
    expect(screen.getByText(/1 event/)).toHaveTextContent(/batch 2/);
    expect(eventSearches.some((search) => search.includes("after_seq=200"))).toBe(true);

    await userEvent.click(screen.getByRole("button", { name: "previous batch" }));

    await waitFor(() => {
      expect(screen.queryByText("Readable line on the second batch.")).not.toBeInTheDocument();
    });
    expect(screen.getByText(/200 events/)).toHaveTextContent(/batch 1/);
  });

  it("omits the issue run rollup panel between the header and tabs", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(runProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    expect(await screen.findByLabelText("issue sections")).toBeInTheDocument();
    expect(screen.queryByLabelText("issue run rollup")).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "cycle 7.1 execution" })).toBeInTheDocument();
  });

  it("opens planned steps for a job before any attempt has started", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(runProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    const jobLabel = await screen.findByText("Run agent", { selector: ".dag-job-title" });
    const jobButton = jobLabel.closest("button");
    if (!jobButton) throw new Error("missing graph job button");
    await userEvent.click(jobButton);

    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent",
      );
    });
    expect(await screen.findByText("native job inspector")).toBeInTheDocument();
    expect(screen.getByText("planned")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Checkout workspace/ })).toBeInTheDocument();
    expect(screen.getByText(/No hot native events recorded/)).toBeInTheDocument();

    await userEvent.click(within(screen.getByLabelText("native job steps")).getByRole("button", { name: /Run agent/ }));
    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent/steps/run-agent",
      );
    });
  });
});

function renderIssueDetail(initialPath: string) {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <Routes>
        <Route element={<TestLayout />}>
          <Route path="/projects/:project/issues/:issueNumber" element={<IssueDetailView />}>
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.summary} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.runs} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.run} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.runCycle} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.runPhase} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.runJob} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.runStep} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.workflow} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.workflowRun} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.touchpoint} element={null} />
          </Route>
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

function TestLayout() {
  const location = useLocation();
  return (
    <>
      <div data-testid="path">{location.pathname}</div>
      <Outlet context={{ signedIn: true, isAdmin: true, snap: { projects: [], workflows: [] } }} />
    </>
  );
}

function activeAgentProjection() {
  return {
    ...runProjection,
    runs: [{
      ...runProjection.runs[0],
      current_phase: "agent-execute",
      phases: runProjection.runs[0].phases.map((phase) => {
        if (phase.name === "env-prep") {
          return {
            ...phase,
            state: "succeeded",
            jobs: phase.jobs.map((job) => ({
              ...job,
              state: "succeeded",
              steps: job.steps.map((step) => ({ ...step, state: "succeeded" })),
            })),
          };
        }
        if (phase.name === "agent-execute") {
          return {
            ...phase,
            state: "active",
            jobs: phase.jobs.map((job) => ({
              ...job,
              state: "active",
              steps: job.steps.map((step) => ({
                ...step,
                state: step.slug === "run-agent" ? "active" : "succeeded",
              })),
            })),
            attempts: [{
              attempt_index: 0,
              state: "active",
              conclusion: null,
              verification_status: null,
              decision: null,
              log_archive_url: null,
              evidence_refs: [],
              job_completions: [],
            }],
          };
        }
        return phase;
      }),
    }],
  };
}

function agentPageEvent(seq: number, content: unknown[]) {
  return {
    project: "ambience",
    run_ref: "ambience#172/runs/7.1",
    attempt_index: 0,
    phase: "agent-execute",
    job_id: "agent",
    seq,
    event: "log",
    step_slug: "run-agent",
    message: JSON.stringify({
      type: "assistant",
      message: { content },
    }),
    exit_code: null,
    metadata: { stream: "stdout" },
    created_at: "2026-05-20T17:24:10.000Z",
  };
}

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter } from "react-router-dom";

import { IssuesView } from "./IssuesView";

const openIssue = {
  ref: "ambience#44",
  project: "ambience",
  workflow: null,
  repo: null,
  number: 44,
  title: "Open issue",
  state: "open",
  labels: [],
  html_url: null,
  last_run_ref: null,
  last_run_number: null,
  last_run_state: null,
  last_run_abort_reason: null,
  issue_lock_held: false,
};

const closedIssue = {
  ...openIssue,
  ref: "ambience#172",
  number: 172,
  title: "Closed issue",
  state: "closed",
  last_run_ref: "ambience#172/runs/14.3",
  last_run_number: 14,
  last_run_state: "aborted",
  last_run_abort_reason: "aborted_via_admin_api",
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("IssuesView", () => {
  it("can switch the project issue list to closed issues", async () => {
    const requests: string[] = [];
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(
        typeof input === "string" ? input : input instanceof URL ? input.href : input.url,
        "http://localhost",
      );
      requests.push(`${url.pathname}${url.search}`);
      const state = url.searchParams.get("state") ?? "open";
      const rows = state === "closed" ? [closedIssue] : [openIssue];
      return new Response(JSON.stringify(rows), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }));

    render(
      <MemoryRouter>
        <IssuesView
          signedIn
          projectFilter="ambience"
          showProjectColumn={false}
          allowStateFilter
        />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Open issue")).toBeInTheDocument();
    expect(requests).toContain("/v1/issues?project=ambience");

    await userEvent.click(screen.getByRole("button", { name: "closed" }));

    expect(await screen.findByText("Closed issue")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "closed" })).toHaveAttribute("aria-pressed", "true");
    expect(requests).toContain("/v1/issues?project=ambience&state=closed");
    expect(screen.queryByRole("button", { name: "dispatch" })).not.toBeInTheDocument();
  });
});

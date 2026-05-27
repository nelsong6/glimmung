import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { MemoryRouter } from "react-router-dom";

import {
  App,
  CONNECTION_DEAD_AFTER_MS,
  CONNECTION_STALE_AFTER_MS,
  connectionStateFromSnapshotClock,
} from "./App";
import { installMockFetch, isMockMode } from "./mockApi";

afterEach(() => {
  sessionStorage.clear();
  window.history.pushState({}, "", "/");
});

describe("connection status", () => {
  it("keeps transient SSE reconnects out of the dead state", () => {
    const startedAt = 10_000;
    const lastSeen = startedAt + 2_000;

    expect(connectionStateFromSnapshotClock(startedAt + 1_000, startedAt, 0)).toBe("stale");
    expect(connectionStateFromSnapshotClock(lastSeen + CONNECTION_STALE_AFTER_MS - 1, startedAt, lastSeen)).toBe("live");
    expect(connectionStateFromSnapshotClock(lastSeen + CONNECTION_STALE_AFTER_MS, startedAt, lastSeen)).toBe("stale");
    expect(connectionStateFromSnapshotClock(lastSeen + CONNECTION_DEAD_AFTER_MS, startedAt, lastSeen)).toBe("dead");
  });
});

describe("mock mode", () => {
  it("does not persist mock mode onto ordinary paths", () => {
    window.history.pushState({}, "", "/?mock=1");
    expect(isMockMode()).toBe(true);

    sessionStorage.setItem("glimmung.mock.enabled", "1");
    window.history.pushState({}, "", "/");

    expect(isMockMode()).toBe(false);
  });
});

describe("test environment slots", () => {
  it("links a slot row to its inspectable detail page", async () => {
    window.history.pushState({}, "", "/projects/glimmung/leases/test?mock=1");
    installMockFetch();

    render(
      <MemoryRouter initialEntries={["/projects/glimmung/leases/test?mock=1"]}>
        <App />
      </MemoryRouter>,
    );

    const slotLink = await screen.findByRole("link", { name: "glimmung-test-1" });
    expect(slotLink).toHaveAttribute("href", "/projects/glimmung/leases/test/slots/1");

    await userEvent.click(slotLink);

    expect(await screen.findByRole("heading", { name: "glimmung-test-1" })).toBeInTheDocument();
    expect(screen.getByText("Raw slot snapshot")).toBeInTheDocument();
    expect(screen.getAllByText("glimmung/glimmung-test-1/leases/42").length).toBeGreaterThan(0);
  });
});

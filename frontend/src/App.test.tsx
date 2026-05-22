import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { MemoryRouter } from "react-router-dom";

import { App } from "./App";
import { installMockFetch } from "./mockApi";

afterEach(() => {
  sessionStorage.clear();
  window.history.pushState({}, "", "/");
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

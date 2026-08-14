import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import Header from "./Header";

/**
 * The collapsible nav (issue #12 item 2). CSS decides *when* the Menu
 * button is visible (app.css hides it above the 48rem breakpoint; vitest
 * runs with css disabled, so visibility itself isn't assertable here) —
 * these tests pin the state/aria contract the CSS keys off: aria-expanded,
 * aria-controls, the nav's `is-open` class, and close-on-navigate.
 */

function renderHeader(initialEntries: string[] = ["/runs"]) {
  return render(
    <MemoryRouter initialEntries={initialEntries}>
      <Header />
    </MemoryRouter>,
  );
}

describe("Header collapsible nav", () => {
  it("starts collapsed: Menu reports aria-expanded=false and the nav has no is-open class", () => {
    renderHeader();
    const menu = screen.getByRole("button", { name: "Menu" });
    expect(menu).toHaveAttribute("aria-expanded", "false");
    expect(menu).toHaveAttribute("aria-controls", "app-header-nav");
    expect(screen.getByRole("navigation", { name: "Primary" })).not.toHaveClass(
      "is-open",
    );
  });

  it("toggles open and closed from the Menu button", async () => {
    const user = userEvent.setup();
    renderHeader();
    const menu = screen.getByRole("button", { name: "Menu" });
    const nav = screen.getByRole("navigation", { name: "Primary" });

    await user.click(menu);
    expect(menu).toHaveAttribute("aria-expanded", "true");
    expect(nav).toHaveClass("is-open");

    await user.click(menu);
    expect(menu).toHaveAttribute("aria-expanded", "false");
    expect(nav).not.toHaveClass("is-open");
  });

  it("closes when a nav link is chosen", async () => {
    const user = userEvent.setup();
    renderHeader();
    await user.click(screen.getByRole("button", { name: "Menu" }));
    await user.click(screen.getByRole("link", { name: "Board" }));
    expect(
      screen.getByRole("navigation", { name: "Primary" }),
    ).not.toHaveClass("is-open");
  });
});

describe("Header mesh link (task t18)", () => {
  it("routes to /mesh and marks it active there", () => {
    renderHeader(["/mesh"]);
    const mesh = screen.getByRole("link", { name: "Mesh" });
    expect(mesh).toHaveAttribute("href", "/mesh");
    expect(mesh).toHaveClass("is-active");
  });
});

describe("Header node graphs link (task t28)", () => {
  it("routes to /graphs and marks it active there", () => {
    renderHeader(["/graphs"]);
    const graphs = screen.getByRole("link", { name: "Node Graphs" });
    expect(graphs).toHaveAttribute("href", "/graphs");
    expect(graphs).toHaveClass("is-active");
  });

  it("stays marked active on a Node Graphs sub-tab URL", () => {
    renderHeader(["/graphs?tab=active"]);
    expect(screen.getByRole("link", { name: "Node Graphs" })).toHaveClass(
      "is-active",
    );
  });
});

describe("Header plan link (task t23)", () => {
  it("routes to /plan and marks it active there", () => {
    renderHeader(["/plan"]);
    const plan = screen.getByRole("link", { name: "Plan" });
    expect(plan).toHaveAttribute("href", "/plan");
    expect(plan).toHaveClass("is-active");
  });

  it("stays marked active on a specific plan slug URL", () => {
    renderHeader(["/plan/economy-discord-graphs"]);
    expect(screen.getByRole("link", { name: "Plan" })).toHaveClass(
      "is-active",
    );
  });
});

describe("Header active view marking", () => {
  it("marks the current view's link with is-active and no other", () => {
    renderHeader(["/board"]);
    expect(screen.getByRole("link", { name: "Board" })).toHaveClass(
      "is-active",
    );
    expect(screen.getByRole("link", { name: "Runs" })).not.toHaveClass(
      "is-active",
    );
    expect(screen.getByRole("link", { name: "Jobs" })).not.toHaveClass(
      "is-active",
    );
  });
});

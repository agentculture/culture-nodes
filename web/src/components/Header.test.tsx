import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import Header from "./Header";
import { getVersion } from "../api/client";
import type { Version } from "../api/types";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, getVersion: vi.fn() };
});

const mockGetVersion = vi.mocked(getVersion);

const STAMPED: Version = {
  version: "0.42.0",
  revision: "a".repeat(39) + "b",
  revision_source: "build_flag",
  staleness:
    "this binary was stamped at build time with the revision the deploy shipped",
};

beforeEach(() => {
  // Every existing test renders the header, and the header now reads
  // GET /v1alpha1/version on mount. Default it to a never-settling promise so
  // those tests exercise the nav and nothing else; the version tests below
  // resolve it explicitly.
  mockGetVersion.mockReturnValue(new Promise<Version>(() => {}));
});

afterEach(() => {
  vi.clearAllMocks();
});

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


describe("Header docs link and version readout (task t27)", () => {
  it("links to the README on GitHub, in a new tab", () => {
    renderHeader();
    const docs = screen.getByRole("link", { name: "Docs" });
    expect(docs).toHaveAttribute(
      "href",
      "https://github.com/agentculture/culture-nodes#readme",
    );
    expect(docs).toHaveAttribute("target", "_blank");
    expect(docs).toHaveAttribute("rel", expect.stringContaining("noreferrer"));
  });

  it("reads GET /v1alpha1/version and shows the short revision", async () => {
    mockGetVersion.mockResolvedValue(STAMPED);
    renderHeader();
    await waitFor(() =>
      expect(screen.getByText(/0\.42\.0 · aaaaaaa/)).toBeInTheDocument(),
    );
    expect(document.getElementById("app-header-version")).toHaveAttribute(
      "data-revision",
      STAMPED.revision!,
    );
  });

  it("carries the API's own staleness sentence as the tooltip, verbatim", async () => {
    mockGetVersion.mockResolvedValue(STAMPED);
    renderHeader();
    await waitFor(() =>
      expect(document.getElementById("app-header-version")).toHaveAttribute(
        "title",
        STAMPED.staleness,
      ),
    );
  });

  it("marks a dirty build dirty rather than reporting a clean revision", async () => {
    mockGetVersion.mockResolvedValue({
      ...STAMPED,
      revision_source: "go_vcs_stamp",
      revision_is_dirty: true,
      staleness: "that checkout had UNCOMMITTED changes",
    });
    renderHeader();
    await waitFor(() =>
      expect(screen.getByText(/aaaaaaa\+dirty/)).toBeInTheDocument(),
    );
  });

  it("says the revision is unknown for an unstamped build rather than showing a blank", async () => {
    mockGetVersion.mockResolvedValue({
      version: "0.42.0",
      staleness: "this binary's revision CANNOT BE ESTABLISHED",
    });
    renderHeader();
    await waitFor(() =>
      expect(screen.getByText("0.42.0 · revision unknown")).toBeInTheDocument(),
    );
  });

  it("says so when the version call fails, rather than rendering nothing", async () => {
    mockGetVersion.mockRejectedValue(new Error("unreachable"));
    renderHeader();
    await waitFor(() =>
      expect(screen.getByText("version unavailable")).toBeInTheDocument(),
    );
  });
});

function LocationProbe() {
  const location = useLocation();
  return <span data-testid="location">{location.pathname}</span>;
}

function renderHeaderWithLocation() {
  return render(
    <MemoryRouter initialEntries={["/runs"]}>
      <Header />
      <Routes>
        <Route path="*" element={<LocationProbe />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("Header tickets entry (task t27)", () => {
  it("opens /tickets/<key> from the typed key", async () => {
    const user = userEvent.setup();
    renderHeaderWithLocation();

    await user.type(screen.getByLabelText("Tickets"), "SCRUM-6");
    await user.click(screen.getByRole("button", { name: "Open" }));

    expect(screen.getByTestId("location")).toHaveTextContent("/tickets/SCRUM-6");
  });

  it("stays put on an empty or whitespace-only key", async () => {
    const user = userEvent.setup();
    renderHeaderWithLocation();

    const open = screen.getByRole("button", { name: "Open" });
    expect(open).toBeDisabled();

    await user.type(screen.getByLabelText("Tickets"), "   ");
    expect(open).toBeDisabled();
    expect(screen.getByTestId("location")).toHaveTextContent("/runs");
  });

  it("percent-encodes a key that would otherwise change the path", async () => {
    const user = userEvent.setup();
    renderHeaderWithLocation();

    await user.type(screen.getByLabelText("Tickets"), "A/B");
    await user.click(screen.getByRole("button", { name: "Open" }));

    expect(screen.getByTestId("location")).toHaveTextContent("/tickets/A%2FB");
  });
});

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import Header from "./Header";
import { ApiError, getVersion, getWhoami } from "../api/client";
import type { Version, Whoami } from "../api/types";
import {
  WHOAMI_BOUND,
  WHOAMI_EMAIL,
  WHOAMI_SERVICE_TOKEN,
  WHOAMI_UNBOUND,
  WHOAMI_UNBOUND_EMAIL,
} from "../fixtures/whoami-fixture";
import { resetWhoamiForTests } from "../hooks/useWhoami";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, getVersion: vi.fn(), getWhoami: vi.fn() };
});

const mockGetVersion = vi.mocked(getVersion);
const mockGetWhoami = vi.mocked(getWhoami);

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
  // Likewise the identity readout (task t9): default to never settling; the
  // identity tests below resolve it explicitly.
  resetWhoamiForTests();
  mockGetWhoami.mockReturnValue(new Promise<Whoami>(() => {}));
});

afterEach(() => {
  vi.clearAllMocks();
  resetWhoamiForTests();
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
    await user.click(screen.getByRole("link", { name: "Runs" }));
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

/**
 * The PRD §8.6 spine (task t9). The nav is eight destinations in two groups —
 * the work a person came for, then the engine — and the count is asserted
 * rather than counted by eye, so adding a ninth tab is a decision someone has
 * to make in this file. The three projections of the runs dataset (list,
 * board, jobs) became one Runs page with a projection toggle, and the
 * authoring doors moved under Design; nothing was retired, every old URL
 * redirects (App.test.tsx walks them).
 */
const PRIMARY_NAV: ReadonlyArray<readonly [string, string]> = [
  ["Your work", "/"],
  ["Inbox", "/inbox"],
  ["Decisions", "/decisions"],
  ["Design", "/design"],
  ["Runs", "/runs"],
  ["Mesh", "/mesh"],
  ["Ledger-and-plan", "/plan"],
  ["Statistics", "/stats"],
];

function primaryLinks() {
  return within(
    screen.getByRole("navigation", { name: "Primary" }),
  ).getAllByRole("link");
}

describe("Header primary nav on the PRD §8.6 spine (task t9)", () => {
  it("renders exactly eight primary links, named in spine order", () => {
    renderHeader();
    const links = primaryLinks();
    expect(links).toHaveLength(8);
    expect(links.map((link) => link.textContent)).toEqual(
      PRIMARY_NAV.map(([name]) => name),
    );
  });

  it("points each link at the surface that renders it", () => {
    renderHeader();
    for (const [name, href] of PRIMARY_NAV) {
      expect(screen.getByRole("link", { name })).toHaveAttribute("href", href);
    }
  });

  it("offers no separate Board, Jobs, Node Graphs or Generate destination — they are projections and sub-routes now", () => {
    renderHeader();
    const nav = screen.getByRole("navigation", { name: "Primary" });
    for (const gone of ["Board", "Jobs", "Node Graphs", "Generate", "Plan"]) {
      expect(within(nav).queryByRole("link", { name: gone })).toBeNull();
    }
  });
});

describe("Header design link (task t9, replacing t28's Node Graphs)", () => {
  it("routes to /design and marks it active there", () => {
    renderHeader(["/design"]);
    const design = screen.getByRole("link", { name: "Design" });
    expect(design).toHaveAttribute("href", "/design");
    expect(design).toHaveClass("is-active");
  });

  it("stays marked active on a Design sub-tab URL", () => {
    renderHeader(["/design?tab=active"]);
    expect(screen.getByRole("link", { name: "Design" })).toHaveClass(
      "is-active",
    );
  });

  it("stays marked active on the authoring sub-routes it now hosts", () => {
    renderHeader(["/design/new"]);
    expect(screen.getByRole("link", { name: "Design" })).toHaveClass(
      "is-active",
    );
  });
});

describe("Header ledger-and-plan link (task t23, renamed by t9)", () => {
  it("routes to /plan and marks it active there", () => {
    renderHeader(["/plan"]);
    const plan = screen.getByRole("link", { name: "Ledger-and-plan" });
    expect(plan).toHaveAttribute("href", "/plan");
    expect(plan).toHaveClass("is-active");
  });

  it("stays marked active on a specific plan slug URL", () => {
    renderHeader(["/plan/economy-discord-graphs"]);
    expect(screen.getByRole("link", { name: "Ledger-and-plan" })).toHaveClass(
      "is-active",
    );
  });
});

describe("Header active view marking", () => {
  it("marks the current view's link with is-active and no other", () => {
    renderHeader(["/mesh"]);
    expect(screen.getByRole("link", { name: "Mesh" })).toHaveClass("is-active");
    for (const [name] of PRIMARY_NAV.filter(([name]) => name !== "Mesh")) {
      expect(screen.getByRole("link", { name })).not.toHaveClass("is-active");
    }
  });

  it("keeps Runs marked active on a projection of the runs page, since it is one page", () => {
    renderHeader(["/runs?view=board"]);
    expect(screen.getByRole("link", { name: "Runs" })).toHaveClass("is-active");
    expect(screen.getByRole("link", { name: "Design" })).not.toHaveClass(
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

/**
 * The signed-in readout (task t9, spec c8): the header renders who the
 * Cloudflare edge says is here, read from `GET /v1alpha1/whoami`. There is
 * no login form and no token field anywhere — Access is the login.
 */
describe("Header signed-in readout (task t9)", () => {
  it("says who is signed in, by email, for a bound Access login", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
    renderHeader();
    await waitFor(() =>
      expect(document.getElementById("app-header-identity")).toHaveTextContent(
        `signed in as ${WHOAMI_EMAIL}`,
      ),
    );
    expect(document.getElementById("app-header-identity")).toHaveAttribute(
      "data-identity-status",
      "bound",
    );
  });

  it("names a service token by its common_name when there is no email", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_SERVICE_TOKEN);
    renderHeader();
    await waitFor(() =>
      expect(document.getElementById("app-header-identity")).toHaveTextContent(
        "signed in as ops-cli",
      ),
    );
  });

  it("marks an unbound login as such rather than presenting it as signed in", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_UNBOUND);
    renderHeader();
    await waitFor(() =>
      expect(document.getElementById("app-header-identity")).toHaveAttribute(
        "data-identity-status",
        "unbound",
      ),
    );
    expect(document.getElementById("app-header-identity")).toHaveTextContent(
      WHOAMI_UNBOUND_EMAIL,
    );
    expect(document.getElementById("app-header-identity")).toHaveTextContent(
      /no actor bound/,
    );
  });

  it("says not signed in on a 401, and offers no token field or login form", async () => {
    mockGetWhoami.mockRejectedValue(
      new ApiError(401, "request refused", "authenticate with a bound principal"),
    );
    renderHeader();
    await waitFor(() =>
      expect(document.getElementById("app-header-identity")).toHaveTextContent(
        "not signed in",
      ),
    );
    expect(screen.queryByLabelText(/token/i)).toBeNull();
    expect(document.querySelector('input[type="password"]')).toBeNull();
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

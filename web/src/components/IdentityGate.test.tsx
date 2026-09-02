import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { ApiError, getWhoami } from "../api/client";
import {
  WHOAMI_BOUND,
  WHOAMI_UNBOUND,
  WHOAMI_UNBOUND_EMAIL,
  WHOAMI_UNBOUND_SUBJECT,
} from "../fixtures/whoami-fixture";
import { resetWhoamiForTests } from "../hooks/useWhoami";
import IdentityGate, { PEOPLE_DOC_URL, SIGN_IN_ORIGIN } from "./IdentityGate";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, getWhoami: vi.fn() };
});

const mockGetWhoami = vi.mocked(getWhoami);

function renderGate() {
  return render(
    <IdentityGate>
      <p data-testid="routed-view">the routed view</p>
    </IdentityGate>,
  );
}

beforeEach(() => {
  resetWhoamiForTests();
  mockGetWhoami.mockReset();
});

afterEach(() => {
  resetWhoamiForTests();
});

/**
 * The two identity states a person can land in that the routed views must
 * not paper over (task t9, spec c8 / the onboarding requirement): a login
 * with no actor bound, and no login at all. Neither builds a login form —
 * Cloudflare Access is the login.
 */
describe("IdentityGate", () => {
  it("renders the routed view for a bound principal", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
    renderGate();
    await waitFor(() => expect(mockGetWhoami).toHaveBeenCalled());
    expect(screen.getByTestId("routed-view")).toBeInTheDocument();
    expect(screen.queryByRole("heading")).toBeNull();
  });

  it("renders the routed view while whoami is still loading, rather than a blank page", () => {
    mockGetWhoami.mockReturnValue(new Promise(() => {}));
    renderGate();
    expect(screen.getByTestId("routed-view")).toBeInTheDocument();
  });

  it("replaces the page with the unbound state naming the principal and the onboarding recipe", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_UNBOUND);
    renderGate();
    expect(
      await screen.findByRole("heading", { name: "No actor is bound to this login" }),
    ).toBeInTheDocument();
    const state = document.getElementById("identity-unbound")!;
    expect(state).toHaveTextContent(WHOAMI_UNBOUND_EMAIL);
    expect(state).toHaveTextContent(WHOAMI_UNBOUND_SUBJECT);
    expect(screen.getByRole("link", { name: /docs\/operations\/people\.md/ })).toHaveAttribute(
      "href",
      PEOPLE_DOC_URL,
    );
    // An unbound login is never a silent viewer: the routed view is gone.
    expect(screen.queryByTestId("routed-view")).toBeNull();
  });

  it("shows the sign-in-required state with the Access origin on a 401, and builds no login form", async () => {
    mockGetWhoami.mockRejectedValue(
      new ApiError(401, "request refused", "authenticate with a bound principal"),
    );
    renderGate();
    expect(
      await screen.findByRole("heading", { name: "Sign in required" }),
    ).toBeInTheDocument();
    expect(SIGN_IN_ORIGIN).toBe("https://nodes.culture.dev");
    expect(screen.getByRole("link", { name: SIGN_IN_ORIGIN })).toHaveAttribute(
      "href",
      SIGN_IN_ORIGIN,
    );
    expect(document.querySelector("form")).toBeNull();
    expect(document.querySelector("input")).toBeNull();
    // Reads stay open on the LAN (spec c9): the view still renders beneath
    // the notice; every write is refused server-side without a principal.
    expect(screen.getByTestId("routed-view")).toBeInTheDocument();
  });

  it("keeps the routed view when whoami is merely unreachable", async () => {
    mockGetWhoami.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderGate();
    await waitFor(() => expect(mockGetWhoami).toHaveBeenCalled());
    expect(screen.getByTestId("routed-view")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Sign in required" })).toBeNull();
  });
});

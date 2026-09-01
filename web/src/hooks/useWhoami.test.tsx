import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { ApiError, getWhoami } from "../api/client";
import {
  WHOAMI_ACTOR_ID,
  WHOAMI_BOUND,
  WHOAMI_EMAIL,
  WHOAMI_SERVICE_TOKEN,
  WHOAMI_UNBOUND,
  WHOAMI_UNBOUND_EMAIL,
} from "../fixtures/whoami-fixture";
import { principalDisplayName, resetWhoamiForTests, useWhoami } from "./useWhoami";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, getWhoami: vi.fn() };
});

const mockGetWhoami = vi.mocked(getWhoami);

/**
 * The browser's identity model (task t9, spec c8): one `GET /v1alpha1/whoami`
 * per session, shared by every component that asks. There is nothing to
 * type — the Cloudflare edge cookie carries the identity and the control
 * plane says which registered actor it is bound to.
 */
function Probe({ label = "probe" }: { label?: string }) {
  const whoami = useWhoami();
  return (
    <p data-testid={label} data-status={whoami.status}>
      {whoami.status === "bound" ? `${whoami.actorId} ${whoami.displayName}` : whoami.status}
    </p>
  );
}

beforeEach(() => {
  resetWhoamiForTests();
  mockGetWhoami.mockReset();
});

afterEach(() => {
  resetWhoamiForTests();
});

describe("useWhoami", () => {
  it("starts loading, then reports a bound principal with its actor id and display name", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
    render(<Probe />);
    expect(screen.getByTestId("probe")).toHaveAttribute("data-status", "loading");
    await waitFor(() =>
      expect(screen.getByTestId("probe")).toHaveAttribute("data-status", "bound"),
    );
    expect(screen.getByTestId("probe")).toHaveTextContent(
      `${WHOAMI_ACTOR_ID} ${WHOAMI_EMAIL}`,
    );
  });

  it("fetches whoami once per session, however many components ask", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
    const first = render(<Probe label="a" />);
    render(<Probe label="b" />);
    await waitFor(() =>
      expect(screen.getByTestId("b")).toHaveAttribute("data-status", "bound"),
    );
    first.unmount();
    render(<Probe label="c" />);
    await waitFor(() =>
      expect(screen.getByTestId("c")).toHaveAttribute("data-status", "bound"),
    );
    expect(mockGetWhoami).toHaveBeenCalledTimes(1);
  });

  it("reports an unbound principal as unbound, never as a silent viewer", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_UNBOUND);
    render(<Probe />);
    await waitFor(() =>
      expect(screen.getByTestId("probe")).toHaveAttribute("data-status", "unbound"),
    );
  });

  it("maps a 401 to unauthenticated", async () => {
    mockGetWhoami.mockRejectedValue(
      new ApiError(401, "request refused", "authenticate with a bound principal"),
    );
    render(<Probe />);
    await waitFor(() =>
      expect(screen.getByTestId("probe")).toHaveAttribute(
        "data-status",
        "unauthenticated",
      ),
    );
  });

  it("maps any other failure to unavailable rather than to signed-out", async () => {
    mockGetWhoami.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    render(<Probe />);
    await waitFor(() =>
      expect(screen.getByTestId("probe")).toHaveAttribute("data-status", "unavailable"),
    );
  });
});

describe("principalDisplayName", () => {
  it("prefers the email, then the service token's common_name, then the subject", () => {
    expect(principalDisplayName(WHOAMI_BOUND.principal)).toBe(WHOAMI_EMAIL);
    expect(principalDisplayName(WHOAMI_SERVICE_TOKEN.principal)).toBe("ops-cli");
    expect(principalDisplayName(WHOAMI_UNBOUND.principal)).toBe(WHOAMI_UNBOUND_EMAIL);
    expect(
      principalDisplayName({ provider: "cloudflare-access", subject: "only-a-subject" }),
    ).toBe("only-a-subject");
  });
});

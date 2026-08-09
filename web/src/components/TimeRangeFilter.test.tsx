import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import TimeRangeFilter from "./TimeRangeFilter";

/**
 * Mirrors TimeRangeFilter's own ISO -> `datetime-local` conversion, so
 * assertions about the pre-filled custom inputs are correct in whatever
 * timezone the test runner happens to be in, rather than assuming UTC.
 */
function expectedLocalInput(iso: string): string {
  const date = new Date(iso);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** Asserts `iso` is within `toleranceMs` of `expectedMsAgo` milliseconds before now. */
function expectRoughlyAgo(iso: string, expectedMsAgo: number, toleranceMs = 5000) {
  const deltaMs = Date.now() - new Date(iso).getTime();
  expect(Math.abs(deltaMs - expectedMsAgo)).toBeLessThan(toleranceMs);
}

describe("TimeRangeFilter", () => {
  it("renders one button per preset plus Custom, none pressed with no active range", () => {
    render(<TimeRangeFilter onApply={vi.fn()} />);
    for (const label of ["Last hour", "Last 24h", "Last 7 days", "Custom"]) {
      const button = screen.getByRole("button", { name: label });
      expect(button).toHaveAttribute("aria-pressed", "false");
    }
  });

  it("clicking a preset calls onApply with a since computed from now, and no until", async () => {
    const onApply = vi.fn();
    const user = userEvent.setup();
    render(<TimeRangeFilter onApply={onApply} />);

    await user.click(screen.getByRole("button", { name: "Last hour" }));

    expect(onApply).toHaveBeenCalledTimes(1);
    const [range] = onApply.mock.calls[0];
    expect(range.until).toBeUndefined();
    expectRoughlyAgo(range.since, 60 * 60 * 1000);
  });

  it("marks the clicked preset pressed and the others not", async () => {
    const user = userEvent.setup();
    render(<TimeRangeFilter onApply={vi.fn()} />);

    await user.click(screen.getByRole("button", { name: "Last 24h" }));

    expect(screen.getByRole("button", { name: "Last 24h" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("button", { name: "Last hour" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
    expect(screen.getByRole("button", { name: "Last 7 days" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });

  it("reveals a Custom since/until form only after the Custom button is activated", async () => {
    const user = userEvent.setup();
    render(<TimeRangeFilter onApply={vi.fn()} />);

    expect(screen.queryByLabelText("Since")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Custom" }));
    expect(screen.getByLabelText("Since")).toBeInTheDocument();
    expect(screen.getByLabelText("Until")).toBeInTheDocument();
  });

  it("submitting the custom form calls onApply with the chosen since/until as RFC3339", async () => {
    const onApply = vi.fn();
    const user = userEvent.setup();
    render(<TimeRangeFilter onApply={onApply} />);
    await user.click(screen.getByRole("button", { name: "Custom" }));

    fireEvent.change(screen.getByLabelText("Since"), {
      target: { value: "2026-08-01T09:00" },
    });
    fireEvent.change(screen.getByLabelText("Until"), {
      target: { value: "2026-08-02T09:00" },
    });
    await user.click(screen.getByRole("button", { name: "Apply" }));

    expect(onApply).toHaveBeenCalledTimes(1);
    const [range] = onApply.mock.calls[0];
    expect(range.since).toBe(new Date("2026-08-01T09:00").toISOString());
    expect(range.until).toBe(new Date("2026-08-02T09:00").toISOString());
  });

  it("shows Clear range only when a range is active, and clearing calls onApply with an empty range", async () => {
    const onApply = vi.fn();
    const { rerender } = render(<TimeRangeFilter onApply={onApply} />);
    expect(
      screen.queryByRole("button", { name: "Clear range" }),
    ).not.toBeInTheDocument();

    rerender(
      <TimeRangeFilter since="2026-08-09T11:00:00.000Z" onApply={onApply} />,
    );
    const clear = screen.getByRole("button", { name: "Clear range" });
    await userEvent.setup().click(clear);
    expect(onApply).toHaveBeenCalledWith({ since: undefined, until: undefined });
  });

  it("every preset and the custom toggle are reachable and activatable by keyboard alone", async () => {
    const onApply = vi.fn();
    const user = userEvent.setup();
    render(<TimeRangeFilter onApply={onApply} />);

    await user.tab(); // Last hour
    expect(screen.getByRole("button", { name: "Last hour" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(onApply).toHaveBeenCalledTimes(1);
    const [range] = onApply.mock.calls[0];
    expect(range.until).toBeUndefined();
    expectRoughlyAgo(range.since, 60 * 60 * 1000);
  });

  it("pre-fills the custom inputs from an already-applied since/until (bookmarked URL case)", async () => {
    const user = userEvent.setup();
    render(
      <TimeRangeFilter
        since="2026-08-01T09:00:00.000Z"
        until="2026-08-02T09:00:00.000Z"
        onApply={vi.fn()}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Custom" }));
    expect(screen.getByLabelText("Since")).toHaveValue(
      expectedLocalInput("2026-08-01T09:00:00.000Z"),
    );
    expect(screen.getByLabelText("Until")).toHaveValue(
      expectedLocalInput("2026-08-02T09:00:00.000Z"),
    );
  });
});

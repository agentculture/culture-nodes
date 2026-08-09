import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import RunStateChip from "./RunStateChip";
import type { RunState } from "../api/types";

describe("RunStateChip", () => {
  const states: RunState[] = [
    "created",
    "running",
    "waiting",
    "completed",
    "failed",
    "cancelled",
  ];

  it.each(states)(
    "renders the literal RunState word %s, with an icon hidden from assistive tech",
    (state) => {
      const { container } = render(<RunStateChip state={state} />);
      const chip = container.querySelector<HTMLElement>(".status-chip");
      expect(chip?.dataset.runState).toBe(state);
      // The visible label is the actual enum value — never a paraphrase like
      // "queued" for "created".
      expect(screen.getByText(state)).toBeInTheDocument();
      const icon = container.querySelector(".status-chip__icon");
      expect(icon).toHaveAttribute("aria-hidden", "true");
      expect(icon?.textContent).toBeTruthy();
    },
  );

  it("carries a status-chip--<state> modifier so it inherits the shared chip accent styling", () => {
    const { container } = render(<RunStateChip state="completed" />);
    expect(
      container.querySelector(".status-chip.status-chip--completed"),
    ).toBeInTheDocument();
  });

  it("accepts an additional className without dropping the base ones", () => {
    const { container } = render(
      <RunStateChip state="waiting" className="extra" />,
    );
    const chip = container.querySelector(".status-chip");
    expect(chip).toHaveClass("status-chip", "status-chip--waiting", "extra");
  });
});

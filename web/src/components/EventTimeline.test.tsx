import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import EventTimeline from "./EventTimeline";
import { RUN_EVENTS } from "../fixtures/run-fixture";

describe("EventTimeline", () => {
  it("says so plainly when no event has been committed yet", () => {
    render(<EventTimeline events={[]} />);
    expect(screen.getByText(/No committed events yet/)).toBeInTheDocument();
  });

  it("renders one row per committed event, in stream order", () => {
    render(<EventTimeline events={RUN_EVENTS} />);
    const list = screen.getByRole("list", { name: "Run event timeline" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(RUN_EVENTS.length);
  });

  it("shows type, node and time for a transition", () => {
    render(<EventTimeline events={RUN_EVENTS} />);
    const list = screen.getByRole("list", { name: "Run event timeline" });
    const rows = within(list).getAllByRole("listitem");
    const transition = rows.find(
      (row) => row.dataset.eventType === "dev.culture.nodes.token.transitioned",
    );
    expect(transition).toBeDefined();
    expect(transition).toHaveTextContent("token.transitioned");
    expect(transition?.querySelector("time")).toHaveAttribute(
      "datetime",
      expect.stringContaining("2026-08-09T09:"),
    );
  });

  it("carries the same information the graph does — every node the run touched", () => {
    render(<EventTimeline events={RUN_EVENTS} />);
    for (const nodeId of ["intake", "plan", "build", "test", "verify"]) {
      expect(
        screen.getAllByText(nodeId).length,
        `timeline mentions ${nodeId}`,
      ).toBeGreaterThan(0);
    }
  });

  it("opens a node's detail from the timeline, so the non-graph view is not a dead end", async () => {
    const onSelectNode = vi.fn();
    const user = userEvent.setup();
    render(<EventTimeline events={RUN_EVENTS} onSelectNode={onSelectNode} />);
    await user.click(
      screen.getAllByRole("button", { name: "open detail for node verify" })[0],
    );
    expect(onSelectNode).toHaveBeenCalledWith("verify");
  });

  it("highlights the rows touching the selected node", () => {
    const { container } = render(
      <EventTimeline events={RUN_EVENTS} selectedNodeId="verify" />,
    );
    expect(
      container.querySelectorAll(".timeline__item.is-selected").length,
    ).toBeGreaterThan(0);
  });
});

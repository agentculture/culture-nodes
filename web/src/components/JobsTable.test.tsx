import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import JobsTable from "./JobsTable";
import { JOB_RUNS_PAGE_1 } from "../fixtures/node-runs-fixture";

function renderTable(items = JOB_RUNS_PAGE_1) {
  return render(
    <MemoryRouter>
      <JobsTable items={items} id="jobs-table" />
    </MemoryRouter>,
  );
}

describe("JobsTable", () => {
  it("shows the empty state and no table when there are no items", () => {
    render(
      <MemoryRouter>
        <JobsTable items={[]} id="jobs-table" />
      </MemoryRouter>,
    );
    expect(screen.getByText("No node runs in this range.")).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("renders one row per node run, with the required columns", () => {
    renderTable();
    expect(
      screen.getByRole("columnheader", { name: "run" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("columnheader", { name: "node" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("columnheader", { name: "actor / runner" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("columnheader", { name: "state" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("columnheader", { name: "outcome" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("columnheader", { name: "started" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("columnheader", { name: "updated" }),
    ).toBeInTheDocument();

    const rows = screen.getAllByRole("row");
    // 1 header row + 1 per fixture item.
    expect(rows).toHaveLength(JOB_RUNS_PAGE_1.length + 1);
  });

  it("links the run column to /runs/:id", () => {
    renderTable();
    const item = JOB_RUNS_PAGE_1[0];
    const link = screen.getByRole("link", { name: item.run_id });
    expect(link).toHaveAttribute("href", `/runs/${item.run_id}`);
  });

  it("renders the node run's state through the shared StatusChip vocabulary", () => {
    const { container } = renderTable();
    const runningRow = container.querySelector(
      `[data-node-run-id="${JOB_RUNS_PAGE_1[0].id}"]`,
    ) as HTMLElement;
    expect(
      within(runningRow).getByText("running", {
        selector: ".status-chip__label",
      }),
    ).toBeInTheDocument();
    // The failed fixture row's `outcome` column is *also* the word "failed"
    // (openapi.yaml's outcome is free text) — scope to the chip specifically
    // so this assertion is about the state column, not a text-content clash.
    const failedRow = container.querySelector(
      `[data-node-run-id="${JOB_RUNS_PAGE_1[2].id}"]`,
    ) as HTMLElement;
    const chip = failedRow.querySelector(".status-chip");
    expect(chip).toHaveAttribute("data-state", "failed");
    expect(
      within(failedRow).getByText("failed", { selector: ".status-chip__label" }),
    ).toBeInTheDocument();
  });

  it("falls back to an em dash for a missing actor_id or outcome, never blank", () => {
    const { container } = renderTable();
    const waitingRow = container.querySelector(
      `[data-node-run-id="${JOB_RUNS_PAGE_1[1].id}"]`,
    ) as HTMLElement;
    // waiting_external row has no actor_id and no outcome in the fixture.
    expect(within(waitingRow).getAllByText("—").length).toBeGreaterThanOrEqual(2);
  });

  it("renders started/updated as real <time> elements carrying the API's timestamps", () => {
    const { container } = renderTable();
    const item = JOB_RUNS_PAGE_1[0];
    const row = container.querySelector(
      `[data-node-run-id="${item.id}"]`,
    ) as HTMLElement;
    const times = row.querySelectorAll("time");
    expect(times).toHaveLength(2);
    expect(times[0]).toHaveAttribute("dateTime", item.created_at);
    expect(times[1]).toHaveAttribute("dateTime", item.updated_at);
  });
});

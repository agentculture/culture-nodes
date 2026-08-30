import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import JobsTable from "./JobsTable";
import {
  JOB_RUNS_ALL,
  JOB_RUNS_NAMED_RUNS,
  JOB_RUNS_PAGE_1,
} from "../fixtures/node-runs-fixture";

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

  describe("per-node-run usage (task t5)", () => {
    it("renders tokens compactly for a row that reported usage", () => {
      const { container } = renderTable();
      // JOB_RUNS_PAGE_1[3] (verify) carries USAGE_WITH_COST.
      const row = container.querySelector(
        `[data-node-run-id="${JOB_RUNS_PAGE_1[3].id}"]`,
      ) as HTMLElement;
      expect(within(row).getByText("12.3k in / 4.1k out")).toBeInTheDocument();
    });

    it("renders the not-reported state, never '0 tokens', for a row with no usage", () => {
      const { container } = renderTable();
      // JOB_RUNS_PAGE_1[0] (build) carries USAGE_NOT_REPORTED.
      const row = container.querySelector(
        `[data-node-run-id="${JOB_RUNS_PAGE_1[0].id}"]`,
      ) as HTMLElement;
      expect(within(row).getByText("not reported")).toBeInTheDocument();
      expect(within(row).queryByText(/0 in \/ 0 out/)).not.toBeInTheDocument();
    });
  });

  describe("run name/category lookup (task t5)", () => {
    const runsById = Object.fromEntries(
      JOB_RUNS_NAMED_RUNS.map((run) => [run.id, run]),
    );

    it("falls back to the bare run id when no lookup is provided", () => {
      renderTable();
      const item = JOB_RUNS_PAGE_1[0];
      expect(
        screen.getByRole("link", { name: item.run_id }),
      ).toBeInTheDocument();
    });

    it("shows the run's given name and category chip when the lookup has one", () => {
      render(
        <MemoryRouter>
          <JobsTable items={JOB_RUNS_PAGE_1} runsById={runsById} />
        </MemoryRouter>,
      );
      const item = JOB_RUNS_PAGE_1[1]; // human-review row, named "nightly regression sweep"
      const row = screen
        .getByText(item.node_id, { selector: "code" })
        .closest("tr") as HTMLElement;
      const name = within(row).getByText("nightly regression sweep");
      expect(name).toHaveAttribute("data-derived", "false");
      expect(within(row).getByText("ci")).toBeInTheDocument();
    });

    it("shows a derived display_hint marked distinctly, never as a given name", () => {
      render(
        <MemoryRouter>
          <JobsTable items={JOB_RUNS_ALL} runsById={runsById} />
        </MemoryRouter>,
      );
      // The second named fixture run belongs to a JOB_RUNS_PAGE_2 row.
      const item = JOB_RUNS_ALL.find(
        (candidate) => candidate.run_id === JOB_RUNS_NAMED_RUNS[1].id,
      )!;
      const row = screen
        .getByText(item.node_id, { selector: "code" })
        .closest("tr") as HTMLElement;
      const hint = within(row).getByText(
        "fix the flaky pytest-report parser",
      );
      expect(hint).toHaveAttribute("data-derived", "true");
      expect(hint.className).toContain("run-name--derived");
    });
  });
});


describe("JobsTable humanised timestamps (task t27)", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("renders started and updated relative, with the exact instant on title", () => {
    const item = {
      ...JOB_RUNS_PAGE_1[0],
      created_at: "2026-08-30T09:00:00Z",
      updated_at: "2026-08-30T11:30:00Z",
    };
    vi.useFakeTimers({
      now: new Date("2026-08-30T12:00:00Z"),
      shouldAdvanceTime: true,
    });
    render(
      <MemoryRouter>
        <JobsTable items={[item]} id="jobs-table" />
      </MemoryRouter>,
    );

    const times = document.querySelectorAll(
      "#jobs-table tbody time",
    ) as NodeListOf<HTMLTimeElement>;
    expect(times).toHaveLength(2);
    expect(times[0]).toHaveTextContent("3 hours ago");
    expect(times[0]).toHaveAttribute("title", item.created_at);
    expect(times[0]).toHaveAttribute("dateTime", item.created_at);
    expect(times[1]).toHaveTextContent("30 minutes ago");
    expect(times[1]).toHaveAttribute("title", item.updated_at);
  });
});

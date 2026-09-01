import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import RunDecisionCard, {
  confirmAllVerdicts,
  recordsWithVerdict,
} from "./RunDecisionCard";
import { PENDING_RUN } from "../fixtures/pending-decisions-fixture";

/**
 * The shared run-decision card (task t12): the one rendering of "here are a
 * run's undecided records, and here is what you are about to say about each
 * of them". It was `RunDecisionCard` inside Decisions.tsx until the ticket
 * page needed the same thing (spec c11) — two renderings of the record a
 * decider reads before confirming it are two chances for one of them to
 * summarise away the qualifying half.
 */
function renderCard(
  verdicts = confirmAllVerdicts(PENDING_RUN),
  onVerdictChange = vi.fn(),
) {
  render(
    <MemoryRouter>
      <ul>
        <RunDecisionCard
          group={PENDING_RUN}
          verdicts={verdicts}
          onVerdictChange={onVerdictChange}
        />
      </ul>
    </MemoryRouter>,
  );
  return {
    card: document.querySelector(
      `[data-run-id="${PENDING_RUN.run_id}"]`,
    ) as HTMLElement,
    onVerdictChange,
  };
}

describe("RunDecisionCard", () => {
  it("names the run, the version a review must be opened against, and every record", () => {
    const { card } = renderCard();
    expect(within(card).getByText(/ledger version 7/)).toBeInTheDocument();
    for (const record of PENDING_RUN.records) {
      expect(within(card).getByText(record.id)).toBeInTheDocument();
    }
  });

  it("renders the claim payload in full, including the qualifying half", () => {
    const { card } = renderCard();
    expect(
      within(card).getByText(/could not run the suite locally/),
    ).toBeInTheDocument();
    // The statement is prose, not a quoted JSON string with literal \n.
    expect(
      card.querySelector(".decisions-record__statement"),
    ).not.toBeNull();
    expect(card.querySelector(".decisions-record__data")?.textContent).not.toContain(
      "could not run the suite locally",
    );
  });

  it("offers confirm, reject and not-now per record and reports the change", async () => {
    const user = userEvent.setup();
    const { card, onVerdictChange } = renderCard();
    const record = PENDING_RUN.records[0];
    const group = within(card).getByRole("group", {
      name: `Verdict for ${record.id}`,
    });
    expect(
      within(group).getAllByRole("radio").map((radio) => radio.getAttribute("value")),
    ).toEqual(["confirm", "reject", "skip"]);

    await user.click(within(group).getByRole("radio", { name: "reject" }));
    expect(onVerdictChange).toHaveBeenCalledWith(record.id, "reject");
  });

  it("disables every verdict when nothing can be recorded", () => {
    render(
      <MemoryRouter>
        <ul>
          <RunDecisionCard
            group={PENDING_RUN}
            verdicts={confirmAllVerdicts(PENDING_RUN)}
            onVerdictChange={vi.fn()}
            disabled
          />
        </ul>
      </MemoryRouter>,
    );
    for (const radio of screen.getAllByRole("radio")) {
      expect(radio).toBeDisabled();
    }
  });

  /**
   * A review names records; it never rewrites them (PRD §10.8). Once a run's
   * review has committed, the record it decided still reads `proposed` — the
   * verdict shows up beside it as its own thing.
   */
  it("shows a decided record as still proposed, with the review beside it", () => {
    render(
      <MemoryRouter>
        <ul>
          <RunDecisionCard
            group={PENDING_RUN}
            verdicts={confirmAllVerdicts(PENDING_RUN)}
            onVerdictChange={vi.fn()}
            reviewedRecordIds={PENDING_RUN.records.map((record) => record.id)}
          />
        </ul>
      </MemoryRouter>,
    );
    const card = document.querySelector(
      `[data-run-id="${PENDING_RUN.run_id}"]`,
    ) as HTMLElement;
    const row = card.querySelector(
      `[data-record-id="${PENDING_RUN.records[0].id}"]`,
    ) as HTMLElement;
    expect(row.querySelector('[data-authority="proposed"]')).not.toBeNull();
    expect(row.querySelector('[data-authority="confirmed"]')).not.toBeNull();
    // No verdict is offered on a record whose review has already committed.
    expect(within(card).queryAllByRole("radio")).toHaveLength(0);
  });
});

describe("verdict helpers", () => {
  it("defaults every record to confirm", () => {
    expect(confirmAllVerdicts(PENDING_RUN)).toEqual({
      [PENDING_RUN.records[0].id]: "confirm",
      [PENDING_RUN.records[1].id]: "confirm",
    });
  });

  it("selects only the records carrying the asked-for verdict", () => {
    const verdicts = {
      [PENDING_RUN.records[0].id]: "confirm" as const,
      [PENDING_RUN.records[1].id]: "skip" as const,
    };
    expect(recordsWithVerdict(PENDING_RUN, verdicts, "confirm")).toEqual([
      PENDING_RUN.records[0].id,
    ]);
    expect(recordsWithVerdict(PENDING_RUN, verdicts, "reject")).toEqual([]);
  });
});

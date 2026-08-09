import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import AuthorityChip, { edgeStyleForAuthority } from "./AuthorityChip";
import LedgerTable from "./LedgerTable";
import {
  DASHED,
  LEDGER_AUTHORITY_EDGE_STYLE,
  SOLID,
} from "../culture-design/edges";
import { LEDGER_RECORDS } from "../fixtures/run-fixture";

describe("edgeStyleForAuthority", () => {
  it("reads the authority mapping straight off culture-design/edges.ts", () => {
    expect(edgeStyleForAuthority("proposed")).toBe(
      LEDGER_AUTHORITY_EDGE_STYLE.proposed,
    );
    expect(edgeStyleForAuthority("confirmed")).toBe(
      LEDGER_AUTHORITY_EDGE_STYLE.confirmed,
    );
  });

  it("draws an agent's unconfirmed proposal dashed and everything with authority solid", () => {
    expect(edgeStyleForAuthority("proposed")).toBe(DASHED);
    expect(edgeStyleForAuthority("confirmed")).toBe(SOLID);
    expect(edgeStyleForAuthority("observed")).toBe(SOLID);
    expect(edgeStyleForAuthority("derived")).toBe(SOLID);
  });

  it("treats a human rejection as authority (solid) and a superseded record as a soft link", () => {
    expect(edgeStyleForAuthority("rejected").name).toBe("SOLID");
    expect(edgeStyleForAuthority("superseded").name).toBe("DOTTED");
  });
});

describe("AuthorityChip", () => {
  it("renders proposed with a dashed outline", () => {
    const { container } = render(<AuthorityChip authority="proposed" />);
    const chip = container.querySelector<HTMLElement>(".authority-chip");
    expect(chip?.dataset.authority).toBe("proposed");
    expect(chip?.dataset.edgeStyle).toBe("DASHED");
    expect(chip?.style.borderStyle).toBe("dashed");
    expect(screen.getByText("proposed")).toBeInTheDocument();
  });

  it("renders confirmed with a solid outline", () => {
    const { container } = render(<AuthorityChip authority="confirmed" />);
    const chip = container.querySelector<HTMLElement>(".authority-chip");
    expect(chip?.dataset.edgeStyle).toBe("SOLID");
    expect(chip?.style.borderStyle).toBe("solid");
  });

  it("pairs every authority with a glyph and its word", () => {
    const { container } = render(<AuthorityChip authority="observed" />);
    expect(container.querySelector(".authority-chip__icon")).toHaveAttribute(
      "aria-hidden",
      "true",
    );
    expect(screen.getByText("observed")).toBeInTheDocument();
  });
});

describe("LedgerTable", () => {
  it("lists type, authority, origin and time for every record", () => {
    render(<LedgerTable records={LEDGER_RECORDS} id="ledger-table" />);
    const table = screen.getByRole("table");
    expect(table).toHaveAttribute("id", "ledger-table");
    for (const header of ["record type", "authority", "origin", "time"]) {
      expect(screen.getByRole("columnheader", { name: header })).toBeInTheDocument();
    }
    // one header row + one row per record
    expect(screen.getAllByRole("row")).toHaveLength(LEDGER_RECORDS.length + 1);
    // Two of the fixture's records share an origin actor, so both rows name it.
    expect(screen.getAllByText("actor://company/intake")).toHaveLength(2);
  });

  it("renders the run's proposed records dashed and its confirmed ones solid", () => {
    const { container } = render(<LedgerTable records={LEDGER_RECORDS} />);
    expect(
      container.querySelectorAll('.authority-chip[data-authority="proposed"]'),
    ).toHaveLength(2);
    expect(
      container.querySelectorAll('.authority-chip--dashed[data-authority="proposed"]'),
    ).toHaveLength(2);
    expect(
      container.querySelectorAll('.authority-chip--solid[data-authority="confirmed"]'),
    ).toHaveLength(1);
  });

  it("says so plainly when there is nothing to show", () => {
    render(<LedgerTable records={[]} />);
    expect(screen.getByText("No ledger records.")).toBeInTheDocument();
  });
});

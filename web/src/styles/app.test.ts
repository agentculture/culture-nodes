import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

describe("runs board horizontal reachability", () => {
  it("keeps the columns in an overflow-x auto container with a visible scrollbar", () => {
    const css = readFileSync("src/styles/app.css", "utf8");
    const rule = css.match(/\.runs-board__columns\s*\{([^}]+)\}/)?.[1] ?? "";
    expect(rule).toMatch(/overflow-x:\s*auto/);
    expect(rule).toMatch(/scrollbar-width:\s*auto/);
    expect(css).toMatch(/\.runs-board__columns::\-webkit-scrollbar\s*\{\s*height:/);
  });
});


describe("scroll affordances (task t27)", () => {
  const css = readFileSync("src/styles/app.css", "utf8");

  it("gives a wide table's scroll box a visible scrollbar and an edge gradient", () => {
    const rule = css.match(/\.table-scroll\s*\{([^}]+)\}/)?.[1] ?? "";
    expect(rule).toMatch(/overflow-x:\s*auto/);
    expect(rule).toMatch(/scrollbar-width:\s*auto/);
    expect(rule).toMatch(/linear-gradient/);
    expect(css).toMatch(/\.table-scroll::-webkit-scrollbar\s*\{\s*height:/);
  });

  it("shades the board's right edge while a column is still off-screen", () => {
    const rule = css.match(/\.runs-board__columns\s*\{([^}]+)\}/)?.[1] ?? "";
    expect(rule).toMatch(/linear-gradient/);
    expect(rule).toMatch(/background-attachment:\s*local,\s*scroll/);
  });

  it("caps a board column's card list and lets it scroll in its own box", () => {
    const rule = css.match(/\.runs-board__cards\s*\{([^}]+)\}/)?.[1] ?? "";
    expect(rule).toMatch(/max-height:/);
    expect(rule).toMatch(/overflow-y:\s*auto/);
    expect(rule).toMatch(/scrollbar-width:/);
    expect(css).toMatch(/\.runs-board__cards::-webkit-scrollbar\s*\{\s*width:/);
  });
});

describe("ticket page rail (task t27)", () => {
  const css = readFileSync("src/styles/app.css", "utf8");

  it("no longer caps the ticket page's rail below the app's full width", () => {
    // `.view-rail` sets max-width: none. A `.ticket-view { max-width: ... }`
    // rule beat it and left this one page visibly inset from its own header.
    // The prose measure moved onto the sections; the rail is the app's.
    const rule = css.match(/\n\.ticket-view\s*\{([^}]+)\}/)?.[1] ?? null;
    expect(rule).toBeNull();
    expect(css).toMatch(/\.ticket-view\s*>\s*section,/);
  });
});

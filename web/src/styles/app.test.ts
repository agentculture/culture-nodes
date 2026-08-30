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

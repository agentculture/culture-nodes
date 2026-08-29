import { readdirSync, readFileSync } from "node:fs";
import { extname, join, relative } from "node:path";
import { describe, expect, it } from "vitest";

const SOURCE_ROOT = join(process.cwd(), "src");

function sourceFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return sourceFiles(path);
    return [".ts", ".tsx"].includes(extname(path)) ? [path] : [];
  });
}

describe("browser credential boundary", () => {
  it("contains no server secret name outside the decision-token contract", () => {
    const suffix = "_" + "SECRET";
    const findings = sourceFiles(SOURCE_ROOT).flatMap((path) => {
      const matches = readFileSync(path, "utf8").match(new RegExp(`[A-Z][A-Z0-9_]+${suffix}`, "g")) ?? [];
      return matches.map((name) => ({ path: relative(SOURCE_ROOT, path), name }));
    });
    expect(findings).toEqual([
      { path: "api/decision-token.ts", name: ["NODES", "HUMAN", "DECISION", "TOKEN", "SECRET"].join("_") },
    ]);
  });
});

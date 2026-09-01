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

/**
 * No server secret is named anywhere in the bundle's source. The allowlist
 * used to carry one entry — the decision-token contract that documented the
 * human-decision secret for the operator who pasted it — and task t9 (spec
 * c8) shrank it to nothing: identity comes from the Cloudflare
 * edge and `GET /v1alpha1/whoami`, so there is no secret for a person to
 * hold and no file that needs to mention one.
 */
describe("browser credential boundary", () => {
  it("contains no server secret name at all", () => {
    const suffix = "_" + "SECRET";
    const findings = sourceFiles(SOURCE_ROOT).flatMap((path) => {
      const matches = readFileSync(path, "utf8").match(new RegExp(`[A-Z][A-Z0-9_]+${suffix}`, "g")) ?? [];
      return matches.map((name) => ({ path: relative(SOURCE_ROOT, path), name }));
    });
    expect(findings).toEqual([]);
  });
});

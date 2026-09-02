import { existsSync, readdirSync, readFileSync } from "node:fs";
import { extname, join, relative } from "node:path";
import { describe, expect, it } from "vitest";

const SOURCE_ROOT = join(process.cwd(), "src");

/**
 * The browser identity boundary (task t9, spec c8's honesty condition):
 * identity is DERIVED from the signed-in principal — `GET /v1alpha1/whoami`
 * behind the Cloudflare edge cookie — never typed, pasted or remembered.
 *
 * Concretely, across web/src (tests excluded):
 *   - no sessionStorage decision token and no localStorage actor id;
 *   - no DeciderActorField and no free-text decider / reviewer / replier
 *     input;
 *   - the API client attaches no Authorization header to anything: a
 *     same-origin request behind Access needs nothing from the page.
 *
 * The forbidden names are assembled from fragments so this file does not
 * match itself, and every source file is scanned so a panel that comes back
 * under a new name is caught by its storage call or its label, not by its
 * filename.
 */
function sourceFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return sourceFiles(path);
    if (![".ts", ".tsx"].includes(extname(path))) return [];
    if (/\.test\.tsx?$/.test(path)) return [];
    return [path];
  });
}

function findings(pattern: RegExp): string[] {
  return sourceFiles(SOURCE_ROOT).flatMap((path) => {
    const source = readFileSync(path, "utf8");
    return pattern.test(source) ? [relative(SOURCE_ROOT, path)] : [];
  });
}

describe("browser identity boundary", () => {
  it("has no decision-token module and no decider field component", () => {
    expect(existsSync(join(SOURCE_ROOT, "api", ["decision", "token.ts"].join("-")))).toBe(false);
    expect(
      existsSync(join(SOURCE_ROOT, "components", ["Decider", "ActorField.tsx"].join(""))),
    ).toBe(false);
    expect(findings(new RegExp(["Decider", "ActorField"].join("")))).toEqual([]);
  });

  it("never reads or writes web storage for identity — no session token, no remembered actor id", () => {
    expect(findings(new RegExp(["session", "Storage"].join("")))).toEqual([]);
    expect(findings(new RegExp(["local", "Storage"].join("")))).toEqual([]);
    expect(findings(new RegExp(["human-decision", "token"].join("-")))).toEqual([]);
    expect(findings(new RegExp(["human-decision", "actor-id"].join("-")))).toEqual([]);
  });

  it("offers no free-text identity input: no decider, reviewer, replier or token field", () => {
    for (const label of [
      ["Decider", "actor id"].join(" "),
      ["Reviewer", "actor id"].join(" "),
      ["Your", "name"].join(" "),
      ["Decision", "token"].join(" "),
      ["Hold", "token"].join(" "),
    ]) {
      expect(findings(new RegExp(label, "i")), label).toEqual([]);
    }
  });

  it("attaches no Authorization header from the API client", () => {
    const client = readFileSync(join(SOURCE_ROOT, "api", "client.ts"), "utf8");
    expect(client).not.toMatch(new RegExp(["author", "ization"].join(""), "i"));
    expect(client).not.toMatch(new RegExp(["Bea", "rer"].join("")));
  });
});

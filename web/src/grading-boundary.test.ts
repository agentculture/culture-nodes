import { readdirSync, readFileSync } from "node:fs";
import { extname, join, relative } from "node:path";
import { describe, expect, it } from "vitest";

const SOURCE_ROOT = join(process.cwd(), "src");

/**
 * There is no grading UI in web/src (spec c12, honesty condition h19).
 *
 * Grading is deliberately out of the ticket page's first cut: the cycle gates
 * `POST /v1alpha1/runs/{id}/grades` with the same principal check every other
 * mutating route gets, and builds no page for it. h19's second half — "no
 * route under web/src renders a grading form" — is the kind of sentence that
 * is true the day it is written and quietly stops being true the first time
 * somebody adds a textarea to a run view, so it is pinned here rather than
 * left to a grep somebody remembers to run.
 *
 * The check is deliberately coarse: any source file that both names the
 * grades route and renders a form control fails it. A page that merely
 * *displays* a recorded grade is not a grading form and does not trip it.
 *
 * The forbidden strings are assembled from fragments so this file does not
 * match itself.
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

const GRADES_ROUTE = ["/runs/", "/", "grades"].join("");
const GRADE_CALL = ["post", "Grade"].join("");
const FORM_CONTROL = /<(form|textarea|select|input)\b/;

describe("no grading surface in the browser (spec c12 / h19)", () => {
  it("has no client helper for the grades route", () => {
    const offenders = sourceFiles(SOURCE_ROOT).filter((path) => {
      const source = readFileSync(path, "utf8");
      return source.includes(GRADES_ROUTE) || source.includes(GRADE_CALL);
    });
    expect(offenders.map((path) => relative(SOURCE_ROOT, path))).toEqual([]);
  });

  it("has no route that renders a grading form", () => {
    const grading = /\bgrad(e|es|ing)\b/i;
    const offenders = sourceFiles(SOURCE_ROOT).filter((path) => {
      const source = readFileSync(path, "utf8");
      return grading.test(source) && FORM_CONTROL.test(source);
    });
    expect(offenders.map((path) => relative(SOURCE_ROOT, path))).toEqual([]);
  });
});

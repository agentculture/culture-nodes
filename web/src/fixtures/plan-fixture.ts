/**
 * Fixture data for the Plan view (`/plan/:slug`, task t23), used by both the
 * vitest route test and the Playwright e2e spec (via
 * e2e/fixtures/api.ts's mockPlanApi).
 *
 * Mirrors internal/devague/testdata/plan-show.json + deviations.json (the
 * same fixture task t22's own Go tests round-trip), reshaped as the wire
 * response `GET /v1alpha1/plan-imports/{id}` returns rather than the raw
 * devague documents:
 *
 * - t1/t2 are wave 1 (no deps); t3 depends on t1 alone (wave 2), even
 *   though t2 is also in t1's wave — proving the view renders REAL
 *   per-task edges, not "everything in the previous wave" (spec c15).
 * - t4 depends on both t1 and t2 (wave 2) and is `origin_kind: "human"`,
 *   the one task an operator authored directly rather than devague/an
 *   agent proposing it.
 * - t5 is `rejected` and carries no wave at all (PlanTask.Wave's contract:
 *   a rejected task never ships, so it occupies no wave) — the
 *   `unscheduled` bucket domain/plan.ts's groupTasksByWave renders.
 * - Two plan-import snapshots exist for the same slug (PLAN_IMPORT_OLDER,
 *   PLAN_IMPORT), proving listPlanImports' "most recent first" ordering
 *   the view relies on to pick "the current one".
 *
 * Deviations exercise the origin distinction this task exists for:
 * - d1: origin `human` ("user reports"), status `approved`.
 * - d2: origin `agent` ("system knows"), status `proposed`.
 * - d3: origin `agent` ("system knows"), status `rejected`.
 */

import type { PlanImport, PlanImportSummary } from "../api/types";

export const PLAN_SLUG = "economy-discord-graphs";

export const PLAN_IMPORT: PlanImport = {
  id: "pi-current",
  slug: PLAN_SLUG,
  title: "Build Plan — economy-discord-graphs",
  source_slug: PLAN_SLUG,
  source_status: "exported",
  source_digest: "sha256:planimportfixture0000000000000000000000000000000000000000",
  imported_at: "2026-08-14T05:20:00Z",
  tasks: [
    {
      task_ref: "t1",
      summary: "No-dependency setup task",
      instruction: "Bootstrap the fixture",
      origin_kind: "agent",
      source_status: "confirmed",
      depends_on: [],
      wave: 1,
      acceptance_criteria: ["t1 has no prerequisites"],
      covers: ["c1", "h1"],
    },
    {
      task_ref: "t2",
      summary: "Another independent setup task",
      instruction: "Bootstrap something else",
      origin_kind: "agent",
      source_status: "proposed",
      depends_on: [],
      wave: 1,
      acceptance_criteria: ["t2 has no prerequisites either"],
      covers: ["c2", "h2"],
    },
    {
      task_ref: "t3",
      summary: "Depends on t1 only, not on t2",
      instruction: "Build on t1's output",
      origin_kind: "agent",
      source_status: "confirmed",
      depends_on: ["t1"],
      wave: 2,
      acceptance_criteria: ["t3 depends only on t1"],
      covers: ["c3", "h3"],
    },
    {
      task_ref: "t4",
      summary: "Depends on both t1 and t2",
      instruction: "Combine t1 and t3's output",
      origin_kind: "human",
      source_status: "confirmed",
      depends_on: ["t1", "t2"],
      wave: 2,
      acceptance_criteria: ["t4 depends on t1 and t2"],
      covers: ["c4", "h4"],
    },
    {
      task_ref: "t5",
      summary: "Scoped out during planning",
      instruction: "This task gets rejected",
      origin_kind: "agent",
      source_status: "rejected",
      depends_on: [],
      acceptance_criteria: ["t5 never ships"],
      covers: ["c5", "h5"],
    },
  ],
  deviations: [
    {
      deviation_ref: "d1",
      what: "Swapped t3's approach after a live capability check",
      task_ref: "t3",
      reason:
        "verified against the installed toolchain; the original approach was infeasible",
      affects: ["t3", "c4"],
      origin_kind: "human",
      source_status: "approved",
      classification: "acceptable",
    },
    {
      deviation_ref: "d2",
      what: "Propose folding t4 into t1's scope",
      task_ref: "t4",
      reason: "found while scoping t4; t1 already produces most of what t4 needs",
      affects: ["t4", "t1"],
      origin_kind: "agent",
      source_status: "proposed",
      classification: "needs-follow-up",
    },
    {
      deviation_ref: "d3",
      what: "Considered dropping t2 entirely",
      task_ref: "t2",
      reason: "explored during planning; ultimately t2's output is still needed",
      affects: [],
      origin_kind: "agent",
      source_status: "rejected",
    },
  ],
};

/** An earlier snapshot of the same plan slug — proves "most recent first". */
export const PLAN_IMPORT_OLDER: PlanImport = {
  ...PLAN_IMPORT,
  id: "pi-older",
  imported_at: "2026-08-13T09:00:00Z",
  tasks: PLAN_IMPORT.tasks.slice(0, 2),
  deviations: [],
};

/** `GET /v1alpha1/plan-imports?slug=economy-discord-graphs`'s body. */
export const PLAN_IMPORT_SUMMARIES: PlanImportSummary[] = [
  PLAN_IMPORT,
  PLAN_IMPORT_OLDER,
].map((pi) => ({
  id: pi.id,
  slug: pi.slug,
  title: pi.title,
  source_slug: pi.source_slug,
  source_status: pi.source_status,
  source_digest: pi.source_digest,
  imported_at: pi.imported_at,
}));

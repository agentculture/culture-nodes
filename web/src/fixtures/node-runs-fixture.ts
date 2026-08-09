/**
 * Fixture data for the jobs timeline (`/jobs`, task t15): two pages of
 * `GET /v1alpha1/node-runs` (task t11), newest-first by `updated_at`, used
 * by both the vitest component/route tests and the Playwright e2e spec via
 * request interception (e2e/fixtures/api.ts).
 *
 * Deliberately covers a spread of `NodeRunState` values (ready, running,
 * waiting_external, failed, completed, cancelled) plus one row with no
 * `actor_id` and one with no `outcome`, so the table's "—" fallbacks are
 * exercised without a bespoke per-test fixture.
 *
 * Plain TypeScript, like run-fixture.ts, so both the app's tsconfig and the
 * Playwright/node one compile it.
 */

import type { NodeRunListItem } from "../api/types";

const t = (minute: number, second = 0) =>
  `2026-08-09T09:${String(minute).padStart(2, "0")}:${String(second).padStart(2, "0")}Z`;

/** The first (newest) page — what a plain `GET /v1alpha1/node-runs` returns. */
export const JOB_RUNS_PAGE_1: NodeRunListItem[] = [
  {
    id: "nr-jobs-01J8XKJOBS000004",
    run_id: "run-jobs-01J8XKJOBS0000004",
    node_id: "build",
    actor_id: "actor://company/developer",
    state: "running",
    created_at: t(58),
    updated_at: t(59),
  },
  {
    id: "nr-jobs-01J8XKJOBS000003",
    run_id: "run-jobs-01J8XKJOBS0000003",
    node_id: "human-review",
    // No actor yet — nobody has picked up this approval wait.
    state: "waiting_external",
    created_at: t(50),
    updated_at: t(52),
  },
  {
    id: "nr-jobs-01J8XKJOBS000002",
    run_id: "run-jobs-01J8XKJOBS0000002",
    node_id: "test",
    actor_id: "runner://headspace/docker",
    state: "failed",
    outcome: "failed",
    created_at: t(30),
    updated_at: t(36),
    completed_at: t(36),
  },
  {
    id: "nr-jobs-01J8XKJOBS000001",
    run_id: "run-jobs-01J8XKJOBS0000001",
    node_id: "verify",
    actor_id: "actor://company/verifier",
    state: "completed",
    outcome: "changes_required",
    created_at: t(10),
    updated_at: t(20),
    completed_at: t(20),
  },
];

/** The opaque cursor `next_cursor` carries after page 1. */
export const JOB_RUNS_CURSOR = "cursor-jobs-page-2";

/** The second (older) page, fetched by replaying `next_cursor` as `cursor`. */
export const JOB_RUNS_PAGE_2: NodeRunListItem[] = [
  {
    id: "nr-jobs-01J8XKJOBS000000B",
    run_id: "run-jobs-01J8XKJOBS000000B",
    node_id: "plan",
    actor_id: "actor://company/planner",
    state: "ready",
    created_at: t(2),
    updated_at: t(2),
  },
  {
    id: "nr-jobs-01J8XKJOBS000000A",
    run_id: "run-jobs-01J8XKJOBS000000A",
    node_id: "intake",
    actor_id: "actor://company/intake",
    state: "cancelled",
    created_at: t(0),
    updated_at: t(1),
    completed_at: t(1),
  },
];

/** Every job across both pages, for assertions that don't care about paging. */
export const JOB_RUNS_ALL: NodeRunListItem[] = [
  ...JOB_RUNS_PAGE_1,
  ...JOB_RUNS_PAGE_2,
];

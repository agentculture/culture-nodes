/**
 * Fixture data for the jobs timeline (`/jobs`, task t15): two pages of
 * `GET /v1alpha1/node-runs` (task t11), newest-first by `updated_at`, used
 * by both the vitest component/route tests and the Playwright e2e spec via
 * request interception (e2e/fixtures/api.ts).
 *
 * Deliberately covers a spread of `NodeRunState` values (ready, running,
 * waiting_external, failed, completed, cancelled) plus one row with no
 * `actor_id` and one with no `outcome`, so the table's "—" fallbacks are
 * exercised without a bespoke per-test fixture. Task t5 adds `usage` (now
 * required on every `NodeRunListItem`, per-t2) — deliberately spread across
 * "not reported" (attempts_reported: 0), reported-with-no-cost, and
 * reported-with-cost, so the honesty-condition rendering (never "0 tokens"
 * for absent usage) has real rows to exercise.
 *
 * Plain TypeScript, like run-fixture.ts, so both the app's tsconfig and the
 * Playwright/node one compile it.
 */

import type { NodeRunListItem, Run, Usage } from "../api/types";

const t = (minute: number, second = 0) =>
  `2026-08-09T09:${String(minute).padStart(2, "0")}:${String(second).padStart(2, "0")}Z`;

const USAGE_NOT_REPORTED: Usage = {
  input_tokens: 0,
  output_tokens: 0,
  cached_input_tokens: 0,
  reasoning_tokens: 0,
  attempts_reported: 0,
  attempts_not_reported: 1,
};

const USAGE_NO_COST: Usage = {
  input_tokens: 1450,
  output_tokens: 620,
  cached_input_tokens: 0,
  reasoning_tokens: 0,
  attempts_reported: 1,
  attempts_not_reported: 0,
};

const USAGE_WITH_COST: Usage = {
  input_tokens: 12300,
  output_tokens: 4100,
  cached_input_tokens: 9600,
  reasoning_tokens: 300,
  cost: 0.42,
  currency: "USD",
  attempts_reported: 1,
  attempts_not_reported: 0,
};

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
    usage: USAGE_NOT_REPORTED,
  },
  {
    id: "nr-jobs-01J8XKJOBS000003",
    run_id: "run-jobs-01J8XKJOBS0000003",
    node_id: "human-review",
    // No actor yet — nobody has picked up this approval wait.
    state: "waiting_external",
    created_at: t(50),
    updated_at: t(52),
    usage: USAGE_NOT_REPORTED,
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
    usage: USAGE_NO_COST,
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
    usage: USAGE_WITH_COST,
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
    usage: USAGE_NOT_REPORTED,
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
    usage: USAGE_NOT_REPORTED,
  },
];

/** Every job across both pages, for assertions that don't care about paging. */
export const JOB_RUNS_ALL: NodeRunListItem[] = [
  ...JOB_RUNS_PAGE_1,
  ...JOB_RUNS_PAGE_2,
];

/**
 * The runs two of the JOB_RUNS_* rows' `run_id` belong to (task t5): `GET
 * /v1alpha1/node-runs` carries no run name/category of its own, so
 * JobsTimeline separately fetches `GET /v1alpha1/runs` for the same window
 * and joins by id — this is that side's fixture. Deliberately keyed to
 * `JOB_RUNS_PAGE_1[1]` and `JOB_RUNS_PAGE_2[0]` (not `[0]`/`[2]`, which the
 * jobs-timeline e2e spec's pre-existing tests already reference by their
 * *bare* run id — this fixture must not turn those into name/hint text
 * out from under them) so the fallback-to-run_id path stays exercised for
 * every row this doesn't name.
 */
export const JOB_RUNS_NAMED_RUNS: Run[] = [
  {
    id: "run-jobs-01J8XKJOBS0000003",
    workflow_digest: "sha256:jobs-fixture",
    state: "waiting",
    name: "nightly regression sweep",
    category: "ci",
    created_at: t(50),
    updated_at: t(52),
  },
  {
    id: "run-jobs-01J8XKJOBS000000B",
    workflow_digest: "sha256:jobs-fixture",
    state: "created",
    display_hint: "fix the flaky pytest-report parser",
    created_at: t(2),
    updated_at: t(2),
  },
];

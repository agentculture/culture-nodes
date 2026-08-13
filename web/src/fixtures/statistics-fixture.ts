/**
 * Fixture data for the Statistics view (`/stats`, task t6), used by both the
 * vitest route test and the Playwright e2e spec (via
 * e2e/fixtures/api.ts's mockStatisticsApi).
 *
 * Deliberately shaped to exercise every honesty/aggregation rule in one
 * small, hand-checkable set:
 *
 * - run-a is split across TWO node runs (nr-stat-a1/a2) to prove the view
 *   groups by run_id before averaging, not by node run.
 * - run-e reports no usage at all (attempts_reported: 0) — the excluded
 *   count this fixture proves is 1, never folded into the token/cost
 *   average as a zero.
 * - run-d carries no category — the uncategorized bucket.
 * - run-a/run-b/run-e share category "ci" (run-e still counts toward "ci"'s
 *   total-runs, but not its reported-runs or its average), run-c is
 *   "review" alone.
 * - Every reported run's cost is USD — the single-currency case; a mixed
 *   currency scenario is exercised in domain/stats.test.ts directly rather
 *   than duplicated here.
 *
 * Reported totals: input 7500 (1000+2000+3000+1500), output 3750
 * (500+1000+1500+750), cost $15.00 USD (2+4+6+3). avg input 1875, median
 * input 1750 (sorted [1000,1500,2000,3000] -> (1500+2000)/2). avg output
 * 937.5, median output 875. avg cost 3.75, median cost 3.5.
 *
 * Cache telemetry (task t2, ADR 0009): only nr-stat-a1 (200 of 400 input)
 * and nr-stat-b1 (800 of 2000 input) report any cached_input_tokens — every
 * other reporting node run's contract exposes none, contributing nothing
 * (never a fabricated zero standing in for "unmeasurable"). Total cached
 * 1000 / total input 7500 = cache_ratio ~0.1333 ("13.3% cached").
 */

import type { NodeRunListItem, Run, Usage } from "../api/types";

const t = (minute: number, second = 0) =>
  `2026-08-10T10:${String(minute).padStart(2, "0")}:${String(second).padStart(2, "0")}Z`;

function reportedUsage(
  input: number,
  output: number,
  cost: number,
  cachedInput = 0,
  reasoningTokens = 0,
): Usage {
  return {
    input_tokens: input,
    output_tokens: output,
    cached_input_tokens: cachedInput,
    reasoning_tokens: reasoningTokens,
    cost,
    currency: "USD",
    attempts_reported: 1,
    attempts_not_reported: 0,
  };
}

const USAGE_NOT_REPORTED: Usage = {
  input_tokens: 0,
  output_tokens: 0,
  cached_input_tokens: 0,
  reasoning_tokens: 0,
  attempts_reported: 0,
  attempts_not_reported: 1,
};

export const STATS_WORKFLOW_DIGEST = "sha256:stats-fixture";

export const STATS_RUN_A = "run-stat-01J8XKSTAT0000A";
export const STATS_RUN_B = "run-stat-01J8XKSTAT0000B";
export const STATS_RUN_C = "run-stat-01J8XKSTAT0000C";
export const STATS_RUN_D = "run-stat-01J8XKSTAT0000D";
export const STATS_RUN_E = "run-stat-01J8XKSTAT0000E";

/** `GET /v1alpha1/runs` for the window — joined by `run_id` for category (task t5). */
export const STATS_RUNS: Run[] = [
  {
    id: STATS_RUN_A,
    workflow_digest: STATS_WORKFLOW_DIGEST,
    state: "completed",
    category: "ci",
    name: "nightly build",
    created_at: t(0),
    updated_at: t(5),
  },
  {
    id: STATS_RUN_B,
    workflow_digest: STATS_WORKFLOW_DIGEST,
    state: "completed",
    category: "ci",
    name: "release build",
    created_at: t(6),
    updated_at: t(10),
  },
  {
    id: STATS_RUN_C,
    workflow_digest: STATS_WORKFLOW_DIGEST,
    state: "completed",
    category: "review",
    name: "PR review sweep",
    created_at: t(11),
    updated_at: t(15),
  },
  {
    id: STATS_RUN_D,
    workflow_digest: STATS_WORKFLOW_DIGEST,
    state: "completed",
    name: "one-off task",
    created_at: t(16),
    updated_at: t(20),
  },
  {
    id: STATS_RUN_E,
    workflow_digest: STATS_WORKFLOW_DIGEST,
    state: "failed",
    category: "ci",
    name: "flaky retry",
    created_at: t(21),
    updated_at: t(25),
  },
];

/** First page of `GET /v1alpha1/node-runs`: run-a split across two rows, plus run-b. */
export const STATS_NODE_RUNS_PAGE_1: NodeRunListItem[] = [
  {
    id: "nr-stat-a1",
    run_id: STATS_RUN_A,
    node_id: "build",
    state: "completed",
    outcome: "succeeded",
    created_at: t(0),
    updated_at: t(2),
    completed_at: t(2),
    usage: reportedUsage(400, 200, 0.8, 200, 50),
  },
  {
    id: "nr-stat-a2",
    run_id: STATS_RUN_A,
    node_id: "test",
    state: "completed",
    outcome: "succeeded",
    created_at: t(2),
    updated_at: t(5),
    completed_at: t(5),
    usage: reportedUsage(600, 300, 1.2),
  },
  {
    id: "nr-stat-b1",
    run_id: STATS_RUN_B,
    node_id: "build",
    state: "completed",
    outcome: "succeeded",
    created_at: t(6),
    updated_at: t(10),
    completed_at: t(10),
    usage: reportedUsage(2000, 1000, 4, 800, 100),
  },
];

/** The opaque cursor `next_cursor` carries after page 1. */
export const STATS_CURSOR = "cursor-stats-page-2";

/** Second page: run-c, run-d, and run-e (unreported). */
export const STATS_NODE_RUNS_PAGE_2: NodeRunListItem[] = [
  {
    id: "nr-stat-c1",
    run_id: STATS_RUN_C,
    node_id: "review",
    state: "completed",
    outcome: "succeeded",
    created_at: t(11),
    updated_at: t(15),
    completed_at: t(15),
    usage: reportedUsage(3000, 1500, 6),
  },
  {
    id: "nr-stat-d1",
    run_id: STATS_RUN_D,
    node_id: "task",
    state: "completed",
    outcome: "succeeded",
    created_at: t(16),
    updated_at: t(20),
    completed_at: t(20),
    usage: reportedUsage(1500, 750, 3),
  },
  {
    id: "nr-stat-e1",
    run_id: STATS_RUN_E,
    node_id: "flaky",
    state: "failed",
    outcome: "failed",
    created_at: t(21),
    updated_at: t(25),
    completed_at: t(25),
    usage: USAGE_NOT_REPORTED,
  },
];

export const STATS_NODE_RUNS_ALL: NodeRunListItem[] = [
  ...STATS_NODE_RUNS_PAGE_1,
  ...STATS_NODE_RUNS_PAGE_2,
];

/**
 * A smaller, distinctly different dataset served whenever the request
 * carries a time bound (task t6 e2e: "the time filter changing the
 * aggregate") — just run-c's node run, proving the aggregate genuinely
 * changes rather than the view silently re-showing the unfiltered totals.
 * Real date filtering is not simulated (no fixture* here does that — see
 * mockJobsTimelineApi's cursor-only filtering); the point is proving the
 * client requests the bound and re-renders from whatever the server sends.
 */
export const STATS_NODE_RUNS_FILTERED: NodeRunListItem[] = [
  STATS_NODE_RUNS_PAGE_2[0],
];

export const STATS_RUNS_FILTERED: Run[] = [STATS_RUNS[2]];

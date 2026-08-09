/**
 * Fixture runs for the runs board (`/board`, task t14): one run per
 * `RunState` column (openapi.yaml's `created, running, waiting, completed,
 * failed, cancelled`), so both the vitest grouping tests and the Playwright
 * board spec can assert every column renders from the same data.
 *
 * `run-waiting-approval` models the "paused at an approval node" case the
 * acceptance criteria calls out explicitly: the run list endpoint reports
 * only `state: "waiting"` — there is no separate "approval_paused" value in
 * the API (api/openapi/openapi.yaml's RunState enum) — so this run is
 * indistinguishable, at this layer, from any other external wait. The board
 * must not invent a distinction the API doesn't make; it must simply place
 * this run under "waiting" like every other one. `input.note` only documents
 * the *scenario* the fixture stands in for — the board never reads `input`.
 *
 * Plain TypeScript, like run-fixture.ts, so both the app's tsconfig and the
 * Playwright/node one compile it.
 */

import type { Run } from "../api/types";
import { WORKFLOW_DIGEST } from "./run-fixture";

const t = (minute: number) =>
  `2026-08-09T09:${String(minute).padStart(2, "0")}:00Z`;

export const BOARD_RUNS: Run[] = [
  {
    id: "run-created-01J8XKBOARD0000000001",
    workflow_digest: WORKFLOW_DIGEST,
    state: "created",
    created_at: t(50),
    updated_at: t(50),
  },
  {
    id: "run-running-01J8XKBOARD0000000002",
    workflow_digest: WORKFLOW_DIGEST,
    state: "running",
    input: { title: "add the ledger projection endpoint" },
    created_at: t(30),
    updated_at: t(55),
  },
  {
    id: "run-waiting-approval-01J8XKBOARD003",
    workflow_digest: WORKFLOW_DIGEST,
    state: "waiting",
    input: { note: "paused at human-review, awaiting approval" },
    created_at: t(10),
    updated_at: t(52),
  },
  {
    id: "run-completed-01J8XKBOARD0000000004",
    workflow_digest: WORKFLOW_DIGEST,
    state: "completed",
    created_at: t(0),
    updated_at: t(40),
    completed_at: t(40),
  },
  {
    id: "run-failed-01J8XKBOARD0000000005",
    workflow_digest: WORKFLOW_DIGEST,
    state: "failed",
    created_at: t(5),
    updated_at: t(38),
    completed_at: t(38),
  },
  {
    id: "run-cancelled-01J8XKBOARD0000000006",
    workflow_digest: WORKFLOW_DIGEST,
    state: "cancelled",
    created_at: t(2),
    updated_at: t(20),
    completed_at: t(20),
  },
];

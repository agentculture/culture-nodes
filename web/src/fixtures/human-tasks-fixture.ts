/**
 * Fixture data for the Inbox view (`/inbox`, task t14): one pending approval
 * task carrying the full PRD §9.9 request payload (decision schema ref,
 * approver ref, deadline, allowed outcomes, context refs, audit block), one
 * pending task with the minimal payload the engine can legally write (no
 * deadline, no context refs — the honesty case: absent fields must render
 * as absent, never fabricated), and one decided task whose resolution the
 * inbox shows read-only.
 *
 * Plain TypeScript, like run-fixture.ts, so both the app's tsconfig and the
 * Playwright/node one compile it.
 */

import type { HumanTask, HumanTaskDecisionResult } from "../api/types";

export const PENDING_TASK: HumanTask = {
  id: "ht-01J8XKINBOX0000000000000001",
  run_id: "run-01J8XKINBOXRUN000000000001",
  node_run_id: "nr-01J8XKINBOXNR0000000000001",
  kind: "approval",
  assigned_owner_id: "team/platform-ai-approvers",
  status: "pending",
  request: {
    decision_schema_ref: "schemas/decisions/release-signoff.json",
    approver_ref: "team/platform-ai-approvers",
    deadline: "2026-08-20T12:00:00Z",
    allowed_outcomes: ["approved", "changes_required", "rejected"],
    context_refs: {
      from: "nodes.build.output",
      bindings: {
        diff: "nodes.build.output.diff",
        // A literal binding (issue #73): the declaration lives in the
        // workflow text, so the task shows the value rather than a pointer.
        observe: { literal: { kind: "github_pr_merged", pr: 42 } },
      },
    },
    audit: {
      node_id: "release-signoff",
      token_id: "tok-01J8XKINBOXTOK000000000001",
      workflow_digest:
        "sha256:1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff",
      from_node: "build",
      from_outcome: "succeeded",
    },
  },
  created_at: "2026-08-13T09:00:00Z",
};

/** The minimal legal payload: no deadline, no context refs, no schema ref. */
export const PENDING_TASK_MINIMAL: HumanTask = {
  id: "ht-01J8XKINBOX0000000000000002",
  run_id: "run-01J8XKINBOXRUN000000000002",
  kind: "approval",
  status: "pending",
  request: {
    allowed_outcomes: ["approved", "rejected"],
    audit: { node_id: "gate" },
  },
  created_at: "2026-08-13T10:00:00Z",
};

export const DECIDED_TASK: HumanTask = {
  id: "ht-01J8XKINBOX0000000000000003",
  run_id: "run-01J8XKINBOXRUN000000000003",
  node_run_id: "nr-01J8XKINBOXNR0000000000003",
  kind: "approval",
  status: "decided",
  request: {
    allowed_outcomes: ["approved", "rejected"],
    audit: { node_id: "review-gate" },
  },
  response: { note: "looks right", outcome_reason: "diff matches the spec" },
  created_at: "2026-08-12T08:00:00Z",
  resolved_at: "2026-08-12T09:30:00Z",
};

/** The ledger version the run currently reports (the stale-guard read). */
export const LEDGER_VERSION = 7;

export const DECISION_RESULT: HumanTaskDecisionResult = {
  human_task_id: PENDING_TASK.id,
  run_id: PENDING_TASK.run_id,
  node_run_id: PENDING_TASK.node_run_id!,
  outcome: "approved",
  ledger_records: [],
  next_node_id: "deploy",
  run_state: "running",
};

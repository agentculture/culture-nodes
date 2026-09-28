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
    // Far in the future on purpose (task t44): a pending task whose deadline
    // has passed moves to the Inbox's Waiting tab, and the Playwright specs
    // run on the real clock.
    deadline: "2099-08-20T12:00:00Z",
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

/**
 * Task t44: the clock the Inbox tab tests render with, and one task per tab
 * edge — pending past its deadline (Waiting), pending with a deadline that
 * does not parse (treated as no deadline: Open), and one the engine expired
 * (Decided, never a human decision).
 */
export const INBOX_NOW = new Date("2026-08-15T00:00:00Z");

export const WAITING_TASK: HumanTask = {
  id: "ht-01J8XKINBOX0000000000000004",
  run_id: "run-01J8XKINBOXRUN000000000004",
  kind: "approval",
  status: "pending",
  request: {
    allowed_outcomes: ["approved", "rejected"],
    // Twelve hours before INBOX_NOW.
    deadline: "2026-08-14T12:00:00Z",
    audit: { node_id: "stale-gate" },
  },
  created_at: "2026-08-10T08:00:00Z",
};

export const BAD_DEADLINE_TASK: HumanTask = {
  id: "ht-01J8XKINBOX0000000000000005",
  run_id: "run-01J8XKINBOXRUN000000000005",
  kind: "approval",
  status: "pending",
  request: {
    allowed_outcomes: ["approved", "rejected"],
    deadline: "not-a-timestamp",
    audit: { node_id: "odd-gate" },
  },
  created_at: "2026-08-11T08:00:00Z",
};

export const EXPIRED_TASK: HumanTask = {
  id: "ht-01J8XKINBOX0000000000000006",
  run_id: "run-01J8XKINBOXRUN000000000006",
  kind: "approval",
  status: "expired",
  request: {
    allowed_outcomes: ["approved", "rejected", "expired"],
    audit: { node_id: "human-merges-pr" },
  },
  response: { reason: "pr_merged" },
  created_at: "2026-08-12T10:00:00Z",
  // Resolved after DECIDED_TASK, so it sorts first on the Decided tab.
  resolved_at: "2026-08-13T11:00:00Z",
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

/**
 * Issue #332: the two real task shapes, with the API's resolved context.
 * `human-merges-pr` (examples/pr-upkeep/workflow.yaml) binds the fix node's
 * output and the run input; the declaration `human.ask` blocked-step task
 * (examples/pr-upkeep/declarations/blocked-stamp-pr.json) reads the run input.
 */
export const MERGE_HEAD_SHA = "e128a17c0ffee0000000000000000000000000ab";

export const MERGE_TASK: HumanTask = {
  id: "ht-01M278CXQZM6M6F0J7H1WHT0FV",
  run_id: "run-01M278MERGE00000000000001",
  kind: "approval",
  status: "pending",
  request: {
    approver_ref: "group/platform-maintainers",
    decision_schema_ref: "schema://pr-upkeep/merge-decision/v1",
    allowed_outcomes: ["approved", "rejected", "expired"],
    context_refs: {
      bindings: {
        finding: "/run/input",
        fix: "/nodes/fix/output",
        readiness: "/nodes/readiness/output",
      },
    },
    audit: {
      node_id: "human-merges-pr",
      token_id: "tok-01M278MERGE000000000001",
      workflow_digest:
        "sha256:2222333344445555666677778888999900001111aaaabbbbccccddddeeeeffff",
      from_node: "readiness",
      from_outcome: "passed",
    },
  },
  resolved_context: [
    {
      name: "finding",
      ref: "/run/input",
      value: {
        number: 326,
        source: "github_pr",
        repository: "agentculture/culture-nodes",
        head_sha: MERGE_HEAD_SHA,
        findings: [
          {
            id: "AZ1",
            file: "tests/test_hand_turn_cli.py",
            line: 141,
            rule: "python:S9073",
            kind: "CODE_SMELL",
            severity: "MAJOR",
            source: "sonarcloud",
            title: "Split this composite assertion into separate assertions.",
          },
        ],
      },
    },
    {
      name: "fix",
      ref: "/nodes/fix/output",
      value: {
        summary:
          "**Finding taken:** python:S9073 at tests/test_hand_turn_cli.py:141\n\n**Verdict:** real defect, fixed",
      },
    },
    {
      name: "readiness",
      ref: "/nodes/readiness/output",
      unresolved:
        'node "readiness" has no succeeded attempt in this run, so it has no output',
    },
  ],
  created_at: "2026-09-28T08:00:00Z",
};

export const BLOCKED_TASK: HumanTask = {
  id: "ht-01M278BLOCKED000000000001",
  run_id: "run-01M278BLOCKED0000000001",
  kind: "approval",
  status: "pending",
  firing: { declaration_name: "pr-upkeep-blocked-stamp-pr" },
  request: {
    approver_ref: "group/platform-maintainers",
    allowed_outcomes: ["approved", "rejected"],
    context_refs: { from: "/run/input" },
    audit: { node_id: "blocked-stamp-pr" },
  },
  resolved_context: [
    {
      name: "from",
      ref: "/run/input",
      value: {
        question:
          "The pr-upkeep stamp-pr agent could not complete its task. Review the reason and report.",
        blocked_step: "stamp-pr",
        ticket: "SCRUM-15",
        number: "329",
        repository: "agentculture/culture-nodes",
        // A template: `#` + three digits in a plain string reads as a hex colour
        // to tests/lint/webtokens_test.go.
        work_item: `gh:agentculture/culture-nodes#${329}`,
        agent_report: {
          reason: "The PR has no push credential for the stamp commit.",
          pr: 329,
          evidence: ["git push: 403 Forbidden"],
        },
        marker: "m1",
      },
    },
  ],
  created_at: "2026-09-28T09:00:00Z",
};

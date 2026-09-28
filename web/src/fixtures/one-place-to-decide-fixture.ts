/**
 * Fixtures for the one decision page (task t46, owner decision d19): a
 * blocked-agent task whose `agent_report` arrives as a STRING containing
 * JSON — declaration templates render every value as a string — offering
 * t47's outcomes; a decided task with a note; and a committed review.
 */
import type { ReviewedRecord } from "../api/decisionTypes";
import type { HumanTask } from "../api/types";
import { CLAIM_RUN_ID, PENDING_RUN } from "./pending-decisions-fixture";

// A template: `#` + three digits in a plain string reads as a hex colour to
// tests/lint/webtokens_test.go.
const PR = `agentculture/culture-nodes#${329}`;

/** The shape of the real report the owner pasted (task t46). */
export const AGENT_REPORT_OBJECT = {
  changes_made: "none — the PR was not edited",
  intended_line: "Jira: SCRUM-16",
  pr: PR,
  reason: "No usable GitHub credential was granted to the worker.",
  remediation: "Refresh/reissue the GITHUB_TOKEN_WORKER PAT and re-grant it.",
  summary: "I could not edit the PR.\n\n**Evidence**\n- git push: 403",
  evidence: ["gh auth status: not logged in"],
  workspace_measured: { head: "abc1234", dirty: false, files_changed: 0 },
  new_future_key: "kept for the audit",
};

export const REPORT_TASK: HumanTask = {
  id: "ht-01M46REPORT00000000000001",
  run_id: "run-01M46REPORT000000000001",
  kind: "approval",
  status: "pending",
  firing: { declaration_name: "pr-upkeep-blocked-edit-pr" },
  request: {
    allowed_outcomes: ["retry", "abandon", "acknowledged"],
    context_refs: { from: "/run/input" },
    audit: { node_id: "blocked-edit-pr" },
  },
  allowed_outcomes: ["retry", "abandon", "acknowledged"],
  resolved_context: [
    {
      name: "from",
      ref: "/run/input",
      value: {
        question: "The edit-pr agent is blocked. Retry, abandon, or acknowledge.",
        blocked_step: "edit-pr",
        agent_report: JSON.stringify(AGENT_REPORT_OBJECT),
      },
    },
  ],
  created_at: "2026-09-28T10:00:00Z",
};

export const NOTED_DECIDED_TASK: HumanTask = {
  id: "ht-01M46NOTED000000000000001",
  run_id: "run-01M46NOTED0000000000001",
  kind: "approval",
  status: "decided",
  request: { allowed_outcomes: ["approved", "rejected"], audit: { node_id: "review-gate" } },
  response: {
    outcome: "approved",
    decider_actor_id: "actor-human-ori",
    note: "read the diff; the risky part is flagged off",
    decided_at: "2026-09-28T11:00:00Z",
  },
  note: "read the diff; the risky part is flagged off",
  created_at: "2026-09-28T09:00:00Z",
  resolved_at: "2026-09-28T11:00:00Z",
};

/** A committed review, newer than NOTED_DECIDED_TASK. */
export const REVIEWED_RECORD: ReviewedRecord = {
  review_record_id: "rec-01M46REVIEW0000000000001",
  run_id: CLAIM_RUN_ID,
  reviewer_actor_id: "actor-human-ori",
  verdict: "reject",
  rationale: "the evidence is process-reported, not measured",
  reviewed_at: "2026-09-28T12:00:00Z",
  record: PENDING_RUN.records[0],
};

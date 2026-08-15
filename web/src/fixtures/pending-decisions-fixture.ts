/**
 * Fixture data for the Decisions view (`/decisions`, task t30 / issue #99):
 * two runs' worth of proposed ledger records awaiting a human decision.
 *
 * The shapes mirror what `GET /v1alpha1/pending-decisions` returns — records
 * grouped by run, each group carrying the ledger version the review must be
 * opened against. One record carries the qualifying half of its claim in the
 * payload, because that is what a decider actually has to read; a view that
 * summarised it away would be asking for a decision on something nobody saw.
 *
 * Plain TypeScript, like run-fixture.ts, so both the app's tsconfig and the
 * Playwright/node one compile it.
 */

import type {
  PendingDecisionList,
  PendingDecisionRun,
  ReviewCommitResult,
  ReviewRequest,
} from "../api/types";

export const CLAIM_RUN_ID = "run-01J8XKDECIDE0000000000001";
export const CLAIM_LEDGER_VERSION = 7;

export const PENDING_RUN: PendingDecisionRun = {
  run_id: CLAIM_RUN_ID,
  ledger_version: CLAIM_LEDGER_VERSION,
  records: [
    {
      id: "rec-01J8XKDECIDEREC000000000001",
      record_type: "claim",
      origin_kind: "agent",
      origin_actor_id: "codex-thor",
      node_run_id: "nr-01J8XKDECIDENR00000000001",
      created_at: "2026-08-15T09:14:00Z",
      data: {
        kind: "completion",
        statement:
          "Added the Go job to tests.yml and watched it go red on a deleted assertion — but I could not run the suite locally, so the red is CI's, not mine.",
      },
    },
    {
      id: "rec-01J8XKDECIDEREC000000000002",
      record_type: "evidence",
      origin_kind: "agent",
      origin_actor_id: "codex-thor",
      created_at: "2026-08-15T09:15:00Z",
      data: { collection_method: "process_reported", command: "go test ./..." },
    },
  ],
};

export const SECOND_PENDING_RUN: PendingDecisionRun = {
  run_id: "run-01J8XKDECIDE0000000000002",
  ledger_version: 3,
  records: [
    {
      id: "rec-01J8XKDECIDEREC000000000003",
      record_type: "claim",
      origin_kind: "agent",
      origin_actor_id: "codex-orin",
      created_at: "2026-08-15T10:02:00Z",
      data: { kind: "completion", statement: "Probed the toolchain; uv is a snap here." },
    },
  ],
};

export const PENDING_DECISIONS: PendingDecisionList = {
  items: [PENDING_RUN, SECOND_PENDING_RUN],
  record_count: 3,
};

export const REVIEW_REQUEST: ReviewRequest = {
  id: "review-01J8XKDECIDEREV000000001",
  run_id: CLAIM_RUN_ID,
  reviewer_actor_id: "actor-human-ori",
  status: "requested",
  ledger_version: CLAIM_LEDGER_VERSION,
  frame_checksum: "sha256:aa11bb22",
  record_ids: PENDING_RUN.records.map((record) => record.id),
  created_at: "2026-08-15T11:00:00Z",
};

export const COMMIT_RESULT: ReviewCommitResult = {
  review_id: REVIEW_REQUEST.id,
  ledger_version: CLAIM_LEDGER_VERSION + 2,
  records: [
    {
      id: "rec-01J8XKDECIDEREC000000000009",
      schema_version: "1",
      record_type: "review",
      run_id: CLAIM_RUN_ID,
      origin: { kind: "human", actor_id: "actor-human-ori" },
      authority: "confirmed",
      subject_ref: PENDING_RUN.records[0].id,
      data: {
        verdict: "confirm",
        rationale: "re-ran the suite on spark",
        reviewed_refs: [PENDING_RUN.records[0].id],
      },
      provenance_refs: [PENDING_RUN.records[0].id],
      content_digest: "sha256:cc33dd44",
      created_at: "2026-08-15T11:01:00Z",
    },
  ],
};

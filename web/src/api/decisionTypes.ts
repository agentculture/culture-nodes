import type { LedgerRecord, PendingDecisionRecord } from "./types";

/**
 * One review record and the record it decided (components.schemas.
 * ReviewedRecord, task t46): the review half of the Inbox's Decided tab.
 *
 * This file holds the decision surface's wire types beside types.ts rather
 * than in it: types.ts sits at the 1000-line source limit.
 */
export interface ReviewedRecord {
  review_record_id: string;
  run_id: string;
  reviewer_actor_id?: string;
  /** `confirm` or `reject`. */
  verdict: string;
  /** The reviewer's stated reason; absent when none was recorded. */
  rationale?: string;
  reviewed_at: string;
  record: PendingDecisionRecord;
}

export interface ReviewedRecordList {
  items: ReviewedRecord[];
  next_cursor?: string;
}

/** `POST /v1alpha1/human-tasks/{id}/decision` request body. */
export interface HumanTaskDecisionRequest {
  outcome: string;
  decider_actor_id: string;
  response?: unknown;
  /**
   * The run's ledger version the decider last read (`GET
   * /runs/{id}/ledger`'s `ledger_version`). A stale expectation is refused
   * atomically with nothing written — same guard as PRD §10.8 review
   * commits.
   */
  expected_ledger_version: number;
  record_ids?: string[];
  /** Optional reason (task t46, ≤2000 chars), sent only when written. */
  note?: string;
}

/** `POST .../decision`'s result (components.schemas.HumanTaskDecisionResult). */
export interface HumanTaskDecisionResult {
  human_task_id: string;
  run_id: string;
  node_run_id: string;
  outcome: string;
  ledger_records: LedgerRecord[];
  next_node_id?: string;
  next_node_run_id?: string;
  next_human_task_id?: string;
  run_state: string;
  run_output?: unknown;
  /** The note recorded with the decision (task t46); absent when none. */
  note?: string;
}

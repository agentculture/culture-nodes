import type {
  PendingDecisionRun,
  TicketProjection,
  TicketReviewsResult,
} from "../api/types";

export const TICKET_ID = "SCRUM-42";
export const TICKET_URL = "https://jira.example.test/browse/SCRUM-42";
/** What a frame authored BEFORE the API composed the link says. The
 *  projection's own `ticket_url` must win over it (task t18). */
export const STALE_FRAME_TICKET_URL = "https://stale.example.test/browse/SCRUM-42";
export const PENDING_TASK_ID = "01HTASKPENDING000000000001";

/**
 * The two runs this ticket owns. A ticket's claims live on runs, and a review
 * is opened against ONE run at ONE ledger version (PRD §10.8), so a ticket
 * page that can decide anything has to be able to decide two runs at two
 * versions in one action (decision c40) — which is why the fixture carries
 * two rather than one.
 */
export const TICKET_RUN_ID = "run-ticket-1";
export const TICKET_RUN_LEDGER_VERSION = 7;
export const SECOND_TICKET_RUN_ID = "run-ticket-2";
export const SECOND_TICKET_RUN_LEDGER_VERSION = 4;

export const TICKET_CLAIM_RECORD_ID = "rec-01JTICKETCLAIM0000000001";
export const TICKET_EVIDENCE_RECORD_ID = "rec-01JTICKETCLAIM0000000002";
export const SECOND_TICKET_CLAIM_RECORD_ID = "rec-01JTICKETCLAIM0000000003";

/**
 * `GET /v1alpha1/tickets/{id}`'s `pending_records` (task t14, spec c11): the
 * ticket's undecided ledger claims, grouped by run and quoted at the
 * `ledger_version` that same response read. The version travels with the
 * records because the review submitted from this page is measured against
 * it — a client that fetched it separately would be attesting to a frame it
 * never showed anyone.
 *
 * The first record carries the qualifying half of its claim, for the same
 * reason pending-decisions-fixture.ts does: a decider who cannot read the
 * qualification is being asked to rubber-stamp.
 */
export const TICKET_PENDING_RECORDS: PendingDecisionRun[] = [
  {
    run_id: TICKET_RUN_ID,
    ledger_version: TICKET_RUN_LEDGER_VERSION,
    records: [
      {
        id: TICKET_CLAIM_RECORD_ID,
        record_type: "claim",
        origin_kind: "agent",
        origin_actor_id: "codex-thor",
        node_run_id: "nr-01JTICKETCLAIM000000001",
        created_at: "2026-08-29T08:40:00Z",
        data: {
          kind: "completion",
          statement:
            "Wired the reply box to the engine and watched a reply land — but the Jira mirror was stubbed, so the board half is unproven.",
        },
      },
      {
        id: TICKET_EVIDENCE_RECORD_ID,
        record_type: "evidence",
        origin_kind: "agent",
        origin_actor_id: "codex-thor",
        created_at: "2026-08-29T08:41:00Z",
        data: { collection_method: "process_reported", command: "go test ./internal/api/..." },
      },
    ],
  },
  {
    run_id: SECOND_TICKET_RUN_ID,
    ledger_version: SECOND_TICKET_RUN_LEDGER_VERSION,
    records: [
      {
        id: SECOND_TICKET_CLAIM_RECORD_ID,
        record_type: "claim",
        origin_kind: "agent",
        origin_actor_id: "codex-orin",
        created_at: "2026-08-29T08:45:00Z",
        data: { kind: "completion", statement: "Read the sweep's watermark; nothing to change." },
      },
    ],
  },
];

/**
 * What the batch route answers when one run committed and the other moved
 * under the decider. Partial success is this route's NORMAL answer, not an
 * edge case (decision c40), so the fixture is the partial one.
 */
export const TICKET_REVIEWS_RESULT: TicketReviewsResult = {
  ticket_id: TICKET_ID,
  committed_runs: 1,
  runs: [
    {
      run_id: TICKET_RUN_ID,
      status: "committed",
      review_id: "review-01JTICKETREVIEW00000001",
      ledger_version: TICKET_RUN_LEDGER_VERSION + 2,
    },
    {
      run_id: SECOND_TICKET_RUN_ID,
      status: "conflict",
      message: "the run moved since this page was read: ledger version 5, expected 4",
    },
  ],
};

export const TICKET_PROJECTION: TicketProjection = {
  ticket_id: TICKET_ID,
  ticket_url: TICKET_URL,
  pending_tasks: [
    {
      id: PENDING_TASK_ID,
      run_id: TICKET_RUN_ID,
      kind: "approval",
      allowed_outcomes: ["approved", "rejected"],
      decision_schema_ref: "./contracts/review-decision.schema.json",
      deadline: "2026-08-30T09:00:00Z",
      created_at: "2026-08-29T09:00:00Z",
      ledger_version: TICKET_RUN_LEDGER_VERSION,
    },
  ],
  latest_frame: {
    ticket_id: TICKET_ID,
    version: 3,
    posted_by: "spec-chain",
    created_at: "2026-08-29T09:00:00Z",
    frame: {
      ticket_url: STALE_FRAME_TICKET_URL,
      claims: [
        { id: "c1", text: "The projection is readable", state: "confirmed" },
        { id: "c2", text: "The reply resumes the flow", state: "proposed" },
        { id: "c3", text: "The old route remains available", state: "rejected" },
      ],
      questions: [
        { id: "q1", text: "Ship the keyboard path?", state: "answered", answer: "Yes" },
        { id: "q2", text: "Which actor replies?", state: "open" },
      ],
      decisions: [
        { id: "d1", question_id: "q1", outcome: "yes", text: "Keep the form in document order" },
      ],
    },
  },
  pending_records: TICKET_PENDING_RECORDS,
  runs: [
    {
      id: TICKET_RUN_ID,
      workflow_digest: "sha256:ticket",
      state: "waiting",
      created_at: "2026-08-29T08:00:00Z",
      updated_at: "2026-08-29T09:00:00Z",
    },
    {
      id: SECOND_TICKET_RUN_ID,
      workflow_digest: "sha256:ticket",
      state: "waiting",
      created_at: "2026-08-29T08:05:00Z",
      updated_at: "2026-08-29T09:05:00Z",
    },
  ],
  ledger: [],
  human_tasks: [],
  ticket_reports: [
    { id: "report-1", run_id: TICKET_RUN_ID, phase: "start", status: "delivered", payload: {}, created_at: "2026-08-29T08:01:00Z" },
    { id: "report-2", run_id: TICKET_RUN_ID, phase: "finish", status: "pending", payload: {}, created_at: "2026-08-29T09:01:00Z" },
  ],
  replies: [
    { id: "reply-1", replier: "orin", text: "Yes, use the existing token.", question_id: "q1", created_at: "2026-08-29T08:30:00Z" },
  ],
};

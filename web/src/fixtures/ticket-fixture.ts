import type { TicketProjection } from "../api/types";

export const TICKET_ID = "SCRUM-42";
export const TICKET_URL = "https://jira.example.test/browse/SCRUM-42";

export const TICKET_PROJECTION: TicketProjection = {
  ticket_id: TICKET_ID,
  latest_frame: {
    ticket_id: TICKET_ID,
    version: 3,
    posted_by: "spec-chain",
    created_at: "2026-08-29T09:00:00Z",
    frame: {
      ticket_url: TICKET_URL,
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
  runs: [
    {
      id: "run-ticket-1",
      workflow_digest: "sha256:ticket",
      state: "waiting",
      created_at: "2026-08-29T08:00:00Z",
      updated_at: "2026-08-29T09:00:00Z",
    },
  ],
  ledger: [],
  human_tasks: [],
  ticket_reports: [
    { id: "report-1", run_id: "run-ticket-1", phase: "start", status: "delivered", payload: {}, created_at: "2026-08-29T08:01:00Z" },
    { id: "report-2", run_id: "run-ticket-1", phase: "finish", status: "pending", payload: {}, created_at: "2026-08-29T09:01:00Z" },
  ],
  replies: [
    { id: "reply-1", replier: "orin", text: "Yes, use the existing token.", question_id: "q1", created_at: "2026-08-29T08:30:00Z" },
  ],
};

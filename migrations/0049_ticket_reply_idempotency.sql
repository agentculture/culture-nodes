-- Expand-only. A page reply's fact, its ticket_replies row and its Jira
-- mirror row are now written in ONE transaction, idempotent on the client's
-- reply_id through signal_event_watermarks (PR #244, Qodo finding 1). These
-- two unique keys turn "one fact, one reply row, one mirror row" from a
-- convention the handler follows into a constraint the database enforces.
CREATE UNIQUE INDEX ticket_replies_signal_event_idx
    ON ticket_replies (signal_event_id) WHERE signal_event_id IS NOT NULL;

CREATE UNIQUE INDEX jira_ticket_reply_mirror_idx
    ON jira_ticket_report_outbox ((payload->>'signal_event_id'))
    WHERE phase = 'reply';

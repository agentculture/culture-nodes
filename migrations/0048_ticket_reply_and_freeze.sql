-- Widen the existing Jira bridge outbox for ticket-scoped intents which do
-- not belong to a run lifecycle (page replies and the singleton page link).
ALTER TABLE jira_ticket_report_outbox ALTER COLUMN run_id DROP NOT NULL;
ALTER TABLE jira_ticket_report_outbox ALTER COLUMN trigger_event_id DROP NOT NULL;
ALTER TABLE jira_ticket_report_outbox DROP CONSTRAINT jira_ticket_report_outbox_phase_check;
ALTER TABLE jira_ticket_report_outbox ADD CONSTRAINT jira_ticket_report_outbox_phase_check
    CHECK (phase IN ('start', 'finish', 'reply', 'page-link'));
ALTER TABLE jira_ticket_report_outbox DROP CONSTRAINT jira_ticket_report_outbox_run_id_phase_key;
CREATE UNIQUE INDEX jira_ticket_report_run_phase_idx
    ON jira_ticket_report_outbox (run_id, phase) WHERE run_id IS NOT NULL;
CREATE UNIQUE INDEX jira_ticket_page_link_idx
    ON jira_ticket_report_outbox (namespace_id, issue_key, phase)
    WHERE phase = 'page-link';

CREATE TABLE ticket_freezes (
    namespace_id TEXT NOT NULL REFERENCES namespaces (id),
    ticket_id TEXT NOT NULL,
    merged_pr JSONB,
    frozen_by TEXT NOT NULL,
    signal_event_id TEXT REFERENCES signal_events (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, ticket_id)
);

-- Ticket lifecycle intents introduced by login-from-anywhere task t15.
ALTER TABLE human_tasks ADD COLUMN source_signal_event_id TEXT REFERENCES signal_events(id);
CREATE UNIQUE INDEX human_tasks_source_signal_event_idx
    ON human_tasks (source_signal_event_id) WHERE source_signal_event_id IS NOT NULL;

ALTER TABLE jira_ticket_report_outbox DROP CONSTRAINT jira_ticket_report_outbox_phase_check;
ALTER TABLE jira_ticket_report_outbox ADD CONSTRAINT jira_ticket_report_outbox_phase_check
    CHECK (phase IN ('start', 'finish', 'reply', 'page-link', 'in-review'));

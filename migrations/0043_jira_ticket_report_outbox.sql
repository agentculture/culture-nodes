-- Expand-only provenance and transactional Jira report outbox.
ALTER TABLE signal_events ADD COLUMN source_key TEXT;
ALTER TABLE runs ADD COLUMN trigger_event_id TEXT REFERENCES signal_events (id);

CREATE TABLE jira_ticket_report_outbox (
    id               TEXT PRIMARY KEY,
    namespace_id     TEXT NOT NULL REFERENCES namespaces (id),
    run_id            TEXT NOT NULL REFERENCES runs (id),
    trigger_event_id  TEXT NOT NULL REFERENCES signal_events (id),
    phase             TEXT NOT NULL CHECK (phase IN ('start', 'finish')),
    target_actor_key  TEXT NOT NULL,
    issue_key         TEXT NOT NULL,
    payload           JSONB NOT NULL,
    status            TEXT NOT NULL DEFAULT 'pending',
    attempts          INTEGER NOT NULL DEFAULT 0,
    available_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, phase)
);

CREATE INDEX jira_ticket_report_outbox_pending_idx
    ON jira_ticket_report_outbox (status, available_at) WHERE status = 'pending';

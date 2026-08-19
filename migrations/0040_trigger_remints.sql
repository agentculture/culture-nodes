-- 0040_trigger_remints.sql
-- Durable, bounded re-mint requests for technically failed trigger runs.
CREATE TABLE trigger_remints (
    id                  TEXT PRIMARY KEY,
    namespace_id        TEXT NOT NULL REFERENCES namespaces (id),
    source_run_id       TEXT NOT NULL REFERENCES runs (id),
    original_event_id   TEXT NOT NULL REFERENCES signal_events (id),
    workflow_digest     TEXT NOT NULL,
    subject             TEXT,
    attempt             INTEGER NOT NULL CHECK (attempt > 0),
    window_started_at   TIMESTAMPTZ NOT NULL,
    available_at        TIMESTAMPTZ NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending',
    minted_run_id       TEXT REFERENCES runs (id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT trigger_remints_source_run_key UNIQUE (source_run_id),
    CONSTRAINT trigger_remints_event_window_attempt_key UNIQUE (namespace_id, original_event_id, window_started_at, attempt)
);

CREATE INDEX trigger_remints_due_idx
    ON trigger_remints (namespace_id, available_at, id) WHERE status = 'pending';
CREATE INDEX trigger_remints_event_window_idx
    ON trigger_remints (namespace_id, original_event_id, window_started_at);

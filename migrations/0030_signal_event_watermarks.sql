-- Durable emitter cursors consumed by DeliverSignalEvent. signal_events
-- remains the only event log and cursor advancement shares its transaction.
CREATE TABLE signal_event_watermarks (
    namespace_id TEXT NOT NULL REFERENCES namespaces (id),
    source_key TEXT NOT NULL,
    watermark JSONB NOT NULL,
    event_id TEXT NOT NULL REFERENCES signal_events (id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, source_key)
);

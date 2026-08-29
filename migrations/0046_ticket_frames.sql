-- Append-only ticket frame snapshots published by the /think lane.
CREATE TABLE ticket_frames (
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    ticket_id     TEXT NOT NULL,
    version       INTEGER NOT NULL CHECK (version > 0),
    frame_json    JSON NOT NULL,
    posted_by     TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, ticket_id, version)
);

CREATE INDEX ticket_frames_latest_idx
    ON ticket_frames (namespace_id, ticket_id, version DESC);

-- Reply storage is created with the projection it feeds. The guarded write
-- endpoint lands in t10; t9's GET can compose rows from its first release.
CREATE TABLE ticket_replies (
    id             TEXT PRIMARY KEY,
    namespace_id   TEXT NOT NULL REFERENCES namespaces (id),
    ticket_id      TEXT NOT NULL,
    replier        TEXT NOT NULL,
    text           TEXT NOT NULL,
    question_id    TEXT,
    signal_event_id TEXT REFERENCES signal_events (id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ticket_replies_ticket_idx
    ON ticket_replies (namespace_id, ticket_id, created_at, id);

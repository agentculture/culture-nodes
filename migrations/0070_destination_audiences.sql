-- t49: namespace-scoped, append-only actor destination audiences.
-- An absent actor retains its kind-based target audience.
CREATE TABLE destination_audiences (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces (id),
    actor        TEXT NOT NULL CHECK (actor ~ '^[a-z0-9_.-]+/[a-z0-9_.-]+$'),
    audience     TEXT NOT NULL CHECK (audience IN ('public', 'org', 'team', 'operators')),
    set_by       TEXT NOT NULL CHECK (set_by <> ''),
    note         TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (namespace_id, id)
);
CREATE INDEX destination_audiences_current_idx
    ON destination_audiences (namespace_id, actor, created_at DESC, id DESC);
CREATE TRIGGER destination_audiences_immutable BEFORE UPDATE OR DELETE ON destination_audiences
    FOR EACH ROW EXECUTE FUNCTION declaration_immutable_row();

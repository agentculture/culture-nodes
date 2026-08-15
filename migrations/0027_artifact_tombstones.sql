-- Append-only retention records. Artifact metadata remains resolvable after
-- content is removed; corrections append and link through supersedes.
CREATE TABLE artifact_tombstones (
    id TEXT PRIMARY KEY,
    artifact_id TEXT NOT NULL REFERENCES artifacts (id),
    artifact_ref TEXT NOT NULL,
    reaped_at TIMESTAMPTZ NOT NULL,
    reason TEXT NOT NULL CHECK (length(btrim(reason)) > 0),
    name TEXT NOT NULL DEFAULT '',
    media_type TEXT NOT NULL DEFAULT '',
    size_bytes BIGINT NOT NULL,
    digest TEXT NOT NULL,
    supersedes TEXT REFERENCES artifact_tombstones (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX artifact_tombstones_artifact_id_reaped_idx
    ON artifact_tombstones (artifact_id, reaped_at DESC, id DESC);

CREATE FUNCTION reject_artifact_tombstone_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'artifact tombstones are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER artifact_tombstones_no_update
    BEFORE UPDATE OR DELETE ON artifact_tombstones
    FOR EACH ROW EXECUTE FUNCTION reject_artifact_tombstone_mutation();

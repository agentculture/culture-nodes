-- 0003_ledger.sql
--
-- The agent-native work ledger (prd-spec §10): immutable typed records with
-- the common envelope, review transactions, and projection checkpoints.

-- ledger_records: immutable typed claims, tasks, decisions, results, and
-- evidence (prd-spec §10.2, §10.3). Ledger records are never updated or
-- deleted -- corrections append a new record with `supersedes`. The trigger
-- below is the enforcement mechanism: it is not merely a convention.
CREATE TABLE ledger_records (
    id                TEXT PRIMARY KEY,
    namespace_id      TEXT NOT NULL REFERENCES namespaces (id),
    schema_version    TEXT NOT NULL,
    record_type       TEXT NOT NULL,
    run_id            TEXT REFERENCES runs (id),
    node_run_id       TEXT REFERENCES node_runs (id),
    attempt_id        TEXT REFERENCES attempts (id),
    origin_kind       TEXT NOT NULL,
    origin_actor_id   TEXT REFERENCES actors (id),
    authority         TEXT NOT NULL CHECK (
        authority IN ('proposed', 'confirmed', 'observed', 'derived', 'rejected', 'superseded')
    ),
    subject_ref       TEXT,
    data              JSONB NOT NULL DEFAULT '{}'::jsonb,
    provenance_refs   JSONB NOT NULL DEFAULT '[]'::jsonb,
    supersedes        TEXT REFERENCES ledger_records (id),
    content_digest    TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ledger_records_namespace_id_idx ON ledger_records (namespace_id);

-- Required index: recent-first ledger reads scoped to a run.
CREATE INDEX ledger_records_run_id_created_at_idx ON ledger_records (run_id, created_at);

CREATE INDEX ledger_records_node_run_id_idx ON ledger_records (node_run_id);
CREATE INDEX ledger_records_supersedes_idx ON ledger_records (supersedes) WHERE supersedes IS NOT NULL;

-- Immutability guard: ledger_records has no UPDATE or DELETE path. Every
-- write is an INSERT; corrections are new rows carrying `supersedes`. This
-- is enforced in the database, not just in application code, because the
-- ledger's evidentiary value depends on records never changing after the
-- fact (prd-spec §10.3, §10.5).
CREATE FUNCTION ledger_records_forbid_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'ledger_records is immutable: % is not permitted (id=%)',
        TG_OP,
        COALESCE(OLD.id, 'unknown');
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_records_no_update
    BEFORE UPDATE ON ledger_records
    FOR EACH ROW
    EXECUTE FUNCTION ledger_records_forbid_mutation();

CREATE TRIGGER ledger_records_no_delete
    BEFORE DELETE ON ledger_records
    FOR EACH ROW
    EXECUTE FUNCTION ledger_records_forbid_mutation();

-- ledger_reviews: atomic review transactions and stale-frame guards
-- (prd-spec §10.8). A review batch is confirm/reject, all-or-nothing, and
-- carries the ledger version and frame checksum it reviewed so a stale
-- review (the ledger moved on since the reviewer's frame) is rejected
-- instead of silently applied.
CREATE TABLE ledger_reviews (
    id                        TEXT PRIMARY KEY,
    namespace_id              TEXT NOT NULL REFERENCES namespaces (id),
    run_id                    TEXT NOT NULL REFERENCES runs (id),
    reviewer_actor_id         TEXT REFERENCES actors (id),
    reviewed_ledger_version   BIGINT NOT NULL,
    frame_checksum            TEXT NOT NULL,
    decision                  TEXT NOT NULL,
    record_ids                JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ledger_reviews_namespace_id_idx ON ledger_reviews (namespace_id);
CREATE INDEX ledger_reviews_run_id_idx ON ledger_reviews (run_id);

-- ledger_projection_versions: checkpoints for deterministic projections
-- (prd-spec §10.9). Identical ledger inputs must produce identical
-- projection digests; this table records the digest for each checkpoint so
-- that property can be tested and audited.
CREATE TABLE ledger_projection_versions (
    id                TEXT PRIMARY KEY,
    namespace_id      TEXT NOT NULL REFERENCES namespaces (id),
    run_id            TEXT NOT NULL REFERENCES runs (id),
    projection_kind   TEXT NOT NULL,
    ledger_version    BIGINT NOT NULL,
    digest            TEXT NOT NULL,
    data              JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ledger_projection_versions_run_kind_version_key
        UNIQUE (run_id, projection_kind, ledger_version)
);

CREATE INDEX ledger_projection_versions_namespace_id_idx ON ledger_projection_versions (namespace_id);

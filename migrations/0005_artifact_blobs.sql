-- 0005_artifact_blobs.sql
--
-- Small-artifact byte storage for the Postgres artifact driver
-- (internal/artifacts/postgres, task t15; prd-spec §9.1/§14/§19.1).
-- Artifacts at or under the driver's configured size cap (default 1 MiB)
-- store their content directly in Postgres rather than round-tripping to
-- the S3-compatible object store. Metadata for every artifact -- including
-- ones whose bytes live here -- is always the `artifacts` table
-- (migrations/0004_observability.sql); this table holds only the bytes,
-- keyed by the same id, so the metadata row is always the single place a
-- caller (or Router, internal/artifacts/router.go) looks first to learn
-- size, digest, and which backend holds an artifact's content.
--
-- Expand-contract policy (docs/adr/0002-migration-policy.md): purely
-- additive -- one new table, one new unique index. No prior binary version
-- reads or writes either, so there is nothing for either to break.

CREATE TABLE artifact_blobs (
    id    TEXT PRIMARY KEY REFERENCES artifacts (id) ON DELETE CASCADE,
    data  BYTEA NOT NULL
);

-- artifacts.uri was not made unique in 0004 because nothing wrote to the
-- table yet. internal/artifacts (both the Postgres and S3 drivers) looks up
-- rows by (namespace_id, uri) on every Get, Stat, and Delete, and relies on
-- there being exactly one row per ref -- this index is both the lookup's
-- index and its correctness guarantee.
CREATE UNIQUE INDEX artifacts_namespace_uri_key ON artifacts (namespace_id, uri);

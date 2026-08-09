-- 0004_observability.sql
--
-- Artifacts, the append-only event log, the transactional outbox, and
-- idempotency keys (prd-spec §14, §15.1).

-- artifacts: metadata and object-store references. Artifact content itself
-- lives in the artifact store (filesystem/S3 driver, task t15); this table
-- carries only the reference and observable metadata.
CREATE TABLE artifacts (
    id                TEXT PRIMARY KEY,
    namespace_id      TEXT NOT NULL REFERENCES namespaces (id),
    run_id            TEXT REFERENCES runs (id),
    attempt_id        TEXT REFERENCES attempts (id),
    uri               TEXT NOT NULL,
    media_type        TEXT,
    size_bytes        BIGINT,
    digest            TEXT,
    storage_backend   TEXT NOT NULL,
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX artifacts_namespace_id_idx ON artifacts (namespace_id);
CREATE INDEX artifacts_run_id_idx ON artifacts (run_id);

-- events: append-only audit events, CloudEvents-compatible (prd-spec
-- §15.1). Sequence is monotonic per aggregate; the unique index is the
-- enforcement mechanism for that guarantee (writers retry on conflict, see
-- internal/store/postgres.Store.InsertEvent).
CREATE TABLE events (
    id               TEXT PRIMARY KEY,
    namespace_id     TEXT NOT NULL REFERENCES namespaces (id),
    aggregate_type   TEXT NOT NULL,
    aggregate_id     TEXT NOT NULL,
    sequence         BIGINT NOT NULL,
    event_type       TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'nodes',
    data             JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT events_aggregate_id_sequence_key UNIQUE (aggregate_id, sequence)
);

CREATE INDEX events_namespace_id_idx ON events (namespace_id);
CREATE INDEX events_aggregate_idx ON events (aggregate_type, aggregate_id);

-- outbox: transactional event and queue publication (prd-spec §12.5,
-- §12.3). Rows are inserted in the same transaction as the state change
-- they announce; a relay process (task t10) is the only publisher and
-- marks rows published after a successful hand-off to the queue driver.
CREATE TABLE outbox (
    id             TEXT PRIMARY KEY,
    namespace_id   TEXT NOT NULL REFERENCES namespaces (id),
    topic          TEXT NOT NULL,
    payload        JSONB NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending',
    available_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    attempts       INTEGER NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX outbox_namespace_id_idx ON outbox (namespace_id);
CREATE INDEX outbox_pending_idx ON outbox (status, available_at) WHERE status = 'pending';

-- idempotency_keys: external request and dispatch deduplication (prd-spec
-- §20.3).
CREATE TABLE idempotency_keys (
    id                 TEXT PRIMARY KEY,
    namespace_id       TEXT NOT NULL REFERENCES namespaces (id),
    scope              TEXT NOT NULL,
    idempotency_key    TEXT NOT NULL,
    request_digest     TEXT,
    response           JSONB,
    status             TEXT NOT NULL DEFAULT 'in_progress',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at       TIMESTAMPTZ,

    CONSTRAINT idempotency_keys_namespace_scope_key_key UNIQUE (namespace_id, scope, idempotency_key)
);

CREATE INDEX idempotency_keys_namespace_id_idx ON idempotency_keys (namespace_id);

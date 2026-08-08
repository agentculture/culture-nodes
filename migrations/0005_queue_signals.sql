-- 0005_queue_signals.sql
--
-- Disposable delivery storage for the Postgres queue driver
-- (internal/queue/postgres, prd-spec §12.3, task t10).
--
-- queue_signals is NOT authoritative workflow state. It exists only so a
-- single-node/dev deployment can run the four-method queue interface
-- (Publish/Receive/Ack/Delay) against Postgres instead of standing up SQS
-- (prd-spec §12.3: "local mode needs no queue product"). A row here is a
-- disposable notification that a work.WorkRef *might* be ready -- never a
-- payload, and never the record a worker actually claims.
--
-- Deliberately NOT work_items (migrations/0002_runtime_execution.sql):
-- work_items is the authoritative, fenced-claim record a worker claims
-- against via an atomic UPDATE ... FOR UPDATE SKIP LOCKED (task t7).
-- Acking or delaying a queue_signals row must never touch work_items --
-- doing so would let the queue itself grant or revoke work, which
-- contradicts the §12.3 rule enforced throughout internal/queue: receiving
-- (or acking, or delaying) a signal never grants or removes work. The
-- worker's fenced claim against work_items is the only thing that does.
--
-- id is caller-supplied (the WorkRef.WorkID the publisher already minted,
-- e.g. the outbox row id -- see internal/events/relay.go), and Publish
-- inserts with ON CONFLICT (id) DO NOTHING, so re-publishing the same
-- WorkID after a crash is a harmless no-op rather than a duplicate row.
CREATE TABLE queue_signals (
    id             TEXT PRIMARY KEY,
    namespace_id   TEXT NOT NULL REFERENCES namespaces (id),
    node_run_id    TEXT NOT NULL DEFAULT '',
    available_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX queue_signals_ready_idx ON queue_signals (available_at);
CREATE INDEX queue_signals_namespace_id_idx ON queue_signals (namespace_id);

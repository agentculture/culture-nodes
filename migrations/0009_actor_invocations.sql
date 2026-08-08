-- Asynchronous actor invocations (prd-spec §12.6, §13.3, §13.4).
--
-- A synchronous invocation needs no durable row: the worker holds the lease
-- for the whole call and reports the completion before releasing it. An
-- ASYNCHRONOUS one does, because §12.6 requires the opposite -- "workers must
-- not hold leases or goroutines for long-running agents" -- so between the
-- actor's 202 and its terminal callback there is no process anywhere holding
-- anything about the invocation in memory. Everything a later callback needs
-- to commit state (which work item, under which lease owner, fencing token,
-- and attempt) has to survive the worker exiting, so it lives here.
--
-- The primary key is the PROTOCOL attempt id -- the `attempt_id` the worker
-- mints and puts in the §13.1 invocation body and in the attempt-scoped
-- callback token. It is deliberately not the same identifier as attempts.id:
-- an attempts row is written by the engine at completion time and does not
-- exist while the invocation is in flight, so keying on it would mean keying
-- on a row that is not there yet.
--
-- last_sequence is the per-attempt high-water mark §13.4's "monotonically
-- increasing actor sequence" is checked against. It is a column rather than a
-- derived MAX over an events table because the check has to be a single
-- atomic compare-and-set: a late or duplicated callback must lose the race
-- outright, not read a stale maximum and then overwrite it.
CREATE TABLE actor_invocations (
    attempt_id               TEXT PRIMARY KEY,
    namespace_id             TEXT NOT NULL REFERENCES namespaces (id),
    run_id                   TEXT NOT NULL REFERENCES runs (id),
    node_run_id              TEXT NOT NULL REFERENCES node_runs (id),
    token_id                 TEXT REFERENCES tokens (id),
    work_id                  TEXT NOT NULL REFERENCES work_items (id),
    worker_id                TEXT NOT NULL,
    fencing_token            BIGINT NOT NULL,
    attempt                  INTEGER NOT NULL,
    node_key                 TEXT NOT NULL,
    actor_ref                TEXT,
    invocation_id            TEXT,
    state                    TEXT NOT NULL DEFAULT 'waiting_external',
    last_sequence            BIGINT NOT NULL DEFAULT 0,
    heartbeat_after_seconds  INTEGER,
    deadline_timer_id        TEXT REFERENCES timers (id),
    supports_cancellation    BOOLEAN NOT NULL DEFAULT false,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX actor_invocations_namespace_id_idx ON actor_invocations (namespace_id);
CREATE INDEX actor_invocations_node_run_id_idx ON actor_invocations (node_run_id);
CREATE INDEX actor_invocations_run_id_idx ON actor_invocations (run_id);

-- Waiting invocations are the operational hot set: "what is this deployment
-- currently blocked on" is a scan of exactly these rows.
CREATE INDEX actor_invocations_waiting_idx ON actor_invocations (state, updated_at)
    WHERE state = 'waiting_external';

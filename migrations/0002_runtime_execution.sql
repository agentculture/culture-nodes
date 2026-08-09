-- 0002_runtime_execution.sql
--
-- Runtime execution tables: runs, tokens, node runs, attempts, runner
-- operations, work claiming, timers, and human tasks (prd-spec §14, §12.4).

-- runs: one workflow execution.
CREATE TABLE runs (
    id                    TEXT PRIMARY KEY,
    namespace_id          TEXT NOT NULL REFERENCES namespaces (id),
    workflow_version_id   TEXT NOT NULL REFERENCES workflow_versions (id),
    status                TEXT NOT NULL DEFAULT 'running',
    input                 JSONB NOT NULL DEFAULT '{}'::jsonb,
    started_by_actor_id   TEXT REFERENCES actors (id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at          TIMESTAMPTZ
);

CREATE INDEX runs_namespace_id_idx ON runs (namespace_id);
CREATE INDEX runs_workflow_version_id_idx ON runs (workflow_version_id);
CREATE INDEX runs_namespace_status_idx ON runs (namespace_id, status);

-- tokens: active and historical control tokens (prd-spec §3.2, §9.7).
CREATE TABLE tokens (
    id               TEXT PRIMARY KEY,
    namespace_id     TEXT NOT NULL REFERENCES namespaces (id),
    run_id           TEXT NOT NULL REFERENCES runs (id),
    node_key         TEXT NOT NULL,
    state            TEXT NOT NULL DEFAULT 'active',
    parent_token_id  TEXT REFERENCES tokens (id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    consumed_at      TIMESTAMPTZ
);

CREATE INDEX tokens_namespace_id_idx ON tokens (namespace_id);
CREATE INDEX tokens_run_id_idx ON tokens (run_id);
CREATE INDEX tokens_run_state_idx ON tokens (run_id, state);

-- node_runs: logical node executions.
CREATE TABLE node_runs (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    token_id      TEXT REFERENCES tokens (id),
    node_key      TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'ready',
    outcome       TEXT,
    visit_count   INTEGER NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at  TIMESTAMPTZ
);

CREATE INDEX node_runs_namespace_id_idx ON node_runs (namespace_id);
CREATE INDEX node_runs_run_id_idx ON node_runs (run_id);
CREATE INDEX node_runs_run_status_idx ON node_runs (run_id, status);

-- attempts: individual dispatch attempts against a node run.
CREATE TABLE attempts (
    id               TEXT PRIMARY KEY,
    namespace_id     TEXT NOT NULL REFERENCES namespaces (id),
    node_run_id      TEXT NOT NULL REFERENCES node_runs (id),
    attempt_number   INTEGER NOT NULL,
    actor_id         TEXT REFERENCES actors (id),
    status           TEXT NOT NULL DEFAULT 'dispatched',
    fencing_token    BIGINT,
    result           JSONB,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,

    CONSTRAINT attempts_node_run_attempt_number_key UNIQUE (node_run_id, attempt_number)
);

CREATE INDEX attempts_namespace_id_idx ON attempts (namespace_id);
CREATE INDEX attempts_node_run_id_idx ON attempts (node_run_id);

-- runner_operations: code-runner requests, policy digests, and observed
-- results (prd-spec §13.7). Evidence here is `observed`-authority only --
-- the runner reports what it directly measured, never what a process printed.
CREATE TABLE runner_operations (
    id               TEXT PRIMARY KEY,
    namespace_id     TEXT NOT NULL REFERENCES namespaces (id),
    attempt_id       TEXT REFERENCES attempts (id),
    operation_kind   TEXT NOT NULL,
    policy_digest    TEXT NOT NULL,
    request          JSONB NOT NULL,
    result           JSONB,
    status           TEXT NOT NULL DEFAULT 'pending',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ
);

CREATE INDEX runner_operations_namespace_id_idx ON runner_operations (namespace_id);
CREATE INDEX runner_operations_attempt_id_idx ON runner_operations (attempt_id);

-- work_items: ready work, leases, and fencing (prd-spec §12.4). Claiming is
-- a single atomic UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)
-- (implemented in task t7); the partial index below serves that claim query
-- and the scheduler's ready-work scan.
CREATE TABLE work_items (
    id                 TEXT PRIMARY KEY,
    namespace_id       TEXT NOT NULL REFERENCES namespaces (id),
    node_run_id        TEXT NOT NULL REFERENCES node_runs (id),
    state              TEXT NOT NULL DEFAULT 'ready',
    state_version      BIGINT NOT NULL DEFAULT 0,
    lease_owner        TEXT,
    lease_expires_at   TIMESTAMPTZ,
    fencing_token      BIGINT NOT NULL DEFAULT 0,
    attempt            INTEGER NOT NULL DEFAULT 0,
    available_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX work_items_namespace_id_idx ON work_items (namespace_id);
CREATE INDEX work_items_node_run_id_idx ON work_items (node_run_id);

-- Partial index for the ready-work claim/scan path: only rows in state
-- 'ready' are ever scanned by the scheduler or worker claim query, and this
-- index carries exactly the (state, available_at) shape that query filters
-- and orders on.
CREATE INDEX work_items_ready_idx ON work_items (state, available_at) WHERE state = 'ready';

-- timers: durable waits, deadlines, and retry availability (prd-spec §12.7).
CREATE TABLE timers (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    run_id        TEXT REFERENCES runs (id),
    node_run_id   TEXT REFERENCES node_runs (id),
    timer_kind    TEXT NOT NULL,
    fire_at       TIMESTAMPTZ NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    claimed_by    TEXT,
    claimed_at    TIMESTAMPTZ,
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX timers_namespace_id_idx ON timers (namespace_id);
CREATE INDEX timers_due_idx ON timers (status, fire_at) WHERE status = 'pending';

-- human_tasks: approval and human-input work (prd-spec §9.9).
CREATE TABLE human_tasks (
    id                  TEXT PRIMARY KEY,
    namespace_id        TEXT NOT NULL REFERENCES namespaces (id),
    run_id              TEXT NOT NULL REFERENCES runs (id),
    node_run_id         TEXT REFERENCES node_runs (id),
    kind                TEXT NOT NULL,
    assigned_owner_id   TEXT REFERENCES owners (id),
    status              TEXT NOT NULL DEFAULT 'pending',
    request             JSONB NOT NULL DEFAULT '{}'::jsonb,
    response            JSONB,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at         TIMESTAMPTZ
);

CREATE INDEX human_tasks_namespace_id_idx ON human_tasks (namespace_id);
CREATE INDEX human_tasks_run_id_idx ON human_tasks (run_id);
CREATE INDEX human_tasks_pending_idx ON human_tasks (namespace_id, status) WHERE status = 'pending';

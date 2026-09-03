-- Worker liveness is an additive, deployment-wide observation. Older workers
-- neither read nor write this table and continue operating unchanged.
CREATE TABLE worker_presence (
    worker_id  TEXT PRIMARY KEY,
    hostname   TEXT NOT NULL,
    revision   TEXT NOT NULL,
    actor_keys TEXT[] NOT NULL DEFAULT '{}',
    last_seen  TIMESTAMPTZ NOT NULL
);

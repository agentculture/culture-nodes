-- 0019_parallel_tokens.sql
--
-- Split/join substrate for parallel tokens (issue #43, design
-- docs/design/2026-08-13-parallel-tokens-full.md §7; the design draft
-- numbered this 0017, which was taken by attempt-usage telemetry before this
-- landed). Expand-only per docs/adr/0002-migration-policy.md: nothing is
-- dropped, renamed, or tightened, and an N-1 binary that never reads these
-- tables keeps working -- it writes tokens without group_id (NULL, the same
-- value every pre-split token carries) and never joins through token_groups.
--
--   * token_groups -- one row per split fan-out. Cardinality is fixed at
--     creation and is DISCOVERED (how many guarded split edges actually
--     passed), never declared at the join (design D4): a 4-edge split whose
--     guards pass 3 must join at 3. parent_group_id records nesting, so a
--     join can hand its post-join token back to the enclosing group and a
--     sibling reap can find nested descendants by group parentage.
--   * tokens.group_id -- which split's fan-out set a token belongs to.
--     NULL for the entry token, for every token outside any split, and for
--     every token an N-1 binary wrote. No backfill needed: NULL already
--     means exactly what those rows are.
--   * join_arrivals -- one row per branch reaching a barrier (design §4.1).
--     Append-only; counting them under the run's advisory lock is the whole
--     race-free barrier (design §4.2), so there is no counter column to keep
--     consistent. UNIQUE (join_node_run_id, token_id) makes a double arrival
--     of one branch a constraint violation rather than a silent extra count.
--
-- The new node-run status value 'waiting_join' needs no migration:
-- node_runs.status is unconstrained TEXT (0002 -- no CHECK), so a new value
-- is purely additive.

CREATE TABLE token_groups (
    id                 TEXT PRIMARY KEY,
    namespace_id       TEXT NOT NULL REFERENCES namespaces (id),
    run_id             TEXT NOT NULL REFERENCES runs (id),
    split_node_run_id  TEXT NOT NULL REFERENCES node_runs (id),
    parent_group_id    TEXT REFERENCES token_groups (id),
    cardinality        INTEGER NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX token_groups_run_id_idx ON token_groups (run_id);

ALTER TABLE tokens ADD COLUMN group_id TEXT REFERENCES token_groups (id);
CREATE INDEX tokens_group_id_idx ON tokens (group_id);

-- The open-barrier lookup (design §4.1: locate the waiting_join node run
-- for (run, node, group) by joining node_runs -> tokens on token_id) filters
-- node_runs first; waiting_join rows are a tiny hot set, so a partial index
-- keeps the probe exact whatever a run's total node-run count grows to.
CREATE INDEX node_runs_waiting_join_idx
    ON node_runs (run_id, node_key) WHERE status = 'waiting_join';

CREATE TABLE join_arrivals (
    id                TEXT PRIMARY KEY,
    namespace_id      TEXT NOT NULL REFERENCES namespaces (id),
    run_id            TEXT NOT NULL REFERENCES runs (id),
    join_node_run_id  TEXT NOT NULL REFERENCES node_runs (id),
    group_id          TEXT NOT NULL REFERENCES token_groups (id),
    token_id          TEXT NOT NULL REFERENCES tokens (id),
    from_node         TEXT NOT NULL,
    outcome           TEXT NOT NULL,
    output            JSONB,
    arrived_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (join_node_run_id, token_id)
);
CREATE INDEX join_arrivals_join_node_run_id_idx ON join_arrivals (join_node_run_id);

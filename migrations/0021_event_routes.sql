-- 0021_event_routes.sql
--
-- Event-driven continuation (issue #43, task t21; design
-- docs/design/2026-08-13-parallel-tokens-full.md §6). The design draft
-- numbered this 0018, which attempt continuation_ref took before this
-- landed; 0019 is the split/join substrate and 0020 is the concurrent
-- capacity-breaker task's. Expand-only per docs/adr/0002-migration-policy.md:
-- one new table, one new nullable column, three new indexes. Nothing is
-- dropped, renamed, or tightened, and an N-1 binary that never reads any of
-- it keeps working — it writes tokens without origin_event_id (NULL, the same
-- value every non-pickup token carries) and never queries event_routes.
--
--   * event_routes -- the durable, run-scoped pickup routes materialized
--     from the pinned IR's `onEvent` edges at run creation (design D9). A
--     table rather than an IR walk at delivery time because delivery matches
--     with one indexed SQL scan today; asking it to load and parse every
--     running run's pinned IR per event would turn an O(matching rows)
--     operation into O(running runs) IR decodes. Routes are multi-fire
--     (design D10): status moves active -> retired only when the run reaches
--     a terminal state, never on a match.
--
--   * tokens.origin_event_id -- honest provenance for a pickup token
--     (review finding D4). A token an event created has no PARENT TOKEN:
--     nothing in this run handed it control, and the emitter may be another
--     run or an external system entirely, so stamping the emitter's token as
--     its parent would make the ancestry tree assert a causal edge that does
--     not exist. Instead the token names the FACT that created it, and the
--     run-detail surface renders it as an explained root rather than an
--     orphan. NULL for every other token, and for every token an N-1 binary
--     wrote.
--
--   * signal_events_replay_idx -- the catch-up scan behind D12's replay
--     cursor (design §6.3): the oldest matching fact for (namespace, name)
--     since the run's own creation and since the newest fact already fired
--     to that run for that name.

-- event_routes: run-scoped durable pickup routes derived from the pinned
-- IR's onEvent edges at run creation.
CREATE TABLE event_routes (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    event_name    TEXT NOT NULL,
    target_node   TEXT NOT NULL,
    -- The edge's CEL guard source, carried verbatim from the pinned IR. It
    -- is stored so an operator reading the row can see WHY a delivery did or
    -- did not pick up; the authoritative evaluation still happens against
    -- the program the engine rebuilds from the pinned IR, never against this
    -- copy (design §6.1).
    guard         TEXT,
    status        TEXT NOT NULL DEFAULT 'active',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at    TIMESTAMPTZ
);

-- Delivery's matching scan: active routes for (namespace, name), the same
-- partial-index shape signal_subscriptions_pending_idx already serves.
CREATE INDEX event_routes_pending_idx
    ON event_routes (namespace_id, event_name) WHERE status = 'active';
CREATE INDEX event_routes_run_id_idx ON event_routes (run_id);

-- tokens.origin_event_id: the signal_events fact a pickup token came from.
ALTER TABLE tokens ADD COLUMN origin_event_id TEXT REFERENCES signal_events (id);
CREATE INDEX tokens_origin_event_id_idx
    ON tokens (origin_event_id) WHERE origin_event_id IS NOT NULL;

-- The replay scan (design §6.3): ordered by created_at within (namespace,
-- name) so the "oldest unseen fact since the run began" probe is an index
-- range scan rather than a filter over the whole name's history.
CREATE INDEX signal_events_replay_idx
    ON signal_events (namespace_id, name, created_at);

-- 0023_run_sessions.sql
--
-- The session ledger the declared economic budget spends against (task t11
-- of the economy-discord-graphs plan; issue #48 item 5, spec claim c6,
-- honesty h5). Expand-only per docs/adr/0002-migration-policy.md: one new
-- table, nothing dropped or tightened, and an N-1 binary that never reads it
-- keeps dispatching exactly as it does today (it simply has no budget).
--
-- WHAT ONE ROW IS. One NEW provider session a run started -- a COLD START.
-- Not one dispatch, not one attempt: a dispatch that carried a prior
-- `continuation_ref` (0018, ADR 0010) continues a conversation the run has
-- already paid to open, and it writes nothing here. That distinction is the
-- whole reason the table exists. `budget.maxSessions` is a bound on cold
-- starts because counting warm turns would make the economic contract fight
-- the session stickiness it was built beside: a workstream of N node turns
-- on one warm session would count as N and always exhaust the budget it was
-- designed to conserve (spec claim c46, scope entry s26).
--
-- WHY NOT A COLUMN ON `attempts`. Three reasons, in increasing order of
-- weight:
--
--   1. An attempts row is written by the ENGINE at completion time, inside
--      the §12.5 transaction. The cold/warm fact is known by the WORKER at
--      dispatch time, one step before the actor is invoked -- and for an
--      asynchronous invocation the completion happens in a different process
--      entirely (the callback handler), which resolved nothing and would
--      have to be handed the fact through actor_invocations as well (the
--      shape migration 0015 already had to take for actor_id).
--   2. `attempts` holds rows for kinds that never touch a provider at all --
--      code nodes, decision nodes, dispatches refused before any actor was
--      reached. A NULL-means-cold column over that population would count
--      sessions that never existed, and a NULL-means-not-applicable column
--      would be a three-valued boolean nobody could read.
--   3. A session is a fact about the RUN's spend, and this table says
--      exactly that in one line per session. "How many sessions has this run
--      started" is COUNT(*) over one index, evaluated on the dispatch path.
--
-- WHY THE PROTOCOL ATTEMPT ID IS THE KEY. `attempt_id` here is the §13.1
-- attempt id the worker mints for the invocation (the same identifier
-- actor_invocations keys on, and deliberately NOT attempts.id, which does
-- not exist yet when this row is written). It is freshly minted per
-- dispatch, so it is the natural idempotency key: a worker that records a
-- start, crashes, and re-enters with the same context charges the session
-- once. Two different dispatches are two different sessions and charge
-- twice, which is the honest reading -- each really did open a conversation.
--
-- WHAT IT DELIBERATELY OVER-COUNTS. The row is written immediately before
-- the invocation, not after it. A session recorded for an invocation that
-- then failed in transport may never have been opened provider-side. That is
-- the conservative direction on purpose: a budget that under-counts spends
-- money the author forbade, and the alternative (record after a successful
-- return) would charge nothing for exactly the long, expensive dispatches
-- that die mid-turn.
--
-- NOT EVIDENCE. Like actor_invocations beside it, this is control-plane
-- bookkeeping about dispatch, not a work-ledger record. Nothing here is an
-- `observed` claim about what an actor did, and no surface may present it as
-- one.
CREATE TABLE run_sessions (
    -- The §13.1 protocol attempt id of the dispatch that opened the session.
    attempt_id   TEXT        PRIMARY KEY,
    namespace_id TEXT        NOT NULL REFERENCES namespaces (id),
    run_id       TEXT        NOT NULL REFERENCES runs (id),
    node_run_id  TEXT        NOT NULL REFERENCES node_runs (id),
    -- The node's key in the pinned definition, so a row reads without a
    -- join (actor_invocations.node_key's precedent).
    node_key     TEXT        NOT NULL,
    -- The actor REFERENCE the node named. Kept unconditionally because it is
    -- always available at the dispatch site.
    actor_ref    TEXT        NOT NULL,
    -- The resolved actors-table row id, when the registry could answer one.
    -- NULL means the dispatch was unattributed -- which is also why it was
    -- cold: a continuation handle belongs to an identity, and a dispatch
    -- with no identity has no conversation to continue (ADR 0010 §4).
    actor_id     TEXT        REFERENCES actors (id),
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The one read on the dispatch path: how many sessions has this run started.
CREATE INDEX run_sessions_run_id_idx ON run_sessions (run_id);
CREATE INDEX run_sessions_namespace_id_idx ON run_sessions (namespace_id);

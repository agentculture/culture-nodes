-- The resolved actors-table row id for an asynchronous invocation.
--
-- attempts.actor_id (migration 0002) is how per-actor surfaces --
-- GET /v1alpha1/actors/{id}/stats, the jobs view's actor column --
-- attribute work. The sync dispatch path can attribute at completion time
-- because the worker still holds the resolution in memory; an async
-- attempt completes from a callback in a process that resolved nothing,
-- so the id has to survive in the invocation row the callback already
-- reads (same reasoning as every other column here: between the 202 and
-- the terminal callback there is no process holding anything in memory).
--
-- Nullable, no default: expand-only per docs/adr/0002-migration-policy.md.
-- A pre-0015 binary never names this column; an invocation parked by one
-- simply completes unattributed, exactly as it would have before.
ALTER TABLE actor_invocations
    ADD COLUMN actor_id TEXT REFERENCES actors (id);

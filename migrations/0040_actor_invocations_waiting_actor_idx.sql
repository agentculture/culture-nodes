-- 0040_actor_invocations_waiting_actor_idx.sql
--
-- Serves task t16's per-actor concurrency ceiling (issue #166's second
-- half, "one ticket per machine";
-- internal/store/postgres/actorconcurrency.go's CountWaitingActorInvocations):
-- "how many asynchronous invocations of THIS actor are in flight right
-- now", read on every dispatch to that actor once the ceiling is
-- configured (internal/worker/concurrency.go).
--
-- 0009's actor_invocations_waiting_idx already narrows to the hot
-- 'waiting_external' slice on (state, updated_at); this adds the
-- (namespace_id, actor_id) pair the new query filters on ADDITIONALLY, so a
-- deployment with many actors in flight across many namespaces does not
-- have to scan every waiting row in the namespace to count one actor's.
-- Expand-only (docs/adr/0002-migration-policy.md): a new index, nothing
-- else touched. An N-1 binary never queries this shape and is unaffected.
CREATE INDEX actor_invocations_waiting_actor_idx
    ON actor_invocations (namespace_id, actor_id)
    WHERE state = 'waiting_external';

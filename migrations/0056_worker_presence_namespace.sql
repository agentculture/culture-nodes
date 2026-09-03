-- Worker presence is namespace-scoped, not deployment-wide (PR #292 review).
--
-- `0055` keyed presence on worker_id alone and the mesh read model returned
-- every row, so a namespace-scoped GET /v1alpha1/mesh answered with the
-- worker topology of every OTHER namespace sharing the database, and two
-- workers carrying the same NODES_WORKER_ID in different namespaces
-- overwrote each other's row. Both follow from the missing column.
--
-- Presence is a liveness cache, not authoritative state: every worker
-- re-upserts its own row on each poll tick. Existing rows therefore cannot
-- be attributed to a namespace after the fact and do not need to be — they
-- are deleted here and rewritten within one tick by whichever workers are
-- actually running.
DELETE FROM worker_presence;

ALTER TABLE worker_presence
    ADD COLUMN namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE;

ALTER TABLE worker_presence DROP CONSTRAINT worker_presence_pkey;
ALTER TABLE worker_presence ADD PRIMARY KEY (namespace_id, worker_id);

-- 0020_actor_availability.sql
--
-- The capacity circuit breaker's durable state (issue #48 item 1, task t9 of
-- the economy-discord-graphs plan; spec claim c4, honesty conditions h3/h38).
-- Expand-only per docs/adr/0002-migration-policy.md: one new table, nothing
-- dropped, renamed, or tightened, and an N-1 binary that never reads it keeps
-- dispatching exactly as it does today (it simply has no breaker).
--
-- WHY A NEW TABLE AND NOT A COLUMN ON `actors`. The actors table is
-- append-only by contract -- "a new capability or endpoint change is a new
-- row (a new revision), never an update to an existing one" (migration 0001,
-- restated in internal/store/postgres/actorstats.go's Actor doc comment). A
-- pause is the opposite kind of fact: mutable, short-lived, and cleared
-- again. Putting it on `actors` would either violate that contract with an
-- UPDATE or mint a whole identity revision every time a provider ran out of
-- quota. So availability lives beside identity, in its own mutable row.
--
-- WHY KEYED BY actor_key AND NOT BY actors.id. Provider capacity belongs to
-- the IDENTITY, not to one revision of its registration: re-registering
-- codex-thor with a new endpoint does not give it a fresh quota. The
-- dispatch site also has the actor key in hand unconditionally
-- (internal/worker/registry.go's actorKeyOf over the node's `uses`), while
-- the actors-table row id is best-effort and may be "" for a registry that
-- cannot resolve one -- a breaker that could be bypassed by an unresolvable
-- row id would not be a breaker. The actors read surface joins the other
-- way (actor_key -> pause) to render it.
--
-- CONCURRENCY. Two workers can trip the same actor at the same moment. The
-- write is an idempotent upsert on the primary key, and the ON CONFLICT
-- branch keeps the LATER paused_until (see PauseActor in
-- internal/store/postgres/actoravailability.go): last-writer-wins on the
-- later deadline is both safe and the conservative direction -- a concurrent
-- trip may extend a pause, never silently shorten one another worker already
-- committed.
--
-- PROVENANCE. tripped_by_run_id / tripped_by_node_run_id /
-- tripped_by_attempt_id / tripped_by_work_id record which dispatch tripped
-- the breaker, so "why is this actor paused" is answerable from the row
-- alone rather than by correlating timestamps against the event log. They
-- are plain TEXT rather than foreign keys on purpose: the pause is a fact
-- about the ACTOR, and its lifetime is deliberately independent of the run
-- that happened to discover the exhaustion -- an FK would make a pause's
-- validity depend on a run's, which is not the relationship.

CREATE TABLE actor_availability (
    namespace_id            TEXT        NOT NULL REFERENCES namespaces (id),
    actor_key               TEXT        NOT NULL,
    -- The instant the actor becomes dispatchable again. A row whose
    -- paused_until is in the past is history, not a pause: every read path
    -- compares against now() rather than treating row-presence as paused.
    paused_until            TIMESTAMPTZ NOT NULL,
    -- The §13.5 error class that tripped it ("capacity_exhausted" today).
    -- Stored rather than assumed so a later pause source reads honestly.
    reason                  TEXT        NOT NULL,
    -- The provider's own Retry-After, in seconds, when it named one. NULL
    -- means it named none and the deadline came from the bounded default --
    -- never 0, which would read as "retry immediately".
    retry_after_seconds     INTEGER,
    -- One human line naming what happened, for the actors read surface.
    detail                  TEXT,
    tripped_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    tripped_by_run_id       TEXT,
    tripped_by_node_run_id  TEXT,
    tripped_by_attempt_id   TEXT,
    tripped_by_work_id      TEXT,
    -- Set when an operator cleared the pause early (POST
    -- /v1alpha1/actors/{id}/resume). The clear also moves paused_until to
    -- now(), so the "is it paused" predicate stays a single comparison;
    -- these two columns exist so the clear is explainable afterwards rather
    -- than indistinguishable from an expiry. A later trip resets them to
    -- NULL -- the new pause was not cleared by anyone.
    cleared_at              TIMESTAMPTZ,
    cleared_by              TEXT,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, actor_key)
);

-- The read the actors list surface makes: every currently-paused actor in
-- one namespace.
CREATE INDEX actor_availability_active_idx
    ON actor_availability (namespace_id, paused_until);

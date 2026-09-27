-- Task t10 (#328): loop-guard backstops (c46/c93) and per-subject
-- concurrency (c84/h57) for the declaration engine (internal/declengine).
--
-- The re-entry limit, hop limit and self-retrigger backstops read only the
-- already-loaded causal lineage and need no new storage. The rate ceiling
-- (30 firings/declaration/hour by default) is answered by counting existing
-- declaration_firings rows, so it needs only an index. Per-subject
-- concurrency is new state: which of a declaration's firings for a subject
-- are still in flight, and what got queued when its cap had no room.

-- Subject is the caller-supplied correlation key a firing's triggering
-- event carried (mirrors runs.subject / SignalEvent.Subject, task t15's
-- per-subject model). NULL when the event carried none -- most firings.
ALTER TABLE declaration_firings ADD COLUMN subject TEXT;

-- Rate ceiling (c93): "N firings per declaration per hour" counts logical
-- firings created within the trailing window. A re-mint (remint_of_id NOT
-- NULL) is the same logical firing dispatched again and must not inflate
-- the rate, so the partial index -- and the query it serves
-- (PostgresBackend.RecentFirings) -- only ever look at remint_of_id IS NULL
-- rows.
CREATE INDEX declaration_firings_declaration_rate_idx
    ON declaration_firings (namespace_id, declaration_id, created_at)
    WHERE remint_of_id IS NULL;

-- Per-subject concurrency in-flight count (c84/h57): a canonical firing is
-- "in flight" for its subject while its opened declaration_nodes row is
-- still 'open'. subject lives on the physical firing row (set once, at
-- Claim, from the event); a re-mint of one logical firing keeps the same
-- canonical_firing_id and is never itself the row declaration_nodes.
-- opening_firing_id references, so this index only needs to serve lookups
-- keyed by (namespace, declaration, subject).
CREATE INDEX declaration_firings_subject_idx
    ON declaration_firings (namespace_id, declaration_id, subject)
    WHERE subject IS NOT NULL;

-- declaration_subject_deferrals is declarations' mirror of migration
-- 0039's deferred_triggers (internal/store/postgres/subjectconcurrency.go):
-- when a declaration's max_concurrent_subject cap already has K firings in
-- flight for a subject, the next event for that SAME subject is queued
-- here instead of claimed. A second (or third, ...) event for a subject
-- that is already queued REPLACES the queued entry rather than piling up
-- -- the unique (namespace_id, declaration_id, subject) key is what makes
-- that an UPSERT (ON CONFLICT DO UPDATE) instead of a second row, the exact
-- replace rule 0039 uses for deferred_triggers.
--
-- The queued row stores the whole triggering declengine.Event as JSON
-- rather than a handful of columns: replay (Engine.DrainSubject) re-runs it
-- through the ordinary Handle path, so it needs everything Handle would
-- have needed the first time (kind, node, variables, and the origin marker
-- that resolves lineage) -- not a bespoke subset that silently drifts from
-- Event's fields as the engine grows.
CREATE TABLE declaration_subject_deferrals (
    id             TEXT PRIMARY KEY,
    namespace_id   TEXT NOT NULL REFERENCES namespaces (id),
    declaration_id TEXT NOT NULL,
    subject        TEXT NOT NULL,
    event          JSONB NOT NULL,
    attempts       INTEGER NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT declaration_subject_deferrals_key UNIQUE (namespace_id, declaration_id, subject)
);

-- DrainSubject pops the longest-queued entry for a declaration, across
-- every subject -- "the rest run later in arrival order" (h57) -- when a
-- slot frees. created_at is never touched by the replace-rule UPSERT
-- (only event, attempts and updated_at are), so a subject's place in that
-- order is fixed at first queue, exactly like 0039's TouchDeferredTrigger.
CREATE INDEX declaration_subject_deferrals_drain_idx
    ON declaration_subject_deferrals (namespace_id, declaration_id, created_at, id);

-- 0039_deferred_triggers.sql
--
-- The durable queue behind task t16's cross-issue concurrency ceiling (spec
-- c36/h21), built on 0038's per-subject dedup. 0038 answers "is a subject
-- already in flight" so a second event on the same Jira issue attaches to
-- the run already open rather than spawning a sibling. This migration
-- answers the next question a Jira-driven deployment asks: "how many
-- DIFFERENT issues may this workflow run at once" -- a workflow author now
-- declares `limits.maxConcurrentSubjectRuns`, and a trigger that would
-- exceed it must not create a run at all.
--
-- WHY A TABLE AND NOT A SILENT DROP. Refusing outright would lose the
-- event -- and unlike 0021's event-route pickups (design D13, "an external
-- event must not kill a healthy run because it arrived at a busy moment"),
-- there is no run yet for a REFUSED-at-creation trigger to record a refusal
-- against. This table is that missing aggregate: one row per subject
-- currently waiting for a slot, holding everything
-- internal/engine/trigger.go's createTriggeredRunTx needs to create the run
-- LATER, exactly as it would have at match time. See
-- internal/engine/trigger.go's DeferredTrigger doc comment for the field-by-
-- field reasoning (why the workflow version travels whole rather than by
-- digest alone, why a subject holds at most one row, why CreatedAt survives
-- a refresh).
--
-- WHY A TABLE AND NOT internal/scheduler'S TIMER MACHINERY. Both were read
-- before choosing (task t16's brief asked for exactly that comparison).
-- internal/scheduler's `timers` table (migration 0002) and its single-
-- active/standby tick loop (internal/scheduler/scheduler.go) exist to answer
-- "wake something up at an INSTANT nobody can predict any earlier" -- a
-- deadline, a retry backoff, a cron-like schedule. A queued trigger has no
-- such instant: nothing here is due at a computed time, it is due the moment
-- SOME OTHER run of the same workflow goes terminal, which is an event this
-- process already observes synchronously, inside the exact transaction that
-- causes it (DrainSubjectTriggerQueue, called from every terminal-transition
-- site in internal/engine/complete.go and humandecision.go). Routing that
-- through a poll-based timer would mean creating a timer row per deferred
-- trigger, firing it on every tick just to re-check a condition that is
-- already known to be false between drains, and — because the scheduler's
-- effect-application is a SEPARATE transaction from the run completion that
-- actually freed the slot (scheduler.go's own package doc: "a schedule fire
-- appends a signal event... in the store's own transaction") — losing the
-- one property that makes this safe without a distributed lock: pop and
-- create happening in the SAME transaction as the completion that freed the
-- slot, under the same workflow-scoped advisory lock TriggerEvent already
-- takes. A plain table, read and consumed inside that existing transaction,
-- needs none of the scheduler's clock machinery and loses none of its
-- durability -- see internal/engine/trigger.go's DrainSubjectTriggerQueue.
--
-- Expand-only (docs/adr/0002-migration-policy.md): one new table, no
-- changes to any existing one. An N-1 binary never reads or writes it, so it
-- simply never queues -- every subject-bearing trigger it evaluates behaves
-- exactly as it did before this migration, because `limits` carries no
-- default for `maxConcurrentSubjectRuns` (see compiler.expandLimits) and an
-- N-1 binary cannot decode a field it has no struct tag for in the first
-- place.
CREATE TABLE deferred_triggers (
    id                TEXT PRIMARY KEY,
    namespace_id      TEXT NOT NULL REFERENCES namespaces (id),
    workflow_key      TEXT NOT NULL,
    -- The exact workflow_versions row this event matched against, carried
    -- whole (digest, source, IR) rather than as a foreign key alone: a
    -- deferred entry may still be queued when a REPLACEMENT publication of
    -- the same workflow_key lands, and draining must create the run from
    -- the version that actually matched, never from whatever the newest
    -- version happens to be by then (workflow_versions rows are themselves
    -- immutable and are never deleted, so this is redundant with a lookup
    -- by digest today -- it is carried whole so that stays true even if a
    -- retention policy is ever added for old versions, exactly the caution
    -- 0038's run_trigger_subject.sql documents for `runs.subject`).
    workflow_digest   TEXT NOT NULL,
    source_format     TEXT NOT NULL,
    source            TEXT NOT NULL,
    normalized_ir     JSONB NOT NULL,
    subject           TEXT NOT NULL,
    trigger_event_id  TEXT NOT NULL,
    event_name        TEXT NOT NULL,
    event_emitter     TEXT NOT NULL,
    event_payload     JSONB NOT NULL,
    -- How many matching events this queue entry has absorbed via
    -- TouchDeferredTrigger, starting at 1. Operator-visible contention
    -- signal only; no decision reads it.
    attempts          INTEGER NOT NULL DEFAULT 1,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A subject holds at most one queued row: TriggerEvent's
    -- FindDeferredTrigger/TouchDeferredTrigger pair (internal/engine/
    -- trigger.go) is what keeps a second matching event from creating a
    -- sibling row instead of refreshing this one -- the same invariant
    -- 0038's dedup keeps for ACTIVE runs, extended to the queue.
    CONSTRAINT deferred_triggers_workflow_subject_key UNIQUE (namespace_id, workflow_key, subject)
);

-- Serves both queries TriggerEvent and DrainSubjectTriggerQueue perform,
-- taken under the workflow-scoped advisory lock (triggerWorkflowLockKey)
-- that already serializes every reader and writer of one workflow's queue,
-- so this index never has to defend against a concurrent writer the way a
-- partial-index-plus-race would: FindDeferredTrigger's equality lookup on
-- (namespace_id, workflow_key, subject) is served by the UNIQUE constraint's
-- own index above, and OldestDeferredTrigger's
-- "(namespace_id, workflow_key) ORDER BY created_at ASC LIMIT 1" -- FIFO
-- drain order across every subject -- is served by this one.
CREATE INDEX deferred_triggers_workflow_created_idx
    ON deferred_triggers (namespace_id, workflow_key, created_at);

-- 0033_schedules.sql
--
-- The declared cadence that makes upkeep start itself (issue #107, task
-- t33). Task t17b gave the control plane a TRIGGER: an inbound event that
-- matches a published workflow's `spec.triggers` creates a run inside the
-- same transaction that appends the event fact. What was still missing was
-- the thing that makes an event happen with nobody typing a command. This
-- table is that thing.
--
-- Expand-only (docs/adr/0002-migration-policy.md): one new table, two new
-- indexes, nothing dropped, renamed, or tightened. An N-1 binary never reads
-- this table, so it simply has no schedules -- the same "no breaker" shape
-- migration 0020 documents for actor_availability.
--
-- WHY A TABLE AND NOT A FIELD ON THE WORKFLOW. A workflow version is
-- immutable and content-addressed: its bytes ARE its identity. Acceptance
-- criterion 1 of this task requires that "disabling the schedule stops it
-- starting", and there is no way to disable a declaration that lives inside
-- an immutable digest -- you would have to publish a new version, which
-- changes what the graph IS in order to change how often it RUNS. Those are
-- different questions. A cadence is deployment configuration in exactly the
-- sense §9.5 makes an actor endpoint deployment configuration: which
-- environment sweeps how often is not a property of the graph, and
-- examples/pr-upkeep/workflow.yaml already says so in its own header
-- ("Deployment configuration: register actor ... The event payload supplies
-- repository identity ... no discovery credentials or sweep process are part
-- of this graph").
--
-- WHY IT EMITS AN EVENT RATHER THAN CREATING A RUN DIRECTLY. There is
-- already exactly one path from "something happened" to "a run exists": the
-- signal event and the workflow triggers that match it. A schedule that
-- called CreateRun itself would be a second such path, with its own
-- condition evaluation, its own input validation, and its own set of bugs --
-- and it would produce runs with no durable fact explaining why they exist.
-- A schedule fire appends a signal_events row like any other emitter, and
-- everything downstream (trigger matching, run creation, route pickup) is
-- the code t17b already shipped and tested. The schedule is an emitter with
-- a clock, nothing more.
--
-- DURABILITY (acceptance criterion 2). Two failure modes, and the columns
-- that answer them:
--
--   * Two control planes tick at once. next_fire_at is advanced in the SAME
--     transaction that appends the event, under a row lock taken with FOR
--     UPDATE SKIP LOCKED (see FireSchedule in
--     internal/store/postgres/schedules.go). A second ticker either skips the
--     locked row outright or, arriving after the winner commits, re-reads
--     next_fire_at inside its own lock and finds the schedule no longer due.
--     There is no window in which both see it as due AND both can commit.
--
--   * The process dies between deciding a schedule is due and creating the
--     run. That is not a timing question but a transactional one, and it is
--     why fire-and-advance is one transaction: either the event fact, the
--     triggered run, and the advanced next_fire_at all committed, or none of
--     them did and the row is still due for the next tick. The restarted
--     process does not have to REMEMBER which happened; it reads
--     next_fire_at, which is the answer.
--
-- No fencing token column, deliberately. Fencing exists in this codebase
-- (work_items.fencing_token) because a work item's execution ESCAPES its
-- claiming transaction -- a worker holds a lease while it talks to an actor
-- over the network, so a resurrected zombie holder has to be refused at
-- commit time by a token comparison. A schedule fire escapes nothing: the
-- decision, the effect, and the advance are the same transaction, so the row
-- lock IS the fence and a token would be a second, weaker copy of a
-- guarantee PostgreSQL is already giving.
CREATE TABLE schedules (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    -- Operator-facing name, unique per namespace: the handle used to enable,
    -- disable, and find a schedule without carrying its id around.
    name          TEXT NOT NULL,
    -- The signal event this schedule emits, and the payload it emits with.
    -- Both are plain declared data: a schedule discovers nothing and reads
    -- no external system. What it emits is what an operator declared.
    event_name    TEXT NOT NULL,
    emitter       TEXT NOT NULL,
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- The declared cadence. Seconds rather than INTERVAL because every
    -- consumer wants a Go time.Duration and INTERVAL's month/day components
    -- are not convertible to one without a calendar.
    interval_seconds BIGINT NOT NULL CHECK (interval_seconds > 0),
    -- What to do about an occurrence that came due while nothing was
    -- running. 'fire-once' fires late, exactly once, and realigns; 'skip'
    -- declines a missed occurrence and waits for the next boundary. See
    -- ScheduleCatchUp in internal/store/postgres/schedules.go for the whole
    -- argument. Deliberately declared per schedule: a nightly sweep and a
    -- five-minute poll want opposite answers.
    catch_up      TEXT NOT NULL DEFAULT 'fire-once'
                  CHECK (catch_up IN ('fire-once', 'skip')),
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    -- The next instant this schedule is due. It is the whole durable state
    -- of the loop: advancing it is what "this occurrence happened" MEANS,
    -- and it is advanced only in the transaction that also appended the
    -- event.
    next_fire_at  TIMESTAMPTZ NOT NULL,
    last_fired_at TIMESTAMPTZ,
    -- The signal_events row the last fire appended. A real foreign key, not
    -- a label: "show me the event this schedule started" has to be a join,
    -- and a dangling id would make the schedule's own history unreadable.
    last_event_id TEXT REFERENCES signal_events (id),
    fire_count    BIGINT NOT NULL DEFAULT 0,
    -- Occurrences declined under catch_up = 'skip'. Counted rather than
    -- inferred, because "the schedule was down and chose not to backfill"
    -- and "the schedule never came due" must not look the same afterwards.
    skip_count    BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX schedules_namespace_name_idx ON schedules (namespace_id, name);
-- The tick's one read: due, enabled schedules. Same partial-index shape
-- timers_due_idx serves for the timer half of the same loop.
CREATE INDEX schedules_due_idx ON schedules (next_fire_at) WHERE enabled;

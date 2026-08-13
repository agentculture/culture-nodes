-- 0016_signal_events.sql
--
-- The first-class event surface (task t10, issue #39, spec decision c35):
-- external signal events and the waiting-signal registry a wait node's
-- until.signal parks on.
--
-- Expand-only (docs/adr/0002-migration-policy.md): two new tables, three
-- new indexes, nothing dropped, renamed, or tightened. An N-1 binary never
-- reads either table and keeps working unchanged.
--
-- Why these are NOT rows in the existing `events` table
-- (migrations/0004_observability.sql): that table is the internal,
-- CloudEvents-shaped audit log — per-aggregate monotonic sequence, written
-- only by this control plane about state changes it committed. A signal
-- event is the opposite thing: an EXTERNAL fact delivered *to* the control
-- plane (a Slack message arrived, a deploy finished, a human said
-- green-light), with no aggregate sequence, an untrusted free-text emitter,
-- and append semantics independent of any run. Folding the two together
-- would either force fake aggregate/sequence values onto external facts or
-- weaken the audit log's per-aggregate ordering contract.
--
-- Forward compatibility (spec decision c35; issue #43): the confirmed graph
-- model makes continuation event-based — a node may emit an event
-- mid-execution and keep working, and any number of nodes (multiple tokens,
-- multiple runs) may pick one event up. This schema is shaped so that
-- neither future needs a schema change:
--
--   * signal_events rows are append-only facts, owned by no subscriber:
--     run_id is optional scope, not a delivery target; emitter is free text
--     (actor/node/external), so a mid-execution emission from a node is
--     just another INSERT with emitter naming the node.
--   * signal_subscriptions carries NO uniqueness over (run_id, event_name)
--     or (event_name) — any number of waiters, across tokens and runs, may
--     subscribe to the same name concurrently, and delivery fires all
--     matching pending rows. One waiter is this batch's single-token reality
--     (non-goal c18), not a schema constraint.
--   * fired_event_id records which fact resumed which waiter, N:1 — one
--     event may fire many subscriptions; each subscription is fired by at
--     most one event.
--
-- Delivery semantics in this pass (documented limitation, issue #43):
-- subscription-then-event resumes; event-then-subscription stays parked. A
-- subscription only matches events delivered AFTER it exists — delivery
-- fires pending subscriptions at append time and never scans history, so a
-- later subscriber does not retroactively consume an earlier fact. Replay /
-- catch-up subscription is issue #43's multi-consumer territory; the
-- append-only event table is what makes it buildable without touching this
-- schema.

-- signal_events: append-only external signal facts.
CREATE TABLE signal_events (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    -- Optional scope: an event delivered for one run resumes only that
    -- run's subscriptions. NULL means namespace-wide.
    run_id        TEXT REFERENCES runs (id),
    name          TEXT NOT NULL,
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Free text naming who emitted this fact (an actor ref, a node id, an
    -- external system). Attribution for operators, not an authority claim:
    -- an inbound event is actor-reported information, never observed
    -- evidence (PRD §10.4).
    emitter       TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX signal_events_namespace_id_idx ON signal_events (namespace_id);
CREATE INDEX signal_events_namespace_name_idx ON signal_events (namespace_id, name);

-- signal_subscriptions: the waiting-signal registry. Mirrors the timers
-- table's waiting discipline (migrations/0002_runtime_execution.sql): a
-- pending row is the ONLY thing that will ever wake its parked node run,
-- status moves pending -> fired | canceled and never back, and run
-- cancellation retires pending rows alongside the run's timers and work
-- items so a dead run's subscription can never fire back to life.
CREATE TABLE signal_subscriptions (
    id              TEXT PRIMARY KEY,
    namespace_id    TEXT NOT NULL REFERENCES namespaces (id),
    run_id          TEXT NOT NULL REFERENCES runs (id),
    node_run_id     TEXT NOT NULL REFERENCES node_runs (id),
    event_name      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    -- The signal_events row whose delivery fired this subscription; NULL
    -- while pending or canceled.
    fired_event_id  TEXT REFERENCES signal_events (id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    fired_at        TIMESTAMPTZ
);

CREATE INDEX signal_subscriptions_namespace_id_idx ON signal_subscriptions (namespace_id);
CREATE INDEX signal_subscriptions_run_id_idx ON signal_subscriptions (run_id);
-- Delivery's matching scan: pending subscriptions for (namespace, name),
-- the same partial-index shape timers_due_idx serves for the scheduler.
CREATE INDEX signal_subscriptions_pending_idx
    ON signal_subscriptions (namespace_id, event_name) WHERE status = 'pending';

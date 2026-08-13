-- 0022_dispatch_rate_state.sql
--
-- The dispatch pacing control's durable state (issue #48 item 2, task t10 of
-- the economy-discord-graphs plan; spec claims c5/c43, honesty conditions
-- h4/h36). Expand-only per docs/adr/0002-migration-policy.md: one new table,
-- nothing dropped, renamed, or tightened, and an N-1 binary that never reads
-- it dispatches exactly as it does today (it simply has no pacing).
--
-- WHY THE RATE STATE IS IN THE DATABASE AT ALL. The obvious implementation of
-- a dispatch rate is an in-memory limiter in the worker loop. It is also
-- wrong here, and provably so from this repo's own comments: "a process may
-- run several, and several processes may run one each"
-- (internal/worker/worker.go's Worker doc comment). N processes each holding
-- their own limiter enforce N times the declared rate, and the operator who
-- declared "ten sessions per window" discovers the real number at the
-- provider's wall. The state that has to be shared is small -- how much this
-- window has consumed, and when the next slot opens -- and PostgreSQL is
-- already the thing every worker shares (§12.3: PostgreSQL is authoritative,
-- the queue is a disposable signal). So the limiter lives here, and the
-- decision is made inside a transaction that holds the row's lock.
--
-- WHY KEYED BY (scope, scope_key) AND NOT BY A REGISTRATION ROW ID. Exactly
-- t9's reasoning for actor_availability (migration 0020), and the two tables
-- are deliberately siblings: capacity and pace both belong to the actor
-- IDENTITY, not to one append-only registration revision of it, and the
-- dispatch site has the actor key in hand unconditionally
-- (internal/worker/registry.go's actorKeyOf over the node's `uses`) while the
-- actors-table row id is best-effort and may be "". A rate that could be
-- bypassed by an unresolvable row id would not be a rate.
--
-- The scope column exists because the control is BOTH global and per actor:
-- 'global' with an empty scope_key is the whole installation's session rate,
-- 'actor' with an actor_key is one actor's. A dispatch consults every scope
-- that applies and needs headroom in all of them. Deliberately no CHECK
-- constraint pins the vocabulary: a later scope (per workflow, per owner)
-- must be an expand-only insert, not a contracting migration on this column.
--
-- WHY THE CONFIGURATION IS STORED ON THE ROW. window_anchor, window_seconds
-- and limit_per_window are not needed to make the next decision -- the
-- deciding worker carries its own configuration. They are here so the
-- OPERATOR surface can render the rate that is actually being enforced
-- (GET /v1alpha1/dispatch-rates) without the API process having to be
-- configured identically to the worker processes and hope. A row therefore
-- reports what the last worker to consume a slot believed the rate was, which
-- is the honest thing to show: if two workers disagree about the configured
-- rate, this column is where that shows up rather than being averaged away.
--
-- CONCURRENCY. Every decision happens inside one transaction that upserts
-- (and thereby row-locks) each scope it consults, in a deterministic scope
-- order, and either writes all of them or none -- see ConsumeDispatchSlots in
-- internal/store/postgres/dispatchrate.go. Two workers racing for the last
-- slot of a window serialize on the row lock: one wins, the other is told to
-- come back at a named instant. A refusal writes nothing at all, so asking is
-- free and idempotent -- a work item deferred by pacing has not spent
-- anything by having been considered.
--
-- WHY A COUNTER AND A DEADLINE RATHER THAN A CLASSIC TOKEN BUCKET. A token
-- bucket refills at a fixed rate and has no idea when the window it is
-- pacing ends. Session windows reset on a fixed clock (spec claim c43), so
-- the arithmetic has to be anchored: window_started_at says which window the
-- counter belongs to, and a row whose window_started_at is older than the
-- current window has consumed nothing in this one -- no sweep, no expiry job,
-- and a worker that has been asleep for a day reaches the same conclusion as
-- one that has been running all along. The arithmetic itself lives in
-- internal/pacing, unit-tested without a database.

CREATE TABLE dispatch_rate_state (
    namespace_id      TEXT        NOT NULL REFERENCES namespaces (id),
    -- 'global' (whole-installation rate) or 'actor' (one actor key's rate).
    scope             TEXT        NOT NULL,
    -- The actor key for scope='actor'; the empty string for 'global'. Empty
    -- rather than NULL so the primary key stays a plain equality lookup and
    -- two global rows can never coexist.
    scope_key         TEXT        NOT NULL,

    -- The configuration the last consuming worker enforced. See the header:
    -- these are for the operator read surface, not for the next decision.
    window_anchor     TIMESTAMPTZ NOT NULL,
    window_seconds    INTEGER     NOT NULL,
    limit_per_window  INTEGER     NOT NULL,

    -- Which window `dispatched` counts. Compared against the window the
    -- anchor arithmetic puts "now" in: older means this window has consumed
    -- nothing yet, whatever the counter says.
    window_started_at TIMESTAMPTZ NOT NULL,
    -- Dispatches admitted in that window. Only ever incremented by an
    -- allowed decision -- a refusal leaves the row untouched.
    dispatched        INTEGER     NOT NULL DEFAULT 0,
    -- The earliest instant the next dispatch in this scope may go: the pace,
    -- made durable. NULL means no dispatch has been admitted yet, which is
    -- distinct from "a slot that opened in the past" and is why this column
    -- is nullable rather than defaulting to the epoch.
    next_dispatch_at  TIMESTAMPTZ,
    -- When the last admitted dispatch went out. Provenance for the operator
    -- surface ("is this rate actually being used"), never read by a decision.
    last_dispatch_at  TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, scope, scope_key)
);

-- No secondary index. The operator read is
-- "WHERE namespace_id = $1 ORDER BY scope, scope_key", which the primary
-- key's own index serves leftmost-first; and the decision path reads exactly
-- one row by full key. An index added here would be an index nothing uses.

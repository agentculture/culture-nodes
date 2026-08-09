-- Parked runner-protocol operations (task t9; prd-spec §12.6, §13.7 and
-- api/runner-protocol).
--
-- This is `actor_invocations` (0009) for the OTHER asynchronous boundary. A
-- code node dispatched to a registered runner SERVICE gets a 202 and nothing
-- else: the operation runs in another process, usually on another machine,
-- and its outcome is learned only by an authenticated GET of the status
-- endpoint. Between those samples §12.6's rule applies exactly as it does to
-- a long-running agent -- "workers must not hold leases or goroutines" -- so
-- no process anywhere holds anything about the operation in memory, and
-- everything a later sample needs to commit state has to survive the worker
-- exiting. That is what this table is.
--
-- Why a second table rather than columns on `actor_invocations`: the two
-- records answer different questions and are woken by different machinery. An
-- actor invocation is resumed by an inbound §13.4 callback carrying a
-- sequence, and its hot query is "what is this deployment blocked on". A
-- runner operation is resumed by an OUTBOUND status sample this runtime
-- initiates, and its hot query is "which operations are due to be sampled
-- now" -- a time-ordered work queue, which is the `(state, next_poll_at)`
-- partial index below and has no counterpart in 0009. Folding both into one
-- table would mean half its columns are NULL for whichever kind wrote the
-- row, and one index serving two access patterns badly.
--
-- Why not the existing `runner_operations` table (0002): that one is the
-- EVIDENCE record -- the typed operation that was sent, the policy digest,
-- and the result document that came back, keyed to a completed attempt. It is
-- written once the outcome is known. This table is the opposite half of the
-- life cycle: the in-flight tracking state that exists precisely while no
-- outcome is known, keyed to the work item and its fencing tuple. Both
-- survive; `runner_operations` still gets its row when the completion commits.
--
-- Expand-contract (docs/adr/0002-migration-policy.md): this migration only
-- adds a table and its indexes. No existing column, table, or constraint
-- changes, so a binary that predates it keeps reading and writing every table
-- it knows about exactly as before -- it simply never looks this one up.
CREATE TABLE runner_invocations (
    -- The PROTOCOL attempt id, exactly as actor_invocations keys on: the
    -- value the worker mints for the attempt, not attempts.id (which does not
    -- exist yet while the operation is in flight).
    attempt_id               TEXT PRIMARY KEY,
    namespace_id             TEXT NOT NULL REFERENCES namespaces (id),
    run_id                   TEXT NOT NULL REFERENCES runs (id),
    node_run_id              TEXT NOT NULL REFERENCES node_runs (id),
    token_id                 TEXT REFERENCES tokens (id),

    -- The fencing tuple exactly as ClaimWork handed it out. These four
    -- columns are the whole reason a completion learned twenty minutes and
    -- two samples later can still be told apart from one learned after the
    -- work was reclaimed and re-run: a resume matches on them or commits
    -- nothing.
    work_id                  TEXT NOT NULL REFERENCES work_items (id),
    worker_id                TEXT NOT NULL,
    fencing_token            BIGINT NOT NULL,
    attempt                  INTEGER NOT NULL,

    node_key                 TEXT NOT NULL,
    -- runner_ref is the registry name the node's operation resolved through.
    -- The registry -- not this row -- stays the enforcement point: a sample
    -- re-resolves the identity by this name, so de-registering a runner stops
    -- the runtime talking to it rather than leaving a row that keeps polling.
    runner_ref               TEXT NOT NULL,
    -- endpoint is recorded for the operator question "where is this running",
    -- and for an egress review that wants it without joining configuration.
    -- It is never a credential: a ServiceIdentity holds only a secret_ref.
    endpoint                 TEXT NOT NULL,
    -- operation_id is what the status endpoint is sampled by. It equals the
    -- attempt id today (a code node's dispatch IS its attempt, so the attempt
    -- id is its natural idempotency key) but is stored separately because the
    -- protocol treats them as different things and a future non-attempt-keyed
    -- operation must not require a migration.
    operation_id             TEXT NOT NULL,

    state                    TEXT NOT NULL DEFAULT 'waiting_external',
    -- observed_state is the last non-terminal state a sample read
    -- ('accepted', 'running'). It carries no claim about progress; it is what
    -- an operator sees when asking whether the runner has picked the work up.
    observed_state           TEXT,

    -- poll_after_seconds is the runner's own requested minimum sampling
    -- interval from its acceptance. Advisory -- cadence is the runtime's
    -- decision -- but sampling faster than a runner asked for is load it said
    -- it did not want.
    poll_after_seconds       INTEGER,
    -- next_poll_at is the due time the sampler claims on. Advancing it is how
    -- a sample is claimed atomically: two samplers racing cannot both take the
    -- same row, and a sampler that dies mid-sample strands nothing, because
    -- the row simply becomes due again.
    next_poll_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    poll_count               BIGINT NOT NULL DEFAULT 0,
    last_sampled_at          TIMESTAMPTZ,
    -- last_sample_error records the most recent classified dispatch failure
    -- (a 404 on a forgotten operation, a throttle, a transport error) so a
    -- wait that is failing to make progress says why without a log dive. It
    -- is a diagnostic, never evidence: nothing was measured.
    last_sample_error        TEXT,

    status_retention_seconds INTEGER,
    supports_cancellation    BOOLEAN NOT NULL DEFAULT false,
    supports_callback        BOOLEAN NOT NULL DEFAULT false,
    -- deadline_timer_id links to the §12.7 timer that fails this attempt if
    -- the operation never reaches a terminal state -- the same
    -- waiting_external deadline protection an asynchronous actor gets.
    deadline_timer_id        TEXT REFERENCES timers (id),

    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX runner_invocations_namespace_id_idx ON runner_invocations (namespace_id);
CREATE INDEX runner_invocations_node_run_id_idx ON runner_invocations (node_run_id);
CREATE INDEX runner_invocations_run_id_idx ON runner_invocations (run_id);

-- The sampler's due scan, and the only query on the hot path: "which parked
-- operations in this namespace are due to be sampled now, oldest first".
-- Partial on the waiting state because a completed operation is never
-- sampled again, so the index stays the size of the in-flight set rather than
-- of history -- the same reasoning as 0009's actor_invocations_waiting_idx,
-- with next_poll_at in place of updated_at because this queue is ordered by
-- when work becomes due, not by when it last changed.
CREATE INDEX runner_invocations_due_idx ON runner_invocations (namespace_id, next_poll_at)
    WHERE state = 'waiting_external';

-- A fired deadline timer names a timer, not an attempt, so the reverse lookup
-- needs its own index -- 0009 reaches actor_invocations the same way.
CREATE INDEX runner_invocations_deadline_timer_idx ON runner_invocations (deadline_timer_id)
    WHERE deadline_timer_id IS NOT NULL;

-- 0058_actor_liveness.sql
--
-- Plan loop-closure-claude-codex, task t10 (spec c23/c26/c33, decision c43's
-- "both" shape, honesty h13/h20): the control plane's authority-of-record for
-- whether an actor's SESSION can start -- as opposed to whether its bridge
-- answers /healthz, which both codex lanes did with a spent refresh token
-- while every dispatch into them died (#308).
--
-- One row per (namespace, actor key), upserted. Three writers, one column
-- each says which:
--
--   source = 'collector'  internal/mesh observed a bridge whose
--                         capabilities.preflight.host block carries a
--                         `liveness` fact (t9) and persisted it here, because
--                         the collector's cache lives in the API process and
--                         the worker is a separate container (spec line 52).
--   source = 'worker'     an attempt failed with outcome class
--                         `credential_spent`; the worker writes
--                         session_ok=false, reason=refresh_token_spent and
--                         LOCKS the row. This is the control-plane half of
--                         decision c43's OR rule: whichever side sees the
--                         spent credential first closes the lane.
--   source = 'resume'     POST /v1alpha1/actors/{id}/resume cleared `locked`.
--                         It does NOT touch session_ok: c43's unlock is an
--                         AND -- a human clears the lock AND a subsequent
--                         collector write reports session_ok=true. Resume
--                         alone never reopens a lane whose last fact is false.
--
-- `locked` is what makes a lock different from a stale fact: the router
-- treats a fresh (< 5 min) session_ok=false as "not live", and a locked row
-- as not live REGARDLESS of age -- a lock does not expire by time, only by
-- the AND above. `session_ok` is nullable on purpose, mirroring the bridge
-- fact: NULL is "nobody measured this", which is a different statement from
-- true or false and must never be collapsed into either.
--
-- Keyed by actor_key rather than actors.id for the same reason 0020's
-- actor_availability is: a credential belongs to the identity, not to one
-- append-only registration revision, and the dispatch site can always
-- produce a key.
--
-- Expand-only (docs/adr/0002-migration-policy.md): a new table nothing
-- existing reads, so a binary built before this migration is unaffected.
CREATE TABLE IF NOT EXISTS actor_liveness (
    namespace_id TEXT        NOT NULL REFERENCES namespaces(id),
    actor_key    TEXT        NOT NULL,
    -- true: the session can start; false: it cannot; NULL: unmeasured.
    session_ok   BOOLEAN,
    -- The bridge's closed reason vocabulary (adapters/*/liveness.py REASONS):
    -- ok, unmeasured, refresh_token_spent, credential_expired,
    -- probe_timeout, probe_failed. Stored as text, not an enum, because the
    -- vocabulary is the bridges' to extend.
    reason       TEXT        NOT NULL,
    -- LOCK or CHECK, the bridge's per-lane detection mode (t9); '' when the
    -- writer was not a bridge fact.
    mode         TEXT        NOT NULL DEFAULT '',
    -- When the fact was measured (the bridge's checked_at, or the worker's
    -- clock for a worker write). The router's freshness window reads this.
    checked_at   TIMESTAMPTZ NOT NULL,
    source       TEXT        NOT NULL CHECK (source IN ('collector', 'worker', 'resume')),
    -- The control-plane lock (decision c43). Set only by a worker write;
    -- cleared only by resume. Collector writes leave it exactly as it is.
    locked       BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Provenance for a worker write: the attempt that discovered the spent
    -- credential. NULL for collector and resume writes.
    locked_by_run_id     TEXT,
    locked_by_attempt_id TEXT,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, actor_key)
);

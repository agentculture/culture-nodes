-- 0026_dispatch_preflights.sql
--
-- The clarify-then-commit gate's durable state (issue #67, task t14 of the
-- upkeep-actors-jira plan; spec claims c18/c53/c54, honesty h10/h36/h39).
--
-- Expand-only per docs/adr/0002-migration-policy.md: one new table plus one
-- new CHECK constraint that can only refuse a shape no existing binary
-- writes. Nothing is dropped, renamed, backfilled, or tightened on a column
-- anything reads today, and an N-1 binary that has never heard of a
-- preflight keeps dispatching exactly as it does now -- it simply never
-- inserts a row here, and the gate is per-actor and default-off, so no actor
-- of an N-1 deployment is waiting for one.
--
-- WHY A TABLE AND NOT JUST THE LEDGER RECORDS. The briefing itself and the
-- actor's acknowledgement are ledger records (`dispatch_preflight`,
-- `dispatch_acknowledgement`) and that is where they belong: they are the
-- evidence this feature exists to create. But ledger records are IMMUTABLE
-- by contract (migration 0003's trigger), and the protocol this generalizes
-- -- deploy/prod/install-secrets.sh's confirmation file, pinned by
-- tests/deploy/destructiveconfirm_test.go -- is single-use: the file is
-- CONSUMED on use, so a second rotation needs a second confirmation.
-- "Consumed" is a mutation, and expressing it by appending a third record
-- would still leave two workers able to read the same acknowledgement as
-- unconsumed at the same instant. This table is where the single-use claim
-- becomes a transactional fact: consuming is a conditional UPDATE, so
-- exactly one dispatch can ever ride one acknowledgement.
--
-- WHY IT IS KEYED PER NODE RUN AND NOT PER ATTEMPT. The gate runs BEFORE an
-- attempt exists: its whole point is that nothing billable has happened yet,
-- and the attempt id a dispatch would carry is minted fresh on every claim
-- of the work item. A preflight therefore belongs to the node run -- the
-- durable unit of "this node, this token, this run" -- and the attempt it
-- eventually authorized is recorded on consumption, after the fact.
--
-- LIFECYCLE, and what each column makes answerable afterwards:
--
--   issued            a row exists, acknowledged_at IS NULL. The worker
--                     DEFERS the work item; the actor was never invoked.
--   acknowledged      acknowledged_at set by POST
--                     /v1alpha1/preflights/{id}/acknowledge, alongside the
--                     proposed `dispatch_acknowledgement` ledger record
--                     whose id is kept here so the row and the evidence
--                     point at each other.
--   consumed          consumed_at set by the dispatch that rode it. Single-
--                     use: the conditional UPDATE only fires while
--                     consumed_at IS NULL.
--   expired           expires_at passed. An expired-unacknowledged row is
--                     history; the dispatch is refused rather than sent, and
--                     a later attempt composes a FRESH briefing rather than
--                     reviving this one. Expiry is a comparison against
--                     now(), never a sweep -- exactly like actor_availability
--                     (migration 0020), and for the same reason: an expired
--                     row stays readable as the record of what was asked.
--
-- record_id / acknowledgement_record_id are plain TEXT rather than foreign
-- keys into ledger_records, matching how migration 0020 refers to runs and
-- attempts: the ledger is append-only and independently retained, and an FK
-- would make this row's validity depend on a retention policy that is not
-- part of this relationship.

CREATE TABLE dispatch_preflights (
    id                        TEXT        PRIMARY KEY,
    namespace_id              TEXT        NOT NULL REFERENCES namespaces (id),
    run_id                    TEXT        NOT NULL,
    node_run_id               TEXT        NOT NULL,
    node_id                   TEXT        NOT NULL,
    -- The actor identity the dispatch is addressed to. The KEY, not the
    -- actors-table row id, for migration 0020's reason: the dispatch site
    -- always has a key in hand, while the row id is best-effort and may be
    -- NULL for a registry that cannot resolve one. actor_id is recorded
    -- beside it when it is known, because attribution is worth having.
    actor_key                 TEXT        NOT NULL,
    actor_id                  TEXT,
    -- The derived `dispatch_preflight` ledger record stating what the actor
    -- was told, and its content digest. The digest is stored here as well so
    -- an acknowledgement can be checked against WHAT the briefing said, not
    -- merely against which briefing it was.
    record_id                 TEXT        NOT NULL,
    record_digest             TEXT        NOT NULL,
    issued_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at                TIMESTAMPTZ NOT NULL,
    acknowledged_at           TIMESTAMPTZ,
    -- Who acknowledged, as the acknowledging party identified itself, and
    -- the proposed ledger record that carries the claim. Both NULL until an
    -- acknowledgement lands; neither is ever written without the other.
    acknowledged_by           TEXT,
    acknowledgement_record_id TEXT,
    consumed_at               TIMESTAMPTZ,
    -- The attempt the consumed acknowledgement authorized. It is what makes
    -- "which dispatch rode this briefing" answerable from the row alone.
    consumed_by_attempt_id    TEXT,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The read the dispatch site makes on every claim of a gated work item: the
-- open (unconsumed) preflight for this node run, newest first.
CREATE INDEX dispatch_preflights_node_run_idx
    ON dispatch_preflights (namespace_id, node_run_id, issued_at DESC);

-- The read a bridge or an operator makes: what is waiting for me to
-- acknowledge. Partial, because a consumed row is never in that answer.
CREATE INDEX dispatch_preflights_pending_idx
    ON dispatch_preflights (namespace_id, actor_key, expires_at)
    WHERE consumed_at IS NULL AND acknowledged_at IS NULL;

-- The third door into the gate's configuration.
--
-- internal/preflight.CheckConfiguration refuses a gate-without-surface
-- registration at both Go-level doors -- the API handler (POST
-- /v1alpha1/actors) and the store's RegisterActor. This constraint covers
-- the one that is neither: raw SQL, which is how deploy/prod's original
-- register-actor.sh wrote rows and how an operator with psql still can.
-- Acceptance criterion 3 says enabling the gate for a bridge that does not
-- advertise the capability surface is refused AT CONFIGURATION TIME; a rule
-- enforced only by the code paths that happen to be used is a convention,
-- not a refusal.
--
-- It is deliberately narrower than the Go check (which also validates the
-- protocol version and requires at least one host fact): a database
-- constraint should refuse the unambiguous case and leave judgement to the
-- layer that can explain itself. Only a literal JSON `true` enables the
-- gate here, matching internal/preflight.ParseGate -- a string "true" is not
-- an enabled gate in either place, so the two never disagree about which
-- rows they are talking about.
--
-- NOT VALID: existing rows are not rescanned. They cannot violate it (no row
-- in any deployment carries a preflight_gate block yet -- this migration is
-- what introduces the concept), and skipping the scan keeps the migration
-- from taking an ACCESS EXCLUSIVE lock for the length of a table scan on an
-- upgrade. Every INSERT and UPDATE from here on is checked.
--
-- The COALESCE is load-bearing, not defensive noise: `jsonb_typeof(NULL)` is
-- NULL, and `false OR NULL` is NULL, which a CHECK constraint ACCEPTS. Left
-- bare, the constraint would pass for exactly the rows it exists to refuse —
-- the ones whose capabilities carry no preflight block at all.
ALTER TABLE actors
    ADD CONSTRAINT actors_preflight_gate_requires_surface
    CHECK (
        metadata #> '{preflight_gate,enabled}' IS DISTINCT FROM 'true'::jsonb
        OR COALESCE(jsonb_typeof(capabilities -> 'preflight') = 'object', false)
    ) NOT VALID;

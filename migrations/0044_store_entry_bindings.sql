-- 0044_store_entry_bindings.sql
--
-- Store pull's actor mapping (issue #192's portability half, plan task t8):
-- a pulled entry's graph pins actor://…@sha256 and runner:// ids minted on
-- the SOURCE plane. Making that entry runnable here means binding each
-- declared capability requirement (store_entries.evidence_manifest.
-- required_capabilities, migration 0042) to a LOCAL registration — and the
-- binding lives HERE, outside the graph document, so the workflow source
-- and its content digest stay byte-identical to the exported original. The
-- mapping is applied where actor refs resolve to registrations (publish
-- resolution and the worker registry), never by rewriting the graph.
--
-- Expand-only (docs/adr/0002-migration-policy.md): one new table, its
-- indexes, nothing dropped, renamed, or tightened. An N-1 binary never
-- reads this table, so it simply has no bindings — the 0042 shape again.
--
-- BINDINGS ARE RECORDS. Rows are insert-only: who bound what to what, when
-- (PRD §10 — records are immutable; corrections append). Re-binding a ref
-- appends a new row rather than updating the old one, and "the current
-- binding" is by definition the newest row for (entry, required_ref) — the
-- same append-only newest-wins shape the actors table uses for revisions.
-- There is therefore deliberately NO unique index on (entry_id,
-- required_ref): the trail of superseded bindings is the point.

CREATE TABLE store_entry_bindings (
    id              TEXT        PRIMARY KEY,
    namespace_id    TEXT        NOT NULL REFERENCES namespaces (id),

    -- The pulled catalog entry whose requirement this binds.
    entry_id        TEXT        NOT NULL REFERENCES store_entries (id),

    -- The graph-pinned identifier, verbatim from the entry's evidence
    -- manifest (actor://…@sha256:…, runner://…). Verbatim on purpose: the
    -- resolver matches the ref the graph actually carries, byte for byte.
    required_ref    TEXT        NOT NULL,
    required_kind   TEXT        NOT NULL CHECK (required_kind IN ('actor', 'runner')),

    -- The local registration the requirement is bound to: the actors row
    -- id current at bind time (a durable "which revision satisfied the
    -- check") plus the key, which is the identity dispatch resolves.
    bound_actor_id  TEXT        NOT NULL REFERENCES actors (id),
    bound_actor_key TEXT        NOT NULL,

    -- Who declared this mapping. Caller-supplied identity, required: a
    -- binding with no author is a mapping nobody stands behind.
    bound_by        TEXT        NOT NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The read surface: all bindings of one entry, newest first.
CREATE INDEX store_entry_bindings_entry_idx
    ON store_entry_bindings (namespace_id, entry_id, created_at DESC);

-- The dispatch-resolution lookup: newest binding for a graph-pinned ref.
CREATE INDEX store_entry_bindings_ref_idx
    ON store_entry_bindings (namespace_id, required_ref, created_at DESC);

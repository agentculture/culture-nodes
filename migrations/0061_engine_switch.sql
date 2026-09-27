-- The global, per-namespace before/shadow/after engine switch (#328 t16;
-- spec c80, ADR 0014 "Consequences", honesty h53). Modeled on 0059's
-- declaration_history: an append-only history is the only record kept, and
-- the current mode is its newest row -- there is deliberately no separate
-- mutable "current state" row to drift out of sync with the flip log, so
-- "every flip is recorded" is true by construction rather than by
-- discipline. A namespace that has never flipped reads as 'before' in Go
-- (internal/declengine/switch.go), the state that exists before this
-- migration is ever exercised.
CREATE TABLE engine_switch_history (
    seq          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id           TEXT NOT NULL UNIQUE,
    namespace_id TEXT NOT NULL REFERENCES namespaces (id),
    mode         TEXT NOT NULL CHECK (mode IN ('before', 'shadow', 'after')),
    -- The deciding principal, never an agent's own completion claim
    -- (PRD §10.4): a human or a deterministic migration runner.
    actor        TEXT NOT NULL CHECK (actor <> ''),
    reason       TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, id)
);
CREATE INDEX engine_switch_history_namespace_seq_idx
    ON engine_switch_history (namespace_id, seq);

-- Reuses 0059's immutable-row trigger function: a flip is a fact once
-- recorded, and a correction is a new flip, never an edit.
CREATE TRIGGER engine_switch_history_immutable
    BEFORE UPDATE OR DELETE ON engine_switch_history
    FOR EACH ROW EXECUTE FUNCTION declaration_immutable_row();

-- Shadow dispatches nothing, so it stamps no origin marker for a later
-- firing's lineage to resolve through (declengine/marker.go's
-- MarkerService.Resolve needs a bound artifact, and shadow binds none).
-- c80 derives lineage from the graph engine's real run for the same event
-- instead: this table is that mapping, one row per would-fire firing,
-- pointing at the graph run (runs.id, keyed by runs.trigger_event_id --
-- migration 0043) that handled the same signal event. t17 (drain) and t18
-- (freeze/replay) read this table; neither writes to it or is implemented
-- by this migration.
CREATE TABLE declaration_shadow_lineage (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces (id),
    firing_id    TEXT NOT NULL,
    graph_run_id TEXT NOT NULL REFERENCES runs (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (namespace_id, firing_id) REFERENCES declaration_firings (namespace_id, id),
    CONSTRAINT declaration_shadow_lineage_firing_key UNIQUE (namespace_id, firing_id)
);
CREATE INDEX declaration_shadow_lineage_graph_run_idx
    ON declaration_shadow_lineage (namespace_id, graph_run_id);

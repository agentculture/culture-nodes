-- 0063_decl_budgets.sql
--
-- Task t11 (#328, trigger-condition-action plan): budgets on the
-- declaration engine at four levels -- a declaration's landing node, the
-- target machine (the actor an action's `with.uses` names), the declaration
-- itself, and every alias that directly contains it. The declared spec
-- (docs/specs/2026-09-27-trigger-condition-action.md, "Budgets can be set at
-- four levels ... Any of them may carry a budget") and ADR 0011 give the
-- exact units this migration and internal/declengine/budget.go reuse
-- unchanged: `max_sessions` (new provider sessions -- cold starts only) and
-- `max_uncached_input` (input tokens the provider did not serve from
-- cache). A migrated ADR-0011 workflow keeps meaning exactly what it always
-- meant; this only widens WHERE a budget of that same shape can attach.
--
-- WHY A NEW SPEND LEDGER AND NOT `run_sessions` (migration 0023). 0023's
-- rows are written only for a run whose COMPILED WORKFLOW declares
-- `spec.budget.maxSessions` -- the declaration engine's worker envelope
-- (internal/declengine/dispatch.go workerEnvelope) never does, so reusing
-- it verbatim would record nothing for any declaration firing. Every
-- declaration firing dispatched through WorkerDispatcher opens a brand-new
-- run (one firing, one run id, never resumed -- dispatch.go's Dispatch is
-- idempotent on the firing id, not a continuation), so unlike a graph
-- workflow's node-by-node dispatch, EVERY successfully dispatched firing is
-- unconditionally one cold-start session: there is no warm/cold decision to
-- make here the way ADR 0011 §4 makes one for a graph run's individual node
-- dispatches. `declaration_budget_spend` therefore has exactly one row per
-- dispatched firing, at most, and a budget's `max_sessions` spend is
-- `count(*)` over the rows that match its scope.
--
-- `max_uncached_input` spend still reads the SAME `attempts` rows ADR
-- 0011's `postgres.RunUncachedInput` reads (the compiled envelope's single
-- "action" node still produces attempts the ordinary way), joined through
-- this ledger's `firing_id` (== `runs.id`) instead of one pinned run id, so
-- a scope's spend sums across every firing charged against it.
CREATE TABLE declaration_budgets (
    id                  TEXT PRIMARY KEY,
    namespace_id        TEXT NOT NULL REFERENCES namespaces (id),
    -- 'node' keys on the declaration's landing_node.name (the node the
    -- blocked dispatch would have opened); 'machine' keys on the rendered
    -- action's with.uses (empty for human.ask, which opens no session and so
    -- can never be machine-budgeted, matching ADR 0011 §2's own scope);
    -- 'declaration' keys on declarations.id; 'alias' keys on
    -- declaration_aliases.name (direct membership only -- nested aliases do
    -- not propagate a budget to their children; that is unresolved
    -- vagueness the plan records, not a decision this migration makes).
    scope               TEXT NOT NULL CHECK (scope IN ('node', 'machine', 'declaration', 'alias')),
    scope_key           TEXT NOT NULL CHECK (scope_key <> ''),
    -- Both nullable and independently optional, exactly like
    -- internal/engine/workflow.go's Budget: a budget need only declare the
    -- axis it means to bound. `minimum 1` at the Go layer keeps zero from
    -- meaning two different things (ADR 0011 §1's "zero is refused, twice");
    -- this table enforces the same positivity so a hand-written row cannot
    -- smuggle a zero-means-unbounded reading past it.
    max_sessions        INTEGER CHECK (max_sessions IS NULL OR max_sessions > 0),
    max_uncached_input  BIGINT CHECK (max_uncached_input IS NULL OR max_uncached_input > 0),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT declaration_budgets_declares_something
        CHECK (max_sessions IS NOT NULL OR max_uncached_input IS NOT NULL),
    UNIQUE (namespace_id, scope, scope_key)
);

-- One row per firing dispatched under a t11 budget check that had headroom
-- (internal/declengine/budget.go chargeBudgetSpend, called immediately
-- BEFORE the dispatcher is invoked -- ADR 0011 §2's conservative
-- over-count direction: a budget that under-counts spends money the author
-- forbade). firing_id doubles as the run id (dispatch.go: "one firing is
-- one run ID"), which is what lets the uncached-input query below join
-- straight through to node_runs/attempts without a second lookup table.
CREATE TABLE declaration_budget_spend (
    firing_id       TEXT PRIMARY KEY REFERENCES declaration_firings (id),
    namespace_id    TEXT NOT NULL REFERENCES namespaces (id),
    node_name       TEXT NOT NULL,
    -- '' when the action carried no with.uses (human.ask): never matches a
    -- machine-scoped budget, which is the correct reading -- there is no
    -- machine this dispatch spent against.
    machine         TEXT NOT NULL DEFAULT '',
    declaration_id  TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX declaration_budget_spend_node_idx
    ON declaration_budget_spend (namespace_id, node_name);
CREATE INDEX declaration_budget_spend_machine_idx
    ON declaration_budget_spend (namespace_id, machine) WHERE machine <> '';
CREATE INDEX declaration_budget_spend_declaration_idx
    ON declaration_budget_spend (namespace_id, declaration_id);

-- Alias membership is a many-to-many fact about the FIRING (a declaration
-- can belong to several aliases at once), so it gets its own junction table
-- rather than a column on declaration_budget_spend.
CREATE TABLE declaration_budget_spend_aliases (
    firing_id  TEXT NOT NULL REFERENCES declaration_budget_spend (firing_id),
    alias_id   TEXT NOT NULL,
    PRIMARY KEY (firing_id, alias_id)
);
CREATE INDEX declaration_budget_spend_aliases_alias_idx
    ON declaration_budget_spend_aliases (alias_id);

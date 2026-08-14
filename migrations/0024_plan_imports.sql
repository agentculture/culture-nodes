-- 0024_plan_imports.sql
--
-- Durable plan/wave/task/deviation state for the generic decompose-pipeline
-- import surface (task t22 of the economy-discord-graphs plan; issue #45,
-- spec claims c10/c15, honesty h7/h11). internal/devague's ParsePlanShow/
-- ParseDeviations translate an external plan's faithful view (devague is
-- instance one; a non-code document is instance two, task t24) into these
-- rows. Expand-only per docs/adr/0002-migration-policy.md: three new
-- tables, nothing dropped or tightened, and an N-1 binary that has never
-- heard of a plan import keeps doing exactly what it did before.
--
-- WHY NOT ledger_records. Three reasons:
--
--   1. ledger_records.run_id references runs(id) (migration 0003). A plan
--      import is not scoped to a run -- the entire point of an import
--      surface is to make a plan browsable and confirmable BEFORE it is
--      ever attached to a run (or, per t24, before "run" is even the right
--      word for the domain being decomposed). Forcing an import through
--      ledger_records would mean minting a fake run row purely to satisfy a
--      foreign key that means something real elsewhere in this schema.
--   2. ledger_records is immutable by DB trigger (migration 0003's
--      ledger_records_no_update/_no_delete) because it is the evidentiary
--      work ledger the PRD's authority model governs (proposed/confirmed/
--      observed/derived, reviewed only through CommitReview). A plan import
--      snapshot is a lighter kind of fact -- "this is what the source
--      system said, as of this import" -- and does not need or want that
--      machinery bolted on.
--   3. internal/devague's Map* functions (MapPlanShow, MapDeviations) DO
--      still produce ledger.Record values for callers that want the
--      ledger's own vocabulary (round-trip/authority tests, a future run
--      that actually imports a plan's tasks as its own work ledger). This
--      schema and those functions are deliberately independent: nothing
--      here reuses a ledger.Record id or is written through
--      internal/ledger's Append path.
--
-- IMMUTABILITY BY CONVENTION, NOT BY TRIGGER. Every import is its own row,
-- inserted once and never updated (internal/store/postgres's plan-import
-- methods expose no UPDATE path at all) -- re-importing the same plan slug
-- again simply inserts a new plan_imports row. This is the "records are
-- immutable; corrections append" discipline CLAUDE.md states for the
-- ledger, applied here by convention rather than by the heavier DB-trigger
-- enforcement ledger_records carries, because this is not the evidentiary
-- ledger the PRD's immutability guarantee is written for (see point 2
-- above) -- there is deliberately no supersedes chain here either; an
-- operator comparing two imports of the same slug does so by imported_at
-- order, the same way any other insert-only table in this schema is read.
CREATE TABLE plan_imports (
    id             TEXT        PRIMARY KEY,
    namespace_id   TEXT        NOT NULL REFERENCES namespaces (id),
    -- The source plan's own slug (devague plan.slug) -- NOT unique on its
    -- own: re-importing the same slug (a plan that kept evolving) is a new
    -- row, not an overwrite. An operator wanting "the current one" reads by
    -- (namespace_id, slug) ORDER BY imported_at DESC LIMIT 1.
    slug           TEXT        NOT NULL,
    title          TEXT        NOT NULL,
    -- The source document/frame this plan was decomposed from (devague
    -- frame_slug) -- generic naming on purpose (see the header): this is
    -- devague's own vocabulary for "the thing the plan came from", not a
    -- code-repo concept, and instance two (t24, a newsletter) reuses the
    -- same column unchanged.
    source_slug    TEXT        NOT NULL,
    -- The source system's own status for the plan as a whole (devague
    -- Plan.status: drafting | converged | exported) -- carried verbatim as
    -- SOURCE status, never translated into a ledger authority value. See
    -- internal/devague/deviations.go's MapDeviations doc comment for the
    -- authority reasoning this column deliberately stays outside of.
    source_status  TEXT        NOT NULL,
    -- sha256 of the raw plan-show JSON bytes this row was imported from --
    -- provenance for "was this a re-import of identical content or a
    -- genuinely changed plan", without building a supersedes chain on top
    -- of it (see the header).
    source_digest  TEXT        NOT NULL,
    imported_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX plan_imports_namespace_id_idx ON plan_imports (namespace_id);
CREATE INDEX plan_imports_namespace_id_slug_idx ON plan_imports (namespace_id, slug, imported_at);

-- plan_import_tasks: one row per task of one plan_imports snapshot, carrying
-- the REAL per-task status and REAL dependency edges (task_ref -> task_ref)
-- devague plan show --json handed over -- never the "depends on the whole
-- previous wave" approximation MapPlanWaves reads from `plan waves --json`
-- (internal/devague/plan.go's doc comment). This table is the durable
-- "task" state t22 is required to ship; wave_index below is the durable
-- "wave" state.
CREATE TABLE plan_import_tasks (
    id                  TEXT    PRIMARY KEY,
    plan_import_id      TEXT    NOT NULL REFERENCES plan_imports (id),
    namespace_id        TEXT    NOT NULL REFERENCES namespaces (id),
    task_ref            TEXT    NOT NULL, -- the source system's own task id, e.g. devague's "t3"
    summary             TEXT    NOT NULL,
    instruction         TEXT    NOT NULL DEFAULT '',
    -- human | agent -- the ledger producer kind the source's own origin
    -- (devague user|llm) maps to (internal/devague's claimOriginKind).
    origin_kind         TEXT    NOT NULL,
    -- The source system's own per-task decision status verbatim (devague
    -- proposed | confirmed | rejected) -- kept as SOURCE status, not
    -- translated into an authority value here either; see plan_imports.
    source_status       TEXT    NOT NULL,
    -- Real per-task dependency edges: a JSON array of OTHER ROWS' task_ref
    -- values within the same plan_import_id, exactly as the source
    -- recorded them (internal/devague.PlanTask.DependsOn) -- never a wave
    -- approximation.
    depends_on          JSONB   NOT NULL DEFAULT '[]'::jsonb,
    -- Computed LOCALLY at import time via topological layering over
    -- depends_on (internal/devague.PlanTask.Wave) -- the source system does
    -- not emit this field itself; NULL for a rejected task, which occupies
    -- no wave (internal/devague/plan_show.go's planTaskWaves).
    wave_index          INT,
    acceptance_criteria JSONB   NOT NULL DEFAULT '[]'::jsonb,
    covers              JSONB   NOT NULL DEFAULT '[]'::jsonb,

    CONSTRAINT plan_import_tasks_plan_import_id_task_ref_key UNIQUE (plan_import_id, task_ref)
);

CREATE INDEX plan_import_tasks_plan_import_id_idx ON plan_import_tasks (plan_import_id);
CREATE INDEX plan_import_tasks_namespace_id_idx ON plan_import_tasks (namespace_id);

-- plan_import_deviations: one row per deviation of one plan_imports
-- snapshot -- the durable home for the origin distinction issue #45 asks
-- for ("system knows" llm vs "user reports" user), carried through exactly
-- as internal/devague.Deviation preserves it.
CREATE TABLE plan_import_deviations (
    id                  TEXT    PRIMARY KEY,
    plan_import_id      TEXT    NOT NULL REFERENCES plan_imports (id),
    namespace_id        TEXT    NOT NULL REFERENCES namespaces (id),
    deviation_ref       TEXT    NOT NULL, -- the source system's own deviation id, e.g. devague's "d1"
    what                TEXT    NOT NULL,
    task_ref            TEXT    NOT NULL, -- the plan_import_tasks.task_ref this deviation relates to
    reason               TEXT   NOT NULL,
    affects              JSONB  NOT NULL DEFAULT '[]'::jsonb,
    -- human | agent, mapped the same way plan_import_tasks.origin_kind is.
    -- This IS the issue #45 split: never inferred, never defaulted, always
    -- the source's own stated origin.
    origin_kind          TEXT   NOT NULL,
    -- The source system's own deviation status verbatim (devague proposed |
    -- approved | rejected -- note "approved", not "confirmed":
    -- internal/devague/types.go's deliveryDeviation doc comment) -- again
    -- SOURCE status, not a manufactured authority value.
    source_status         TEXT  NOT NULL,
    classification         TEXT, -- acceptable | risky | needs-follow-up | NULL

    CONSTRAINT plan_import_deviations_plan_import_id_deviation_ref_key UNIQUE (plan_import_id, deviation_ref)
);

CREATE INDEX plan_import_deviations_plan_import_id_idx ON plan_import_deviations (plan_import_id);
CREATE INDEX plan_import_deviations_namespace_id_idx ON plan_import_deviations (namespace_id);

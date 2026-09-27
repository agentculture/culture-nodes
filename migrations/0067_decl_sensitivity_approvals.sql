-- 0067_decl_sensitivity_approvals.sql
--
-- Task t30 (#328): variable sensitivity and owner-approved widening (spec
-- owner decision q22; ADR 0014 loop/safety limits;
-- internal/decl/sensitivity.go, internal/declengine/sensitivity.go).
--
-- A firing whose action would render a variable into a system with a wider
-- audience than the variable came from is recorded as 'sensitivity-blocked'
-- and not dispatched until the variable's owner approves. The approval is a
-- human inbox task addressed to that owner.
--
-- WHY NOT human_tasks (0002). A human_tasks row belongs to a graph run
-- (run_id NOT NULL REFERENCES runs) and is resolved by UPDATE-ing its status.
-- A blocked firing has no run -- it is refused before it is claimed -- and
-- the task's own acceptance criterion is that approval and refusal are
-- APPEND-ONLY records. So:
--
--   declaration_sensitivity_approvals  one immutable task per (declaration
--     version, source version, variable, target system) -- the unique key is
--     what makes repeated blocked events open exactly one task. `owner` is
--     the author of the source declaration version (the version whose firing,
--     or whose trigger, produced the variable).
--   declaration_sensitivity_decisions  the append-only answers. The newest
--     decision is the task's state; a correction supersedes the previous
--     head, and the partial unique index plus UNIQUE (supersedes_id) keep the
--     chain linear.
CREATE TABLE declaration_sensitivity_approvals (
    id                    TEXT PRIMARY KEY,
    namespace_id          TEXT NOT NULL REFERENCES namespaces (id),
    declaration_id        TEXT NOT NULL,
    declaration_version   TEXT NOT NULL,
    source_declaration_id TEXT NOT NULL,
    source_version        TEXT NOT NULL,
    variable              TEXT NOT NULL CHECK (variable <> ''),
    source_system         TEXT NOT NULL,
    source_audience       TEXT NOT NULL,
    target_system         TEXT NOT NULL,
    target_audience       TEXT NOT NULL,
    owner                 TEXT NOT NULL CHECK (owner <> ''),
    first_event_id        TEXT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, id),
    UNIQUE (namespace_id, declaration_version, source_version, variable, target_system),
    FOREIGN KEY (namespace_id, declaration_version) REFERENCES declaration_versions (namespace_id, id),
    FOREIGN KEY (namespace_id, source_version) REFERENCES declaration_versions (namespace_id, id)
);
CREATE INDEX declaration_sensitivity_approvals_owner_idx
    ON declaration_sensitivity_approvals (namespace_id, owner, created_at);

CREATE TABLE declaration_sensitivity_decisions (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    approval_id   TEXT NOT NULL,
    decision      TEXT NOT NULL CHECK (decision IN ('approved', 'refused')),
    decider       TEXT NOT NULL CHECK (decider <> ''),
    note          TEXT NOT NULL DEFAULT '',
    supersedes_id TEXT UNIQUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, id),
    FOREIGN KEY (namespace_id, approval_id) REFERENCES declaration_sensitivity_approvals (namespace_id, id),
    FOREIGN KEY (namespace_id, supersedes_id) REFERENCES declaration_sensitivity_decisions (namespace_id, id)
);
CREATE UNIQUE INDEX declaration_sensitivity_decisions_first_key
    ON declaration_sensitivity_decisions (approval_id) WHERE supersedes_id IS NULL;
CREATE INDEX declaration_sensitivity_decisions_approval_idx
    ON declaration_sensitivity_decisions (namespace_id, approval_id, created_at DESC, id DESC);

-- Same immutability 0059 gives the declaration catalog.
CREATE TRIGGER declaration_sensitivity_approvals_immutable BEFORE UPDATE OR DELETE ON declaration_sensitivity_approvals
    FOR EACH ROW EXECUTE FUNCTION declaration_immutable_row();
CREATE TRIGGER declaration_sensitivity_decisions_immutable BEFORE UPDATE OR DELETE ON declaration_sensitivity_decisions
    FOR EACH ROW EXECUTE FUNCTION declaration_immutable_row();

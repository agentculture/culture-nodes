-- 0068_decl_exposure_approvals.sql
--
-- Task t30b (#328, owner decision d4; ADR 0014 addendum "Sensitivity:
-- per-repository audience, per-variable exposure";
-- internal/decl/sensitivity.go, internal/decl/exposure.go,
-- internal/declengine/sensitivity.go).
--
-- Two changes to 0067's model:
--
-- 1. Exposure is approved PER VARIABLE, through a declaration's `exposes`
--    list, not per (declaration version, source version, variable, target
--    system). One task per (declaration NAME, variable, owner):
--
--      declaration_exposure_approvals  one immutable task per key. `variable`
--        is the exposes entry exactly as the list writes it (`summary`,
--        `1:summary`, `jira-intake:reporter`). `owner` is the author of the
--        declaration version that produced the variable, resolved at firing
--        time; a new author publishing that producing declaration is a new
--        owner, so a new key and a new task -- the old approval does not
--        carry over. Republishing the FIRING declaration keeps its name, so
--        its approved entries stand. The declaration/source version, systems
--        and event are the first blocked firing's, kept for the owner to
--        read; they are not part of the key.
--      declaration_exposure_decisions  the append-only answers, exactly as
--        0067's: the newest decision is the task's state, each correction
--        supersedes the previous head.
--
--    Withdrawal needs no row of its own: an approval stands only while every
--    version of the declaration published after the approving decision still
--    lists the entry (declengine derives `withdrawn` from
--    declaration_versions.body->'exposes'), so removing an entry withdraws
--    it for good; relisting it needs the owner again.
--
-- 2. GitHub's audience is decided per repository. repository_visibility is
--    the namespace's own record of which repositories are public or private,
--    set by an operator or actor and read by the engine at firing time
--    without calling GitHub. Append-only: the newest row per repository is
--    its visibility, and the history says who changed it. A repository with
--    no row is unknown, which fails closed (public as a target, org as a
--    source).
--
-- RETIRING 0067. declaration_sensitivity_approvals / _decisions keyed
-- consent to one declaration version. Migrating those rows into the new
-- per-name key would silently widen what an owner agreed to (an approval of
-- v1 would cover every later version), so they are NOT migrated: they stay
-- as read-only history (the tables were new in this unreleased build and
-- hold no production data), grant nothing, and refuse new rows, so nothing
-- can keep writing the old model by mistake.

CREATE FUNCTION declaration_sensitivity_retired_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'retired by 0068: sensitivity approvals are per declaration name and variable (declaration_exposure_approvals)';
END;
$$;
CREATE TRIGGER declaration_sensitivity_approvals_retired BEFORE INSERT ON declaration_sensitivity_approvals
    FOR EACH ROW EXECUTE FUNCTION declaration_sensitivity_retired_row();
CREATE TRIGGER declaration_sensitivity_decisions_retired BEFORE INSERT ON declaration_sensitivity_decisions
    FOR EACH ROW EXECUTE FUNCTION declaration_sensitivity_retired_row();
COMMENT ON TABLE declaration_sensitivity_approvals IS 'Retired by 0068 (t30b): per-version consent, read-only history; see declaration_exposure_approvals.';
COMMENT ON TABLE declaration_sensitivity_decisions IS 'Retired by 0068 (t30b): read-only history; see declaration_exposure_decisions.';

CREATE TABLE declaration_exposure_approvals (
    id                    TEXT PRIMARY KEY,
    namespace_id          TEXT NOT NULL REFERENCES namespaces (id),
    declaration_name      TEXT NOT NULL CHECK (declaration_name <> ''),
    variable              TEXT NOT NULL CHECK (variable <> ''),
    owner                 TEXT NOT NULL CHECK (owner <> ''),
    declaration_id        TEXT NOT NULL,
    declaration_version   TEXT NOT NULL,
    source_declaration_id TEXT NOT NULL,
    source_version        TEXT NOT NULL,
    source_system         TEXT NOT NULL,
    source_audience       TEXT NOT NULL,
    target_system         TEXT NOT NULL,
    target_audience       TEXT NOT NULL,
    first_event_id        TEXT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, id),
    UNIQUE (namespace_id, declaration_name, variable, owner),
    FOREIGN KEY (namespace_id, declaration_id) REFERENCES declarations (namespace_id, id),
    FOREIGN KEY (namespace_id, declaration_version) REFERENCES declaration_versions (namespace_id, id),
    FOREIGN KEY (namespace_id, source_version) REFERENCES declaration_versions (namespace_id, id)
);
CREATE INDEX declaration_exposure_approvals_owner_idx
    ON declaration_exposure_approvals (namespace_id, owner, created_at);

CREATE TABLE declaration_exposure_decisions (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    approval_id   TEXT NOT NULL,
    decision      TEXT NOT NULL CHECK (decision IN ('approved', 'refused')),
    decider       TEXT NOT NULL CHECK (decider <> ''),
    note          TEXT NOT NULL DEFAULT '',
    supersedes_id TEXT UNIQUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (namespace_id, id),
    FOREIGN KEY (namespace_id, approval_id) REFERENCES declaration_exposure_approvals (namespace_id, id),
    FOREIGN KEY (namespace_id, supersedes_id) REFERENCES declaration_exposure_decisions (namespace_id, id)
);
CREATE UNIQUE INDEX declaration_exposure_decisions_first_key
    ON declaration_exposure_decisions (approval_id) WHERE supersedes_id IS NULL;
CREATE INDEX declaration_exposure_decisions_approval_idx
    ON declaration_exposure_decisions (namespace_id, approval_id, created_at DESC, id DESC);

CREATE TABLE repository_visibility (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces (id),
    repository   TEXT NOT NULL CHECK (repository ~ '^[a-z0-9_.-]+/[a-z0-9_.-]+$'),
    visibility   TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
    set_by       TEXT NOT NULL CHECK (set_by <> ''),
    note         TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (namespace_id, id)
);
CREATE INDEX repository_visibility_current_idx
    ON repository_visibility (namespace_id, repository, created_at DESC, id DESC);

-- Same immutability 0059 gives the declaration catalog.
CREATE TRIGGER declaration_exposure_approvals_immutable BEFORE UPDATE OR DELETE ON declaration_exposure_approvals
    FOR EACH ROW EXECUTE FUNCTION declaration_immutable_row();
CREATE TRIGGER declaration_exposure_decisions_immutable BEFORE UPDATE OR DELETE ON declaration_exposure_decisions
    FOR EACH ROW EXECUTE FUNCTION declaration_immutable_row();
CREATE TRIGGER repository_visibility_immutable BEFORE UPDATE OR DELETE ON repository_visibility
    FOR EACH ROW EXECUTE FUNCTION declaration_immutable_row();

-- Bind each firing to its namespace's catalog and to a version of the same
-- declaration (0060 deferred these until the 0059 catalog had landed). The
-- component digests remain the immutable per-firing pin; the version FK only
-- guarantees that the pinned version exists and belongs to that declaration.
ALTER TABLE declaration_versions
    ADD CONSTRAINT declaration_versions_namespace_declaration_id_key
    UNIQUE (namespace_id, declaration_id, id);
ALTER TABLE declaration_firings
    ADD CONSTRAINT declaration_firings_declaration_fk
    FOREIGN KEY (namespace_id, declaration_id)
    REFERENCES declarations (namespace_id, id),
    ADD CONSTRAINT declaration_firings_version_fk
    FOREIGN KEY (namespace_id, declaration_id, declaration_version)
    REFERENCES declaration_versions (namespace_id, declaration_id, id);

-- The variables a fired evaluation exposes to later declarations in its
-- lineage: the trigger event's variables plus any the action returned
-- synchronously. NULL on every other outcome, so `reason` stays prose.
ALTER TABLE declaration_evaluations ADD COLUMN variables JSONB;

-- Lineage resolves each ancestor's variables from the newest fired
-- evaluation among the physical firings (re-mints) of one logical entry.
CREATE INDEX declaration_firings_canonical_idx
    ON declaration_firings (namespace_id, canonical_firing_id);
CREATE INDEX declaration_evaluations_firing_result_idx
    ON declaration_evaluations (namespace_id, firing_id, created_at DESC, id DESC)
    WHERE outcome = 'fired';

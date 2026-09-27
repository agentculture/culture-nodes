-- Immutable declaration versions and an append-only account of authority changes.
CREATE TABLE declarations (
    id TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id),
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, name),
    UNIQUE (namespace_id, id)
);

CREATE TABLE declaration_versions (
    id TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL,
    declaration_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    digest TEXT NOT NULL,
    body JSONB NOT NULL,
    author TEXT NOT NULL CHECK (author <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (namespace_id, declaration_id) REFERENCES declarations(namespace_id, id),
    UNIQUE (namespace_id, id),
    UNIQUE (declaration_id, version),
    UNIQUE (declaration_id, digest)
);

CREATE TABLE declaration_links (
    id TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL,
    from_declaration_id TEXT NOT NULL,
    to_declaration_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('must', 'can')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (namespace_id, from_declaration_id) REFERENCES declarations(namespace_id, id),
    FOREIGN KEY (namespace_id, to_declaration_id) REFERENCES declarations(namespace_id, id),
    UNIQUE (namespace_id, from_declaration_id, to_declaration_id, kind)
);

CREATE TABLE declaration_aliases (
    id TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id),
    name TEXT NOT NULL,
    parent_alias_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, name),
    UNIQUE (namespace_id, id),
    FOREIGN KEY (namespace_id, parent_alias_id) REFERENCES declaration_aliases(namespace_id, id)
);

CREATE TABLE declaration_alias_members (
    namespace_id TEXT NOT NULL,
    alias_id TEXT NOT NULL,
    declaration_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (alias_id, declaration_id),
    FOREIGN KEY (namespace_id, alias_id) REFERENCES declaration_aliases(namespace_id, id),
    FOREIGN KEY (namespace_id, declaration_id) REFERENCES declarations(namespace_id, id)
);

CREATE TABLE declaration_history (
    seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id TEXT NOT NULL UNIQUE,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id),
    kind TEXT NOT NULL CHECK (kind IN ('activate', 'deactivate', 'alias_move')),
    target_version_id TEXT NOT NULL,
    actor TEXT NOT NULL CHECK (actor <> ''),
    supersedes_id TEXT,
    alias_name TEXT,
    old_parent_name TEXT,
    new_parent_name TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, id),
    FOREIGN KEY (namespace_id, target_version_id) REFERENCES declaration_versions(namespace_id, id),
    FOREIGN KEY (namespace_id, supersedes_id) REFERENCES declaration_history(namespace_id, id),
    CHECK ((kind = 'alias_move' AND alias_name IS NOT NULL) OR
           (kind <> 'alias_move' AND alias_name IS NULL))
);
CREATE INDEX declaration_history_namespace_seq_idx ON declaration_history(namespace_id, seq);

CREATE FUNCTION declaration_immutable_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'declaration records are immutable';
END;
$$;
CREATE TRIGGER declaration_versions_immutable BEFORE UPDATE OR DELETE ON declaration_versions
    FOR EACH ROW EXECUTE FUNCTION declaration_immutable_row();
CREATE TRIGGER declarations_immutable BEFORE UPDATE OR DELETE ON declarations
    FOR EACH ROW EXECUTE FUNCTION declaration_immutable_row();
CREATE TRIGGER declaration_history_immutable BEFORE UPDATE OR DELETE ON declaration_history
    FOR EACH ROW EXECUTE FUNCTION declaration_immutable_row();

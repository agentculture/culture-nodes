-- 0001_namespaces_and_identity.sql
--
-- Installation/tenant boundary plus the identity and workflow-authoring
-- tables that everything else in the schema hangs off of (prd-spec §14).
--
-- Expand-contract policy (docs/adr/0002-migration-policy.md): this migration
-- only adds tables, columns, and indexes. Nothing here is ever dropped or
-- renamed in place.

-- namespaces: installation and tenant boundary. Every other operational
-- table in this schema carries namespace_id from this migration onward,
-- even though today's deployment profile exposes a single namespace.
CREATE TABLE namespaces (
    id           TEXT PRIMARY KEY,
    slug         TEXT NOT NULL,
    display_name TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT namespaces_slug_key UNIQUE (slug)
);

-- owners: human/team ownership references (prd-spec §9.4 ownership).
CREATE TABLE owners (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    kind          TEXT NOT NULL CHECK (kind IN ('human', 'team')),
    external_ref  TEXT,
    display_name  TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT owners_namespace_external_ref_key UNIQUE (namespace_id, external_ref)
);

CREATE INDEX owners_namespace_id_idx ON owners (namespace_id);

-- actors: actor identities and immutable revisions (prd-spec §9.5). A new
-- capability or endpoint change is a new row (revision), never an update to
-- an existing one -- actor identity, like ledger records, is append-only.
CREATE TABLE actors (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    actor_key     TEXT NOT NULL,
    revision      INTEGER NOT NULL,
    kind          TEXT NOT NULL,
    protocol      TEXT NOT NULL,
    endpoint_ref  TEXT,
    capabilities  JSONB NOT NULL DEFAULT '{}'::jsonb,
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT actors_namespace_key_revision_key UNIQUE (namespace_id, actor_key, revision)
);

CREATE INDEX actors_namespace_id_idx ON actors (namespace_id);
CREATE INDEX actors_namespace_actor_key_idx ON actors (namespace_id, actor_key);

-- workflow_drafts: mutable authoring state (prd-spec §14). Drafts are the
-- only mutable definition surface; publishing snapshots a draft into an
-- immutable workflow_versions row.
CREATE TABLE workflow_drafts (
    id             TEXT PRIMARY KEY,
    namespace_id   TEXT NOT NULL REFERENCES namespaces (id),
    workflow_key   TEXT NOT NULL,
    owner_id       TEXT REFERENCES owners (id),
    source_format  TEXT NOT NULL CHECK (source_format IN ('yaml', 'json')),
    source         TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT workflow_drafts_namespace_key_key UNIQUE (namespace_id, workflow_key)
);

CREATE INDEX workflow_drafts_namespace_id_idx ON workflow_drafts (namespace_id);

-- workflow_versions: immutable source, normalized IR, and content digest
-- (prd-spec §11.3). Publishing a definition inserts a row here; it is never
-- updated or deleted. The digest is unique per namespace so an identical
-- definition always resolves to the same immutable version.
CREATE TABLE workflow_versions (
    id                     TEXT PRIMARY KEY,
    namespace_id           TEXT NOT NULL REFERENCES namespaces (id),
    workflow_key           TEXT NOT NULL,
    version                INTEGER NOT NULL,
    draft_id               TEXT REFERENCES workflow_drafts (id),
    owner_id               TEXT REFERENCES owners (id),
    source_format          TEXT NOT NULL CHECK (source_format IN ('yaml', 'json')),
    source                 TEXT NOT NULL,
    normalized_ir          JSONB NOT NULL,
    content_digest         TEXT NOT NULL,
    published_by_actor_id  TEXT REFERENCES actors (id),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT workflow_versions_namespace_digest_key UNIQUE (namespace_id, content_digest),
    CONSTRAINT workflow_versions_namespace_key_version_key UNIQUE (namespace_id, workflow_key, version)
);

CREATE INDEX workflow_versions_namespace_id_idx ON workflow_versions (namespace_id);
CREATE INDEX workflow_versions_namespace_key_idx ON workflow_versions (namespace_id, workflow_key);

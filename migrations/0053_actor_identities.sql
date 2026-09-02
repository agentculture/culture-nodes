-- 0053_actor_identities.sql
--
-- Durable SSO/service-principal bindings to registered actors. This is an
-- expand-only migration: the previous binary neither reads nor writes this
-- new table. Presented credentials and email claims are deliberately absent;
-- the Cloudflare provider's stable subject is the identity key.
CREATE TABLE actor_identities (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces (id),
    provider     TEXT NOT NULL,
    subject      TEXT NOT NULL,
    actor_id     TEXT NOT NULL REFERENCES actors (id),
    roles        TEXT[] NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ,

    CONSTRAINT actor_identities_provider_check CHECK (
        provider IN ('cloudflare-access', 'cloudflare-service-token')
    ),
    CONSTRAINT actor_identities_subject_nonempty CHECK (subject <> ''),
    CONSTRAINT actor_identities_roles_check CHECK (
        roles <@ ARRAY['viewer', 'approver', 'namespace_administrator']::TEXT[]
    )
);

-- Uniqueness applies to the live binding. Revoked rows remain as history, and
-- rebinding appends a new row instead of mutating that history.
CREATE UNIQUE INDEX actor_identities_live_key
    ON actor_identities (namespace_id, provider, subject)
    WHERE revoked_at IS NULL;

CREATE INDEX actor_identities_actor_id_idx ON actor_identities (actor_id);

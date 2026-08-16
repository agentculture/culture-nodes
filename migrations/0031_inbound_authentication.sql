-- 0031_inbound_authentication.sql
--
-- The inbound half of the same no-secret-at-rest rule DBRegistry applies to
-- actors.metadata.auth_token_env. A dump contains only a one-way SHA-256
-- verifier or the NAME of an environment variable whose value is read at use
-- time. Separate nullable columns plus the exactly-one CHECK leave no generic
-- text slot in which a caller could accidentally persist plaintext.
--
-- EXPIRY (#111): this simple record is acceptable only until the dial-in path
-- accepts its first connection; that dial-in event replaces it with #111's
-- per-actor authentication and authorization model.
CREATE TABLE inbound_authentication (
    party_kind         TEXT NOT NULL
                       CHECK (party_kind IN ('actor', 'host')),
    party_key          TEXT NOT NULL,
    verifier_sha256    BYTEA,
    verifier_env_name  TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (party_kind, party_key),
    CONSTRAINT inbound_authentication_one_verifier CHECK (
        (verifier_sha256 IS NULL) <> (verifier_env_name IS NULL)
    ),
    CONSTRAINT inbound_authentication_sha256_width CHECK (
        verifier_sha256 IS NULL OR octet_length(verifier_sha256) = 32
    ),
    CONSTRAINT inbound_authentication_env_name CHECK (
        verifier_env_name IS NULL
        OR verifier_env_name ~ '^[A-Z_][A-Z0-9_]*$'
    ),
    -- Every IPv6 spelling contains a colon. The dotted-quad rejection is
    -- intentionally shape-based (not only 0..255): an address-like typo must
    -- not become durable identity merely because one octet was invalid.
    CONSTRAINT inbound_authentication_key_is_not_address CHECK (
        party_key !~ '^[0-9]{1,3}(\.[0-9]{1,3}){3}$'
        AND party_key !~ ':'
    ),
    CONSTRAINT inbound_authentication_key_shape CHECK (
        (party_kind = 'actor' AND party_key ~ '^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)+$')
        OR
        (party_kind = 'host' AND party_key ~ '^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$')
    )
);

-- Durable mailbox for reverse actor transport. PostgreSQL is authoritative;
-- bridge long polls are only disposable wakeups and no peer address is kept.
CREATE TABLE inbound_actor_presence (
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    actor_key TEXT NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (namespace_id, actor_key)
);

CREATE TABLE inbound_actor_mailbox (
    id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    actor_key TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    request JSONB NOT NULL,
    response JSONB,
    response_status INTEGER,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at TIMESTAMPTZ,
    claim_until TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    UNIQUE (namespace_id, attempt_id),
    CHECK ((response IS NULL) = (response_status IS NULL)),
    CHECK ((claimed_at IS NULL) = (claim_until IS NULL))
);
CREATE INDEX inbound_actor_mailbox_pickup
    ON inbound_actor_mailbox (namespace_id, actor_key, created_at)
    WHERE completed_at IS NULL;

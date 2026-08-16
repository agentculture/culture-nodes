-- Explicit revocation and credential-adjacent admission state. A non-null
-- revoked_at is positive durable evidence; deletion is not revocation because
-- restore/replay could recreate a usable verifier. No column can retain a
-- presented value: only counts and instants are written on failed dials.
ALTER TABLE inbound_authentication
    ADD COLUMN revoked_at TIMESTAMPTZ,
    ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN locked_until TIMESTAMPTZ,
    ADD COLUMN rate_window_started_at TIMESTAMPTZ,
    ADD COLUMN rate_attempt_count INTEGER NOT NULL DEFAULT 0,
    ADD CONSTRAINT inbound_authentication_failure_count_nonnegative
        CHECK (failure_count >= 0),
    ADD CONSTRAINT inbound_authentication_rate_attempt_count_nonnegative
        CHECK (rate_attempt_count >= 0),
    ADD CONSTRAINT inbound_authentication_rate_window_pair CHECK (
        (rate_window_started_at IS NULL AND rate_attempt_count = 0)
        OR rate_window_started_at IS NOT NULL
    );

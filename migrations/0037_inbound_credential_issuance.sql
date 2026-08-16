-- 0037_inbound_credential_issuance.sql
--
-- Issuance provenance for the dial-in credential record 0031 created and
-- 0032 gave revocation, lockout and rate state (issue #111's dial-in half,
-- decided for issue #136). The gap those two left is not storage,
-- verification or admission control — it is MINTING: until now the value a
-- bridge presented was operator-invented and the control plane only ever
-- learned a digest of it, so "who issued this identity" had no durable
-- answer and 0031's own EXPIRY note could not be discharged.
--
-- These two columns are that answer, and they are the only two facts a
-- credential's issuance produces that are safe to keep: an INSTANT and a
-- COUNT. The posture 0031 and 0032 set is unchanged — no column here can
-- retain a presented value, and nothing about a failed dial is recorded
-- beyond the counters 0032 already added.
--
--   issued_at       non-null iff this control plane minted the credential
--                   (internal/actors.MintInboundCredential). NULL means the
--                   row was provisioned by hand, which the shipped admission
--                   default now refuses as `not_control_plane_issued`.
--   issuance_count  how many times a credential has been minted for this
--                   party. A reissue REPLACES the verifier — the previous
--                   plaintext is unrecoverable, since only its digest was
--                   ever stored and that digest is overwritten.
--
-- Expand-only, so an N-1 binary is unaffected: it neither writes nor reads
-- these columns, and every row it inserts takes the defaults (NULL / 0),
-- which is honestly "not issued by the control plane".
ALTER TABLE inbound_authentication
    ADD COLUMN issued_at      TIMESTAMPTZ,
    ADD COLUMN issuance_count INTEGER NOT NULL DEFAULT 0,
    ADD CONSTRAINT inbound_authentication_issuance_count_nonnegative
        CHECK (issuance_count >= 0),
    -- Issued and never-issued are the only two states; a half-written
    -- provenance would let an unissued record look issued.
    ADD CONSTRAINT inbound_authentication_issuance_pair CHECK (
        (issued_at IS NULL) = (issuance_count = 0)
    ),
    -- An issued credential is a minted secret, so it is always stored as a
    -- SHA-256 verifier. An environment-variable reference is by definition
    -- operator-provisioned and can never claim issuance.
    ADD CONSTRAINT inbound_authentication_issued_is_digest CHECK (
        issued_at IS NULL OR verifier_sha256 IS NOT NULL
    );

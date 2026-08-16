-- CONTRACT MIGRATION -- HUMAN-APPROVED ADR 0002 BYPASS (close-the-backlog t24).
--
-- ADR 0002 normally requires this drop to wait one full release after the
-- last binary that reads or writes these columns.  Expand-contract protects a
-- rolling fleet: it prevents an N-1 binary from meeting a schema it cannot
-- read.  Production here is not a rolling fleet; it is exactly two workers
-- and one API, and deploy/prod/deploy.sh restarts all three together.  The
-- human-approved bypass therefore permits this contract step in the same
-- release as the code that stops using the columns.
--
-- This exception does not generalise.  If the fleet ever grows past what one
-- deploy.sh operation can restart, the next contract step needs the full
-- expand-contract sequence and N-1 compatibility required by ADR 0002.
--
-- EXECUTION PRECONDITION: do not apply this migration while mixed mode is in
-- use.  docs/decisions/transport-inversion.md retains endpoint_ref as the
-- outbound fallback.  Every bridge must first be converted to authenticated
-- dial-in, and outbound fallback must be disabled.  Applying this migration
-- before that cutover removes the rollback path on which mixed mode depends.

ALTER TABLE actors DROP COLUMN endpoint_ref;
ALTER TABLE runner_invocations DROP COLUMN endpoint;

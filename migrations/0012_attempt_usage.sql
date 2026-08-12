-- 0012_attempt_usage.sql
--
-- Expand-only: adds nullable per-attempt usage telemetry (prd-spec §13.2)
-- to `attempts`. Both completion seams that hand the engine an actor's
-- report -- internal/worker/dispatch.go's synchronous completeFromResult
-- and internal/actors/callback.go's async commitTerminal -- can now carry
-- the Usage block the actor's InvocationResult or CompletedPayload
-- reported (internal/actors/protocol.go's Usage type). Before this
-- migration the block was decoded off the wire and then silently
-- dropped: no non-test code consumed it. Task t2 aggregates these columns
-- into node-run/run rollups; this migration only adds the storage.
--
-- All four columns are nullable with no default, and no single "usage
-- reported" boolean stands in for them: an attempt that never reported
-- usage leaves all four NULL end to end -- no fabricated zero, no
-- backfill for the attempts recorded before this shipped -- and
-- usage_cost / usage_currency stay independently nullable from
-- usage_input_tokens / usage_output_tokens because §13.2 shows Cost and
-- Currency as nullable on their own: an actor that reports token counts
-- but does not price its work says so with a null cost, not a zero that
-- reads as "free" (see actors.Usage's doc comment). The engine writes
-- usage_input_tokens and usage_output_tokens together whenever an
-- attempt carries a Usage block at all (they are not independently
-- nullable from each other), so `usage_input_tokens IS NOT NULL` is what
-- "this attempt reported usage" means downstream.
--
-- N-1 compatibility (docs/adr/0002-migration-policy.md): a binary built
-- before this migration existed still inserts attempts with its original
-- fixed column list (internal/store/postgres/engine_store.go's
-- insertAttemptSQL names every column explicitly, never `INSERT INTO
-- attempts ...` bare) and still selects with its original column list,
-- so four new nullable columns with no default change nothing it reads
-- or writes.
ALTER TABLE attempts ADD COLUMN usage_input_tokens BIGINT;
ALTER TABLE attempts ADD COLUMN usage_output_tokens BIGINT;
ALTER TABLE attempts ADD COLUMN usage_cost DOUBLE PRECISION;
ALTER TABLE attempts ADD COLUMN usage_currency TEXT;

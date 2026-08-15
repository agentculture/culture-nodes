-- 0029_attempt_late_callback_delivery.sql
--
-- Expand-only: the delivery identity of the late §13.4 callback an attempts
-- row records, so appending that row is idempotent in BOTH flavours of
-- lateness (docs/adr/0012-late-callback-supersession.md §5).
--
-- The gap this closes: 0028 made the reconciliation idempotent with a partial
-- UNIQUE index over `supersedes`, and a partial index `WHERE supersedes IS
-- NOT NULL` constrains nothing when the column is NULL. A reconciliation that
-- finds no earlier record under its dispatch's fencing tuple — the "a newer
-- worker reclaimed the item" flavour of lateness — writes exactly that NULL.
-- §13.4 delivery is at-least-once and the ingest deliberately re-processes a
-- report whose first pass failed part-way (internal/actors/callback.go's
-- HandleCallback releases the event-id claim and rolls back the sequence mark
-- on every error out of handleClaimed, and late()'s CloseInvocation can fail
-- after the append), so the same report could be persisted twice: two attempt
-- rows for one session, neither superseding the other, inflating exactly the
-- retry burn ADR 0012 is otherwise careful to keep honest.
--
-- late_callback_attempt_id: the §13.1 PROTOCOL attempt id
--   (actor_invocations.attempt_id) whose report this row records. It is not
--   attempts.id and not a foreign key to it -- the two identifier spaces are
--   deliberately separate (see 0009's header), and an attempts row must
--   outlive any cleanup of the invocation row it came from.
-- late_callback_event_id: the §13.4 callback event id of the delivery that
--   produced this row.
--
-- Both are NULL on every ordinary dispatch, which corrects nothing and
-- records no callback. That is what makes this migration expand-only in ADR
-- 0002's full sense rather than only in the "adds a column" sense: the unique
-- index below constrains ONLY rows written by a binary that knows about it,
-- so it can neither fail against existing data nor make an N-1 binary's
-- INSERT start failing. The rejected alternative -- a unique index over
-- (node_run_id, fencing_token) WHERE supersedes IS NULL -- would have
-- constrained every ordinary attempt row an old binary writes, which is the
-- N-1 promise, not a detail of it. ADR 0012 §5 records the full comparison.
--
-- WHY THE PAIR IS THE KEY. It is what a redelivery repeats. The ingest's own
-- idempotency authority is already the (attempt, event id) claim
-- (ClaimCallbackEvent, migrations/0005's idempotency_keys), and a redelivery
-- is the same event id arriving again; keying the row on the same pair makes
-- the schema fact and the delivery fact one fact instead of two
-- approximations. The attempt id is in the key because event ids are minted
-- by actors: they are unique per invocation, not globally.
--
-- 0028's attempts_supersedes_key is deliberately NOT dropped. Its remaining
-- job is the semantic one only it can do -- a record is corrected at most
-- once, so corrections chain rather than fan out -- while idempotency moves
-- here. An INSERT can arbitrate on one index, so the writer targets this one
-- and resolves a `supersedes` collision (a DIFFERENT late event correcting
-- the same record) by returning the correction already recorded.
ALTER TABLE attempts ADD COLUMN late_callback_attempt_id TEXT;
ALTER TABLE attempts ADD COLUMN late_callback_event_id TEXT;

CREATE UNIQUE INDEX attempts_late_callback_delivery_key
    ON attempts (late_callback_attempt_id, late_callback_event_id)
    WHERE late_callback_event_id IS NOT NULL;

-- The two columns are one key and are always written together. Stating that
-- in the schema keeps a half-written pair -- which would be a row the unique
-- index above silently does not cover -- from being representable at all.
--
-- NOT VALID skips the validation scan: both columns were introduced by the
-- statements above, so every existing row has NULL in both and cannot
-- violate this. New writes are checked regardless of NOT VALID, which is the
-- only population that could. An N-1 binary writes NULL in both and passes.
ALTER TABLE attempts
    ADD CONSTRAINT attempts_late_callback_delivery_pair
    CHECK ((late_callback_attempt_id IS NULL) = (late_callback_event_id IS NULL))
    NOT VALID;

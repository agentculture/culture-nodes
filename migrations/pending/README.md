# Migrations that exist but must not be applied yet

`migrations/*.sql` is applied in filename order by every `nodes migrate`, and
by `pgtest.Run` before every database-backed test. A file placed there **will**
be applied — there is no "not yet" flag, and `//go:embed *.sql` does not
descend into this directory, which is what makes holding one here effective.

A migration belongs here when it is correct, reviewed and complete, but its own
execution has a precondition that is not yet met.

## Current contents

### `0036_retire_stored_participant_addresses.sql`

Drops `actors.endpoint_ref` and `runner_invocations.endpoint` under the
human-approved ADR 0002 bypass (task t24). The rationale and the
non-generalisation warning are in the file itself, which is where task t24's
first criterion requires them.

**Precondition, from the migration and from
`docs/decisions/transport-inversion.md`:** every bridge must be converted to
authenticated dial-in and the outbound fallback disabled *first*. t23 chose
mixed mode, which keeps `endpoint_ref` alive as the fallback the rollback
depends on. Applying this before that cutover removes the rollback path.

Merging it into the applied sequence was tried and reverted the same hour: it
dropped the column while the worker still writes it, and fourteen tests failed
with `column "endpoint_ref" of relation "actors" does not exist`. That is the
precondition being enforced by the database rather than by a comment.

**To apply it:** convert the fleet, disable the outbound fallback, then
`git mv` this file up one directory and run `nodes migrate`. Renumber it if
anything has claimed 0036 in the meantime.

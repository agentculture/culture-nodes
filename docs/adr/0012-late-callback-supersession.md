# ADR 0012: a late callback appends a superseding attempt row, and a superseded row leaves every rollup

- Status: accepted
- Date: 2026-08-15
- Task: t11 (own-the-work-end-to-end plan,
  `docs/plans/2026-08-15-own-the-work-end-to-end.md`)
- Issues: #82 (post-commit cancel / late-callback reconciliation), #77
  (attempt attribution), #28 (per-actor analytics)
- Frame: `docs/specs/2026-08-14-own-the-work-end-to-end.md`, claims c37/c39
  with honesty conditions h16/h26, and scope entry `s23`

## Context

When a node run's deadline expires, `internal/scheduler`'s
`failWaitingExternal` resumes the parked work item, completes the attempt as
`timed_out`, and closes the wait record. The actor session it bounded does
not stop just because the engine stopped waiting — and once the deadline's
cancel reaches the bridge (task t9), the bridge measures its workspace, runs
preserve-on-failure, and reports a `failed` event with everything it knows:
the tokens it burned, the model that burned them, the provider's termination
reason, and the branch it committed the work onto.

§13.4 is right to refuse that report a state commit — the attempt was
replaced, and "completion after cancellation or attempt replacement is
recorded as a late diagnostic event but cannot commit workflow state" is
exactly the rule that keeps a superseded session from resurrecting a run.
But `internal/actors/callback.go`'s `late()` went further than the rule
requires: it appended a `dev.culture.nodes.actor.callback-late` event and
wrote nothing else. The work happened, the actor reported it, and the run's
own history did not show it. Every fact about that session was reachable
only by parsing a diagnostic event body, which means it was invisible to the
API, to the run-detail page, to the usage rollup, and to per-actor
statistics alike.

The obvious fix — write the facts onto an attempts row — collides with two
measured constraints, and the collision is the whole design problem:

1. `migrations/0002_runtime_execution.sql:71` declares
   `attempts_node_run_attempt_number_key UNIQUE (node_run_id,
   attempt_number)`, and `engine_store.go`'s `NextAttemptNumber` mints the
   next number as `MAX(attempt_number) + 1`. A correction therefore *cannot*
   reuse the timed-out attempt's number; it has to take its own.
2. `internal/store/postgres/actorstats.go`'s `loadActorRetryBurnAttempts`
   counts "every attempt this actor made, in scope, regardless of its
   technical outcome", and that count is issue #28's headline
   attempts-per-completion quality measure.

Put together: a naive second row makes one dispatch look like two tries and
inflates the retry burn of an actor that did nothing wrong — re-creating,
one layer down, the exact actor-scoring distortion issue #82 exists to
remove. The spec's scope entry `s23` names this trap directly.

## Decision

### 1. The reconciliation is an appended attempts row, not an update

PRD §10.4: records are immutable, and corrections append with `supersedes`.
The timed-out record is not rewritten, not deleted, and not softened. It
stands exactly as the scheduler wrote it — the deadline really did expire,
and an operator reading the run's history should see that it did. The late
report becomes a *new* row that names the row it corrects.

Rejected alternative: `UPDATE attempts SET usage_... WHERE id = ...` on the
timed-out row. It is one statement and no migration, and it makes an
immutable record lie: the row would carry a `timed_out` status alongside a
usage block the timeout never observed, with nothing anywhere saying the two
facts were learned minutes apart from different reporters.

Rejected alternative: carry the facts somewhere other than `attempts` (a
side table, or the diagnostic event's body). It dodges the retry-burn
problem entirely, and it fails the thing the task exists for — every reader
that matters (`GET /v1alpha1/runs/{id}`, the run-detail page, the usage
rollup, per-actor stats) reads `attempts`. A fact only a bespoke query can
reach is a fact the run does not explain.

### 2. The link is one nullable self-FK: `attempts.supersedes`

`migrations/0028_attempt_supersedes.sql` adds `supersedes TEXT REFERENCES
attempts (id)`, NULL on every ordinary dispatch, plus a partial UNIQUE index
`attempts_supersedes_key ON attempts (supersedes) WHERE supersedes IS NOT
NULL`.

Uniqueness is doing two jobs. It makes "a record is corrected at most once"
a schema fact — a further correction supersedes the correction, forming a
chain rather than a fan-out that would leave "the current record"
ambiguous — and it is what makes appending a reconciliation idempotent under
callback redelivery, which §13.4 guarantees will happen: the ingest's
event-id claim is released whenever processing fails part-way, so the same
late report can legitimately be processed twice, and the second write must
conflict rather than append a twin.

Rejected alternative: a `record_kind` / `is_reconciliation` flag column. It
answers "is this row a correction?" but not "of what?", so it cannot express
the reader rule in §3 below, and it leaves the corrected row in every rollup
alongside its own correction.

### 3. The reader rule: a superseded row leaves every rollup

One sentence, applied uniformly: **a row that another row supersedes is
superseded history; the row that supersedes it is current.** Concretely,
every aggregate over `attempts` gains

```sql
AND NOT EXISTS (SELECT 1 FROM attempts sup WHERE sup.supersedes = a.id)
```

and this is what keeps the retry burn honest without special-casing
anything: the timed-out row drops out at the moment its correction lands, so
one deadline reconciliation still describes one attempt. The same rule makes
the usage rollup read the tokens the session actually reported instead of
the superseded record's silence, and stops one dispatch counting once as
"reported" and once as "not reported".

The rule is applied to per-actor retry burn, duration percentiles, usage
totals and usage cost-by-currency (`actorstats.go`), and to the namespace
and run usage rollups (`usage_rollup.go`). It is deliberately NOT applied to
the run-detail read path: `Attempts(nodeRunID)` returns both rows, because a
reader reconstructing what happened needs to see that the deadline fired
*and* that the session later reported. The `supersedes` field on the wire is
what tells them which is which.

### 4. The deadline's own attempt is attributed to its actor

`failWaitingExternal` built its `engine.CompletionRequest` without an
`ActorID`, so every deadline-expired attempt persisted `actor_id` NULL —
invisible to the per-actor statistics the correction is careful not to
distort. Fixing that is part of this decision rather than a separate one:
without it, "the count is unchanged before and after a reconciliation" is
true only because both numbers are zero, and the correction would appear to
create burn out of nothing. The invocation record already holds the resolved
actor row id (migration 0015), so this is attribution the deployment already
measured, not an inference.

## Consequences

- A node run whose deadline expires and whose session later reports back has
  **two** attempt rows: the `timed_out` record, and the correction carrying
  usage, `usage_model`, `termination_reason`, `continuation_ref` and the
  preserve block. Anything counting raw rows per node run sees two; anything
  applying §3's rule sees one.
- `attempt_number` is not dense per node run in the way it was: a correction
  consumes a number. Numbers were already only an ordering, never a count of
  tries (the unique constraint is what they exist for), and §3's rule is
  what counting goes through now.
- A reconciliation that finds no earlier record for its own fencing tuple —
  the "a newer worker reclaimed the item" flavour of §13.4 lateness, as
  distinct from a deadline — appends a row with `supersedes` NULL. That row
  counts as its own attempt, which is correct: a second session genuinely
  ran, and the reclaiming worker records its own row too. The idempotency
  the UNIQUE index provides does not cover this case; a redelivery whose
  first pass failed after the append can leave two such rows.
- An N-1 binary (ADR 0002) keeps counting superseded rows because it cannot
  know about the correction. That is exactly what it does today, so it stays
  self-consistent through a rollout rather than becoming wrong.
- The `supersedes` link is surfaced on the Attempt API schema, so a reader
  seeing two rows is told which of them the deployment considers current.
  Surfacing the *usage* block on that schema is task t14's work, not this
  one's; until it lands, the API shows the correction exists without showing
  everything the correction carries.

# The hand-turn ledger

Every step a person performs by hand between a finished actor run and a landed
change is a **hand-turn**, and as of task t16 of `loop-closure-claude-codex`
(issue #319, decision c25) a hand-turn is a ledger record, not a sentence in an
issue. This page is the operator recipe: how a turn gets recorded, how it gets
confirmed, and which number a delivery summary may cite.

## The loop, in one paragraph

A person **defines** what counts as a hand-turn and at which stage — a
`hand_turn_definition` record (`schemas/ledger/hand_turn_definition.schema.json`)
carrying `stages` and `rules`, written proposed and confirmed through a review
like any other record. An **observing agent** (`examples/hand-turn-observer`)
reads a work item's runs, handover refs, PR events and issue comments and
writes `hand_turn` records (`schemas/ledger/hand_turn.schema.json`:
`what`, `stage`, `work_item`, `definition_ref`) as `origin=agent authority=proposed`
against that definition. The person **confirms or rejects the batch** in one
review (`nodes review create` / `commit`, or the web review surface) and
iterates the definition — a new record with `supersedes` — from what the
observer got wrong. A person can also enter a turn the observer missed:
`nodes hand-turn "<what>" --stage <stage> --work-item KEY`; it lands proposed
under their identity and is confirmed the same way.

## What did not change

The producer/authority matrix (`internal/ledger/authority.go`). An agent
proposes; a human confirms through a review transaction; a human cannot write
`observed` (that belongs to a measuring runner) and cannot write `confirmed`
outside a review — there is deliberately no grade-style direct-confirm
carve-out for hand-turns, because a hand-turn is a claim about the world, not
an opinion. `internal/ledger/handturn_test.go` and
`internal/api/handturns_test.go` pin each of those refusals.

## The number a delivery summary cites

`GET /v1alpha1/actors/{id}/stats` gains `hand_turns_by_stage`: **confirmed**
`hand_turn` records only — a `review` record with `authority: confirmed`
names the turn — per `stage` and `work_item`, on runs the actor attempted,
in the total and in each category bucket. Proposed and rejected turns count
zero. The scope is the actor whose *work* needed the turn, not whoever
noticed it, so the number reads as "how much hand-work did this actor's runs
need to land" — the comparative fact the dogfooding reflex collects.

That count is only as good as the ledger having one record per turn, which
is why the observer de-duplicates before it posts: it re-recognises a work
item's whole history on every tick and the create route appends without an
idempotency key, so an un-guarded rerun would put two records under one turn
and a person confirming both would double the number above. The guard is
read-then-write (`examples/hand-turn-observer/README.md` states what it does
not cover), so **one observation of a work item at a time** is an operating
rule, not a detail.

When `/summarize-delivery` writes a cycle's summary, the hand-turn count it
reports is that confirmed count, read from the work item's ledger
(`nodes ledger records <run-id>` filtered to confirmed `hand_turn` records,
or the per-actor `hand_turns_by_stage` buckets), and the summary says so:
*"N hand-turns this cycle (confirmed `hand_turn` records on SCRUM-K), of the
six recurring turns in c18: gone — …; remaining — …"*. A count that comes
from issue prose or from memory is not that number and must not be written as
if it were. Proposed-but-unreviewed turns are reported separately, as
"M proposed, not yet reviewed", never folded into the count.

## Recipe

```bash
# 1. the definition (once per iteration): write it proposed, then confirm it.
#    POST /v1alpha1/hand-turn-definitions files it against the work item's
#    newest run; an iteration names the record it replaces in "supersedes"
#    -- another hand_turn_definition, never a hand_turn or any other record
#    (naming one is refused 400, because it would drop that record from
#    every projection, including the confirmed count above).
jq -n --arg wi SCRUM-9 --arg actor <your-actor-id> \
  --slurpfile d examples/hand-turn-observer/definition.json \
  '$d[0] + {work_item: $wi, actor_id: $actor}' |
  curl -s -X POST "$NODES_API_URL/v1alpha1/hand-turn-definitions" \
    -H "Authorization: Bearer $DECISION_BEARER" -H 'Content-Type: application/json' -d @-
# DECISION_BEARER is the same bearer `nodes human-tasks decide --token` takes:
# a person's decision credential, never an agent lane's token (c45).
nodes review create <run-id> --records <definition-id> --ledger-version N
nodes review commit <review-id> --confirm <definition-id> --ledger-version N

# 2. observe one work item (offline core; --post to append as the observer).
#    Re-runnable: --post reads the item's existing hand_turn records first and
#    skips the turns already filed, so a second tick -- or a retry after a
#    batch failed halfway -- does not file a second copy. The output reports
#    `proposed` (recognised), `posted` (appended) and `already_recorded`.
python3 examples/hand-turn-observer/observer.py --inputs pr.json \
  --definition examples/hand-turn-observer/definition.json --definition-ref <definition-id>

# 3. confirm the batch on the work item's newest run
nodes ledger records <run-id>
nodes review create <run-id> --records id1,id2,... --ledger-version N
nodes review commit <review-id> --confirm id1,id2 --reject id3 --ledger-version N

# 4. a turn the observer missed
nodes hand-turn "reset the thor checkout over ssh" --stage dispatch --work-item SCRUM-9 --as <your-actor-id>

# 5. read the count
curl -s "$NODES_API_URL/v1alpha1/actors/<actor-id>/stats" | jq '.total.hand_turns_by_stage'
```

Every hand-turn is still an issue or an issue comment too (CLAUDE.md's
"every piece of operator work opens or updates an issue"); the record is what
makes the issue trail *countable*, it does not replace it.

# spec-chain-lane — the board-driven /think leg, as sessions on one lane

Plan `jira-flow-spec-read-related-bugs`, task t13; issue #199 (dispatch
log #230); spec claim c7 / honesty h5; frame decisions **q1** (custody)
and **q4 = B** (sessions, not the code-node graph).

[`examples/spec-chain`](../spec-chain/) expresses the whole devague chain as
a graph of `code` nodes running each move through the runner boundary. It
compiles and is unwired; that form is deferred to #237. This directory is
the leg a **ticket** can drive today:

- a Jira ticket transitioning to *In Progress* mints a run
  (`trigger_event_id` is the sweep's ticket fact);
- the developer lane runs the `/think` moves as `devague` commands in its
  own worktree, commits `.devague/` after every move, and posts the frame
  snapshot to `POST /v1alpha1/tickets/{id}/frame` after every move;
- every frame decision a person must make is posted to the ticket as a
  **marked question** through the narrow jira-comment actor;
- the person's reply — a Jira comment picked up by the sweep, or a reply on
  the ticket page (one schema, `schemas/events/jira_comment.schema.json`) —
  transacts **exactly one stated devague move** and nothing else.

## What is deterministic, and where

devague makes no LLM call (devague#20); the session is the judgement. Three
things are deliberately not judgement:

| Fact | Where it is pinned |
|---|---|
| The stated move: `devague question --resolve <qid> --decision <reply> --frame <slug>` | `frame_moves.py` `stated_move` / `transact`; `tests/test_spec_chain_lane.py` |
| Which wake is this run's answer | the `correlate*` **decision** nodes in `workflow.yaml`, evaluating the same three clauses as `question_correlation.correlates` (same id, an answer present, not self-originated) — at engine cost, before any session is billed |
| The marker id `<frame-slug>.<devague qid>` (e.g. `scrum-9.q1`) | `frame_moves.py` `question_id`; the jira-comment actor appends the marker line itself |

Confirmations stay a **user** act made off the board (spec c7): no reply
ever transacts `devague confirm`, and a frame blocked on confirmations parks
on a human task instead of asking the board.

## Custody

Only the developer lane may write `.devague/`, and only in its own
worktree. That is declared twice and enforced twice:

- the lane's bridge config carries a `custody` block (`checkout`,
  `branch_prefix`, `devague_write: true`); every mover in this graph binds
  `devague_write: true`, and the bridge answers the request from its own
  declaration — 403 on any lane that cannot honour it;
- the operator's `nodes-op.sh assign --devague-write` refuses every other
  actor before anything is billed.

## The bound

Three board decisions per run, then a person. Bindings are static pointers,
so each round's question comes from a named node (`think`, `transact`,
`transact-2`); a fourth decision reaches `needs-human`. A run that parks
there leaves the frame committed and posted; moving the ticket back through
*To Do* to *In Progress* mints a fresh run that continues the same frame.

## Honest gaps

- **Agent-reported moves.** A session saying "I ran `devague capture`" is a
  `proposed` completion claim (PRD §10.4). The committed `.devague/` and the
  posted snapshot are what a reader checks it against; the engine-verified
  form is the code-node graph (#237).
- **Cross-ticket wakes** (#239). A signal park matches `(namespace,
  event_name)` only, so every ticket's reply wakes every parked round; the
  decision nodes re-park an uncorrelated wake at engine cost, bounded by
  `maxVisitsPerNode`.
- **No silence timeout.** A question nobody answers parks its round
  indefinitely, exactly as `examples/jira-question-round-trip` documents.

Operating it — publishing, granting the token, triggering, reading the
result — is `docs/operations/spec-chain-lane.md`.

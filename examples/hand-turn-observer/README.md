# Hand-turn observer

The agent half of the human-guided hand-turn loop (issue #319, decision c25;
task t16 of `loop-closure-claude-codex`). A person **defines** what counts as a
hand-turn and at which stage — a `hand_turn_definition` ledger record they
confirm; an **agent** observes one work item's history and writes `hand_turn`
records as `origin=agent authority=proposed` against that definition; the
person **confirms or rejects the batch** through the ordinary review surface
and iterates the definition from what the observer got wrong. The
producer/authority matrix is unchanged; `GET /v1alpha1/actors/{id}/stats`
reports `hand_turns_by_stage` over the confirmed subset only, and that is the
number a delivery summary cites (`docs/operations/hand-turn-ledger.md`).

## Files

| File | What it is |
|------|------------|
| `observer.py` | Stdlib-only node program. `propose(inputs, definition)` is the deterministic core; `http_json` is the one network helper (`list_runs`, `fetch_runs`, `recorded_identities`, `post_proposals`). |
| `definition.json` | The first definition's `data` — four rules read off PR #307. The ledger record is the authority; this is its published copy for the bootstrap to fetch by digest. |
| `workflow.yaml` | One code node (`observe`) that fetches the observer and definition by digest, reads the item's runs, proposes, and posts; then an approval node where the graph waits for the review. Its `input.bindings` (`item: /run/input`) is what makes the engine forward the run input as `NODES_INPUT_JSON` — a code node without one is dispatched with nothing. |
| `../../tests/fixtures/hand-turn-observer/pr-307.json` | PR #307's shape as neutral placeholders: two runs, two handover refs, eight `review-fix/*` branches created and deleted, the `a029689` cherry-pick, three operator replies on loop-fixed threads, one ssh checkout reset noted on #286 — plus the near-misses the rules must not match. |

## Run it by hand

```bash
python3 examples/hand-turn-observer/observer.py \
  --inputs tests/fixtures/hand-turn-observer/pr-307.json \
  --definition examples/hand-turn-observer/definition.json \
  --definition-ref <ledger id of the confirmed definition>
```

That prints the batch (13 proposals for the fixture: 1 cherry-pick, 8
branch-deletes, 3 replies, 1 reset) without touching the network. A dispatched
run gets the same document from `$NODES_INPUT_JSON` instead, under the node's
`item` binding — `--inputs` accepts either shape. Add
`--fetch-runs` to read the item's runs from `$NODES_API_URL`, and `--post` to
append each proposal through `POST /v1alpha1/hand-turns` as
`$NODES_OBSERVER_ACTOR_ID` with `$NODES_HAND_TURN_TOKEN` (the observer actor's
own bearer — it opens that route and nothing else).

## Why `--post` reads before it writes

The recogniser reads a work item's **whole** history, so every rerun
re-recognises every turn it recognised last time — and
`POST /v1alpha1/hand-turns` appends, with no idempotency key and no
existing-record check. Left alone, a second tick on the same item, or a retry
after a batch failed halfway through, would file a second copy of turns
already on the ledger; two copies confirmed separately are **two** hand-turns
in `hand_turns_by_stage`, which is the number a delivery summary cites
(`docs/operations/hand-turn-ledger.md`). The observer running twice would
inflate the count of how much hand-work the loop cost.

So `--post` first reads the item's existing `hand_turn` records across all of
its runs (`GET /v1alpha1/runs/{id}/ledger`, unauthenticated like the runs
listing — this needs no wider grant than the observer already has) and skips
the proposals that repeat one. The output says so: `proposed` is the whole
recognised batch, `posted` is what was appended, `already_recorded` is the
difference.

Two hand-turns are **the same turn** when the work item, the rule that fired,
the `observed_at` of the source event and the matched `evidence_refs` all
match. `what` is out of that identity because it is generated prose that moves
when a rule's wording does; `definition_ref` is out because iterating the
definition is the loop this node exists for, and an iteration must not re-file
what the previous definition already found. Rejected records count as recorded
too: a turn a person ruled on is not re-proposed each tick — the fix for a
wrong rejection is a new definition.

**The residual.** This is read-then-write, not an idempotency key, so it does
not close the window between two observer runs on the same item overlapping —
both can read before either writes. The graph runs the node once per dispatch
(`maxVisitsPerNode: 1`, `retry.maxAttempts: 1`), so the shapes that actually
occur are covered; do not schedule concurrent observations of one work item.
Closing the race needs an idempotency identity enforced inside the append
transaction, which is a control-plane change rather than a node one.

## Collecting the inputs

The node reads runs live and takes PR events and comments pre-fetched, so it
holds no GitHub credential. The operator lane collects them with `gh api`:

- commits: `gh api repos/{owner}/{repo}/pulls/{n}/commits` → `pr_events` entries
  `{type: commit, sha, author, branch, message, at}`;
- branch creations/deletions: `gh api repos/{owner}/{repo}/events` filtered to
  `CreateEvent`/`DeleteEvent` with `ref_type: branch` → `{type: branch_created|branch_deleted, branch, author, at}`;
- review comments and issue comments: `gh api repos/{owner}/{repo}/pulls/{n}/comments`
  and `.../issues/{n}/comments` → `issue_comments` entries `{author, body, url, at, thread_fixed_by_loop}`,
  where `thread_fixed_by_loop` is true for a thread whose fix the land node pushed;
- handover refs: `git ls-remote origin 'refs/heads/handover/*'` → `{ref, commit, run_id}`.

`human_logins` in the definition names the people whose acts count; a bot
login never proposes a hand-turn, whatever it did.

## Writing the definition

The definition is itself a ledger record (`POST /v1alpha1/hand-turn-definitions`,
same decision bearer as `nodes hand-turn`): `definition.json` plus `work_item`
and `actor_id` is the request body, and the record lands proposed under the
person's identity until they confirm it through a review. An iteration is a
new record naming the old one in `supersedes`; a second replacement of the
same record is refused. The published `definition.json` the node fetches by
digest must match the confirmed record's `data` -- the record is the authority.

## Confirming the batch

```bash
nodes ledger records <run-id>                       # the work item's newest run
nodes review create <run-id> --records id1,id2,... --ledger-version N
nodes review commit <review-id> --confirm id1,id2 --reject id3 --ledger-version N
```

A rejected proposal names its `rule`; change the definition (a new record
with `supersedes`), not the observer. A person can also file a turn the
observer missed directly: `nodes hand-turn "<what>" --stage <stage> --work-item KEY`
— it lands proposed under their identity and is confirmed the same way.
